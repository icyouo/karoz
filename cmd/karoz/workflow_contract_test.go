package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRunHandoffArtifactTaskContract(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	t.Setenv("KAROZ_TASK_AUTO_RUN", "0")
	root := t.TempDir()
	projectPath := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: projectID(projectPath), Name: "demo", Path: projectPath, WorkspaceRoot: root, DefaultBranch: "main"}
	designer := Agent{ID: "designer", ProjectID: project.ID, Name: "product-designer", Role: "design"}
	reviewer := Agent{ID: "reviewer", ProjectID: project.ID, Name: "design-critic", Role: "review"}
	builder := Agent{ID: "builder", ProjectID: project.ID, Name: "implementation-lead", Role: "implementation"}
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	a.agents[project.ID] = []Agent{designer, reviewer, builder}

	designRun, started := a.beginAgentRun(AgentRunInput{
		RunID: "run-design", ProjectID: project.ID, AgentID: designer.ID, Trigger: RunTriggerUserDirect, TurnType: "plan",
	})
	if !started {
		t.Fatal("designer run did not start")
	}
	designerCtx := ResidentToolContext{Project: project, Agent: designer, Workdir: project.Path, RunID: designRun.ID}
	writeResult, err := a.executeResidentTool(context.Background(), designerCtx, codexToolCall{
		Name: "write_workspace_file", Arguments: `{"path":"checkout.html","content":"<!doctype html><html><body>Checkout</body></html>","artifact_kind":"mockup_html","title":"Checkout"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		Artifact Artifact `json:"artifact"`
	}
	if err := json.Unmarshal([]byte(writeResult), &written); err != nil {
		t.Fatal(err)
	}
	artifact := written.Artifact
	if artifact.CreatedByRunID != designRun.ID || artifact.Revision != 1 || artifact.Status != ArtifactDraft {
		t.Fatalf("artifact/run contract = %+v", artifact)
	}

	submitArgs, _ := json.Marshal(map[string]any{"artifact_id": artifact.ID, "note": "ready"})
	if _, err := a.executeResidentTool(context.Background(), designerCtx, codexToolCall{Name: "submit_artifact", Arguments: string(submitArgs)}); err != nil {
		t.Fatal(err)
	}
	handoffArgs, _ := json.Marshal(map[string]any{
		"target_agent_id": reviewer.ID,
		"intent":          "handoff",
		"subject":         "Review checkout",
		"body":            "Review the submitted checkout artifact",
		"objective":       "Approve or request changes",
		"expected_output": "Artifact review decision",
		"artifact_ids":    []string{artifact.ID},
	})
	if _, err := a.executeResidentTool(context.Background(), designerCtx, codexToolCall{Name: "send_to", Arguments: string(handoffArgs)}); err != nil {
		t.Fatal(err)
	}
	designFinished, ok := a.finishAgentRun(project.ID, designer.ID, designRun.ID, RunStateDone, nil)
	if !ok || designFinished.State != RunStateDone || designFinished.EndedAt == nil {
		t.Fatalf("designer run finish = %+v ok=%v", designFinished, ok)
	}

	pending := a.pendingInboxFor(project.ID, reviewer.ID, 10)
	if len(pending) != 1 {
		t.Fatalf("reviewer inbox = %+v", pending)
	}
	handoff := pending[0]
	if handoff.ParentRunID != designRun.ID || handoff.CorrelationID == "" || len(handoff.ArtifactIDs) != 1 || handoff.ArtifactIDs[0] != artifact.ID {
		t.Fatalf("handoff contract = %+v", handoff)
	}

	reviewRun, started := a.beginAgentRun(AgentRunInput{
		RunID: "run-review", ProjectID: project.ID, AgentID: reviewer.ID, Trigger: RunTriggerHandoff,
		TurnType: "ask", SourceID: handoff.ID, MessageID: handoff.ID,
	})
	if !started {
		t.Fatal("reviewer run did not start")
	}
	reviewerCtx := ResidentToolContext{Project: project, Agent: reviewer, Workdir: project.Path, RunID: reviewRun.ID}
	reviewArgs, _ := json.Marshal(map[string]any{"artifact_id": artifact.ID, "decision": "approved", "note": "meets contract"})
	if _, err := a.executeResidentTool(context.Background(), reviewerCtx, codexToolCall{Name: "review_artifact", Arguments: string(reviewArgs)}); err != nil {
		t.Fatal(err)
	}
	replyArgs, _ := json.Marshal(map[string]any{"inbox_message_id": handoff.ID, "body": "Approved artifact " + artifact.ID})
	if _, err := a.executeResidentTool(context.Background(), reviewerCtx, codexToolCall{Name: "reply_to", Arguments: string(replyArgs)}); err != nil {
		t.Fatal(err)
	}
	designerReplies := a.pendingInboxFor(project.ID, designer.ID, 10)
	if len(designerReplies) != 1 || designerReplies[0].MessageType != "reply" {
		t.Fatalf("designer reply delivery = %+v", designerReplies)
	}
	ackArgs, _ := json.Marshal(map[string]any{"inbox_message_id": designerReplies[0].ID})
	if _, err := a.executeResidentTool(context.Background(), designerCtx, codexToolCall{Name: "ack_inbox", Arguments: string(ackArgs)}); err != nil {
		t.Fatal(err)
	}
	reviewFinished, ok := a.finishAgentRun(project.ID, reviewer.ID, reviewRun.ID, RunStateDone, nil)
	if !ok || reviewFinished.SourceID != handoff.ID || reviewFinished.MessageID != handoff.ID {
		t.Fatalf("review run/handoff contract = %+v ok=%v", reviewFinished, ok)
	}
	approved, ok := a.artifactByID(project.ID, artifact.ID)
	if !ok || approved.Status != ArtifactApproved || approved.ApprovedBy != reviewer.ID || approved.Revision != artifact.Revision {
		t.Fatalf("approved artifact = %+v found=%v", approved, ok)
	}
	replied, _ := a.inboxMessage(project.ID, reviewer.ID, handoff.ID)
	if replied.Status != HandoffClosed || replied.ResultMessageID == "" || replied.Result != "Approved artifact "+artifact.ID {
		t.Fatalf("review handoff result = %+v", replied)
	}

	buildRun, started := a.beginAgentRun(AgentRunInput{
		RunID: "run-build", ProjectID: project.ID, AgentID: builder.ID, Trigger: RunTriggerUserDirect, TurnType: "dev",
	})
	if !started {
		t.Fatal("builder run did not start")
	}
	taskArgs, _ := json.Marshal(map[string]any{
		"title": "Implement checkout", "description": "Implement the approved checkout", "type": "feature", "artifact_ids": []string{artifact.ID},
	})
	if _, err := a.executeResidentTool(context.Background(), ResidentToolContext{Project: project, Agent: builder, Workdir: project.Path, RunID: buildRun.ID}, codexToolCall{Name: "create_task", Arguments: string(taskArgs)}); err != nil {
		t.Fatal(err)
	}
	buildFinished, ok := a.finishAgentRun(project.ID, builder.ID, buildRun.ID, RunStateDone, nil)
	if !ok || buildFinished.State != RunStateDone {
		t.Fatalf("builder run finish = %+v ok=%v", buildFinished, ok)
	}
	tasks := a.tasksForProject(project.ID)
	if len(tasks) != 1 || len(tasks[0].ArtifactIDs) != 1 || tasks[0].ArtifactIDs[0] != artifact.ID {
		t.Fatalf("task/artifact contract = %+v", tasks)
	}
}
