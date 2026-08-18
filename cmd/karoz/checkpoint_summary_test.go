package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	runtimedomain "github.com/karoz/karoz/internal/runtime"
)

type checkpointTestProvider struct {
	mu        sync.Mutex
	requests  []CLI2APIRequest
	calls     int
	started   chan int
	supported *bool
	stream    func(int, context.Context, CLI2APIRequest, AgentStreamCallbacks) error
}

func (provider *checkpointTestProvider) Capabilities(CLI2APIRequest) runtimedomain.ProviderCapabilities {
	if provider.supported != nil && !*provider.supported {
		return runtimedomain.ProviderCapabilities{}
	}
	return runtimedomain.ProviderCapabilities{Streaming: true, Tools: true, Interrupts: true}
}

func (provider *checkpointTestProvider) Stream(ctx context.Context, request CLI2APIRequest, _ ResidentToolContext, callbacks AgentStreamCallbacks) error {
	provider.mu.Lock()
	provider.calls++
	call := provider.calls
	provider.requests = append(provider.requests, request)
	provider.mu.Unlock()
	if provider.started != nil {
		select {
		case provider.started <- call:
		default:
		}
	}
	if provider.stream != nil {
		return provider.stream(call, ctx, request, callbacks)
	}
	if callbacks.OnDelta != nil {
		callbacks.OnDelta("semantic checkpoint")
	}
	return nil
}

func (provider *checkpointTestProvider) snapshot() (int, []CLI2APIRequest) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.calls, append([]CLI2APIRequest{}, provider.requests...)
}

func newCheckpointTestApp(t *testing.T, provider *checkpointTestProvider) (*app, Project, Agent) {
	t.Helper()
	root := t.TempDir()
	projectPath := filepath.Join(root, "checkpoint-project")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	t.Cleanup(a.supervisorCancel)
	project := Project{ID: projectID(projectPath), Name: "checkpoint-project", Path: projectPath, WorkspaceRoot: root, WorkspaceType: "main"}
	agent := Agent{
		ID: "checkpoint-agent", ProjectID: project.ID, Name: "implementation-lead", Nickname: "Checkpoint",
		Provider: "codex", Model: "checkpoint-model", ThinkingEffort: "high",
	}
	a.agentDirectoryLocked().agents[project.ID] = []Agent{agent}
	a.modelProvider = provider
	seedCheckpointSession(t, a, project.ID, agent.ID, residentSessionID(project.ID, agent.ID), 1, 80, 0)
	return a, project, agent
}

func checkpointMessages(projectID, agentID, sessionID string, startSeq int64, count int) []AgentMessage {
	now := time.Now().UTC()
	items := make([]AgentMessage, 0, count)
	for index := 0; index < count; index++ {
		seq := startSeq + int64(index)
		items = append(items, AgentMessage{
			ID: fmt.Sprintf("checkpoint-message-%d", seq), ProjectID: projectID, AgentID: agentID, SessionID: sessionID,
			Seq: seq, Role: "user", Intent: "question",
			Body:      fmt.Sprintf("durable checkpoint phrase %d %s", seq, strings.Repeat("x", 1800)),
			CreatedAt: now.Add(time.Duration(seq) * time.Millisecond),
		})
	}
	return items
}

func seedCheckpointSession(t *testing.T, a *app, projectID, agentID, sessionID string, startSeq int64, count int, coveredSeqEnd int64) {
	t.Helper()
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	setAgentSessionForTest(a, key, AgentSessionState{
		SessionID: sessionID, ProjectID: projectID, AgentID: agentID,
		ShortWindowStartSeq: startSeq, BoundarySeq: coveredSeqEnd, CoveredSeqEnd: coveredSeqEnd,
	})
	for _, message := range checkpointMessages(projectID, agentID, sessionID, startSeq, count) {
		a.appendAgentSessionEventLocked(newAgentMessageSessionEvent(message, agentTranscriptAppendMetadata{}))
	}
	a.mu.Unlock()
	if err := a.saveAgentSessionEvents(); err != nil {
		t.Fatal(err)
	}
}

func waitForCheckpoint(t *testing.T, a *app, provider *checkpointTestProvider, condition func(AgentSessionState) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.mu.Lock()
		var state AgentSessionState
		for _, candidate := range agentSessionsForTest(a) {
			state = candidate
			break
		}
		claims := len(a.agentRuntimeLocked().checkpointClaims)
		a.mu.Unlock()
		if condition(state) && claims == 0 {
			return
		}
		if time.Now().After(deadline) {
			calls, requests := provider.snapshot()
			t.Fatalf("checkpoint did not settle: state=%+v claims=%d calls=%d requests=%d", state, claims, calls, len(requests))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForCheckpointCallsAndIdle(t *testing.T, a *app, provider *checkpointTestProvider, wantCalls int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		calls, _ := provider.snapshot()
		a.mu.Lock()
		claims := len(a.agentRuntimeLocked().checkpointClaims)
		a.mu.Unlock()
		if calls >= wantCalls && claims == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("checkpoint did not become idle: calls=%d want=%d claims=%d", calls, wantCalls, claims)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForCheckpointState(t *testing.T, a *app, key string, condition func(AgentSessionState) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.mu.Lock()
		state, _ := a.conversation.Session(key)
		a.mu.Unlock()
		if condition(state) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("checkpoint state did not advance: %+v", state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForCheckpointCoverage(t *testing.T, a *app, projectID, agentID string, coveredSeqEnd int64) {
	t.Helper()
	key := projectAgentKey(projectID, agentID)
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.mu.Lock()
		state, _ := a.conversation.Session(key)
		claims := len(a.agentRuntimeLocked().checkpointClaims)
		a.mu.Unlock()
		if state.CoveredSeqEnd == coveredSeqEnd && claims == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("checkpoint coverage did not settle: state=%+v claims=%d", state, claims)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSemanticCheckpointCommitsBoundedProviderOutputAndReloads(t *testing.T) {
	semantic := "Decision: keep exact archives. Pending: verify the release. Unresolved: provider latency is uncertain."
	provider := &checkpointTestProvider{}
	provider.stream = func(_ int, _ context.Context, request CLI2APIRequest, callbacks AgentStreamCallbacks) error {
		if request.Mode != "checkpoint" || !request.NoTools {
			t.Errorf("checkpoint request mode = %+v", request)
		}
		if request.Provider != "codex" || request.Model != "checkpoint-model" || request.ThinkingEffort != "high" {
			t.Errorf("checkpoint model config = %+v", request)
		}
		if request.Transcript != nil || callbacks.OnToolStart != nil || callbacks.OnToolResult != nil {
			t.Errorf("checkpoint exposed transcript/tools: request=%+v callbacks=%+v", request, callbacks)
		}
		if len(request.Prompt) > checkpointPromptMaxChars {
			t.Errorf("checkpoint prompt length=%d", len(request.Prompt))
		}
		callbacks.OnDelta(semantic + strings.Repeat(" z", checkpointOutputMaxChars))
		return nil
	}
	a, project, agent := newCheckpointTestApp(t, provider)

	a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
	waitForCheckpoint(t, a, provider, func(state AgentSessionState) bool { return state.CoveredSeqEnd == 30 })

	state := a.agentSessionState(project.ID, agent.ID)
	if !strings.HasPrefix(state.ResidentSummary, semantic) || strings.Contains(state.ResidentSummary, "seq 1 user:") ||
		len(state.ResidentSummary) > checkpointOutputMaxChars {
		t.Fatalf("semantic summary was not committed with output bound: %q", state.ResidentSummary)
	}
	if state.CoveredSeqStart != 1 || state.CoveredSeqEnd != 30 || state.BoundarySeq != 30 ||
		state.ShortWindowStartSeq != 31 || state.LongTermVersion != 2 || state.LastCheckpointAt.IsZero() {
		t.Fatalf("checkpoint fields did not advance atomically: %+v", state)
	}
	key := projectAgentKey(project.ID, agent.ID)
	messages := a.conversation.MessagesFor(key)
	archives := a.conversation.ArchivedMessagesFor(key)
	if len(archives) != 30 || archives[0].Body != messages[0].Body {
		t.Fatalf("exact source archive was not retained: %+v", archives)
	}
	var searched struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(a.searchArchive(project.ID, agent.ID, "durable checkpoint phrase 7", 10)), &searched); err != nil {
		t.Fatal(err)
	}
	if len(searched.Messages) == 0 || searched.Messages[0]["seq"] != float64(7) ||
		!strings.HasPrefix(searched.Messages[0]["body"].(string), "durable checkpoint phrase 7 ") {
		t.Fatalf("exact archived source was not searchable: %+v", searched.Messages)
	}

	reloaded := newApp(a.settings)
	t.Cleanup(reloaded.supervisorCancel)
	if err := reloaded.loadAgentSessionEvents(); err != nil {
		t.Fatal(err)
	}
	reloadedState, _ := reloaded.conversation.Session(key)
	if !reflect.DeepEqual(reloadedState, state) || !reflect.DeepEqual(reloaded.conversation.ArchivedMessagesFor(key), a.conversation.ArchivedMessagesFor(key)) {
		t.Fatalf("checkpoint did not survive reload: state=%+v archives=%d", reloadedState, len(reloaded.conversation.ArchivedMessagesFor(key)))
	}
}

func TestNoToolsCheckpointProviderPathNeverEntersToolLoop(t *testing.T) {
	wire := &budgetTestWire{}
	err := invokeResidentNoToolsOnce(context.Background(), wire, AgentStreamCallbacks{})
	if err == nil || wire.steps != 1 || wire.toolsSeen != 0 || wire.limitMessage != "" {
		t.Fatalf("no-tools path entered resident tool loop: err=%v steps=%d tools=%d limit=%q", err, wire.steps, wire.toolsSeen, wire.limitMessage)
	}
}

func TestCheckpointReturnsBeforeBlockedProviderAndStaysSingleFlight(t *testing.T) {
	release := make(chan struct{})
	provider := &checkpointTestProvider{started: make(chan int, 4)}
	provider.stream = func(_ int, ctx context.Context, _ CLI2APIRequest, callbacks AgentStreamCallbacks) error {
		select {
		case <-release:
			callbacks.OnDelta("semantic result after release")
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a, project, agent := newCheckpointTestApp(t, provider)

	startedAt := time.Now()
	a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
	if time.Since(startedAt) > 250*time.Millisecond {
		t.Fatal("checkpoint trigger waited for the provider")
	}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("checkpoint provider did not start")
	}
	for index := 0; index < 20; index++ {
		a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
	}
	promptStarted := time.Now()
	_ = a.buildResidentAgentPromptWithMemoryQuery(project, agent, "continue", "ask", "continue")
	if time.Since(promptStarted) > 250*time.Millisecond {
		t.Fatal("resident prompt path waited for checkpoint provider")
	}
	calls, _ := provider.snapshot()
	a.mu.Lock()
	claims := len(a.agentRuntimeLocked().checkpointClaims)
	a.mu.Unlock()
	if calls != 1 || claims != 1 {
		t.Fatalf("repeated triggers created multiple jobs: calls=%d claims=%d", calls, claims)
	}
	close(release)
	waitForCheckpoint(t, a, provider, func(state AgentSessionState) bool { return state.CoveredSeqEnd == 30 })
}

func TestUnsupportedCheckpointProviderDoesNotCommitAndBacksOff(t *testing.T) {
	supported := false
	provider := &checkpointTestProvider{supported: &supported}
	a, project, agent := newCheckpointTestApp(t, provider)
	a.checkpointRetryDelay = time.Second
	before := agentSessionsForTest(a)

	a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
	waitForCheckpointCallsAndIdle(t, a, provider, 0)
	for index := 0; index < 20; index++ {
		a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
	}

	calls, requests := provider.snapshot()
	if calls != 0 || len(requests) != 0 || !reflect.DeepEqual(agentSessionsForTest(a), before) {
		t.Fatalf("unsupported provider was invoked or committed: calls=%d requests=%d state=%+v", calls, len(requests), agentSessionsForTest(a))
	}
	key := checkpointClaimKey(agentCheckpointClaim{
		ProjectID: project.ID, AgentID: agent.ID, SessionID: residentSessionID(project.ID, agent.ID),
	})
	a.mu.Lock()
	retryAt := a.agentRuntimeLocked().checkpointRetryNotBefore[key]
	a.mu.Unlock()
	if !retryAt.After(time.Now()) {
		t.Fatalf("unsupported provider did not establish retry backoff: %v", retryAt)
	}
}

func TestCheckpointProviderBoilerplateRetriesSameRangeWithoutCommit(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{
			name:   "stub response",
			output: "Karoz received the request. cli2api is running in stub mode; set KAROZ_AGENT_PROVIDER=codex-direct to reuse Codex CLI OAuth and call the upstream API directly.",
		},
		{
			name:   "unavailable configuration response",
			output: "Provider is unavailable. Please configure KAROZ_AGENT_PROVIDER and retry.",
		},
		{
			name:   "known missing credentials response",
			output: "Claude CLI is not logged in and ANTHROPIC_API_KEY is not configured.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &checkpointTestProvider{}
			provider.stream = func(call int, _ context.Context, _ CLI2APIRequest, callbacks AgentStreamCallbacks) error {
				if call == 1 {
					callbacks.OnDelta(test.output)
				} else {
					callbacks.OnDelta("Decision: retain the exact archive. Pending: retry validation.")
				}
				return nil
			}
			a, project, agent := newCheckpointTestApp(t, provider)
			a.checkpointRetryDelay = 200 * time.Millisecond
			before := agentSessionsForTest(a)

			a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
			waitForCheckpointCallsAndIdle(t, a, provider, 1)
			if !reflect.DeepEqual(agentSessionsForTest(a), before) {
				t.Fatalf("provider boilerplate advanced session: before=%+v after=%+v", before, agentSessionsForTest(a))
			}
			for index := 0; index < 20; index++ {
				a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
			}
			if calls, _ := provider.snapshot(); calls != 1 {
				t.Fatalf("provider boilerplate bypassed retry backoff: calls=%d", calls)
			}

			time.Sleep(210 * time.Millisecond)
			a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
			waitForCheckpoint(t, a, provider, func(state AgentSessionState) bool { return state.CoveredSeqEnd == 30 })
			state := a.agentSessionState(project.ID, agent.ID)
			if !strings.HasPrefix(state.ResidentSummary, "Decision: retain the exact archive.") {
				t.Fatalf("later semantic checkpoint did not commit: %+v", state)
			}
			_, requests := provider.snapshot()
			if len(requests) < 2 || requests[0].Prompt != requests[1].Prompt {
				t.Fatalf("provider boilerplate retry changed claimed range: requests=%d", len(requests))
			}
		})
	}
}

func TestCheckpointProviderBoilerplateClassifierAllowsSemanticAvailabilityFacts(t *testing.T) {
	legitimate := "Durable fact: the deployment provider was unavailable during the incident. Pending: confirm the configured fallback."
	if isCheckpointProviderBoilerplate(legitimate) {
		t.Fatalf("legitimate continuity summary was rejected: %q", legitimate)
	}
}

func TestCheckpointProviderFailuresRetryTheSameRange(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		first   func(context.Context, AgentStreamCallbacks) error
	}{
		{name: "provider error", first: func(context.Context, AgentStreamCallbacks) error { return errors.New("provider failed") }},
		{name: "timeout", timeout: 20 * time.Millisecond, first: func(ctx context.Context, _ AgentStreamCallbacks) error {
			<-ctx.Done()
			return ctx.Err()
		}},
		{name: "empty output", first: func(context.Context, AgentStreamCallbacks) error { return nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &checkpointTestProvider{}
			provider.stream = func(call int, ctx context.Context, _ CLI2APIRequest, callbacks AgentStreamCallbacks) error {
				if call == 1 {
					return test.first(ctx, callbacks)
				}
				callbacks.OnDelta("retry semantic summary")
				return nil
			}
			a, project, agent := newCheckpointTestApp(t, provider)
			a.checkpointRetryDelay = 200 * time.Millisecond
			if test.timeout > 0 {
				a.checkpointTimeout = test.timeout
			}
			before := a.agentSessionState(project.ID, agent.ID)
			a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
			waitForCheckpointCallsAndIdle(t, a, provider, 1)
			if after := a.agentSessionState(project.ID, agent.ID); !reflect.DeepEqual(after, before) {
				t.Fatalf("failed checkpoint advanced state: before=%+v after=%+v", before, after)
			}
			for index := 0; index < 20; index++ {
				a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
			}
			if calls, _ := provider.snapshot(); calls != 1 {
				t.Fatalf("failure backoff allowed append-triggered model storm: calls=%d", calls)
			}

			time.Sleep(210 * time.Millisecond)
			a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
			waitForCheckpoint(t, a, provider, func(state AgentSessionState) bool { return state.CoveredSeqEnd == 30 })
			_, requests := provider.snapshot()
			if len(requests) < 2 || requests[0].Prompt != requests[1].Prompt {
				t.Fatalf("retry changed the uncovered range: requests=%d", len(requests))
			}
		})
	}
}

func TestCheckpointSupervisorCancellationLeavesCoverageUnchanged(t *testing.T) {
	provider := &checkpointTestProvider{started: make(chan int, 1)}
	provider.stream = func(_ int, ctx context.Context, _ CLI2APIRequest, _ AgentStreamCallbacks) error {
		<-ctx.Done()
		return ctx.Err()
	}
	a, project, agent := newCheckpointTestApp(t, provider)
	before := agentSessionsForTest(a)
	a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("checkpoint provider did not start")
	}
	a.supervisorCancel()
	waitForCheckpointCallsAndIdle(t, a, provider, 1)
	if !reflect.DeepEqual(agentSessionsForTest(a), before) {
		t.Fatalf("supervisor cancellation advanced checkpoint: before=%+v after=%+v", before, agentSessionsForTest(a))
	}
}

func TestStaleOutOfOrderCheckpointCannotOverwriteNewSession(t *testing.T) {
	releaseOld := make(chan struct{})
	provider := &checkpointTestProvider{started: make(chan int, 8)}
	provider.stream = func(call int, ctx context.Context, _ CLI2APIRequest, callbacks AgentStreamCallbacks) error {
		if call == 1 {
			select {
			case <-releaseOld:
				callbacks.OnDelta("stale old-session summary")
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		callbacks.OnDelta("new-session summary")
		return nil
	}
	a, project, agent := newCheckpointTestApp(t, provider)
	a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("old checkpoint did not start")
	}

	key := projectAgentKey(project.ID, agent.ID)
	newSessionID := "replacement-session"
	a.mu.Lock()
	replaceAgentMessagesForTest(a, key, checkpointMessages(project.ID, agent.ID, newSessionID, 81, 80))
	setAgentSessionForTest(a, key, AgentSessionState{
		SessionID: newSessionID, ProjectID: project.ID, AgentID: agent.ID,
		ShortWindowStartSeq: 81, BoundarySeq: 80, CoveredSeqStart: 1, CoveredSeqEnd: 80,
	})
	a.mu.Unlock()
	a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
	waitForCheckpointState(t, a, key, func(state AgentSessionState) bool {
		return state.SessionID == newSessionID && state.CoveredSeqEnd == 110
	})
	close(releaseOld)
	waitForCheckpointCallsAndIdle(t, a, provider, 3)

	state := a.agentSessionState(project.ID, agent.ID)
	if state.SessionID != newSessionID || !strings.HasPrefix(state.ResidentSummary, "new-session summary") ||
		strings.Contains(state.ResidentSummary, "stale old-session") || state.CoveredSeqEnd != 110 {
		t.Fatalf("stale result overwrote the newer checkpoint: %+v", state)
	}
}

func TestCheckpointSessionSaveFailureHasZeroDriftThenRetries(t *testing.T) {
	provider := &checkpointTestProvider{}
	provider.stream = func(_ int, _ context.Context, _ CLI2APIRequest, callbacks AgentStreamCallbacks) error {
		callbacks.OnDelta("durable semantic summary")
		return nil
	}
	a, project, agent := newCheckpointTestApp(t, provider)
	a.checkpointRetryDelay = 5 * time.Millisecond
	before := agentSessionsForTest(a)
	a.conversationServiceLocked().checkpointSessionSaveOverride = func(map[string]AgentSessionState) error { return errors.New("session save failed") }
	a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
	waitForCheckpointCallsAndIdle(t, a, provider, 1)
	if !reflect.DeepEqual(agentSessionsForTest(a), before) {
		t.Fatalf("session save failure drifted state: before=%+v after=%+v", before, agentSessionsForTest(a))
	}

	a.conversationServiceLocked().checkpointSessionSaveOverride = nil
	time.Sleep(10 * time.Millisecond)
	a.maybeCheckpointAgentSession(project.ID, agent.ID, false)
	waitForCheckpoint(t, a, provider, func(state AgentSessionState) bool { return state.CoveredSeqEnd == 30 })
	_, requests := provider.snapshot()
	if len(requests) < 2 || requests[0].Prompt != requests[1].Prompt {
		t.Fatalf("save failure did not retry the same range: requests=%d", len(requests))
	}
}
