//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"context"
	"encoding/json"
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
	a.agents[project.ID] = []Agent{owner, otherAgent}
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
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	agent := Agent{
		ID:        "deletable",
		ProjectID: project.ID,
		Name:      "deletable",
		Nickname:  "Deletable",
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
	if record.State != "killed" {
		t.Fatalf("owned process after agent delete = %+v", record)
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
	a.agents[project.ID] = []Agent{agent}
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
