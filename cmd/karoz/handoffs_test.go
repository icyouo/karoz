package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newHandoffTestApp(t *testing.T) (*app, Project, Agent, Agent) {
	t.Helper()
	root := t.TempDir()
	projectPath := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: projectID(projectPath), Name: "demo", Path: projectPath, WorkspaceRoot: root, WorkspaceType: "main", DefaultBranch: "main"}
	source := Agent{ID: "product", ProjectID: project.ID, Name: "product", Nickname: "Product"}
	target := Agent{ID: "designer", ProjectID: project.ID, Name: "product-designer", Nickname: "Designer"}
	a := &app{
		settings:        Settings{DataDir: t.TempDir(), ProjectsRoot: root},
		agents:          map[string][]Agent{project.ID: {source, target}},
		inbox:           map[string][]AgentInboxMessage{},
		agentMessages:   map[string][]AgentMessage{},
		agentSessions:   map[string]AgentSessionState{},
		agentRoutes:     map[string][]AgentRoute{},
		tasks:           map[string][]Task{},
		blackboard:      map[string][]AgentBlackboardEntry{},
		artifacts:       map[string][]Artifact{},
		runtimeHooks:    map[string]bool{},
		runtimeWatchers: map[string]map[chan RuntimeEvent]bool{},
	}
	return a, project, source, target
}

func TestHandoffProtocolCarriesRunCorrelationAndCloses(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	a, project, source, target := newHandoffTestApp(t)
	a.artifacts[project.ID] = []Artifact{
		{ID: "requirements-1", ProjectID: project.ID, AgentID: source.ID, Kind: "requirements", Status: ArtifactApproved},
		{ID: "flow-2", ProjectID: project.ID, AgentID: source.ID, Kind: "user_flow", Status: ArtifactApproved},
	}
	args, _ := json.Marshal(map[string]any{
		"target_agent_id": target.ID,
		"subject":         "Design checkout",
		"body":            "Create the checkout design",
		"objective":       "Produce a checkout mockup",
		"expected_output": "HTML mockup and implementation notes",
		"artifact_ids":    []string{"requirements-1", "flow-2"},
	})
	result, err := a.executeResidentTool(context.Background(), ResidentToolContext{
		Project: project, Agent: source, Workdir: project.Path, RunID: "run-product-1",
	}, codexToolCall{Name: "send_to", Arguments: string(args)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"status":"delivered"`) || !strings.Contains(result, `"parent_run_id":"run-product-1"`) {
		t.Fatalf("send result = %s", result)
	}
	items := a.pendingInboxFor(project.ID, target.ID, 10)
	if len(items) != 1 {
		t.Fatalf("target inbox = %+v", items)
	}
	original := items[0]
	if original.Status != HandoffDelivered || original.DeliveredAt == nil || original.CorrelationID == "" || original.ParentRunID != "run-product-1" {
		t.Fatalf("delivered handoff = %+v", original)
	}
	if original.Objective != "Produce a checkout mockup" || original.ExpectedOutput != "HTML mockup and implementation notes" || len(original.ArtifactIDs) != 2 {
		t.Fatalf("handoff contract = %+v", original)
	}

	replyArgs, _ := json.Marshal(map[string]any{"inbox_message_id": original.ID, "body": "Mockup complete"})
	replyResult, err := a.executeResidentTool(context.Background(), ResidentToolContext{
		Project: project, Agent: target, Workdir: project.Path, RunID: "run-designer-1",
	}, codexToolCall{Name: "reply_to", Arguments: string(replyArgs)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replyResult, `"correlation_id":"`+original.CorrelationID+`"`) || !strings.Contains(replyResult, `"parent_run_id":"run-designer-1"`) {
		t.Fatalf("reply result = %s", replyResult)
	}
	replied, _ := a.inboxMessage(project.ID, target.ID, original.ID)
	if replied.Status != HandoffClosed || replied.RepliedAt == nil || replied.ClosedAt == nil || replied.Result != "Mockup complete" || replied.ResultMessageID == "" {
		t.Fatalf("replied handoff = %+v", replied)
	}
	duplicate, err := a.executeResidentTool(context.Background(), ResidentToolContext{Project: project, Agent: target}, codexToolCall{Name: "reply_to", Arguments: string(replyArgs)})
	if err != nil || !strings.Contains(duplicate, "already_closed") {
		t.Fatalf("duplicate reply = %s err=%v", duplicate, err)
	}

	replies := a.pendingInboxFor(project.ID, source.ID, 10)
	if len(replies) != 0 {
		t.Fatalf("terminal reply left pending = %+v", replies)
	}
	var replyPayload struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal([]byte(replyResult), &replyPayload); err != nil || replyPayload.MessageID == "" {
		t.Fatalf("reply result = %s err=%v", replyResult, err)
	}
	replyNotice, ok := a.inboxMessage(project.ID, source.ID, replyPayload.MessageID)
	if !ok || replyNotice.Status != HandoffClosed || replyNotice.AckedAt == nil || replyNotice.ReplyToID != original.ID {
		t.Fatalf("reply notification = %+v ok=%v", replyNotice, ok)
	}
	if got := a.scheduledAgentRunCount(project.ID, source.ID); got != 0 {
		t.Fatalf("terminal reply scheduled %d agent runs", got)
	}
}

func TestReplyToReplyIsRejectedAndConsumed(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	a, project, source, target := newHandoffTestApp(t)
	reply := AgentInboxMessage{
		ID: "reply-1", ProjectID: project.ID, SourceAgentID: target.ID, TargetAgentID: source.ID,
		CorrelationID: "corr-terminal", MessageType: "reply", Intent: "reply", Subject: "Result", Body: "Done",
		ReplyToID: "original-1", Status: HandoffQueued, CreatedAt: time.Now().UTC(),
	}
	if err := a.queueInboxMessage(project.ID, reply); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"inbox_message_id": reply.ID, "body": "Thanks"})
	result, err := a.executeResidentTool(context.Background(), ResidentToolContext{Project: project, Agent: source}, codexToolCall{Name: "reply_to", Arguments: string(args)})
	if err != nil || !strings.Contains(result, "reply_is_terminal") {
		t.Fatalf("reply-to-reply result = %s err=%v", result, err)
	}
	consumed, ok := a.inboxMessage(project.ID, source.ID, reply.ID)
	if !ok || consumed.Status != HandoffClosed || consumed.AckedAt == nil {
		t.Fatalf("terminal reply = %+v ok=%v", consumed, ok)
	}
	if got := a.pendingInboxFor(project.ID, target.ID, 10); len(got) != 0 {
		t.Fatalf("reply-to-reply created another message: %+v", got)
	}
}

func TestScheduledReplyIsConsumedWithoutModelTurn(t *testing.T) {
	a, project, source, target := newHandoffTestApp(t)
	reply := AgentInboxMessage{
		ID: "reply-recovered", ProjectID: project.ID, SourceAgentID: target.ID, TargetAgentID: source.ID,
		CorrelationID: "corr-recovered", MessageType: "reply", Intent: "reply", Subject: "Result", Body: "Done",
		ReplyToID: "original-recovered", Status: HandoffQueued, CreatedAt: time.Now().UTC(),
	}
	if err := a.queueInboxMessage(project.ID, reply); err != nil {
		t.Fatal(err)
	}
	job, err := newScheduledRun(
		ScheduledRunHandoff,
		AgentRunInput{ProjectID: project.ID, AgentID: source.ID, Trigger: RunTriggerHandoff, MessageID: reply.ID},
		"handoff/"+project.ID+"/"+source.ID+"/"+reply.ID,
		HandoffRunPayload{InboxMessageID: reply.ID},
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.executeHandoffScheduledRun(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	consumed, ok := a.inboxMessage(project.ID, source.ID, reply.ID)
	if !ok || consumed.Status != HandoffClosed || consumed.AckedAt == nil {
		t.Fatalf("scheduled reply = %+v ok=%v", consumed, ok)
	}
	if messages := a.agentMessagesFor(project.ID, source.ID); len(messages) != 0 {
		t.Fatalf("terminal reply invoked model turn: %+v", messages)
	}
}

func TestCollaborationCorrelationMessageLimit(t *testing.T) {
	a, project, source, target := newHandoffTestApp(t)
	for i := 0; i < maxCollaborationMessagesPerCorrelation; i++ {
		msg := AgentInboxMessage{
			ID: fmt.Sprintf("msg-%d", i), ProjectID: project.ID, SourceAgentID: source.ID, TargetAgentID: target.ID,
			CorrelationID: "corr-limited", MessageType: "handoff", Intent: "request", Subject: "Work", Body: "Work",
			Status: HandoffQueued, CreatedAt: time.Now().UTC(),
		}
		if err := a.queueInboxMessage(project.ID, msg); err != nil {
			t.Fatalf("queue message %d: %v", i, err)
		}
	}
	overflow := AgentInboxMessage{
		ID: "msg-overflow", ProjectID: project.ID, SourceAgentID: source.ID, TargetAgentID: target.ID,
		CorrelationID: "corr-limited", MessageType: "handoff", Intent: "request", Subject: "Work", Body: "Work",
		Status: HandoffQueued, CreatedAt: time.Now().UTC(),
	}
	if err := a.queueInboxMessage(project.ID, overflow); err == nil || !strings.Contains(err.Error(), "message limit") {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestHandoffWorkingFailureAndRetryLifecycle(t *testing.T) {
	a, project, source, target := newHandoffTestApp(t)
	msg := AgentInboxMessage{
		ID: "handoff-1", ProjectID: project.ID, SourceAgentID: source.ID, TargetAgentID: target.ID,
		MessageType: "handoff", Subject: "Review", Body: "Review it", Status: HandoffQueued, CreatedAt: time.Now().UTC(),
	}
	if err := a.queueInboxMessage(project.ID, msg); err != nil {
		t.Fatal(err)
	}
	if !a.claimHandoff(project.ID, target.ID, msg.ID) {
		t.Fatal("handoff was not claimed")
	}
	working, _ := a.inboxMessage(project.ID, target.ID, msg.ID)
	if working.Status != HandoffWorking || working.ClaimedAt == nil || working.WorkingAt == nil {
		t.Fatalf("working handoff = %+v", working)
	}
	a.failHandoff(project.ID, target.ID, msg.ID, context.DeadlineExceeded)
	failed, _ := a.inboxMessage(project.ID, target.ID, msg.ID)
	if failed.Status != HandoffFailed || failed.FailedAt == nil || failed.FailureReason == "" {
		t.Fatalf("failed handoff = %+v", failed)
	}
	a.retryHandoff(project.ID, target.ID, msg.ID)
	retried, _ := a.inboxMessage(project.ID, target.ID, msg.ID)
	if retried.Status != HandoffDelivered || retried.FailureReason != "" {
		t.Fatalf("retried handoff = %+v", retried)
	}
}

func TestDeclinedHandoffNotifiesRequesterAndThenCloses(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	a, project, source, target := newHandoffTestApp(t)
	msg := AgentInboxMessage{
		ID: "decline-me", ProjectID: project.ID, SourceAgentID: source.ID, TargetAgentID: target.ID,
		CorrelationID: "corr-decline", MessageType: "handoff", Subject: "Impossible request", Body: "Do it",
		Status: HandoffQueued, CreatedAt: time.Now().UTC(),
	}
	if err := a.queueInboxMessage(project.ID, msg); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"inbox_message_id": msg.ID, "reason": "Missing required source material"})
	result, err := a.executeResidentTool(context.Background(), ResidentToolContext{Project: project, Agent: target}, codexToolCall{Name: "decline_handoff", Arguments: string(args)})
	if err != nil || !strings.Contains(result, `"status":"declined"`) {
		t.Fatalf("decline result = %s err=%v", result, err)
	}
	declined, _ := a.inboxMessage(project.ID, target.ID, msg.ID)
	if declined.Status != HandoffDeclined || declined.DeclinedAt == nil || declined.ResultMessageID == "" {
		t.Fatalf("declined handoff = %+v", declined)
	}
	notifications := a.pendingInboxFor(project.ID, source.ID, 10)
	if len(notifications) != 1 || notifications[0].MessageType != "decline" || notifications[0].ReplyToID != msg.ID || notifications[0].CorrelationID != msg.CorrelationID {
		t.Fatalf("decline notification = %+v", notifications)
	}
	ackArgs, _ := json.Marshal(map[string]any{"inbox_message_id": notifications[0].ID})
	if _, err := a.executeResidentTool(context.Background(), ResidentToolContext{Project: project, Agent: source}, codexToolCall{Name: "ack_inbox", Arguments: string(ackArgs)}); err != nil {
		t.Fatal(err)
	}
	closed, _ := a.inboxMessage(project.ID, target.ID, msg.ID)
	if closed.Status != HandoffClosed || closed.ClosedAt == nil {
		t.Fatalf("closed declined handoff = %+v", closed)
	}
}

func TestLoadInboxMigratesLegacyPendingHandoff(t *testing.T) {
	dataDir := t.TempDir()
	legacy := map[string][]AgentInboxMessage{
		"p1/designer": {{
			ID: "legacy", ProjectID: "p1", SourceAgentID: "product", TargetAgentID: "designer",
			Subject: "Legacy request", Body: "Please handle", ThreadKey: "thread-1", Status: "pending", CreatedAt: time.Now().UTC(),
		}},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "agent-inbox.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	a := &app{settings: Settings{DataDir: dataDir}, inbox: map[string][]AgentInboxMessage{}}
	if err := a.loadInbox(); err != nil {
		t.Fatal(err)
	}
	msg, ok := a.inboxMessage("p1", "designer", "legacy")
	if !ok || msg.Status != HandoffDelivered || msg.CorrelationID != "thread-1" || msg.Objective == "" || msg.ExpectedOutput == "" || msg.DeliveredAt == nil {
		t.Fatalf("migrated handoff = %+v ok=%v", msg, ok)
	}
}
