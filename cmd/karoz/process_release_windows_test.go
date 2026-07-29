//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	processdomain "github.com/karoz/karoz/internal/process"
)

func TestBackgroundProcessGuardHelper(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "process-guard" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	os.Exit(runBackgroundProcessGuard(os.Args[index+1:]))
}

func TestWindowsBackgroundReleaseLifecycle(t *testing.T) {
	projectsRoot := t.TempDir()
	projectPath := filepath.Join(projectsRoot, "project")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := projectFromPath(projectPath, projectsRoot, "main")
	dataDir := t.TempDir()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: projectsRoot})
	agent := Agent{
		ID: "owner", ProjectID: project.ID, Name: "owner",
		CreatedAt: time.Now().UTC(),
	}
	a.agents[project.ID] = []Agent{
		{ID: "karoz", ProjectID: project.ID, Name: "karoz"},
		agent,
	}
	if err := a.bootstrapProcessRuntime(); err != nil {
		t.Fatal(err)
	}
	a.processSupervisor.config.GuardExecutable = os.Args[0]
	a.processSupervisor.config.GuardArgsPrefix = []string{
		"-test.run=TestBackgroundProcessGuardHelper",
		"--",
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.shutdownProcessRuntime(ctx); err != nil {
			t.Errorf("shutdown process runtime: %v", err)
		}
	})

	start := func(ctx context.Context, command, runID string) string {
		t.Helper()
		result, err := a.executeResidentRunBackgroundTool(
			ctx,
			ResidentToolContext{
				Project: project, Agent: agent, RunID: runID,
				Workdir: project.Path, TurnType: "dev",
			},
			map[string]any{"command": command},
		)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Process processView `json:"process"`
		}
		if err := json.Unmarshal([]byte(result), &payload); err != nil ||
			payload.Process.ID == "" {
			t.Fatalf("start %q = %s err=%v", command, result, err)
		}
		return payload.Process.ID
	}

	outputID := start(context.Background(), "echo windows-output", "windows-output")
	outputRecord := waitWindowsBackgroundTerminal(t, a, project.ID, outputID)
	if outputRecord.State != processdomain.StateSucceeded {
		t.Fatalf("Windows output process = %+v", outputRecord)
	}
	waitWindowsTerminalReleased(t, a, project.ID, outputID)
	messages := a.agentMessagesFor(project.ID, agent.ID)
	if len(messages) != 1 ||
		messages[0].ID != processTerminalEventID(outputID) ||
		messages[0].Intent != "process_terminal" {
		t.Fatalf("Windows durable terminal message = %+v", messages)
	}

	handler := a.httpHandler()
	for _, suffix := range []string{
		"/processes/" + outputID,
		"/processes/" + outputID + "/log?tail=true&limit=20",
	} {
		request := httptest.NewRequest(
			http.MethodGet,
			"/api/projects/"+project.ID+suffix,
			nil,
		)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("fresh Windows read %s status=%d body=%s", suffix, response.Code, response.Body.String())
		}
		if strings.Contains(suffix, "/log") &&
			!strings.Contains(response.Body.String(), "windows-output") {
			t.Fatalf("fresh Windows log = %s", response.Body.String())
		}
	}

	creator, cancelCreator := context.WithCancel(context.Background())
	longID := start(
		creator,
		"ping -n 30 127.0.0.1 >NUL",
		"windows-context-survival",
	)
	cancelCreator()
	time.Sleep(100 * time.Millisecond)
	if record, err := a.processRecord(project.ID, longID); err != nil ||
		record.State != processdomain.StateRunning {
		t.Fatalf("Windows creator context owned process: %+v err=%v", record, err)
	}
	stopResult, err := a.executeResidentStopProcessTool(
		context.Background(),
		ResidentToolContext{
			Project: project, Agent: agent, RunID: "windows-stop",
			Workdir: project.Path, TurnType: "dev",
		},
		map[string]any{"process_id": longID},
	)
	if err != nil || !strings.Contains(stopResult, `"state":"killed"`) {
		t.Fatalf("Windows stop = %s err=%v", stopResult, err)
	}
	waitWindowsTerminalReleased(t, a, project.ID, longID)

	ownedID := start(
		context.Background(),
		"ping -n 30 127.0.0.1 >NUL",
		"windows-owner-delete",
	)
	if err := a.deleteProjectAgent(project, agent.ID); err != nil {
		t.Fatal(err)
	}
	owned := waitWindowsBackgroundTerminal(t, a, project.ID, ownedID)
	if owned.State != processdomain.StateKilled ||
		owned.Error != "owner deleted" {
		t.Fatalf("Windows owner deletion = %+v", owned)
	}
	waitWindowsTerminalReleased(t, a, project.ID, ownedID)

	if err := a.processRuntime.ApplyRetention(
		project.ID,
		processdomain.RetentionPolicy{MaxRecords: 0, MaxTotalBytes: 0},
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if records := a.processRuntime.List(project.ID); len(records) != 0 {
		t.Fatalf("Windows retention kept records: %+v", records)
	}
	if _, _, err := a.processRuntime.OpenLogReader(
		project.ID,
		outputID,
	); !errors.Is(err, errProcessLogGone) {
		t.Fatalf("Windows retained log error=%v, want gone", err)
	}
}

func waitWindowsBackgroundTerminal(
	t *testing.T,
	a *app,
	projectID, processID string,
) processdomain.Process {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		record, err := a.processRecord(projectID, processID)
		if err == nil && record.State.Terminal() {
			return record
		}
		time.Sleep(20 * time.Millisecond)
	}
	record, err := a.processRecord(projectID, processID)
	t.Fatalf("Windows process %s did not become terminal: %+v err=%v", processID, record, err)
	return processdomain.Process{}
}

func waitWindowsTerminalReleased(
	t *testing.T,
	a *app,
	projectID, processID string,
) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		project := a.processRuntime.projectRuntime(projectID)
		a.processRuntime.authorityMu.Lock()
		partition := a.processRuntime.authority.Projects[project.identity.SafeProjectKey]
		record, exists := partition.Records[processID]
		a.processRuntime.authorityMu.Unlock()
		project.ledgerMu.Lock()
		slots := len(project.ledger.Slots)
		project.ledgerMu.Unlock()
		if exists &&
			record.Event == nil &&
			record.Reservation == nil &&
			record.AcknowledgedEventID == processTerminalEventID(processID) &&
			slots == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Windows process %s terminal reservation was not released", processID)
}
