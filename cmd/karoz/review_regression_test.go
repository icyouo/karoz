package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestStaleRunCompletionCannotFinishReplacementRun(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	first, started := a.beginAgentRun(AgentRunInput{RunID: "run-a", ProjectID: "p1", AgentID: "designer", Trigger: RunTriggerUserDirect})
	if !started {
		t.Fatal("first run did not start")
	}
	if cancelled, ok := a.cancelAgentRun("p1", "designer"); !ok || cancelled.ID != first.ID {
		t.Fatalf("cancelled run = %+v ok=%v", cancelled, ok)
	}
	second, started := a.beginAgentRun(AgentRunInput{RunID: "run-b", ProjectID: "p1", AgentID: "designer", Trigger: RunTriggerHandoff})
	if !started {
		t.Fatal("replacement run did not start")
	}
	if stale, ok := a.finishAgentRun("p1", "designer", first.ID, RunStateDone, nil); ok {
		t.Fatalf("stale completion unexpectedly finished run: %+v", stale)
	}
	active, ok := a.activeAgentRun("p1", "designer")
	if !ok || active.ID != second.ID {
		t.Fatalf("replacement run was terminated: %+v ok=%v", active, ok)
	}
}

func TestRemovingRuntimeWatcherDoesNotCloseChannelDuringBroadcast(t *testing.T) {
	a := newApp(Settings{})
	const iterations = 200
	var group sync.WaitGroup
	for i := 0; i < iterations; i++ {
		ch := make(chan RuntimeEvent, 4)
		a.addRuntimeWatcher("p1", ch)
		group.Add(2)
		go func() {
			defer group.Done()
			a.broadcastRuntimeEvent(RuntimeEvent{ProjectID: "p1", Kind: "test"})
		}()
		go func() {
			defer group.Done()
			a.removeRuntimeWatcher("p1", ch)
		}()
		group.Wait()
		select {
		case ch <- RuntimeEvent{ProjectID: "p1", Kind: "after_remove"}:
		default:
		}
	}
}

func TestInvalidArtifactMetadataCannotReplaceApprovedFile(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir(), DefaultBranch: "main"}
	designer := Agent{ID: "designer", ProjectID: project.ID}
	reviewer := Agent{ID: "reviewer", ProjectID: project.ID}
	a.agents[project.ID] = []Agent{designer, reviewer}
	ctx := ResidentToolContext{Project: project, Agent: designer, Workdir: project.Path, RunID: "run-design"}
	initialContent := "<!doctype html><html><body>approved</body></html>"
	writeResult, err := a.executeResidentTool(context.Background(), ctx, codexToolCall{Name: "write_workspace_file", Arguments: `{"path":"approved.html","content":"` + initialContent + `","artifact_kind":"mockup_html"}`})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Artifact Artifact `json:"artifact"`
	}
	if err := json.Unmarshal([]byte(writeResult), &payload); err != nil {
		t.Fatal(err)
	}
	submit, _ := json.Marshal(map[string]any{"artifact_id": payload.Artifact.ID})
	if _, err := a.executeResidentTool(context.Background(), ctx, codexToolCall{Name: "submit_artifact", Arguments: string(submit)}); err != nil {
		t.Fatal(err)
	}
	review, _ := json.Marshal(map[string]any{"artifact_id": payload.Artifact.ID, "decision": "approved"})
	if _, err := a.executeResidentTool(context.Background(), ResidentToolContext{Project: project, Agent: reviewer}, codexToolCall{Name: "review_artifact", Arguments: string(review)}); err != nil {
		t.Fatal(err)
	}

	invalidResult, err := a.executeResidentTool(context.Background(), ctx, codexToolCall{Name: "write_workspace_file", Arguments: `{"path":"approved.html","content":"UNREVIEWED","artifact_kind":"not_a_kind"}`})
	if err != nil || !strings.Contains(invalidResult, "artifact_metadata_failed") {
		t.Fatalf("invalid write result = %s err=%v", invalidResult, err)
	}
	full, err := a.safeWorkspacePath(project.ID, designer.ID, "approved.html")
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(full)
	if err != nil || string(content) != initialContent {
		t.Fatalf("approved file changed to %q err=%v", content, err)
	}
	artifact, ok := a.artifactByID(project.ID, payload.Artifact.ID)
	if !ok || artifact.Status != ArtifactApproved || artifact.Revision != 1 {
		t.Fatalf("approved metadata changed: %+v found=%v", artifact, ok)
	}
}
