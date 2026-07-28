package main

import (
	"context"
	"encoding/json"
	"fmt"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type scheduledTranscriptTestProvider struct {
	beforeStream func(CLI2APIRequest) error
	prompt       string
	output       string
}

func (provider *scheduledTranscriptTestProvider) Capabilities(CLI2APIRequest) runtimedomain.ProviderCapabilities {
	return runtimedomain.ProviderCapabilities{Streaming: true, Tools: true, Interrupts: true}
}

func (provider *scheduledTranscriptTestProvider) Stream(_ context.Context, request CLI2APIRequest, _ ResidentToolContext, callbacks AgentStreamCallbacks) error {
	provider.prompt = request.Prompt
	if provider.beforeStream != nil {
		if err := provider.beforeStream(request); err != nil {
			return err
		}
	}
	call := codexToolCall{ID: "scheduled-tool", CallID: "scheduled-tool", Name: "repo_search", Arguments: `{"query":"transcript"}`}
	if callbacks.OnToolStart != nil {
		callbacks.OnToolStart(call)
	}
	if callbacks.OnToolResult != nil {
		callbacks.OnToolResult(call, `{"matches":["agent_transcript.go"]}`, true)
	}
	if callbacks.OnDelta != nil {
		callbacks.OnDelta(firstNonEmpty(provider.output, "Scheduled transcript final."))
	}
	return nil
}

func beginScheduledTranscriptTestRun(t *testing.T, a *app, job ScheduledRun) {
	t.Helper()
	run, started := a.beginAgentRun(job.RunInput())
	if !started || run.ID != job.ID {
		t.Fatalf("could not begin scheduled Run: run=%+v started=%t", run, started)
	}
	if _, claimed := a.claimAndBindAgentRunWorkerContext(context.Background(), job.ProjectID, job.AgentID, run.ID); !claimed {
		t.Fatal("could not claim scheduled Run")
	}
	a.createRunLedger(run.ID)
}

func scheduledModelInputOnDisk(dataDir, projectID, agentID, runID, intent, body string) error {
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: os.TempDir()})
	if err := a.loadAgentTranscripts(); err != nil {
		return err
	}
	items := a.agentTranscriptForModel(projectID, agentID)
	for _, item := range items {
		if item.RunID == runID && item.ModelOnly && item.Role == "user" && item.Intent == intent && item.Body == body && !item.Visible {
			return nil
		}
	}
	return fmt.Errorf("durable scheduled model input not found for run %s", runID)
}

func TestStructuredTranscriptPairsToolEventsAcrossTheNextTurn(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent, ok := a.projectAgent(project, "worker-a")
	if !ok {
		t.Fatal("worker-a missing")
	}
	run, started := a.beginAgentRun(AgentRunInput{RunID: "transcript-run", ProjectID: project.ID, AgentID: agent.ID, Trigger: RunTriggerUserDirect, TurnType: "dev"})
	if !started {
		t.Fatal("run did not start")
	}
	if _, ok := a.appendAgentMessageForRun(project.ID, agent.ID, run.ID, "user", "dev", "Inspect the checkout flow."); !ok {
		t.Fatal("user input was not persisted for its run")
	}
	call := codexToolCall{ID: "call-checkout", Name: "repo_search", Arguments: `{"query":"checkout"}`}
	if _, ok := a.appendAgentToolCallForRun(project.ID, agent.ID, run.ID, call); !ok {
		t.Fatal("tool call was not persisted")
	}
	if _, ok := a.appendAgentToolResultForRun(project.ID, agent.ID, run.ID, call, `{"matches":["checkout.go"]}`, true); !ok {
		t.Fatal("tool result was not persisted")
	}
	if !a.commitAgentRunSuccessWithLedger(project, agent, run.ID, "Checkout flow is implemented in checkout.go.") {
		t.Fatal("run result was not committed")
	}
	a.appendAgentMessage(project.ID, agent.ID, "user", "ask", "What did the search find?")

	items := a.agentTranscriptForModel(project.ID, agent.ID)
	if len(items) != 5 {
		t.Fatalf("transcript item count = %d, want 5: %+v", len(items), items)
	}
	if items[0].RunID != run.ID || items[1].ToolCallID != "call-checkout" || items[1].ToolName != "repo_search" || items[1].Kind != "tool_call" {
		t.Fatalf("tool call transcript = %+v", items[1])
	}
	if items[2].ToolCallID != "call-checkout" || items[2].ToolResult == "" || items[2].ToolSuccess == nil || !*items[2].ToolSuccess || items[2].Kind != "tool_result" {
		t.Fatalf("tool result transcript = %+v", items[2])
	}
	if items[3].RunID != run.ID || items[3].Role != "assistant" || items[3].Kind != "message" {
		t.Fatalf("run result transcript = %+v", items[3])
	}

	prompt := a.buildResidentAgentPrompt(project, agent, "What did the search find?", "ask")
	if strings.Contains(prompt, "### Recent structured resident transcript") || strings.Contains(prompt, "tool_call id=call-checkout") {
		t.Fatalf("provider-native history was duplicated into prompt prose:\n%s", prompt)
	}
	history := boundedProviderTranscript(a.agentTranscriptDeltaForModel(project.ID, agent.ID), "", "What did the search find?")
	input := codexTranscriptInput(history)
	if len(input) < 4 || input[1]["type"] != "function_call" || input[2]["type"] != "function_call_output" {
		t.Fatalf("structured next-turn provider history = %#v", input)
	}
}

func TestScheduledPlanTranscriptPersistsInputBeforeToolsAndSurvivesReload(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent, ok := a.projectAgent(project, "worker-a")
	if !ok {
		t.Fatal("worker-a missing")
	}
	now := time.Now().UTC()
	plan := WorkPlan{
		ID: "plan-transcript", ProjectID: project.ID, Title: "Preserve scheduled context", Goal: "Keep the initiating plan event across reload", Status: PlanActive,
		OwnerAgentID: agent.ID, Version: 7, Steps: []PlanStep{{ID: "verify", Title: "Verify transcript", Status: PlanStepRunning, Version: 1}}, CreatedAt: now, UpdatedAt: now,
	}
	a.plans[project.ID] = []WorkPlan{plan}
	payload, err := json.Marshal(PlanEventRunPayload{PlanID: plan.ID, PlanVersion: plan.Version, StepID: "verify", Event: "task_terminal", TaskID: "task-42"})
	if err != nil {
		t.Fatal(err)
	}
	job := ScheduledRun{ID: "scheduled-plan-transcript", ProjectID: project.ID, AgentID: agent.ID, Kind: ScheduledRunPlanEvent, Trigger: RunTriggerPlanEvent, TurnType: "plan", Payload: payload}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	expectedInput := fmt.Sprintf("[plan event] event=%s plan_id=%s plan_version=%d step_id=%s task_id=%s\n\nCurrent WorkPlan:\n%s\n\nYou own this active WorkPlan. Continue advancing its todo list. Inspect task/review/group-result evidence, then call advance_plan with one concrete action. Task completion alone never completes a step. You may accept it, delegate review, request rework, block it, dispatch a local task, delegate cross-group work through the group inbox, or complete the plan when every required step is accepted. Do not only summarize.", "task_terminal", plan.ID, plan.Version, "verify", "task-42", string(planJSON))
	provider := &scheduledTranscriptTestProvider{output: "Plan event processed."}
	provider.beforeStream = func(CLI2APIRequest) error {
		return scheduledModelInputOnDisk(a.settings.DataDir, project.ID, agent.ID, job.ID, "scheduled_plan_event_input", expectedInput)
	}
	a.modelProvider = provider
	beginScheduledTranscriptTestRun(t, a, job)

	if err := a.executePlanEventScheduledRun(context.Background(), job); err != nil {
		t.Fatalf("scheduled plan event failed: %v", err)
	}
	if !strings.Contains(provider.prompt, "[plan event] event=task_terminal") || !strings.Contains(provider.prompt, `"title":"Preserve scheduled context"`) {
		t.Fatalf("provider did not receive scheduled plan input:\n%s", provider.prompt)
	}

	reloaded := newApp(Settings{DataDir: a.settings.DataDir, ProjectsRoot: project.WorkspaceRoot})
	if err := reloaded.loadAgentMessages(); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.loadAgentTranscripts(); err != nil {
		t.Fatal(err)
	}
	visibleMessages := reloaded.agentMessagesForDisplay(project.ID, agent.ID)
	if len(visibleMessages) != 3 {
		t.Fatalf("scheduled model input leaked into visible message history: %+v", visibleMessages)
	}
	page := reloaded.agentMessagesPageForDisplay(project.ID, agent.ID, 0, 80)
	items := reloaded.agentTranscriptForModel(project.ID, agent.ID)
	if len(items) != 4 {
		t.Fatalf("reloaded scheduled transcript item count=%d, want input/tool/result/final: %+v", len(items), items)
	}
	for index, item := range items {
		if item.RunID != job.ID {
			t.Fatalf("scheduled transcript item %d lost RunID: %+v", index, item)
		}
		if index > 0 && item.Seq <= items[index-1].Seq {
			t.Fatalf("scheduled transcript sequence is not strictly ordered: %+v", items)
		}
	}
	input, toolCall, toolResult, final := items[0], items[1], items[2], items[3]
	if !input.ModelOnly || input.Visible || input.Role != "user" || input.Intent != "scheduled_plan_event_input" || input.Body != expectedInput {
		t.Fatalf("scheduled plan input transcript=%+v", input)
	}
	if len(page.ModelContext) != 4 {
		t.Fatalf("context meter projection count=%d, want input/tool/result/final: %+v", len(page.ModelContext), page.ModelContext)
	}
	foundProjectedInput := false
	for _, item := range page.ModelContext {
		if item.Seq != input.Seq {
			continue
		}
		foundProjectedInput = item.Role == "user" && item.Intent == "scheduled_plan_event_input" && item.Body == promptAgentTranscriptBody(input)
	}
	if !foundProjectedInput {
		t.Fatalf("context meter did not expose normalized scheduled plan input: %+v", page.ModelContext)
	}
	if toolCall.Kind != "tool_call" || toolCall.ToolCallID != "scheduled-tool" || toolResult.Kind != "tool_result" || toolResult.ToolCallID != "scheduled-tool" || toolResult.ToolSuccess == nil || !*toolResult.ToolSuccess {
		t.Fatalf("scheduled tool transcript mismatch: call=%+v result=%+v", toolCall, toolResult)
	}
	if final.Role != "assistant" || final.Intent != "plan_result" || final.Body != "Plan event processed." {
		t.Fatalf("scheduled final transcript=%+v", final)
	}
	nextPrompt := reloaded.buildResidentAgentPrompt(project, agent, "What plan event was processed?", "ask")
	if strings.Contains(nextPrompt, "tool_call id=scheduled-tool") {
		t.Fatalf("scheduled provider history was duplicated into prompt prose:\n%s", nextPrompt)
	}
	history := boundedProviderTranscript(reloaded.agentTranscriptDeltaForModel(project.ID, agent.ID), "", "What plan event was processed?")
	wireJSON, err := json.Marshal(codexTranscriptInput(history))
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"[plan event] event=task_terminal", "Current WorkPlan", "Preserve scheduled context", `"call_id":"scheduled-tool"`, "Plan event processed."} {
		if !strings.Contains(string(wireJSON), fragment) {
			t.Fatalf("next provider history lost scheduled plan context %q:\n%s", fragment, wireJSON)
		}
	}
}

func TestScheduledTaskEventPersistsModelOnlyInput(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent, ok := a.projectAgent(project, "worker-a")
	if !ok {
		t.Fatal("worker-a missing")
	}
	task := Task{ID: "task-transcript", ProjectID: project.ID, Status: "done", Result: "Implementation is ready."}
	a.tasks[project.ID] = []Task{task}
	payload, err := json.Marshal(TaskEventRunPayload{TaskID: task.ID, HookID: "hook-transcript"})
	if err != nil {
		t.Fatal(err)
	}
	job := ScheduledRun{ID: "scheduled-task-transcript", ProjectID: project.ID, AgentID: agent.ID, Kind: ScheduledRunTaskEvent, Trigger: RunTriggerTaskEvent, TurnType: "ask", Payload: payload}
	provider := &scheduledTranscriptTestProvider{output: "Task event processed."}
	a.modelProvider = provider
	beginScheduledTranscriptTestRun(t, a, job)
	if err := a.executeTaskEventScheduledRun(context.Background(), job); err != nil {
		t.Fatalf("scheduled task event failed: %v", err)
	}
	items := a.agentTranscriptForModel(project.ID, agent.ID)
	if len(items) < 1 {
		t.Fatal("scheduled task transcript is empty")
	}
	input := items[0]
	if input.RunID != job.ID || !input.ModelOnly || input.Visible || input.Intent != "scheduled_task_event_input" || !strings.Contains(input.Body, "[task hook] task_id=task-transcript") || !strings.Contains(input.Body, "Implementation is ready.") {
		t.Fatalf("scheduled task model input missing or malformed: %+v", input)
	}
}

func TestAgentMessagesPageProjectsOnlyCheckpointedModelContext(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent, ok := a.projectAgent(project, "worker-a")
	if !ok {
		t.Fatal("worker-a missing")
	}
	for i := 1; i <= 80; i++ {
		a.appendAgentMessage(project.ID, agent.ID, "assistant", "result", fmt.Sprintf("checkpointed message %d %s", i, strings.Repeat("x", 430)))
	}
	a.maybeCheckpointAgentSession(project.ID, agent.ID, false)

	page := a.agentMessagesPageForDisplay(project.ID, agent.ID, 0, 80)
	state := a.agentSessionState(project.ID, agent.ID)
	if state.ShortWindowStartSeq != 31 {
		t.Fatalf("short window start=%d, want 31 after 80 records", state.ShortWindowStartSeq)
	}
	if len(page.Messages) != 80 || page.Messages[0].Seq != 1 {
		t.Fatalf("display history should retain archived records: first=%+v count=%d", page.Messages[0], len(page.Messages))
	}
	delta := a.agentTranscriptDeltaForModel(project.ID, agent.ID)
	if len(delta) != 50 || delta[0].Seq != state.ShortWindowStartSeq {
		t.Fatalf("model delta is not the short window: start=%d count=%d", delta[0].Seq, len(delta))
	}
	if len(page.ModelContext) != 50 || page.ModelContext[0].Seq != state.ShortWindowStartSeq {
		t.Fatalf("API context projection ignored checkpoint boundary: %+v", page.ModelContext)
	}
	for _, item := range page.ModelContext {
		if item.Seq > 0 && item.Seq < state.ShortWindowStartSeq {
			t.Fatalf("API context projection leaked archived record: %+v", item)
		}
	}
	lines := renderAgentTranscriptDelta(delta, residentTranscriptPromptMaxItems, residentTranscriptPromptMaxChars)
	if len(lines) != len(page.ModelContext) {
		t.Fatalf("model prompt window count=%d, API projection count=%d", len(lines), len(page.ModelContext))
	}
	for index, line := range lines {
		if line.Role != page.ModelContext[index].Role || line.Body != page.ModelContext[index].Body {
			t.Fatalf("model prompt/API projection diverged at %d: line=%+v context=%+v", index, line, page.ModelContext[index])
		}
	}
	expected := estimateModelBoundTranscriptTokens(delta)
	actual := frontendContextTokenEstimate(t, page.ModelContext)
	if delta := expected - actual; delta < -residentContextEstimatorTolerance || delta > residentContextEstimatorTolerance {
		t.Fatalf("checkpointed context estimator=%d frontend=%d tolerance=%d", expected, actual, residentContextEstimatorTolerance)
	}
}

func TestModelContextProjectionCapsScheduledInputLikePrompt(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent, ok := a.projectAgent(project, "worker-a")
	if !ok {
		t.Fatal("worker-a missing")
	}
	run, started := a.beginAgentRun(AgentRunInput{RunID: "scheduled-context-cap", ProjectID: project.ID, AgentID: agent.ID, Trigger: RunTriggerPlanEvent, TurnType: "plan"})
	if !started {
		t.Fatal("could not begin scheduled context cap Run")
	}
	rawInput := "[plan event]\nCurrent WorkPlan:\n" + strings.Repeat("x", 7_000) + "TAIL_MUST_NOT_REACH_CONTEXT_METER"
	stored, created, err := a.appendAgentModelOnlyTranscriptForRun(project.ID, agent.ID, run.ID, "scheduled_plan_event_input", rawInput)
	if err != nil || !created {
		t.Fatalf("could not persist raw scheduled input: item=%+v created=%t err=%v", stored, created, err)
	}
	if stored.Body != rawInput {
		t.Fatal("durable transcript no longer preserves the exact scheduled model input")
	}
	prompt := a.buildResidentAgentPrompt(project, agent, rawInput, "plan")
	if !strings.Contains(prompt, promptAgentTranscriptBody(stored)) || strings.Contains(prompt, "TAIL_MUST_NOT_REACH_CONTEXT_METER") {
		t.Fatal("resident prompt did not apply the scheduled input rendering cap")
	}

	page := a.agentMessagesPageForDisplay(project.ID, agent.ID, 0, 80)
	if len(page.ModelContext) != 1 {
		t.Fatalf("context projection=%+v, want one scheduled input", page.ModelContext)
	}
	projected := page.ModelContext[0]
	wantBody := promptAgentTranscriptBody(stored)
	if projected.Body != wantBody || len(projected.Body) > 5_000 || strings.Contains(projected.Body, "TAIL_MUST_NOT_REACH_CONTEXT_METER") {
		t.Fatalf("scheduled context projection did not apply prompt cap: %+v", projected)
	}
	payload, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "TAIL_MUST_NOT_REACH_CONTEXT_METER") {
		t.Fatalf("API payload leaked raw model-only input: %s", payload)
	}
	delta := a.agentTranscriptDeltaForModel(project.ID, agent.ID)
	expected := estimateModelBoundTranscriptTokens(delta)
	actual := frontendContextTokenEstimate(t, page.ModelContext)
	if delta := expected - actual; delta < -residentContextEstimatorTolerance || delta > residentContextEstimatorTolerance {
		t.Fatalf("capped scheduled estimator=%d frontend=%d tolerance=%d", expected, actual, residentContextEstimatorTolerance)
	}
}

func TestProviderNeutralTranscriptRenderingIsEquivalent(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent, ok := a.projectAgent(project, "worker-a")
	if !ok {
		t.Fatal("worker-a missing")
	}
	a.appendAgentMessage(project.ID, agent.ID, "user", "ask", "Compare providers.")
	a.appendAgentMessage(project.ID, agent.ID, "assistant", "result", "They share the same resident transcript.")

	codex := agent
	codex.Provider = "codex"
	claude := agent
	claude.Provider = "claude"
	codexPrompt := a.buildResidentAgentPrompt(project, codex, "Compare providers.", "ask")
	claudePrompt := a.buildResidentAgentPrompt(project, claude, "Compare providers.", "ask")
	if codexPrompt != claudePrompt {
		t.Fatalf("provider-neutral transcript prompt differs\ncodex=%s\nclaude=%s", codexPrompt, claudePrompt)
	}
}

func TestStructuredTranscriptPreservesInterruptOrderingAfterReload(t *testing.T) {
	dataDir := t.TempDir()
	projectID, agentID := "p1", "agent-1"
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	a.appendAgentMessage(projectID, agentID, "user", "ask", "first request")
	a.appendAgentMessage(projectID, agentID, "user", "interrupt", "change the priority")
	a.appendAgentMessage(projectID, agentID, "assistant", "result", "updated response")

	reloaded := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	if err := reloaded.loadAgentMessages(); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.loadAgentTranscripts(); err != nil {
		t.Fatal(err)
	}
	items := reloaded.agentTranscriptForModel(projectID, agentID)
	if len(items) != 3 {
		t.Fatalf("reloaded transcript = %+v", items)
	}
	if items[0].Body != "first request" || items[1].Kind != "interrupt" || items[1].Body != "change the priority" || items[2].Body != "updated response" {
		t.Fatalf("interrupt ordering changed after reload: %+v", items)
	}
	for i, item := range items {
		if item.Seq != int64(i+1) || item.CreatedAt.IsZero() {
			t.Fatalf("reloaded transcript sequence/timestamp invalid: %+v", item)
		}
	}
}

func TestLegacyAgentMessagesConvertLazilyAndLosslessly(t *testing.T) {
	dataDir := t.TempDir()
	key := projectAgentKey("legacy-project", "legacy-agent")
	created := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	legacy := map[string][]AgentMessage{key: {{
		ID: "legacy-message", ProjectID: "legacy-project", AgentID: "legacy-agent", SessionID: "legacy-session",
		Seq: 7, Role: "tool_result", Intent: "repo_read", Body: `{"path":"README.md"}`, CreatedAt: created,
	}}}
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "agent-messages.json"), payload, 0644); err != nil {
		t.Fatal(err)
	}

	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	if err := a.loadAgentMessages(); err != nil {
		t.Fatal(err)
	}
	if err := a.loadAgentTranscripts(); err != nil {
		t.Fatal(err)
	}
	items := a.agentTranscriptForModel("legacy-project", "legacy-agent")
	if len(items) != 1 {
		t.Fatalf("legacy transcript = %+v", items)
	}
	item := items[0]
	if item.ID != "legacy-message" || item.MessageID != "legacy-message" || item.Seq != 7 || item.Role != "tool_result" || item.Intent != "repo_read" || item.Body != `{"path":"README.md"}` || item.Kind != "tool_result" || !item.CreatedAt.Equal(created) {
		t.Fatalf("legacy conversion lost data: %+v", item)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "agent-transcripts.json")); !os.IsNotExist(err) {
		t.Fatalf("lazy legacy conversion unexpectedly rewrote transcript storage: %v", err)
	}
}

func TestPromptBoundsStructuredHistoryAndReadsOnlyCurrentGroupInbox(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent, ok := a.projectAgent(project, "worker-a")
	if !ok {
		t.Fatal("worker-a missing")
	}
	agent.GroupID, agent.GroupName, agent.GroupRole = "build", "Build", "builder"
	agents := []Agent{agent}
	for i := 0; i < 30; i++ {
		agents = append(agents, Agent{ID: "peer-" + strconv.Itoa(i), ProjectID: project.ID, Nickname: "Peer " + strconv.Itoa(i), GroupID: "build", GroupName: "Build", GroupRole: "member"})
	}
	a.agents[project.ID] = agents

	key := projectAgentKey(project.ID, agent.ID)
	for i := 1; i <= 80; i++ {
		a.agentTranscripts[key] = append(a.agentTranscripts[key], AgentTranscriptItem{
			ID: "history-" + strconv.Itoa(i), MessageID: "history-" + strconv.Itoa(i), ProjectID: project.ID, AgentID: agent.ID,
			Seq: int64(i), Role: "assistant", Kind: "message", Intent: "result", Body: strings.Repeat("history ", 220), Visible: true, CreatedAt: time.Now().UTC(),
		})
	}
	state := a.ensureAgentSession(project.ID, agent.ID)
	state.ShortWindowStartSeq = 1
	a.updateAgentSessionState(state)

	for i := 0; i < 24; i++ {
		entry := AgentMemoryEntry{ID: "memory-" + strconv.Itoa(i), ProjectID: project.ID, AgentID: agent.ID, Layer: "fact", State: "active", Summary: "needle " + strings.Repeat("summary ", 160), Detail: strings.Repeat("detail ", 160), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		a.memories[key] = append(a.memories[key], entry)
	}
	for i := 0; i < 30; i++ {
		a.inbox[key] = append(a.inbox[key], AgentInboxMessage{ID: "pending-" + strconv.Itoa(i), ProjectID: project.ID, SourceAgentID: "peer-0", TargetAgentID: agent.ID, Subject: strings.Repeat("subject ", 100), Objective: strings.Repeat("objective ", 100), ExpectedOutput: strings.Repeat("expected ", 100), Body: strings.Repeat("body ", 100), Status: HandoffDelivered, CreatedAt: time.Now().UTC()})
	}
	// This deliberately inconsistent foreign storage key still describes a
	// group-to-group message. A whole-map scan would render it; keyed group
	// lookup must never read it.
	a.inbox[projectAgentKey(project.ID, "outsider")] = []AgentInboxMessage{{ID: "foreign", ProjectID: project.ID, SourceAgentID: "peer-0", TargetAgentID: "peer-1", Subject: "FOREIGN_GLOBAL_INBOX_MARKER", Body: "must not be scanned", Status: HandoffDelivered, CreatedAt: time.Now().UTC()}}
	a.inbox[projectAgentKey(project.ID, "peer-1")] = []AgentInboxMessage{{ID: "local", ProjectID: project.ID, SourceAgentID: agent.ID, TargetAgentID: "peer-1", Subject: "CURRENT_GROUP_MARKER", Body: "current group event", Status: HandoffDelivered, CreatedAt: time.Now().UTC()}}

	prompt := a.buildResidentAgentPromptWithMemoryQuery(project, agent, "needle details please", "dev", "needle details please")
	if estimated := estimateModelBoundTranscriptTokens(a.agentTranscriptForModel(project.ID, agent.ID)); estimated > residentTranscriptPromptMaxChars/4+100 {
		t.Fatalf("model-bound transcript counter ignored its character cap: %d", estimated)
	}
	transcriptSection := m6PromptSection(prompt, "### Recent structured resident transcript", "### Current runtime instruction")
	if len(transcriptSection) > residentTranscriptPromptMaxChars+500 {
		t.Fatalf("structured transcript exceeded cap: %d chars", len(transcriptSection))
	}
	if count := strings.Count(transcriptSection, "ASSISTANT:"); count > residentTranscriptPromptMaxItems {
		t.Fatalf("structured transcript item cap exceeded: %d", count)
	}
	pendingSection := m6PromptSection(prompt, "### Pending handoffs for this agent", "### Active pending memory")
	if strings.Count(pendingSection, "; subject:") > 8 {
		t.Fatalf("pending inbox section exceeded item cap:\n%s", pendingSection)
	}
	if strings.Contains(prompt, "FOREIGN_GLOBAL_INBOX_MARKER") || !strings.Contains(prompt, "CURRENT_GROUP_MARKER") {
		t.Fatalf("team activity did not use only current group inbox keys:\n%s", prompt)
	}
	if len(prompt) > 70_000 {
		t.Fatalf("bounded prompt unexpectedly large: %d chars", len(prompt))
	}
}

func TestFrontendContextCounterMatchesModelBoundTranscriptEstimate(t *testing.T) {
	items := make([]AgentTranscriptItem, 0, 52)
	for i := 0; i < 52; i++ {
		role, intent, body := "assistant", "result", "Reply number "+strconv.Itoa(i)
		if i%3 == 0 {
			role, intent, body = "tool_result", "repo_search", "搜索结果 "+strconv.Itoa(i)
		}
		items = append(items, AgentTranscriptItem{Seq: int64(i + 1), Role: role, Intent: intent, Body: body})
	}
	expected := estimateModelBoundTranscriptTokens(items)
	actual := frontendContextTokenEstimate(t, compactTranscriptForContextCounter(items))
	if delta := expected - actual; delta < -residentContextEstimatorTolerance || delta > residentContextEstimatorTolerance {
		t.Fatalf("model-bound transcript estimator=%d frontend=%d tolerance=%d", expected, actual, residentContextEstimatorTolerance)
	}
}

func frontendContextTokenEstimate(t *testing.T, history any) int {
	t.Helper()
	source, err := staticFS.ReadFile("static/js/context-tokens.js")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	program := `const vm=require('vm'); const box={window:{}}; vm.createContext(box); vm.runInContext(` + strconv.Quote(string(source)) + `, box); const history=` + string(payload) + `; process.stdout.write(String(box.window.KarozContextTokens.estimateContextTokens(history, [], '')));`
	output, err := exec.Command("node", "-e", program).CombinedOutput()
	if err != nil {
		t.Fatalf("frontend estimator failed: %v\n%s", err, output)
	}
	actual, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		t.Fatal(err)
	}
	return actual
}

func m6PromptSection(prompt, start, end string) string {
	begin := strings.Index(prompt, start)
	if begin < 0 {
		return ""
	}
	section := prompt[begin:]
	if endIndex := strings.Index(section, end); endIndex >= 0 {
		section = section[:endIndex]
	}
	return section
}
