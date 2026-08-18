package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
	processdomain "github.com/karoz/karoz/internal/process"
)

func gate6Project(t *testing.T) (*app, Project, Agent) {
	t.Helper()
	root := t.TempDir()
	projectPath := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := projectFromPath(projectPath, root, "main")
	owner := Agent{
		ID:        "owner",
		ProjectID: project.ID,
		Nickname:  "Owner",
		CreatedAt: time.Now().UTC(),
	}
	a := newApp(Settings{
		DataDir:      t.TempDir(),
		ProjectsRoot: root,
	})
	a.agentDirectoryLocked().agents[project.ID] = []Agent{owner}
	return a, project, owner
}

func gate6Monitor(project Project, owner Agent) Monitor {
	now := time.Now().UTC()
	return Monitor{
		ID:        "monitor-one",
		ProjectID: project.ID,
		AgentID:   owner.ID,
		Name:      "Watch tasks",
		Revision:  1,
		Trigger: monitordomain.Trigger{
			Kind:       monitordomain.TriggerRuntimeEvent,
			Revision:   1,
			EventKinds: []string{"task_changed"},
		},
		Action: monitordomain.Action{
			Kind:     monitordomain.ActionNotifyAgent,
			Revision: 1,
			AgentID:  owner.ID,
			TurnType: "ask",
			Template: "Inspect the changed task.",
		},
		State:      monitordomain.StateDisabled,
		CooldownMS: monitordomain.DefaultCooldownMS,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func TestGate6SourceGapAcknowledgementIsSeparateFromResume(t *testing.T) {
	a, project, owner := gate6Project(t)
	item := gate6Monitor(project, owner)
	item.ErrorCode = "source_gap"
	item.LastError = "monitor source coverage has a gap"
	item.SourceGaps = map[string]monitordomain.SourceGapStatus{}
	gap := monitordomain.SourceGapStatus{
		AuthorityID:  "task-store",
		SourceKind:   "task_changed",
		GapVersion:   4,
		FirstVersion: 11,
		LastVersion:  13,
		LostCount:    3,
	}
	item.SourceGaps[monitordomain.SourceGapKey(gap.AuthorityID, gap.SourceKind)] = gap
	a.monitors[project.ID] = []Monitor{item}

	handler := a.httpHandler()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/projects/"+project.ID+"/monitors/"+item.ID+"/acknowledge-gap",
		strings.NewReader(`{"authority_id":"task-store","source_kind":"task_changed","expected_gap_version":4}`),
	)
	request.Host = "127.0.0.1:8088"
	request.Header.Set("Origin", "http://127.0.0.1:8088")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ack status=%d body=%s", response.Code, response.Body.String())
	}
	got := a.monitorsForProject(project.ID)[0]
	if got.State != monitordomain.StateDisabled ||
		got.SourceGaps[monitordomain.SourceGapKey("task-store", "task_changed")].AcknowledgedGapVersion != 4 {
		t.Fatalf("ack enabled monitor or missed exact gap: %+v", got)
	}

	resume := httptest.NewRequest(
		http.MethodPost,
		"/api/projects/"+project.ID+"/monitors/"+item.ID+"/resume",
		strings.NewReader(`{}`),
	)
	resume.Host = "127.0.0.1:8088"
	resume.Header.Set("Origin", "http://127.0.0.1:8088")
	resume.Header.Set("Content-Type", "application/json")
	resumed := httptest.NewRecorder()
	handler.ServeHTTP(resumed, resume)
	if resumed.Code != http.StatusOK {
		t.Fatalf("resume status=%d body=%s", resumed.Code, resumed.Body.String())
	}
	got = a.monitorsForProject(project.ID)[0]
	if got.State != monitordomain.StateActive || got.ErrorCode != "" {
		t.Fatalf("explicit resume did not clear recovered gap: %+v", got)
	}
}

func TestGate6MutationRouteMatrixRejectsUnsafeRequestsBeforeLookup(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	handler := a.httpHandler()
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/projects/missing/processes/process/stop"},
		{http.MethodPost, "/api/projects/missing/monitors"},
		{http.MethodPatch, "/api/projects/missing/monitors/monitor"},
		{http.MethodDelete, "/api/projects/missing/monitors/monitor"},
		{http.MethodPost, "/api/projects/missing/monitors/monitor/check"},
		{http.MethodPost, "/api/projects/missing/monitors/monitor/pause"},
		{http.MethodPost, "/api/projects/missing/monitors/monitor/resume"},
		{http.MethodPost, "/api/projects/missing/monitors/monitor/acknowledge-gap"},
		{http.MethodPost, "/api/projects/missing/monitor-probe-approvals/prepare"},
		{http.MethodPost, "/api/projects/missing/monitor-probe-approvals/challenge/confirm"},
	}
	for _, route := range routes {
		for _, test := range []struct {
			name, origin, contentType string
			status                    int
		}{
			{name: "missing_origin", contentType: "application/json", status: http.StatusForbidden},
			{name: "unknown_origin", origin: "http://example.test:8088", contentType: "application/json", status: http.StatusForbidden},
			{name: "form", origin: "http://127.0.0.1:8088", contentType: "application/x-www-form-urlencoded", status: http.StatusUnsupportedMediaType},
			{name: "text", origin: "http://127.0.0.1:8088", contentType: "text/plain", status: http.StatusUnsupportedMediaType},
		} {
			t.Run(route.method+"_"+strings.ReplaceAll(route.path, "/", "_")+"_"+test.name, func(t *testing.T) {
				request := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
				request.Host = "127.0.0.1:8088"
				if test.origin != "" {
					request.Header.Set("Origin", test.origin)
				}
				request.Header.Set("Content-Type", test.contentType)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != test.status {
					t.Fatalf("status=%d want=%d body=%s", response.Code, test.status, response.Body.String())
				}
			})
		}
	}
}

func TestGate6MonitorPromptObservationIsNonemptyBoundedAndRedacted(t *testing.T) {
	a, project, owner := gate6Project(t)
	for index := 0; index < 7; index++ {
		item := gate6Monitor(project, owner)
		item.ID = "monitor-" + string(rune('a'+index))
		item.Name = "Monitor " + string(rune('A'+index))
		item.State = monitordomain.StateActive
		if index == 0 {
			item.Action.Template = "Authorization: Bearer never-render-this"
		}
		a.monitors[project.ID] = append(a.monitors[project.ID], item)
	}
	var prompt strings.Builder
	a.renderResidentMonitorObservation(&prompt, project.ID, owner.ID)
	output := prompt.String()
	if !strings.Contains(output, "### Active monitors (bounded observation)") ||
		strings.Count(output, "\n- id:") != 5 ||
		strings.Contains(output, "never-render-this") ||
		strings.Contains(output, "Authorization") {
		t.Fatalf("unexpected monitor observation: %s", output)
	}
	var empty strings.Builder
	a.renderResidentMonitorObservation(&empty, project.ID, "other")
	if empty.Len() != 0 {
		t.Fatalf("empty owner observation rendered: %q", empty.String())
	}
}

func TestGate6MonitorListExposesCapabilityWithoutProbeSecrets(t *testing.T) {
	a, project, owner := gate6Project(t)
	item := gate6Monitor(project, owner)
	item.Trigger = monitordomain.Trigger{
		Kind:              monitordomain.TriggerScriptProbe,
		Revision:          2,
		ProbeLanguage:     "shell",
		ProbePath:         "monitor-probes/private/source.sh",
		ProbeSHA256:       strings.Repeat("a", 64),
		IntervalMS:        60_000,
		TimeoutMS:         5_000,
		ApprovalReceiptID: "secret-receipt",
	}
	a.monitors[project.ID] = []Monitor{item}
	request := httptest.NewRequest(http.MethodGet, "/api/projects/"+project.ID+"/monitors", nil)
	response := httptest.NewRecorder()
	a.httpHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Monitors             []Monitor `json:"monitors"`
		ScriptProbeSupported bool      `json:"script_probe_supported"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Monitors) != 1 ||
		payload.Monitors[0].Trigger.ProbePath != "" ||
		payload.Monitors[0].Trigger.ApprovalReceiptID != "" ||
		strings.Contains(response.Body.String(), "secret-receipt") ||
		strings.Contains(response.Body.String(), "private/source.sh") {
		t.Fatalf("monitor list leaked probe authorization: %s", response.Body.String())
	}

	pause := httptest.NewRequest(
		http.MethodPost,
		"/api/projects/"+project.ID+"/monitors/"+item.ID+"/pause",
		strings.NewReader(`{}`),
	)
	pause.Host = "127.0.0.1:8088"
	pause.Header.Set("Origin", "http://127.0.0.1:8088")
	pause.Header.Set("Content-Type", "application/json")
	paused := httptest.NewRecorder()
	a.httpHandler().ServeHTTP(paused, pause)
	if paused.Code != http.StatusOK ||
		strings.Contains(paused.Body.String(), "secret-receipt") ||
		strings.Contains(paused.Body.String(), "private/source.sh") {
		t.Fatalf("monitor mutation leaked probe authorization: status=%d body=%s", paused.Code, paused.Body.String())
	}
}

func TestGate6MonitorRoutesKeepProjectOwnership(t *testing.T) {
	a, project, owner := gate6Project(t)
	otherRoot := t.TempDir()
	otherPath := filepath.Join(otherRoot, "other")
	if err := os.MkdirAll(filepath.Join(otherPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := projectFromPath(otherPath, otherRoot, "extra")
	item := gate6Monitor(project, owner)
	a.monitors[project.ID] = []Monitor{item}

	request := httptest.NewRequest(
		http.MethodPatch,
		"/api/projects/"+other.ID+"/monitors/"+item.ID,
		strings.NewReader(`{"revision":1,"name":"cross-project"}`),
	)
	request.Host = "127.0.0.1:8088"
	request.Header.Set("Origin", "http://127.0.0.1:8088")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.httpHandler().ServeHTTP(response, request)
	if response.Code == http.StatusOK {
		t.Fatalf("cross-project monitor mutation succeeded: %s", response.Body.String())
	}
	if got := a.monitorsForProject(project.ID)[0]; got.Name != item.Name {
		t.Fatalf("cross-project mutation changed owner project: %+v", got)
	}
}

func TestGate6ProcessViewCarriesBoundedCoverageSummary(t *testing.T) {
	now := time.Now().UTC()
	record := processdomain.Process{
		ID:                 "process",
		ProjectID:          "project",
		AgentID:            "owner",
		Command:            "echo ready",
		State:              processdomain.StateRunning,
		StartedAt:          now,
		UpdatedAt:          now,
		OutputGaps:         []processdomain.SeqRange{{Start: 3, End: 4}},
		OutputGapCount:     2,
		OutputLostLines:    2,
		OutputGapOldestSeq: 3,
		OutputGapNewestSeq: 4,
	}
	view := newProcessView(record, "ready", now)
	if len(view.OutputGaps) != 1 ||
		view.OutputGaps[0] != (processdomain.SeqRange{Start: 3, End: 4}) ||
		view.OutputGapCount != 2 || view.OutputLostLines != 2 ||
		view.OutputGapOldestSeq != 3 || view.OutputGapNewestSeq != 4 {
		t.Fatalf("process coverage payload is incomplete: %+v", view)
	}
	record.OutputGaps[0].Start = 99
	if view.OutputGaps[0].Start != 3 {
		t.Fatal("process view aliases durable coverage ranges")
	}
}

func TestGate6FrontendBackgroundActivityContract(t *testing.T) {
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	source, err := staticFS.ReadFile("static/js/background-activity.js")
	if err != nil {
		t.Fatal(err)
	}
	panel, err := staticFS.ReadFile("static/js/panels.js")
	if err != nil {
		t.Fatal(err)
	}
	combined := string(source)
	for _, fragment := range []string{
		"Background activity",
		"Runs on the Karoz server; closing this browser will not stop it.",
		"Karoz restarted; process was not resumed.",
		"Live / best effort",
		"Coverage degraded",
		"Complete creation in a dev agent turn.",
		"Dry-run · action not executed",
		"scheduleBackgroundActivityRefresh",
		"state.sidePanel === 'background' && !state.backgroundEditor",
		"/acknowledge-gap",
	} {
		if !strings.Contains(combined, fragment) {
			t.Fatalf("background product contract missing %q", fragment)
		}
	}
	if !strings.Contains(string(index), "/static/js/background-activity.js") ||
		!strings.Contains(string(panel), "chip('background', 'Background'") {
		t.Fatal("background activity entry is not wired into the side pane")
	}
}

// TestGate6BrowserFixture is an opt-in real-browser harness. It deliberately
// starts the process from the Application, not from an HTTP request, so the UI
// proves it is observing server-owned state rather than request-owned state.
func TestGate6BrowserFixture(t *testing.T) {
	if os.Getenv("KAROZ_GATE6_BROWSER_FIXTURE") != "1" {
		t.Skip("manual browser fixture; set KAROZ_GATE6_BROWSER_FIXTURE=1")
	}
	if runtime.GOOS == "windows" {
		t.Skip("background process fixture is Unix-only")
	}
	root := t.TempDir()
	projectPath := filepath.Join(root, "gate6-browser-fixture")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := projectFromPath(projectPath, root, "main")
	project.Name = "Gate6 Background Fixture"
	owner := Agent{
		ID: "karoz", ProjectID: project.ID, Name: "Karoz",
		Nickname: "Karoz", DisplayName: "Karoz", Runtime: "resident",
		CreatedAt: time.Now().UTC(),
	}
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	a.agentDirectoryLocked().agents[project.ID] = []Agent{owner}
	if err := a.bootstrapProcessRuntime(); err != nil {
		t.Fatal(err)
	}
	a.processSupervisor.config.GuardExecutable = os.Args[0]
	a.processSupervisor.config.GuardArgsPrefix = []string{
		"-test.run=TestBackgroundProcessGuardHelper",
		"--",
	}
	record, err := a.processSupervisor.Start(
		context.Background(),
		processStartRequest{
			ID:          "gate6-browser-process",
			ProjectID:   project.ID,
			AgentID:     owner.ID,
			RunID:       "completed-resident-turn",
			Command:     `i=0; while [ "$i" -lt 300 ]; do echo gate6-line-$i; i=$((i+1)); sleep 1; done`,
			Workdir:     project.Path,
			Description: "Gate6 delayed writer",
			Lifetime:    6 * time.Minute,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.shutdownProcessRuntime(ctx); err != nil {
			t.Errorf("shutdown fixture: %v", err)
		}
	})

	done := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__gate6_fixture/stop" && r.Method == http.MethodPost {
			select {
			case <-done:
			default:
				close(done)
			}
			writeJSON(w, map[string]bool{"stopping": true})
			return
		}
		a.httpHandler().ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:18088")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: withRecovery(handler)}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	t.Logf(
		"Gate6 fixture ready: project=%s process=%s url=http://127.0.0.1:18088",
		project.ID,
		record.ID,
	)
	select {
	case <-done:
	case err := <-serveErr:
		if err != nil && !strings.Contains(err.Error(), "Server closed") {
			t.Fatalf("fixture server: %v", err)
		}
	case <-time.After(7 * time.Minute):
		t.Fatal("Gate6 browser fixture timed out")
	}
}
