//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	processdomain "github.com/karoz/karoz/internal/process"
)

func TestProcessReleaseChildServer(t *testing.T) {
	if os.Getenv("KAROZ_PROCESS_RELEASE_CHILD") != "1" {
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = io.WriteString(w, "resident-child")
	})}
	go func() {
		_ = server.Serve(listener)
	}()
	fmt.Printf("server=http://%s\n", listener.Addr().String())
	time.Sleep(200 * time.Millisecond)
	fmt.Println("delayed-line API_TOKEN=child-secret")
	fmt.Println("Authorization: Bearer prompt-bearer-secret")
	time.Sleep(600 * time.Millisecond)
	_ = server.Close()
}

func TestBackgroundProcessOutlivesRunCreatorAndSSERequest(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "project-a")
	otherPath := filepath.Join(root, "project-b")
	for _, path := range []string{projectPath, otherPath} {
		if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	project := projectFromPath(projectPath, root, "main")
	otherProject := projectFromPath(otherPath, root, "main")
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	owner := Agent{
		ID: "owner", ProjectID: project.ID,
		Name: "owner", Nickname: "Owner",
	}
	otherAgent := Agent{
		ID: "other-agent", ProjectID: project.ID,
		Name: "other-agent", Nickname: "Other",
	}
	a.agentDirectoryLocked().agents[project.ID] = []Agent{owner, otherAgent}
	if err := a.bootstrapProcessRuntime(); err != nil {
		t.Fatal(err)
	}
	a.processSupervisor.config.GuardExecutable = os.Args[0]
	a.processSupervisor.config.GuardArgsPrefix = []string{
		"-test.run=TestBackgroundProcessGuardHelper",
		"--",
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := a.shutdownProcessRuntime(ctx); err != nil {
			t.Errorf("shutdown process runtime: %v", err)
		}
	})
	if a.processRuntime.projectRuntime(otherProject.ID) == nil {
		t.Fatal("second project was not registered")
	}

	run, started := a.beginAgentRun(AgentRunInput{
		RunID: "release-run", ProjectID: project.ID, AgentID: owner.ID,
		Trigger: RunTriggerUserDirect, TurnType: "dev",
	})
	if !started {
		t.Fatal("resident run did not start")
	}
	creator, cancelCreator := context.WithCancel(context.Background())
	command := fmt.Sprintf(
		"KAROZ_PROCESS_RELEASE_CHILD=1 %q -test.run=TestProcessReleaseChildServer --",
		os.Args[0],
	)
	result, err := a.executeResidentTool(
		creator,
		ResidentToolContext{
			Project: project, Agent: owner, RunID: run.ID,
			Workdir: project.Path, TurnType: "dev", EnforcePolicy: true,
		},
		codexToolCall{
			Name: "run_background",
			Arguments: toolJSON(map[string]any{
				"command": command, "description": "request-lifetime fixture",
			}),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var startedResult struct {
		Process processView `json:"process"`
	}
	if err := json.Unmarshal([]byte(result), &startedResult); err != nil {
		t.Fatalf("decode start result %s: %v", result, err)
	}
	processID := startedResult.Process.ID
	if processID == "" {
		t.Fatalf("start result missing process: %s", result)
	}

	studio := httptest.NewServer(a.httpHandler())
	defer studio.Close()
	sseContext, cancelSSE := context.WithCancel(context.Background())
	sseRequest, err := http.NewRequestWithContext(
		sseContext,
		http.MethodGet,
		studio.URL+"/api/projects/"+project.ID+"/agents/"+owner.ID+
			"/runs/"+run.ID+"/events",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	sseResponse, err := studio.Client().Do(sseRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = sseResponse.Body.Close()
	cancelSSE()
	cancelCreator()
	a.finishAgentRun(
		project.ID,
		owner.ID,
		run.ID,
		RunStateDone,
		nil,
	)

	freshClient := &http.Client{Timeout: 2 * time.Second}
	var childURL string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		logResponse, requestErr := freshClient.Get(
			studio.URL + "/api/projects/" + project.ID +
				"/processes/" + processID + "/log?tail=true&limit=20",
		)
		if requestErr == nil {
			body, _ := io.ReadAll(logResponse.Body)
			_ = logResponse.Body.Close()
			if logResponse.StatusCode == http.StatusOK {
				var payload struct {
					Log processLogWindow `json:"log"`
				}
				if json.Unmarshal(body, &payload) == nil {
					for _, line := range payload.Log.Lines {
						if strings.HasPrefix(line, "server=http://") {
							childURL = strings.TrimPrefix(line, "server=")
						}
					}
				}
			}
		}
		if childURL != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childURL == "" {
		t.Fatal("fresh client did not observe the child server after creator/SSE cancellation")
	}
	childResponse, err := freshClient.Get(childURL)
	if err != nil {
		t.Fatalf("fresh client could not reach resident child server: %v", err)
	}
	childBody, _ := io.ReadAll(childResponse.Body)
	_ = childResponse.Body.Close()
	if string(childBody) != "resident-child" {
		t.Fatalf("child server response = %q", childBody)
	}

	otherList, err := a.executeResidentListProcessesTool(
		context.Background(),
		ResidentToolContext{Project: project, Agent: otherAgent},
		map[string]any{"limit": 100},
	)
	if err != nil || strings.Contains(otherList, processID) {
		t.Fatalf("cross-agent list leaked process: %s err=%v", otherList, err)
	}
	otherRead, _ := a.executeResidentReadProcessLogTool(
		context.Background(),
		ResidentToolContext{Project: project, Agent: otherAgent},
		map[string]any{"process_id": processID},
	)
	if !strings.Contains(otherRead, `"error":"not_found"`) {
		t.Fatalf("cross-agent log read = %s", otherRead)
	}
	otherStop, _ := a.executeResidentStopProcessTool(
		context.Background(),
		ResidentToolContext{
			Project: project, Agent: otherAgent, TurnType: "dev",
		},
		map[string]any{"process_id": processID},
	)
	if !strings.Contains(otherStop, `"error":"not_found"`) {
		t.Fatalf("cross-agent stop = %s", otherStop)
	}
	crossProject, err := freshClient.Get(
		studio.URL + "/api/projects/" + otherProject.ID +
			"/processes/" + processID,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, crossProject.Body)
	_ = crossProject.Body.Close()
	if crossProject.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-project detail status = %d", crossProject.StatusCode)
	}

	var final processView
	for time.Now().Before(deadline) {
		detailResponse, requestErr := freshClient.Get(
			studio.URL + "/api/projects/" + project.ID +
				"/processes/" + processID,
		)
		if requestErr == nil {
			body, _ := io.ReadAll(detailResponse.Body)
			_ = detailResponse.Body.Close()
			var payload struct {
				Process processView `json:"process"`
			}
			if detailResponse.StatusCode == http.StatusOK &&
				json.Unmarshal(body, &payload) == nil {
				final = payload.Process
				if final.Terminal {
					break
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !final.Terminal || !final.Succeeded {
		t.Fatalf("fresh client terminal snapshot = %+v", final)
	}
	finalLog, err := freshClient.Get(
		studio.URL + "/api/projects/" + project.ID +
			"/processes/" + processID + "/log?tail=true&limit=20",
	)
	if err != nil {
		t.Fatal(err)
	}
	finalLogBody, _ := io.ReadAll(finalLog.Body)
	_ = finalLog.Body.Close()
	if !strings.Contains(string(finalLogBody), "delayed-line") ||
		strings.Contains(string(finalLogBody), "child-secret") ||
		strings.Contains(string(finalLogBody), "prompt-bearer-secret") ||
		!strings.Contains(string(finalLogBody), "[REDACTED]") {
		t.Fatalf("terminal log contract failed: %s", finalLogBody)
	}

	prompt := a.buildResidentAgentPromptWithMemoryQuery(
		project,
		owner,
		"status",
		"ask",
		"",
	)
	if !strings.Contains(prompt, processID) ||
		strings.Contains(prompt, command) ||
		strings.Contains(prompt, "child-secret") ||
		strings.Contains(prompt, "prompt-bearer-secret") ||
		strings.Contains(prompt, "canonical_workdir") {
		t.Fatalf("prompt process observation leaked or omitted data: %s", prompt)
	}

	for attempt := 0; attempt < 2; attempt++ {
		request, err := http.NewRequest(
			http.MethodPost,
			studio.URL+"/api/projects/"+project.ID+
				"/processes/"+processID+"/stop",
			strings.NewReader(`{}`),
		)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", studio.URL)
		request.Header.Set("Content-Type", "application/json")
		response, err := freshClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("idempotent stop %d status = %d", attempt, response.StatusCode)
		}
	}
}

func TestDeletingAgentStopsItsOwnedBackgroundProcesses(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := projectFromPath(projectPath, root, "main")
	dataDir := t.TempDir()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	agent := Agent{
		ID:        "deletable",
		ProjectID: project.ID,
		Name:      "deletable",
		Nickname:  "Deletable",
	}
	a.agentDirectoryLocked().agents[project.ID] = []Agent{
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
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := a.shutdownProcessRuntime(ctx); err != nil {
			t.Errorf("shutdown process runtime: %v", err)
		}
	})
	result, err := a.executeResidentRunBackgroundTool(
		context.Background(),
		ResidentToolContext{
			Project:  project,
			Agent:    agent,
			RunID:    "delete-owner",
			Workdir:  project.Path,
			TurnType: "dev",
		},
		map[string]any{"command": "sleep 30"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Process processView `json:"process"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil ||
		payload.Process.ID == "" {
		t.Fatalf("start process result = %s err=%v", result, err)
	}
	if err := a.deleteProjectAgent(project, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, exists := a.projectAgent(project, agent.ID); exists {
		t.Fatal("agent remained registered after deletion")
	}
	record, err := a.processRecord(project.ID, payload.Process.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != processdomain.StateKilled ||
		record.Error != "owner deleted" {
		t.Fatalf("owned process after agent delete = %+v", record)
	}
	shutdownContext, shutdownCancel := context.WithTimeout(
		context.Background(),
		3*time.Second,
	)
	if err := a.shutdownProcessRuntime(shutdownContext); err != nil {
		shutdownCancel()
		t.Fatal(err)
	}
	shutdownCancel()
	restartedApp := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	if err := restartedApp.loadAgents(); err != nil {
		t.Fatal(err)
	}
	if _, exists := restartedApp.projectAgent(project, agent.ID); exists {
		t.Fatal("deleted agent returned after restart")
	}
	restarted, err := newProcessRuntimePersistence(
		dataDir,
		[]Project{project},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	var durable processdomain.Process
	for _, candidate := range restarted.List(project.ID) {
		if candidate.ID == payload.Process.ID {
			durable = candidate
			break
		}
	}
	if durable.ID == "" ||
		durable.State != processdomain.StateKilled ||
		durable.Error != "owner deleted" {
		t.Fatalf("restarted owner-deleted process = %+v", durable)
	}
}

func TestDeletingAgentFencesApprovalsAndCancelsBeforeProcessStop(
	t *testing.T,
) {
	fixture := newBackgroundProcessTestFixture(t)
	enteredFinalize := make(chan struct{})
	releaseFinalize := make(chan struct{})
	fixture.app.processSupervisor.config.BeforeFinalize = func() {
		close(enteredFinalize)
		<-releaseFinalize
	}
	processID := startBackgroundProcessFixture(t, fixture)
	runCancelled := make(chan struct{})
	key := projectAgentKey(fixture.project.ID, fixture.agent.ID)
	fixture.app.mu.Lock()
	fixture.app.agentRuntimeLocked().cancels[key] = func() { close(runCancelled) }
	fixture.app.mu.Unlock()

	deleteResult := make(chan error, 1)
	go func() {
		deleteResult <- fixture.app.deleteProjectAgent(
			fixture.project,
			fixture.agent.ID,
		)
	}()
	select {
	case <-enteredFinalize:
	case <-time.After(3 * time.Second):
		t.Fatal("owner deletion did not reach the process terminal barrier")
	}
	select {
	case <-runCancelled:
	default:
		t.Fatal("owner run was not cancelled before process stop")
	}
	request := fixture.app.requestResidentBashApproval(
		ResidentToolContext{
			Project: fixture.project,
			Agent:   fixture.agent,
		},
		"printf stale",
	)
	if !strings.Contains(request, `"error":"owner_not_found"`) {
		t.Fatalf("deleting owner created an approval: %s", request)
	}
	fixture.app.mu.Lock()
	approvalCount := len(fixture.app.agentRuntimeLocked().residentBashApprovals)
	fixture.app.mu.Unlock()
	if approvalCount != 0 {
		t.Fatalf("deleting owner retained %d approvals", approvalCount)
	}
	close(releaseFinalize)
	select {
	case err := <-deleteResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("owner deletion did not finish")
	}
	record, err := fixture.app.processRecord(fixture.project.ID, processID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != processdomain.StateKilled ||
		record.Error != "owner deleted" {
		t.Fatalf("fenced owner process = %+v", record)
	}
}

func TestRunBackgroundApprovalSeparationAndInstantExit(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := projectFromPath(projectPath, root, "main")
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	agent := Agent{
		ID:        "owner",
		ProjectID: project.ID,
		Name:      "owner",
		Nickname:  "Owner",
	}
	a.agentDirectoryLocked().agents[project.ID] = []Agent{agent}
	if err := a.bootstrapProcessRuntime(); err != nil {
		t.Fatal(err)
	}
	a.processSupervisor.config.GuardExecutable = os.Args[0]
	a.processSupervisor.config.GuardArgsPrefix = []string{
		"-test.run=TestBackgroundProcessGuardHelper",
		"--",
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := a.shutdownProcessRuntime(ctx); err != nil {
			t.Errorf("shutdown process runtime: %v", err)
		}
	})
	toolContext := ResidentToolContext{
		Project:  project,
		Agent:    agent,
		RunID:    "approval-run",
		Workdir:  project.Path,
		TurnType: "ask",
	}
	request, err := a.executeResidentRunBackgroundTool(
		context.Background(),
		toolContext,
		map[string]any{"command": "true"},
	)
	if err != nil || !toolResultIsChoiceRequest(request) {
		t.Fatalf("background approval request = %s err=%v", request, err)
	}
	choiceID := bashChoiceID(t, request, residentBashApprovePrefix)
	if recognized, err := a.resolveResidentBashChoice(
		project.ID,
		agent.ID,
		toolContext.RunID,
		choiceID,
	); !recognized || err != nil {
		t.Fatalf("resolve background approval recognized=%t err=%v", recognized, err)
	}
	foreground, err := a.executeResidentBashTool(
		context.Background(),
		toolContext,
		map[string]any{"command": "true"},
	)
	if err != nil || !toolResultIsChoiceRequest(foreground) {
		t.Fatalf("background approval authorized foreground: %s err=%v", foreground, err)
	}
	result, err := a.executeResidentRunBackgroundTool(
		context.Background(),
		toolContext,
		map[string]any{"command": "true"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Process processView `json:"process"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatalf("decode instant result %s: %v", result, err)
	}
	if payload.Process.ID == "" ||
		(payload.Process.State != "running" && payload.Process.State != "succeeded") {
		t.Fatalf("instant process start snapshot = %+v", payload.Process)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		record, lookupErr := a.processRecord(project.ID, payload.Process.ID)
		if lookupErr == nil && record.State.Terminal() {
			if !record.State.Succeeded() {
				t.Fatalf("instant process terminal snapshot = %+v", record)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("instant process did not reach a terminal snapshot: %+v", payload.Process)
}

func TestLiveBackgroundTerminalEventIsVisibleOnceAndReleasesRetention(
	t *testing.T,
) {
	fixture := newBackgroundProcessTestFixture(t)
	events := make(chan RuntimeEvent, 4)
	fixture.app.addRuntimeWatcher(fixture.project.ID, events)
	defer fixture.app.removeRuntimeWatcher(fixture.project.ID, events)

	result, err := fixture.app.executeResidentRunBackgroundTool(
		context.Background(),
		ResidentToolContext{
			Project: fixture.project, Agent: fixture.agent,
			RunID: "live-terminal", Workdir: fixture.project.Path,
			TurnType: "dev",
		},
		map[string]any{"command": "true"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Process processView `json:"process"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil ||
		payload.Process.ID == "" {
		t.Fatalf("start terminal process = %s err=%v", result, err)
	}
	processID := payload.Process.ID
	var terminal RuntimeEvent
	select {
	case terminal = <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("live terminal event was not delivered")
	}
	if terminal.ID != processTerminalEventID(processID) ||
		terminal.Kind != processTerminalEventKind ||
		terminal.ProjectID != fixture.project.ID ||
		terminal.AgentID != fixture.agent.ID ||
		terminal.EntityID != processID ||
		terminal.RunID != "live-terminal" ||
		terminal.To != string(processdomain.StateSucceeded) ||
		terminal.ExitCode == nil ||
		*terminal.ExitCode != 0 {
		t.Fatalf("live terminal provenance = %+v", terminal)
	}
	select {
	case duplicate := <-events:
		t.Fatalf("duplicate live terminal event = %+v", duplicate)
	case <-time.After(250 * time.Millisecond):
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		record, lookupErr := fixture.app.processRecord(
			fixture.project.ID,
			processID,
		)
		if lookupErr == nil && record.State.Terminal() {
			durable, slots := processTerminalDurableRecord(
				t,
				fixture.app.processRuntime,
				fixture.project.ID,
				processID,
			)
			if durable.Event == nil && durable.Reservation == nil && slots == 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertProcessTerminalReleased(
		t,
		fixture.app.processRuntime,
		fixture.project.ID,
		processID,
	)
	if err := fixture.app.processRuntime.ApplyRetention(
		fixture.project.ID,
		processdomain.RetentionPolicy{
			MaxRecords: 0, MaxTotalBytes: 0,
		},
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if records := fixture.app.processRuntime.List(fixture.project.ID); len(records) != 0 {
		t.Fatalf("released terminal process resisted retention: %+v", records)
	}
	if _, _, err := fixture.app.processRuntime.OpenLogReader(
		fixture.project.ID,
		processID,
	); !errors.Is(err, errProcessLogGone) {
		t.Fatalf("retired live process log error = %v, want gone", err)
	}
}

type backgroundProcessTestFixture struct {
	app          *app
	project      Project
	agent        Agent
	projectsRoot string
	dataDir      string
}

func newBackgroundProcessTestFixture(t *testing.T) backgroundProcessTestFixture {
	t.Helper()
	projectsRoot := t.TempDir()
	projectPath := filepath.Join(projectsRoot, "project")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := projectFromPath(projectPath, projectsRoot, "main")
	dataDir := t.TempDir()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: projectsRoot})
	agent := Agent{
		ID: "owner", ProjectID: project.ID, Name: "owner", Nickname: "Owner",
	}
	a.agentDirectoryLocked().agents[project.ID] = []Agent{agent}
	if err := a.bootstrapProcessRuntime(); err != nil {
		t.Fatal(err)
	}
	a.processSupervisor.config.GuardExecutable = os.Args[0]
	a.processSupervisor.config.GuardArgsPrefix = []string{
		"-test.run=TestBackgroundProcessGuardHelper",
		"--",
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := a.shutdownProcessRuntime(ctx); err != nil {
			t.Errorf("shutdown process runtime: %v", err)
		}
	})
	return backgroundProcessTestFixture{
		app: a, project: project, agent: agent,
		projectsRoot: projectsRoot, dataDir: dataDir,
	}
}

func runningBackgroundScheduledRun(
	t *testing.T,
	a *app,
	project Project,
	agent Agent,
	id string,
) ScheduledRun {
	t.Helper()
	now := time.Now().UTC()
	job := ScheduledRun{
		ID: id, ProjectID: project.ID, AgentID: agent.ID,
		Kind: ScheduledRunTaskEvent, Status: ScheduledRunQueued,
		MaxAttempts: 3, CreatedAt: now, UpdatedAt: now,
	}
	if result := a.ensureSchedulerQueue().Enqueue(job); !result.Accepted {
		t.Fatalf("enqueue %s: %+v", id, result)
	}
	claimed, ok := a.ensureSchedulerQueue().Claim(
		projectAgentKey(project.ID, agent.ID),
		now.Add(time.Millisecond),
	)
	if !ok || claimed.ID != id || claimed.Status != ScheduledRunRunning {
		t.Fatalf("claim %s: %+v ok=%t", id, claimed, ok)
	}
	return claimed
}

func scheduledRunFromDisk(
	t *testing.T,
	dataDir, runID string,
) ScheduledRun {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dataDir, "agent-run-queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot scheduledRunSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, job := range snapshot.Jobs {
		if job.ID == runID {
			return job
		}
	}
	t.Fatalf("scheduled run %s absent from %s", runID, body)
	return ScheduledRun{}
}

func startBackgroundProcessFixture(
	t *testing.T,
	fixture backgroundProcessTestFixture,
) string {
	t.Helper()
	result, err := fixture.app.executeResidentRunBackgroundTool(
		context.Background(),
		ResidentToolContext{
			Project: fixture.project, Agent: fixture.agent,
			Workdir: fixture.project.Path, TurnType: "dev",
		},
		map[string]any{"command": "sleep 30"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Process processView `json:"process"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil ||
		payload.Process.ID == "" {
		t.Fatalf("start fixture = %s err=%v", result, err)
	}
	return payload.Process.ID
}

func TestRunBackgroundWorkdirContainmentPrecedesApprovalAndEffects(t *testing.T) {
	fixture := newBackgroundProcessTestFixture(t)
	subdir := filepath.Join(fixture.project.Path, "subdir")
	sibling := filepath.Join(fixture.projectsRoot, "sibling")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkEscape := filepath.Join(fixture.project.Path, "escape")
	if err := os.Symlink(sibling, symlinkEscape); err != nil {
		t.Fatal(err)
	}

	for index, workdir := range []string{fixture.project.Path, subdir} {
		result, err := fixture.app.executeResidentRunBackgroundTool(
			context.Background(),
			ResidentToolContext{
				Project: fixture.project, Agent: fixture.agent,
				RunID:   fmt.Sprintf("allowed-%d", index),
				Workdir: workdir, TurnType: "ask",
			},
			map[string]any{"command": fmt.Sprintf("echo allowed-%d", index)},
		)
		if err != nil || !toolResultIsChoiceRequest(result) {
			t.Fatalf("allowed workdir %q = %s err=%v", workdir, result, err)
		}
	}
	fixture.app.mu.Lock()
	fixture.app.agentRuntimeLocked().residentBashApprovals = map[string]ResidentBashApproval{}
	fixture.app.mu.Unlock()

	invalid := []struct {
		name, workdir string
	}{
		{name: "parent", workdir: fixture.projectsRoot},
		{name: "sibling", workdir: sibling},
		{name: "symlink escape", workdir: symlinkEscape},
	}
	for index, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			runID := fmt.Sprintf("invalid-%d", index)
			askResult, askErr := fixture.app.executeResidentRunBackgroundTool(
				context.Background(),
				ResidentToolContext{
					Project: fixture.project, Agent: fixture.agent,
					RunID: runID + "-ask", Workdir: test.workdir, TurnType: "ask",
				},
				map[string]any{"command": "sleep 30"},
			)
			if askErr != nil ||
				!strings.Contains(askResult, `"error":"validation_error"`) {
				t.Fatalf("invalid ask workdir %q = %s err=%v", test.workdir, askResult, askErr)
			}
			fixture.app.mu.Lock()
			approvalCount := len(fixture.app.agentRuntimeLocked().residentBashApprovals)
			fixture.app.mu.Unlock()
			if approvalCount != 0 {
				t.Fatalf("invalid workdir created %d approvals", approvalCount)
			}

			runningBackgroundScheduledRun(
				t, fixture.app, fixture.project, fixture.agent, runID,
			)
			result, err := fixture.app.executeResidentRunBackgroundTool(
				context.Background(),
				ResidentToolContext{
					Project: fixture.project, Agent: fixture.agent,
					RunID: runID, Workdir: test.workdir, TurnType: "dev",
				},
				map[string]any{"command": "sleep 30"},
			)
			if err != nil ||
				!strings.Contains(result, `"error":"validation_error"`) {
				t.Fatalf("invalid workdir %q = %s err=%v", test.workdir, result, err)
			}
			job, found := fixture.app.ensureSchedulerQueue().Job(runID)
			if !found || job.EffectsStarted {
				t.Fatalf("invalid workdir crossed effects barrier: %+v found=%t", job, found)
			}
			if records := fixture.app.processRuntime.List(fixture.project.ID); len(records) != 0 {
				t.Fatalf("invalid workdir spawned processes: %+v", records)
			}
		})
	}
}

func TestBackgroundEffectsMarkerSaveFailureRetriesBeforeEffect(t *testing.T) {
	for _, operation := range []string{
		residentBashOperationBackgroundStart,
		residentBashOperationBackgroundStop,
	} {
		t.Run(operation, func(t *testing.T) {
			fixture := newBackgroundProcessTestFixture(t)
			processID := ""
			if operation == residentBashOperationBackgroundStop {
				processID = startBackgroundProcessFixture(t, fixture)
			}
			runID := "retry-" + operation
			runningBackgroundScheduledRun(
				t, fixture.app, fixture.project, fixture.agent, runID,
			)
			saveErr := errors.New("injected scheduled-run save failure")
			fixture.app.agentRuntimeLocked().scheduledRunsSaveOverride = func(
				scheduledRunSnapshot,
			) error {
				return saveErr
			}
			invoke := func() (string, error) {
				toolContext := ResidentToolContext{
					Project: fixture.project, Agent: fixture.agent,
					RunID: runID, Workdir: fixture.project.Path, TurnType: "dev",
				}
				if operation == residentBashOperationBackgroundStart {
					return fixture.app.executeResidentRunBackgroundTool(
						context.Background(),
						toolContext,
						map[string]any{"command": "sleep 30"},
					)
				}
				return fixture.app.executeResidentStopProcessTool(
					context.Background(),
					toolContext,
					map[string]any{"process_id": processID},
				)
			}
			result, err := invoke()
			if !errors.Is(err, saveErr) ||
				!strings.Contains(result, `"error":"effect_barrier_failed"`) {
				t.Fatalf("first %s = %s err=%v", operation, result, err)
			}
			if _, err := os.Stat(filepath.Join(
				fixture.dataDir,
				"agent-run-queue.json",
			)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed save unexpectedly durable: %v", err)
			}
			stored, found := fixture.app.ensureSchedulerQueue().Job(runID)
			if !found || !stored.EffectsStarted {
				t.Fatalf("in-memory marker missing after failed save: %+v", stored)
			}
			if operation == residentBashOperationBackgroundStart {
				if records := fixture.app.processRuntime.List(fixture.project.ID); len(records) != 0 {
					t.Fatalf("start effect ran after failed marker save: %+v", records)
				}
			} else {
				record, lookupErr := fixture.app.processRecord(
					fixture.project.ID,
					processID,
				)
				if lookupErr != nil || record.State.Terminal() {
					t.Fatalf("stop effect ran after failed marker save: %+v err=%v", record, lookupErr)
				}
			}

			fixture.app.agentRuntimeLocked().scheduledRunsSaveOverride = nil
			result, err = invoke()
			if err != nil {
				t.Fatalf("retry %s = %s err=%v", operation, result, err)
			}
			durable := scheduledRunFromDisk(t, fixture.dataDir, runID)
			if !durable.EffectsStarted || durable.Status != ScheduledRunRunning {
				t.Fatalf("retry marker was not durable before effect: %+v", durable)
			}
			if operation == residentBashOperationBackgroundStart {
				var payload struct {
					Process processView `json:"process"`
				}
				if err := json.Unmarshal([]byte(result), &payload); err != nil ||
					payload.Process.ID == "" {
					t.Fatalf("retry start = %s err=%v", result, err)
				}
			} else {
				record, lookupErr := fixture.app.processRecord(
					fixture.project.ID,
					processID,
				)
				if lookupErr != nil || record.State != "killed" {
					t.Fatalf("retry stop = %+v err=%v", record, lookupErr)
				}
			}
		})
	}
}

func TestBackgroundEffectsMarkerUncertainSaveCrashSuppressesReplay(t *testing.T) {
	for _, operation := range []string{
		residentBashOperationBackgroundStart,
		residentBashOperationBackgroundStop,
	} {
		t.Run(operation, func(t *testing.T) {
			fixture := newBackgroundProcessTestFixture(t)
			processID := ""
			if operation == residentBashOperationBackgroundStop {
				processID = startBackgroundProcessFixture(t, fixture)
			}
			runID := "uncertain-" + operation
			runningBackgroundScheduledRun(
				t, fixture.app, fixture.project, fixture.agent, runID,
			)
			uncertainErr := errors.New("injected uncertain scheduled-run save")
			fixture.app.agentRuntimeLocked().scheduledRunsSaveOverride = func(
				snapshot scheduledRunSnapshot,
			) error {
				if err := writeJSONFileAtomic(
					filepath.Join(fixture.dataDir, "agent-run-queue.json"),
					snapshot,
					0o644,
				); err != nil {
					return err
				}
				return uncertainErr
			}
			toolContext := ResidentToolContext{
				Project: fixture.project, Agent: fixture.agent,
				RunID: runID, Workdir: fixture.project.Path, TurnType: "dev",
			}
			var result string
			var err error
			if operation == residentBashOperationBackgroundStart {
				result, err = fixture.app.executeResidentRunBackgroundTool(
					context.Background(),
					toolContext,
					map[string]any{"command": "sleep 30"},
				)
			} else {
				result, err = fixture.app.executeResidentStopProcessTool(
					context.Background(),
					toolContext,
					map[string]any{"process_id": processID},
				)
			}
			if !errors.Is(err, uncertainErr) ||
				!strings.Contains(result, `"error":"effect_barrier_failed"`) {
				t.Fatalf("uncertain %s = %s err=%v", operation, result, err)
			}
			durable := scheduledRunFromDisk(t, fixture.dataDir, runID)
			if !durable.EffectsStarted || durable.Status != ScheduledRunRunning {
				t.Fatalf("uncertain marker was not durable: %+v", durable)
			}
			if operation == residentBashOperationBackgroundStart {
				if records := fixture.app.processRuntime.List(fixture.project.ID); len(records) != 0 {
					t.Fatalf("uncertain start performed effect: %+v", records)
				}
			} else {
				record, lookupErr := fixture.app.processRecord(
					fixture.project.ID,
					processID,
				)
				if lookupErr != nil || record.State.Terminal() {
					t.Fatalf("uncertain stop performed effect: %+v err=%v", record, lookupErr)
				}
			}

			restarted := newApp(Settings{
				DataDir: fixture.dataDir, ProjectsRoot: fixture.projectsRoot,
			})
			if err := restarted.loadScheduledRuns(); err != nil {
				t.Fatal(err)
			}
			recovered, found := restarted.ensureSchedulerQueue().Job(runID)
			if !found || recovered.Status != ScheduledRunFailed ||
				!recovered.EffectsStarted ||
				!strings.Contains(recovered.Error, "retry suppressed") {
				t.Fatalf("crash recovery replayed uncertain effect: %+v found=%t", recovered, found)
			}
			recoveredDisk := scheduledRunFromDisk(t, fixture.dataDir, runID)
			if recoveredDisk.Status != ScheduledRunFailed ||
				!recoveredDisk.EffectsStarted {
				t.Fatalf("crash recovery was not durable: %+v", recoveredDisk)
			}
		})
	}
}
