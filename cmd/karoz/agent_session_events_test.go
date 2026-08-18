package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAssembledSessionEventReplayRebuildsModelHistoryAndActivity(t *testing.T) {
	dataDir := t.TempDir()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	projectID, agentID := "project-assembled", "agent-assembled"
	run, started := a.beginAgentRun(AgentRunInput{
		RunID: "assembled-run", ProjectID: projectID, AgentID: agentID,
		Trigger: RunTriggerUserDirect, TurnType: "dev",
	})
	if !started {
		t.Fatal("Run did not start")
	}
	if _, ok := a.appendAgentMessageForRun(projectID, agentID, run.ID, "user", "ask", "Review the durable flow."); !ok {
		t.Fatal("user message was not persisted")
	}
	call := codexToolCall{ID: "assembled-call", CallID: "assembled-call", Name: "repo_search", Arguments: `{"query":"event"}`}
	if _, ok := a.appendAgentToolCallForRun(projectID, agentID, run.ID, call); !ok {
		t.Fatal("tool call was not persisted")
	}
	if _, ok := a.appendAgentToolResultForRun(projectID, agentID, run.ID, call, `{"matches":["agent-session-events.go"]}`, true); !ok {
		t.Fatal("tool result was not persisted")
	}
	if _, ok := a.appendAgentMessageForRun(projectID, agentID, run.ID, "assistant", "result", "The event flow is durable."); !ok {
		t.Fatal("assistant result was not persisted")
	}
	if _, finished := a.finishAgentRun(projectID, agentID, run.ID, RunStateCancelled, context.Canceled); !finished {
		t.Fatal("Run cancellation was not persisted")
	}

	checkpoint := a.agentSessionState(projectID, agentID)
	checkpoint.ResidentSummary = "Compacted source is retained in canonical events."
	checkpoint.CoveredSeqStart = 1
	checkpoint.CoveredSeqEnd = 3
	checkpoint.BoundarySeq = 3
	checkpoint.ShortWindowStartSeq = 4
	checkpoint.LongTermVersion = 1
	checkpoint.LastCheckpointAt = time.Now().UTC()
	a.updateAgentSessionState(checkpoint)

	handoff := AgentInboxMessage{
		ID: "assembled-handoff", ProjectID: projectID, SourceAgentID: "source", TargetAgentID: agentID,
		CorrelationID: "assembled-thread", Intent: "review", Subject: "Review persisted flow",
		Body: "Use the replayed model history.", CreatedAt: time.Now().UTC(),
	}
	if err := a.queueInboxMessage(projectID, handoff); err != nil {
		t.Fatal(err)
	}

	key := projectAgentKey(projectID, agentID)
	wantEvents := a.conversation.EventsFor(key)
	wantHistory := a.agentTranscriptForModel(projectID, agentID)
	wantActivity := a.agentMessagesFor(projectID, agentID)
	if len(wantEvents) == 0 || len(wantHistory) != 4 || len(wantActivity) != 4 {
		t.Fatalf("assembled source projections events=%d history=%d activity=%d", len(wantEvents), len(wantHistory), len(wantActivity))
	}

	reloaded := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	if err := reloaded.loadAgentSessionEvents(); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.conversation.EventsFor(key); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("event replay diverged\n got=%+v\nwant=%+v", got, wantEvents)
	}
	if got := reloaded.agentTranscriptForModel(projectID, agentID); !reflect.DeepEqual(got, wantHistory) {
		t.Fatalf("model history replay diverged\n got=%+v\nwant=%+v", got, wantHistory)
	}
	if got := reloaded.agentMessagesFor(projectID, agentID); !reflect.DeepEqual(got, wantActivity) {
		t.Fatalf("Studio activity replay diverged\n got=%+v\nwant=%+v", got, wantActivity)
	}
	state := reloaded.agentSessionState(projectID, agentID)
	if state.CoveredSeqEnd != 3 || state.ShortWindowStartSeq != 4 || state.ResidentSummary != checkpoint.ResidentSummary {
		t.Fatalf("checkpoint replay = %+v", state)
	}
}

func TestAgentSessionEventsReplayRunLifecycleWithoutPollutingTranscript(t *testing.T) {
	dataDir := t.TempDir()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	run, started := a.beginAgentRun(AgentRunInput{
		RunID: "event-run", ProjectID: "project-1", AgentID: "agent-1",
		Trigger: RunTriggerUserDirect, TurnType: "ask",
	})
	if !started {
		t.Fatal("Run did not start")
	}
	a.appendAgentMessage(run.ProjectID, run.AgentID, "user", "ask", "Persist this conversation fact.")
	if _, finished := a.finishAgentRun(run.ProjectID, run.AgentID, run.ID, RunStateCancelled, context.Canceled); !finished {
		t.Fatal("Run did not finish")
	}

	reloaded := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	if err := reloaded.loadAgentSessionEvents(); err != nil {
		t.Fatal(err)
	}
	key := projectAgentKey(run.ProjectID, run.AgentID)
	events := reloaded.conversation.EventsFor(key)
	if len(events) != 3 {
		t.Fatalf("event count = %d, want Run/message/Run: %+v", len(events), events)
	}
	for index, event := range events {
		if event.EventSeq != int64(index+1) {
			t.Fatalf("event order = %+v", events)
		}
	}
	if events[0].Type != agentSessionEventRunStateChanged || events[0].Run == nil || events[0].Run.State != RunStatePreparingContext || events[0].FromState != "idle" {
		t.Fatalf("Run start event = %+v", events[0])
	}
	if events[1].Type != agentSessionEventMessageAppended || events[1].Seq != 1 || events[1].Message == nil || events[1].Transcript == nil {
		t.Fatalf("message event = %+v", events[1])
	}
	if events[2].Type != agentSessionEventRunStateChanged || events[2].Run == nil || events[2].Run.State != RunStateCancelled || events[2].FromState != string(RunStatePreparingContext) {
		t.Fatalf("Run terminal event = %+v", events[2])
	}
	if items := reloaded.agentTranscriptForModel(run.ProjectID, run.AgentID); len(items) != 1 || items[0].Body != "Persist this conversation fact." {
		t.Fatalf("transcript projection = %+v", items)
	}
}

func TestAgentSessionEventsReplayHandoffLifecycleWithoutPollutingTranscript(t *testing.T) {
	dataDir := t.TempDir()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	handoff := AgentInboxMessage{
		ID: "handoff-1", SourceAgentID: "source", TargetAgentID: "target",
		CorrelationID: "thread-1", Intent: "review", Subject: "Review the change",
		Body: "Please review the event log change.", CreatedAt: time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC),
	}
	if err := a.queueInboxMessage("project-1", handoff); err != nil {
		t.Fatal(err)
	}

	reloaded := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	if err := reloaded.loadAgentSessionEvents(); err != nil {
		t.Fatal(err)
	}
	events := reloaded.conversation.EventsFor(projectAgentKey("project-1", "target"))
	if len(events) != 2 {
		t.Fatalf("event count = %d, want created/delivered: %+v", len(events), events)
	}
	if events[0].Type != agentSessionEventHandoffChanged || events[0].Handoff == nil || events[0].Handoff.Status != HandoffQueued || events[0].Reason != "handoff_created" {
		t.Fatalf("handoff creation event = %+v", events[0])
	}
	if events[1].Type != agentSessionEventHandoffChanged || events[1].Handoff == nil || events[1].Handoff.Status != HandoffDelivered || events[1].FromState != HandoffQueued || events[1].Reason != "handoff_delivered" {
		t.Fatalf("handoff delivery event = %+v", events[1])
	}
	if items := reloaded.agentTranscriptForModel("project-1", "target"); len(items) != 0 {
		t.Fatalf("handoff entered transcript projection: %+v", items)
	}
}

func TestAgentSessionEventsReplayResidentBashApprovalWithoutCommandText(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent, ok := a.projectAgent(project, "worker-a")
	if !ok {
		t.Fatal("worker-a missing")
	}
	run, started := a.beginAgentRun(AgentRunInput{
		RunID: "approval-run", ProjectID: project.ID, AgentID: agent.ID,
		Trigger: RunTriggerUserDirect, TurnType: "ask",
	})
	if !started {
		t.Fatal("Run did not start")
	}
	command := "printf 'approval event should be redacted'"
	subject, err := newResidentBashSubject(residentBashOperationForeground, project.ID, agent.ID, project.Path, command, "")
	if err != nil {
		t.Fatal(err)
	}
	a.requestResidentBashApprovalSubject(ResidentToolContext{Project: project, Agent: agent, Workdir: project.Path, RunID: run.ID}, subject, command)
	if len(a.agentRuntimeLocked().residentBashApprovals) != 1 {
		t.Fatalf("approvals = %+v", a.agentRuntimeLocked().residentBashApprovals)
	}
	var approval ResidentBashApproval
	for _, candidate := range a.agentRuntimeLocked().residentBashApprovals {
		approval = candidate
	}
	if recognized, err := a.resolveResidentBashChoice(project.ID, agent.ID, run.ID, residentBashApprovePrefix+approval.ID); !recognized || err != nil {
		t.Fatalf("approval resolution recognized=%t err=%v", recognized, err)
	}
	if !a.consumeResidentBashApprovalSubject(run.ID, subject) {
		t.Fatal("approval was not consumed")
	}

	reloaded := newApp(Settings{DataDir: a.settings.DataDir, ProjectsRoot: project.WorkspaceRoot})
	if err := reloaded.loadAgentSessionEvents(); err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, event := range reloaded.conversation.EventsFor(projectAgentKey(project.ID, agent.ID)) {
		if event.Type != agentSessionEventApprovalChanged || event.Approval == nil || event.Approval.ApprovalID != approval.ID {
			continue
		}
		states = append(states, event.Approval.State)
		if event.Approval.CommandSHA256 != subject.CommandSHA256 || event.Approval.CommandSHA256 == command {
			t.Fatalf("approval event leaked or lost command identity: %+v", event.Approval)
		}
	}
	if got, want := strings.Join(states, ","), "requested,approved,consumed"; got != want {
		t.Fatalf("approval event states = %q, want %q", got, want)
	}
	if items := reloaded.agentTranscriptForModel(project.ID, agent.ID); len(items) != 0 {
		t.Fatalf("approval entered transcript projection: %+v", items)
	}
}

func TestAgentSessionEventsReplayMonitorProbeApprovalWithoutSource(t *testing.T) {
	if !scriptProbeSupported {
		t.Skip("script probes are unsupported on this platform")
	}
	a, project := newHandlerTestApp(t)
	agent, ok := a.projectAgent(project, "worker-a")
	if !ok {
		t.Fatal("worker-a missing")
	}
	source := "printf 'monitor probe source must not enter session events'"
	result := a.prepareMonitorProbeFromTool(ResidentToolContext{Project: project, Agent: agent}, map[string]any{
		"language": "shell", "source": source,
	})
	if strings.Contains(result, `"error"`) {
		t.Fatalf("prepare monitor probe = %s", result)
	}
	var challenge monitorProbeChallenge
	for _, candidate := range a.monitorProbeChallenges {
		challenge = candidate
	}
	if challenge.ID == "" {
		t.Fatal("monitor probe challenge was not created")
	}
	run, started := a.beginAgentRun(AgentRunInput{
		RunID: "monitor-approval-run", ProjectID: project.ID, AgentID: agent.ID,
		Trigger: RunTriggerUserDirect, TurnType: "dev",
	})
	if !started {
		t.Fatal("Run did not start")
	}
	if recognized, err := a.resolveMonitorProbeChoice(project.ID, agent.ID, run.ID, monitorProbeApprovePrefix+challenge.ID); !recognized || err != nil {
		t.Fatalf("monitor approval recognized=%t err=%v", recognized, err)
	}

	reloaded := newApp(Settings{DataDir: a.settings.DataDir, ProjectsRoot: project.WorkspaceRoot})
	if err := reloaded.loadAgentSessionEvents(); err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, event := range reloaded.conversation.EventsFor(projectAgentKey(project.ID, agent.ID)) {
		if event.Type != agentSessionEventApprovalChanged || event.Approval == nil || event.Approval.ApprovalID != challenge.ID {
			continue
		}
		states = append(states, event.Approval.State)
		if event.Approval.Kind != "monitor_probe" || event.Approval.CommandSHA256 != challenge.SourceSHA256 || strings.Contains(event.Approval.CommandSHA256, source) {
			t.Fatalf("monitor approval event leaked or lost source identity: %+v", event.Approval)
		}
		if event.Approval.State == "approved" && event.Approval.ReceiptID == "" {
			t.Fatalf("approved monitor probe event missing receipt: %+v", event.Approval)
		}
		if event.Approval.State == "requested" && event.Approval.ReceiptID != "" {
			t.Fatalf("requested monitor probe event pre-committed a receipt: %+v", event.Approval)
		}
	}
	if got, want := strings.Join(states, ","), "requested,approved"; got != want {
		t.Fatalf("monitor approval event states = %q, want %q", got, want)
	}
}
