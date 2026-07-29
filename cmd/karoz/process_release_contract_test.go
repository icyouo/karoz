package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestResidentCommandApprovalSubjectIsCanonicalAndOperationSeparated(
	t *testing.T,
) {
	workdir := t.TempDir()
	command := "printf 'approval-contract'"
	foreground, err := newResidentBashSubject(
		residentBashOperationForeground,
		"project-a",
		"agent-a",
		workdir,
		command,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	start, err := newResidentBashSubject(
		residentBashOperationBackgroundStart,
		"project-a",
		"agent-a",
		workdir,
		command,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	stopOne, err := newResidentBashSubject(
		residentBashOperationBackgroundStop,
		"project-a",
		"agent-a",
		workdir,
		command,
		"process-one",
	)
	if err != nil {
		t.Fatal(err)
	}
	stopTwo := stopOne
	stopTwo.ProcessID = "process-two"

	canonical, err := start.canonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	expected := fmt.Sprintf(
		`{"version":1,"operation":"background_start","project_id":"project-a","agent_id":"agent-a","canonical_workdir":%q,"process_id":"","command_sha256":%q}`,
		start.CanonicalWorkdir,
		start.CommandSHA256,
	)
	if string(canonical) != expected {
		t.Fatalf("canonical subject = %s, want %s", canonical, expected)
	}

	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	context := ResidentToolContext{
		Project:  Project{ID: "project-a", Name: "demo", Path: workdir},
		Agent:    Agent{ID: "agent-a", ProjectID: "project-a"},
		RunID:    "run-one",
		Workdir:  workdir,
		TurnType: "ask",
	}
	request := a.requestResidentBashApprovalSubject(context, start, command)
	choiceID := bashChoiceID(t, request, residentBashApprovePrefix)
	if recognized, err := a.resolveResidentBashChoice(
		"project-b",
		context.Agent.ID,
		context.RunID,
		choiceID,
	); !recognized || err == nil {
		t.Fatalf("cross-project approval recognized=%t err=%v", recognized, err)
	}
	if recognized, err := a.resolveResidentBashChoice(
		context.Project.ID,
		context.Agent.ID,
		context.RunID,
		choiceID,
	); !recognized || err != nil {
		t.Fatalf("resolve start approval recognized=%t err=%v", recognized, err)
	}
	if a.consumeResidentBashApprovalSubject(context.RunID, foreground) {
		t.Fatal("background-start approval authorized foreground execution")
	}
	if !a.consumeResidentBashApprovalSubject(context.RunID, start) {
		t.Fatal("exact background-start approval was not consumed")
	}
	if a.consumeResidentBashApprovalSubject(context.RunID, start) {
		t.Fatal("background-start approval was replayed")
	}

	request = a.requestResidentBashApprovalSubject(context, stopOne, command)
	choiceID = bashChoiceID(t, request, residentBashApprovePrefix)
	if recognized, err := a.resolveResidentBashChoice(
		context.Project.ID,
		context.Agent.ID,
		context.RunID,
		choiceID,
	); !recognized || err != nil {
		t.Fatalf("resolve stop approval recognized=%t err=%v", recognized, err)
	}
	if a.consumeResidentBashApprovalSubject(context.RunID, stopTwo) {
		t.Fatal("stop approval authorized a different process")
	}
	if !a.consumeResidentBashApprovalSubject(context.RunID, stopOne) {
		t.Fatal("exact stop approval was not consumed")
	}
}

func TestProcessReleaseToolEffectPolicy(t *testing.T) {
	for _, name := range []string{"list_processes", "read_process_log"} {
		if residentToolHasSideEffects(name) {
			t.Fatalf("%s unexpectedly marked effectful", name)
		}
	}
	for _, name := range []string{"run_background", "stop_process"} {
		if !residentToolHasSideEffects(name) {
			t.Fatalf("%s unexpectedly marked read-only", name)
		}
	}

	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	now := time.Now().UTC()
	job := ScheduledRun{
		ID:        "read-only-process-observation",
		ProjectID: "project-a",
		AgentID:   "agent-a",
		Kind:      ScheduledRunTaskEvent,
		Status:    ScheduledRunQueued,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if result := a.ensureSchedulerQueue().Enqueue(job); !result.Accepted {
		t.Fatalf("enqueue read-only fixture: %+v", result)
	}
	for _, name := range []string{"list_processes", "read_process_log"} {
		arguments := `{}`
		if name == "read_process_log" {
			arguments = `{"process_id":"missing"}`
		}
		_, _ = a.executeResidentTool(
			context.Background(),
			ResidentToolContext{
				Project:       Project{ID: job.ProjectID, Path: t.TempDir()},
				Agent:         Agent{ID: job.AgentID, ProjectID: job.ProjectID},
				RunID:         job.ID,
				TurnType:      "ask",
				EnforcePolicy: true,
			},
			codexToolCall{Name: name, Arguments: arguments},
		)
		stored, found := a.ensureSchedulerQueue().Job(job.ID)
		if !found || stored.EffectsStarted {
			t.Fatalf("%s crossed effects barrier: %+v found=%t", name, stored, found)
		}
	}
}

func TestProcessMutationBoundaryRunsBeforeProjectLookup(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	handler := a.httpHandler()
	path := "/api/projects/missing/processes/not-found/stop"

	tests := []struct {
		name, origin, contentType string
		want                      int
	}{
		{name: "missing origin", contentType: "application/json", want: http.StatusForbidden},
		{name: "cross origin", origin: "http://127.0.0.1:9090", contentType: "application/json", want: http.StatusForbidden},
		{name: "non loopback origin", origin: "http://example.test:8088", contentType: "application/json", want: http.StatusForbidden},
		{name: "text plain", origin: "http://127.0.0.1:8088", contentType: "text/plain", want: http.StatusUnsupportedMediaType},
		{name: "form", origin: "http://127.0.0.1:8088", contentType: "application/x-www-form-urlencoded", want: http.StatusUnsupportedMediaType},
		{name: "valid boundary reaches lookup", origin: "http://127.0.0.1:8088", contentType: "application/json", want: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPost,
				path,
				strings.NewReader(`{"ignored":true}`),
			)
			request.Host = "127.0.0.1:8088"
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf(
					"status = %d, want %d; body=%s",
					response.Code,
					test.want,
					response.Body.String(),
				)
			}
		})
	}
}

func TestProcessReleaseConfigDefaultsAndFailClosedCeilings(t *testing.T) {
	names := []string{
		"KAROZ_PROCESS_MAX_CONCURRENT",
		"KAROZ_PROCESS_MAX_LIFETIME",
		"KAROZ_PROCESS_LOG_MAX_BYTES",
		"KAROZ_PROCESS_TAIL_LINES",
		"KAROZ_PROCESS_OUTPUT_EVENT_MAX_BYTES",
		"KAROZ_PROCESS_EXIT_DRAIN",
		"KAROZ_PROCESS_TERMINAL_MAX_RECORDS",
		"KAROZ_PROCESS_TERMINAL_RETENTION",
		"KAROZ_PROCESS_LOG_TOTAL_BYTES",
	}
	for _, name := range names {
		t.Setenv(name, "")
	}
	config, err := processReleaseConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.Supervisor.MaxConcurrent != 8 ||
		config.Supervisor.MaxLifetime != time.Hour ||
		config.Supervisor.LogBytes != 8<<20 ||
		config.Supervisor.TailLines != 200 ||
		config.Supervisor.OutputEventBytes != 8<<10 ||
		config.Supervisor.ExitDrain != 250*time.Millisecond ||
		config.Retention.MaxRecords != 200 ||
		config.Retention.MaxAge != 7*24*time.Hour ||
		config.Retention.MaxTotalBytes != 256<<20 {
		encoded, _ := json.Marshal(config)
		t.Fatalf("unexpected defaults: %s", encoded)
	}

	invalid := map[string]string{
		"KAROZ_PROCESS_MAX_CONCURRENT":         "0",
		"KAROZ_PROCESS_MAX_LIFETIME":           "24h1s",
		"KAROZ_PROCESS_LOG_MAX_BYTES":          "-1",
		"KAROZ_PROCESS_TAIL_LINES":             "nope",
		"KAROZ_PROCESS_OUTPUT_EVENT_MAX_BYTES": "65537",
		"KAROZ_PROCESS_EXIT_DRAIN":             "2001ms",
		"KAROZ_PROCESS_TERMINAL_MAX_RECORDS":   "2001",
		"KAROZ_PROCESS_TERMINAL_RETENTION":     "721h",
		"KAROZ_PROCESS_LOG_TOTAL_BYTES":        "2147483649",
	}
	for name, value := range invalid {
		t.Run(name, func(t *testing.T) {
			for _, clear := range names {
				t.Setenv(clear, "")
			}
			t.Setenv(name, value)
			if _, err := processReleaseConfigFromEnv(); err == nil ||
				!strings.Contains(err.Error(), name) {
				t.Fatalf("%s=%q error = %v", name, value, err)
			}
		})
	}
}

func TestReadProcessLogWindowIsBoundedAndRedacted(t *testing.T) {
	file := t.TempDir() + "/process.log"
	body := "normal\nAPI_TOKEN=super-secret\nPASSWORD:also-secret\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	window, err := readProcessLogWindow(reader, "p1", 3, 0, 200, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(window)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "super-secret") ||
		strings.Contains(string(encoded), "also-secret") ||
		!strings.Contains(string(encoded), "[REDACTED]") {
		t.Fatalf("log redaction failed: %s", encoded)
	}
	if len(encoded) > maxProcessLogResponseBytes+4096 {
		t.Fatalf("log response exceeded bound: %d", len(encoded))
	}
}
