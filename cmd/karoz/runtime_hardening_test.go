package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStaleRunCannotMutateReplacementRun(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir()}
	agent := Agent{ID: "designer", ProjectID: project.ID}
	a.agents[project.ID] = []Agent{agent}

	first, started := a.beginAgentRun(AgentRunInput{RunID: "run-old", ProjectID: project.ID, AgentID: agent.ID, Trigger: RunTriggerUserDirect})
	if !started {
		t.Fatal("old run did not start")
	}
	if _, ok := a.transitionAgentRun(project.ID, agent.ID, first.ID, RunStateInvokingModel); !ok {
		t.Fatal("old run did not enter model state")
	}
	if _, ok := a.cancelAgentRun(project.ID, agent.ID); !ok {
		t.Fatal("old run did not cancel")
	}
	second, started := a.beginAgentRun(AgentRunInput{RunID: "run-new", ProjectID: project.ID, AgentID: agent.ID, Trigger: RunTriggerUserDirect})
	if !started {
		t.Fatal("replacement run did not start")
	}
	interruptMessage := a.appendAgentMessage(project.ID, agent.ID, "user", "interrupt", "new run input")
	if _, ok := a.enqueueAgentInterrupt(project.ID, agent.ID, interruptMessage, "ask"); !ok {
		t.Fatal("replacement interrupt was not queued")
	}

	if _, ok := a.transitionAgentRun(project.ID, agent.ID, first.ID, RunStateExecutingTool); ok {
		t.Fatal("stale transition changed replacement run")
	}
	if _, ok := a.appendAgentMessageForRun(project.ID, agent.ID, first.ID, "tool_result", "remember_fact", `{"ok":true}`); ok {
		t.Fatal("stale run appended a tool result")
	}
	if drained := a.drainAgentInterrupts(project.ID, agent.ID, first.ID); len(drained) != 0 {
		t.Fatalf("stale run drained replacement interrupts: %+v", drained)
	}
	result, err := a.executeResidentTool(context.Background(), ResidentToolContext{
		Project: project, Agent: agent, Workdir: project.Path, RunID: first.ID,
		TurnType: "ask", EnforceRunScope: true, EnforcePolicy: true,
	}, codexToolCall{Name: "remember_fact", Arguments: `{"summary":"stale","detail":"must not persist"}`})
	if !errors.Is(err, context.Canceled) || !strings.Contains(result, "stale_run") {
		t.Fatalf("stale tool result=%s err=%v", result, err)
	}
	if got := a.activeMemoriesFor(project.ID, agent.ID, "fact", 10); len(got) != 0 {
		t.Fatalf("stale tool persisted memory: %+v", got)
	}
	if drained := a.drainAgentInterrupts(project.ID, agent.ID, second.ID); len(drained) != 1 || drained[0].Body != "new run input" {
		t.Fatalf("replacement interrupt = %+v", drained)
	}
	if active, ok := a.activeAgentRun(project.ID, agent.ID); !ok || active.ID != second.ID || active.State != RunStatePreparingContext {
		t.Fatalf("replacement run changed: %+v active=%v", active, ok)
	}
}

func TestResidentToolPolicyAndReadOnlyRepositoryTools(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "app.go"), []byte("package demo\n\nfunc Runtime() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	project := Project{ID: "p1", Name: "demo", Path: root}
	agent := Agent{ID: "designer", ProjectID: project.ID}

	askSpecs := func(turn string) map[string]bool {
		return toolSpecNames(a.residentToolSpecsForContext(context.Background(), ResidentToolContext{
			Project: project, Agent: agent, Workdir: root, TurnType: turn,
		}))
	}
	ask := askSpecs("ask")
	if !ask["repo_list"] || !ask["repo_read"] || !ask["repo_search"] || !ask["bash"] || ask["write_workspace_file"] || ask["create_task"] {
		t.Fatalf("ask policy = %+v", ask)
	}
	plan := askSpecs("plan")
	if !plan["write_workspace_file"] || !plan["bash"] || plan["create_task"] {
		t.Fatalf("plan policy = %+v", plan)
	}
	dev := askSpecs("dev")
	if !dev["write_workspace_file"] || !dev["create_task"] || !dev["bash"] {
		t.Fatalf("dev policy = %+v", dev)
	}

	ctx := ResidentToolContext{Project: project, Agent: agent, Workdir: root, TurnType: "ask", EnforcePolicy: true}
	read, err := a.executeResidentTool(context.Background(), ctx, codexToolCall{Name: "repo_read", Arguments: `{"path":"src/app.go"}`})
	if err != nil || !strings.Contains(read, "func Runtime") {
		t.Fatalf("repo_read = %s err=%v", read, err)
	}
	search, err := a.executeResidentTool(context.Background(), ctx, codexToolCall{Name: "repo_search", Arguments: `{"query":"Runtime"}`})
	if err != nil || !strings.Contains(search, "src/app.go") {
		t.Fatalf("repo_search = %s err=%v", search, err)
	}
	escape, err := a.executeResidentTool(context.Background(), ctx, codexToolCall{Name: "repo_read", Arguments: `{"path":"escape"}`})
	if err != nil || !strings.Contains(escape, "escapes the project workspace") {
		t.Fatalf("symlink escape = %s err=%v", escape, err)
	}
	sensitive, err := a.executeResidentTool(context.Background(), ctx, codexToolCall{Name: "repo_read", Arguments: `{"path":".env"}`})
	if err != nil || !strings.Contains(sensitive, "not available") || strings.Contains(sensitive, "TOKEN") {
		t.Fatalf("sensitive read = %s err=%v", sensitive, err)
	}
	listed, err := a.executeResidentTool(context.Background(), ctx, codexToolCall{Name: "repo_list", Arguments: `{}`})
	if err != nil || strings.Contains(listed, `.env`) {
		t.Fatalf("sensitive list = %s err=%v", listed, err)
	}
	forbidden, err := a.executeResidentTool(context.Background(), ctx, codexToolCall{Name: "write_workspace_file", Arguments: `{"path":"x.txt","content":"x"}`})
	if err != nil || !strings.Contains(forbidden, "tool_forbidden") {
		t.Fatalf("forbidden write = %s err=%v", forbidden, err)
	}
}

func TestResidentBashApprovalPolicy(t *testing.T) {
	root := t.TempDir()
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	project := Project{ID: "p1", Name: "demo", Path: root}
	agent := Agent{ID: "designer", ProjectID: project.ID, Nickname: "Designer"}
	runID := "run-ask"
	runBash := func(turn, command string) (string, error) {
		args, err := json.Marshal(map[string]any{"command": command})
		if err != nil {
			return "", err
		}
		return a.executeResidentTool(context.Background(), ResidentToolContext{
			Project: project, Agent: agent, RunID: runID, Workdir: root, TurnType: turn, EnforcePolicy: true,
		}, codexToolCall{Name: "bash", Arguments: string(args)})
	}

	askCommand := "printf approved > ask-approved.txt"
	requested, err := runBash("ask", askCommand)
	if err != nil || !toolResultIsChoiceRequest(requested) {
		t.Fatalf("ask approval request = %s err=%v", requested, err)
	}
	if _, err := os.Stat(filepath.Join(root, "ask-approved.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ask command ran before approval: %v", err)
	}
	approveID := bashChoiceID(t, requested, residentBashApprovePrefix)
	if _, err := a.resolveResidentBashChoice(project.ID, "other-agent", runID, approveID); err == nil {
		t.Fatal("approval was accepted for a different agent")
	}
	if recognized, err := a.resolveResidentBashChoice(project.ID, agent.ID, runID, "yes"); recognized || err != nil {
		t.Fatalf("plain yes resolved approval: recognized=%t err=%v", recognized, err)
	}
	if recognized, err := a.resolveResidentBashChoice(project.ID, agent.ID, runID, approveID); !recognized || err != nil {
		t.Fatalf("approval resolution: recognized=%t err=%v", recognized, err)
	}

	mismatched, err := runBash("ask", "printf wrong > wrong.txt")
	if err != nil || !toolResultIsChoiceRequest(mismatched) {
		t.Fatalf("mismatched command = %s err=%v", mismatched, err)
	}
	if _, err := os.Stat(filepath.Join(root, "wrong.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched command ran: %v", err)
	}
	runID = "other-run"
	wrongRun, err := runBash("ask", askCommand)
	if err != nil || !toolResultIsChoiceRequest(wrongRun) {
		t.Fatalf("approval escaped its run: %s err=%v", wrongRun, err)
	}
	runID = "run-ask"
	executed, err := runBash("ask", askCommand)
	if err != nil || !strings.Contains(executed, `"ok":true`) {
		t.Fatalf("approved ask command = %s err=%v", executed, err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "ask-approved.txt")); err != nil || string(content) != "approved" {
		t.Fatalf("approved output = %q err=%v", content, err)
	}
	if err := os.Remove(filepath.Join(root, "ask-approved.txt")); err != nil {
		t.Fatal(err)
	}
	reused, err := runBash("ask", askCommand)
	if err != nil || !toolResultIsChoiceRequest(reused) {
		t.Fatalf("single-use approval was reused: %s err=%v", reused, err)
	}

	planCommand := "printf plan > plan.txt"
	runID = "run-plan"
	planRequest, err := runBash("plan", planCommand)
	if err != nil || !toolResultIsChoiceRequest(planRequest) {
		t.Fatalf("plan approval request = %s err=%v", planRequest, err)
	}
	denyID := bashChoiceID(t, planRequest, residentBashDenyPrefix)
	if recognized, err := a.resolveResidentBashChoice(project.ID, agent.ID, runID, denyID); !recognized || err != nil {
		t.Fatalf("denial resolution: recognized=%t err=%v", recognized, err)
	}
	deniedRetry, err := runBash("plan", planCommand)
	if err != nil || !toolResultIsChoiceRequest(deniedRetry) {
		t.Fatalf("denied plan command did not require a new approval: %s err=%v", deniedRetry, err)
	}
	if _, err := os.Stat(filepath.Join(root, "plan.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied plan command ran: %v", err)
	}

	devCommand := "printf dev > dev.txt"
	runID = "run-dev"
	devResult, err := runBash("dev", devCommand)
	if err != nil || !strings.Contains(devResult, `"ok":true`) {
		t.Fatalf("dev command = %s err=%v", devResult, err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "dev.txt")); err != nil || string(content) != "dev" {
		t.Fatalf("dev output = %q err=%v", content, err)
	}
}

func TestResidentBashOutputIsBounded(t *testing.T) {
	result := runResidentBashTool(context.Background(), t.TempDir(), "yes x | head -c 4096", 5000, 128)
	if !result.OK {
		t.Fatalf("bounded command failed: %+v", result)
	}
	if !result.Truncated {
		t.Fatal("oversized output was not marked truncated")
	}
	if len(result.Stdout) > 128 {
		t.Fatalf("output exceeded limit: %d", len(result.Stdout))
	}
}

func TestResidentBashApprovalIsRevokedWhenRunFinishes(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir()}
	agent := Agent{ID: "designer", ProjectID: project.ID}
	a.agents[project.ID] = []Agent{agent}
	command := "printf revoked"
	request := a.requestResidentBashApproval(ResidentToolContext{Project: project, Agent: agent}, command)
	choiceID := bashChoiceID(t, request, residentBashApprovePrefix)
	run, started := a.beginAgentRun(AgentRunInput{RunID: "run-revoke", ProjectID: project.ID, AgentID: agent.ID, Trigger: RunTriggerUserDirect})
	if !started {
		t.Fatal("run did not start")
	}
	if recognized, err := a.resolveResidentBashChoice(project.ID, agent.ID, run.ID, choiceID); !recognized || err != nil {
		t.Fatalf("approval resolution: recognized=%t err=%v", recognized, err)
	}
	a.finishAgentRun(project.ID, agent.ID, run.ID, RunStateDone, nil)
	if a.consumeResidentBashApproval(project.ID, agent.ID, run.ID, command) {
		t.Fatal("approval survived its run")
	}
}

func bashChoiceID(t *testing.T, result, prefix string) string {
	t.Helper()
	var payload struct {
		Choices []struct {
			ID string `json:"id"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatalf("decode choice request: %v", err)
	}
	for _, choice := range payload.Choices {
		if strings.HasPrefix(choice.ID, prefix) {
			return choice.ID
		}
	}
	t.Fatalf("choice with prefix %q missing from %s", prefix, result)
	return ""
}

func TestResidentProviderWithoutRuntimeCapabilitiesIsRejected(t *testing.T) {
	t.Setenv("KAROZ_AGENT_PROVIDER", "stub")
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir()}
	agent := Agent{ID: "karoz", ProjectID: project.ID}
	a.agents[project.ID] = []Agent{agent}
	_, err := a.runResidentAgentTurn(context.Background(), project, agent, "hello", "ask", nil)
	if err == nil || !strings.Contains(err.Error(), "required streaming, tool, and interrupt capabilities") {
		t.Fatalf("capability error = %v", err)
	}
}

func TestCodexStreamInterruptsToollessResponse(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "auth.json")
	auth := map[string]any{
		"tokens":  map[string]any{"access_token": "test-token", "account_id": "test-account"},
		"expired": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	}
	raw, _ := json.Marshal(auth)
	if err := os.WriteFile(authPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"))
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	t.Setenv("KAROZ_CODEX_AUTH_PATH", authPath)
	t.Setenv("KAROZ_CODEX_BASE_URL", server.URL)

	startedAt := time.Now()
	var delivered atomic.Bool
	var observed atomic.Int32
	streamed, interrupts, err := streamCodexStep(context.Background(), []map[string]any{codexMessage("user", "hello")}, nil, AgentStreamCallbacks{
		PollInterrupts: func() []AgentInterrupt {
			if time.Since(startedAt) < 70*time.Millisecond || delivered.Swap(true) {
				return nil
			}
			return []AgentInterrupt{{ID: "interrupt-1", Body: "new direction", TurnType: "ask"}}
		},
		OnInterrupt: func(items []AgentInterrupt) { observed.Add(int32(len(items))) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(interrupts) != 1 || interrupts[0].Body != "new direction" || observed.Load() != 1 {
		t.Fatalf("interrupts=%+v observed=%d", interrupts, observed.Load())
	}
	if !strings.Contains(streamed.Text, "partial") {
		t.Fatalf("partial response was not retained: %+v", streamed)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("interrupt took too long: %s", elapsed)
	}
}

func TestSSEMCPReadHonorsContextCancellation(t *testing.T) {
	client := &mcpClient{messages: make(chan []byte)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	_, err := client.readMessage(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(startedAt) > time.Second {
		t.Fatalf("read cancellation err=%v elapsed=%s", err, time.Since(startedAt))
	}
}

func TestStdioMCPReadHonorsContextCancellation(t *testing.T) {
	reader, writer := io.Pipe()
	pumpCtx, stopPump := context.WithCancel(context.Background())
	client := &mcpClient{
		reader: bufio.NewReader(reader), messages: make(chan []byte), messageErrors: make(chan error, 1),
	}
	go client.readStdio(pumpCtx)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	startedAt := time.Now()
	_, err := client.readMessage(ctx)
	cancel()
	stopPump()
	_ = writer.Close()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(startedAt) > time.Second {
		t.Fatalf("stdio read cancellation err=%v elapsed=%s", err, time.Since(startedAt))
	}
}

func TestProjectMCPRequiresExplicitTrust(t *testing.T) {
	root := t.TempDir()
	config := `{"mcpServers":{"repo-command":{"command":"/bin/echo","args":["unsafe"]}}}`
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	if servers := a.mcpServersForWorkdir(root); len(servers) != 0 {
		t.Fatalf("untrusted project MCP loaded: %+v", servers)
	}
	t.Setenv("KAROZ_TRUST_PROJECT_MCP", "1")
	if servers := a.mcpServersForWorkdir(root); len(servers) != 1 || servers["repo-command"].Command != "/bin/echo" {
		t.Fatalf("explicitly trusted project MCP missing: %+v", servers)
	}
}

func TestWebFetchRejectsPrivateTargets(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1/private")
	if err := validateFetchTarget(context.Background(), target, false); err == nil {
		t.Fatal("private target was accepted")
	}
	if err := validateFetchTarget(context.Background(), target, true); err != nil {
		t.Fatalf("explicitly configured private target was rejected: %v", err)
	}
}
