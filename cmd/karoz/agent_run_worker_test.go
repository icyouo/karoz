package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	runtimedomain "github.com/karoz/karoz/internal/runtime"
)

type controlledRunProvider struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (p *controlledRunProvider) Capabilities(CLI2APIRequest) runtimedomain.ProviderCapabilities {
	return runtimedomain.ProviderCapabilities{Streaming: true, Tools: true, Interrupts: true}
}

func (p *controlledRunProvider) Stream(ctx context.Context, _ CLI2APIRequest, _ ResidentToolContext, callbacks AgentStreamCallbacks) error {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	callbacks.OnDelta("partial ")
	callbacks.OnToolStart(codexToolCall{ID: "call-1", Name: "bash", Arguments: `{"command":"true"}`})
	callbacks.OnToolResult(codexToolCall{ID: "call-1", Name: "bash"}, `{"code":0}`, true)
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
		callbacks.OnDelta("complete")
		return nil
	}
}

func (p *controlledRunProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newAgentRunWorkerFixture(t *testing.T, provider *controlledRunProvider) (*app, Project, Agent) {
	t.Helper()
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	a.modelProvider = provider
	project := Project{ID: "project", Name: "project", Path: t.TempDir(), DefaultBranch: "main"}
	agent := Agent{ID: "agent", ProjectID: project.ID, Name: "Agent", Role: "implementation"}
	a.agents[project.ID] = []Agent{agent}
	return a, project, agent
}

func waitForRun(t *testing.T, a *app, projectID, agentID string, active bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, got := a.activeAgentRun(projectID, agentID); got == active {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("active run=%t, want %t", func() bool { _, got := a.activeAgentRun(projectID, agentID); return got }(), active)
}

func TestRequestDisconnectDoesNotCancelRunAndExplicitCancelDoes(t *testing.T) {
	provider := &controlledRunProvider{started: make(chan struct{}), release: make(chan struct{})}
	a, project, agent := newAgentRunWorkerFixture(t, provider)
	ctx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	request := httptest.NewRequest(http.MethodPost, "/agents/agent/messages", strings.NewReader(`{"message":"hello"}`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		a.handleAgents(recorder, request, project, []string{agent.ID, "messages"})
		close(done)
	}()
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start")
	}
	cancelRequest()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("POST observer did not detach after request cancellation")
	}
	if _, active := a.activeAgentRun(project.ID, agent.ID); !active {
		t.Fatal("request cancellation cancelled the Run-owned worker")
	}
	if _, cancelled := a.cancelAgentRun(project.ID, agent.ID); !cancelled {
		t.Fatal("explicit cancellation did not claim active run")
	}
	waitForRun(t, a, project.ID, agent.ID, false)
	ledger := a.runLedgerFromActiveOrKnown(project.ID, agent.ID)
	if ledger == nil {
		t.Fatal("missing cancelled run ledger")
	}
	waitForLedgerTerminal(t, ledger)
	if !ledgerHasTerminalType(ledger, "cancelled") {
		t.Fatal("missing cancelled terminal ledger event")
	}
}

func TestRunWorkerExecutesOncePublishesOrderedToolEventsAndPersistsOneResult(t *testing.T) {
	provider := &controlledRunProvider{started: make(chan struct{}), release: make(chan struct{})}
	a, project, agent := newAgentRunWorkerFixture(t, provider)
	run, started := a.beginAgentRun(AgentRunInput{RunID: "run-1", ProjectID: project.ID, AgentID: agent.ID, Trigger: RunTriggerUserDirect, TurnType: "dev"})
	if !started {
		t.Fatal("could not begin run")
	}
	a.startAgentRunWorker(project, agent, run, "implement", "dev")
	// A stale second start for the same Run must not replace the first
	// worker's cancellation ownership or execute the provider twice.
	a.startAgentRunWorker(project, agent, run, "implement", "dev")
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start")
	}
	ledger := a.runLedger(run.ID)
	if ledger == nil {
		t.Fatal("missing run ledger")
	}
	firstReplay, _, _, first, stopFirst := ledger.subscribe(0)
	defer stopFirst()
	secondReplay, _, _, second, stopSecond := ledger.subscribe(0)
	defer stopSecond()
	close(provider.release)
	waitForRun(t, a, project.ID, agent.ID, false)
	if provider.callCount() != 1 {
		t.Fatalf("provider executions=%d, want 1", provider.callCount())
	}
	for _, observer := range []struct {
		replay []agentRunLedgerEvent
		live   chan agentRunLedgerEvent
	}{{firstReplay, first}, {secondReplay, second}} {
		got := append(observer.replay, collectLedgerEvents(observer.live)...)
		if !orderedLedgerTypes(got, "tool_start", "tool_result", "done") {
			t.Fatalf("events out of order or incomplete: %+v", got)
		}
	}
	resultCount := 0
	for _, msg := range a.agentMessagesForDisplay(project.ID, agent.ID) {
		if msg.Role == "assistant" && msg.Intent == "result" {
			resultCount++
		}
	}
	if resultCount != 1 {
		t.Fatalf("persisted assistant results=%d, want 1", resultCount)
	}
	toolMessages := map[int64]bool{}
	for _, msg := range a.agentMessagesForDisplay(project.ID, agent.ID) {
		if msg.Role == "tool_call" || msg.Role == "tool_result" {
			toolMessages[msg.Seq] = true
		}
	}
	for _, event := range firstReplay {
		if event.Type != "tool_start" && event.Type != "tool_result" {
			continue
		}
		payload, ok := event.Data.(map[string]any)
		if !ok {
			t.Fatalf("tool event payload=%T, want object", event.Data)
		}
		seq, ok := payload["message_seq"].(int64)
		if !ok || !toolMessages[seq] {
			t.Fatalf("tool event has no durable message identity: %+v", payload)
		}
	}
}

func TestCancelAfterProviderReturnBeforeResultCommitPersistsNoResult(t *testing.T) {
	provider := &controlledRunProvider{started: make(chan struct{}), release: make(chan struct{})}
	a, project, agent := newAgentRunWorkerFixture(t, provider)
	arrived := make(chan struct{})
	releaseCommit := make(chan struct{})
	a.agentRunAfterProviderHook = func() {
		close(arrived)
		<-releaseCommit
	}
	defer func() { a.agentRunAfterProviderHook = nil }()
	run, started := a.beginAgentRun(AgentRunInput{RunID: "cancel-before-commit", ProjectID: project.ID, AgentID: agent.ID, Trigger: RunTriggerUserDirect, TurnType: "ask"})
	if !started {
		t.Fatal("could not begin run")
	}
	a.startAgentRunWorker(project, agent, run, "hello", "ask")
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start")
	}
	close(provider.release)
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not reach result commit barrier")
	}
	if _, cancelled := a.cancelAgentRun(project.ID, agent.ID); !cancelled {
		t.Fatal("cancel did not win before result commit")
	}
	close(releaseCommit)
	waitForRun(t, a, project.ID, agent.ID, false)
	waitForLedgerTerminal(t, a.runLedger(run.ID))
	resultCount, cancelledCount, doneCount := 0, 0, 0
	for _, msg := range a.agentMessagesForDisplay(project.ID, agent.ID) {
		if msg.Role == "assistant" && msg.Intent == "result" {
			resultCount++
		}
	}
	ledger := a.runLedger(run.ID)
	if ledger == nil {
		t.Fatal("missing ledger")
	}
	replay, _, _, _, stop := ledger.subscribe(0)
	defer stop()
	for _, event := range replay {
		switch event.Type {
		case "cancelled":
			cancelledCount++
		case "done":
			doneCount++
		}
	}
	if resultCount != 0 || cancelledCount != 1 || doneCount != 0 {
		t.Fatalf("cancel/result race result=%d cancelled=%d done=%d events=%+v", resultCount, cancelledCount, doneCount, replay)
	}
}

func waitForLedgerTerminal(t *testing.T, ledger *agentRunLedger) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ledger != nil && (ledgerHasTerminalType(ledger, "done") || ledgerHasTerminalType(ledger, "cancelled") || ledgerHasTerminalType(ledger, "error")) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("ledger did not publish terminal event")
}

func TestSuccessClaimRejectsLaterCancelWithOneDoneResult(t *testing.T) {
	provider := &controlledRunProvider{started: make(chan struct{}), release: make(chan struct{})}
	a, project, agent := newAgentRunWorkerFixture(t, provider)
	claimed := make(chan struct{})
	releaseWorker := make(chan struct{})
	a.agentRunAfterSuccessHook = func() {
		close(claimed)
		<-releaseWorker
	}
	defer func() { a.agentRunAfterSuccessHook = nil }()
	run, started := a.beginAgentRun(AgentRunInput{RunID: "success-before-cancel", ProjectID: project.ID, AgentID: agent.ID, Trigger: RunTriggerUserDirect, TurnType: "ask"})
	if !started {
		t.Fatal("could not begin run")
	}
	a.startAgentRunWorker(project, agent, run, "hello", "ask")
	<-provider.started
	close(provider.release)
	select {
	case <-claimed:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not claim successful result")
	}
	if _, accepted := a.cancelAgentRun(project.ID, agent.ID); accepted {
		t.Fatal("cancel was accepted after successful finalization claim")
	}
	close(releaseWorker)
	resultCount := 0
	for _, msg := range a.agentMessagesForDisplay(project.ID, agent.ID) {
		if msg.Role == "assistant" && msg.Intent == "result" {
			resultCount++
		}
	}
	ledger := a.runLedger(run.ID)
	replay, _, _, _, stop := ledger.subscribe(0)
	defer stop()
	doneCount, cancelledCount := 0, 0
	for _, event := range replay {
		if event.Type == "done" {
			doneCount++
		}
		if event.Type == "cancelled" {
			cancelledCount++
		}
	}
	if resultCount != 1 || doneCount != 1 || cancelledCount != 0 {
		t.Fatalf("success/cancel inverse result=%d done=%d cancelled=%d events=%+v", resultCount, doneCount, cancelledCount, replay)
	}
}

func (a *app) runLedgerFromActiveOrKnown(_, _ string) *agentRunLedger {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ledger := range a.agentRunLedgers {
		return ledger
	}
	return nil
}

func ledgerHasTerminalType(ledger *agentRunLedger, want string) bool {
	ledger.mu.Lock()
	if !ledger.terminal {
		ledger.mu.Unlock()
		return false
	}
	replay := append([]agentRunLedgerEvent{}, ledger.events...)
	ledger.mu.Unlock()
	for _, event := range replay {
		if event.Type == want {
			return true
		}
	}
	return false
}

func collectLedgerEvents(ch <-chan agentRunLedgerEvent) []agentRunLedgerEvent {
	var events []agentRunLedgerEvent
	for event := range ch {
		events = append(events, event)
	}
	return events
}

func orderedLedgerTypes(events []agentRunLedgerEvent, wanted ...string) bool {
	position := 0
	for _, event := range events {
		if position < len(wanted) && event.Type == wanted[position] {
			position++
		}
	}
	return position == len(wanted)
}

func TestRunEventsEndpointReplaysTerminalLedgerWithoutWaitingForObserverContext(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	ledger := a.createRunLedger("terminal-run")
	ledger.publish("delta", map[string]any{"delta": "hello"})
	ledger.publish("done", map[string]any{"message": "complete"})
	recorder := httptest.NewRecorder()
	a.streamRunLedger(recorder, httptest.NewRequest(http.MethodGet, "/events?after=0", nil), "terminal-run", 0)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "event: delta") || !strings.Contains(recorder.Body.String(), "event: done") {
		t.Fatalf("terminal replay status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
