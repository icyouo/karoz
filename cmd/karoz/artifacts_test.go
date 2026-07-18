package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func artifactTestProject(t *testing.T) (string, Project) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	return root, Project{ID: projectID(path), Name: "demo", Path: path, WorkspaceRoot: root, DefaultBranch: "main"}
}

func TestDesignerBuilderReviewerArtifactWorkflow(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	t.Setenv("KAROZ_TASK_AUTO_RUN", "0")
	root := t.TempDir()
	projectPath := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: projectID(projectPath), Name: "demo", Path: projectPath, WorkspaceRoot: root, DefaultBranch: "main"}
	designer := Agent{ID: "designer", ProjectID: project.ID, Name: "product-designer", Nickname: "Designer"}
	builder := Agent{ID: "builder", ProjectID: project.ID, Name: "implementation-lead", Nickname: "Build"}
	reviewer := Agent{ID: "reviewer", ProjectID: project.ID, Name: "design-critic", Nickname: "Review"}
	a := &app{
		settings:        Settings{DataDir: t.TempDir(), ProjectsRoot: root},
		tasks:           map[string][]Task{},
		agents:          map[string][]Agent{project.ID: {designer, builder, reviewer}},
		artifacts:       map[string][]Artifact{},
		blackboard:      map[string][]AgentBlackboardEntry{},
		inbox:           map[string][]AgentInboxMessage{},
		taskHooks:       map[string][]TaskRuntimeHook{},
		agentRoutes:     map[string][]AgentRoute{},
		agentMessages:   map[string][]AgentMessage{},
		agentSessions:   map[string]AgentSessionState{},
		memories:        map[string][]AgentMemoryEntry{},
		archives:        map[string][]AgentArchiveMessage{},
		runtimeHooks:    map[string]bool{},
		runtimeWatchers: map[string]map[chan RuntimeEvent]bool{},
	}
	designerCtx := ResidentToolContext{Project: project, Agent: designer, Workdir: project.Path, RunID: "run-design-1"}
	writeResult, err := a.executeResidentTool(context.Background(), designerCtx, codexToolCall{Name: "write_workspace_file", Arguments: `{"path":"checkout.html","content":"<!doctype html><html><body>Checkout</body></html>","artifact_kind":"mockup_html","title":"Checkout mockup","description":"Desktop and mobile checkout"}`})
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
	if artifact.ID == "" || artifact.Kind != "mockup_html" || artifact.Revision != 1 || artifact.Status != ArtifactDraft || artifact.CreatedByRunID != "run-design-1" || !artifact.Previewable {
		t.Fatalf("written artifact = %+v result=%s", artifact, writeResult)
	}

	taskArgs, _ := json.Marshal(map[string]any{"title": "Implement checkout", "description": "Implement approved checkout", "type": "feature", "artifact_ids": []string{artifact.ID}})
	rejected, err := a.executeResidentTool(context.Background(), ResidentToolContext{Project: project, Agent: builder}, codexToolCall{Name: "create_task", Arguments: string(taskArgs)})
	if err != nil || !strings.Contains(rejected, "artifact_not_approved") {
		t.Fatalf("unapproved task result = %s err=%v", rejected, err)
	}

	submitArgs, _ := json.Marshal(map[string]any{"artifact_id": artifact.ID, "note": "Ready for design review"})
	if result, err := a.executeResidentTool(context.Background(), designerCtx, codexToolCall{Name: "submit_artifact", Arguments: string(submitArgs)}); err != nil || !strings.Contains(result, `"status":"reviewing"`) {
		t.Fatalf("submit result = %s err=%v", result, err)
	}
	selfReviewArgs, _ := json.Marshal(map[string]any{"artifact_id": artifact.ID, "decision": "approved", "note": "self approve"})
	if result, _ := a.executeResidentTool(context.Background(), designerCtx, codexToolCall{Name: "review_artifact", Arguments: string(selfReviewArgs)}); !strings.Contains(result, "cannot approve") {
		t.Fatalf("self approval was accepted: %s", result)
	}
	reviewResult, err := a.executeResidentTool(context.Background(), ResidentToolContext{Project: project, Agent: reviewer, RunID: "run-review-1"}, codexToolCall{Name: "review_artifact", Arguments: string(selfReviewArgs)})
	if err != nil || !strings.Contains(reviewResult, `"status":"approved"`) {
		t.Fatalf("review result = %s err=%v", reviewResult, err)
	}
	approved, _ := a.artifactByID(project.ID, artifact.ID)
	if approved.ApprovedBy != reviewer.ID || approved.ApprovedAt == nil || approved.Status != ArtifactApproved {
		t.Fatalf("approved artifact = %+v", approved)
	}

	taskResult, err := a.executeResidentTool(context.Background(), ResidentToolContext{Project: project, Agent: builder, RunID: "run-build-1"}, codexToolCall{Name: "create_task", Arguments: string(taskArgs)})
	if err != nil || strings.Contains(taskResult, `"error"`) {
		t.Fatalf("approved task result = %s err=%v", taskResult, err)
	}
	tasks := a.tasksForProject(project.ID)
	if len(tasks) != 1 || len(tasks[0].ArtifactIDs) != 1 || tasks[0].ArtifactIDs[0] != artifact.ID {
		t.Fatalf("artifact task = %+v", tasks)
	}
	prompt := a.buildDevelopmentPrompt(project, tasks[0])
	if !strings.Contains(prompt, artifact.ID) || !strings.Contains(prompt, "status=approved") || !strings.Contains(prompt, "checkout.html") {
		t.Fatalf("task prompt missing artifact contract:\n%s", prompt)
	}

	rewriteResult, err := a.executeResidentTool(context.Background(), ResidentToolContext{Project: project, Agent: designer, RunID: "run-design-2"}, codexToolCall{Name: "write_workspace_file", Arguments: `{"path":"checkout.html","content":"<!doctype html><html><body>Checkout v2</body></html>","artifact_kind":"mockup_html","title":"Checkout mockup"}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(rewriteResult), &written); err != nil {
		t.Fatal(err)
	}
	if written.Artifact.ID != artifact.ID || written.Artifact.Revision != 2 || written.Artifact.Status != ArtifactDraft || written.Artifact.ApprovedAt != nil || len(written.Artifact.Revisions) != 2 {
		t.Fatalf("revised artifact = %+v", written.Artifact)
	}
	projections := a.blackboardFor(project.ID, 50)
	foundProjection := false
	for _, entry := range projections {
		if entry.SourceType == blackboardSourceArtifact && entry.SourceID == artifact.ID && entry.Status == ArtifactDraft {
			foundProjection = true
		}
	}
	if !foundProjection {
		t.Fatalf("artifact projection missing: %+v", projections)
	}
}

func TestArtifactHTTPReviewAndPreview(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	dataDir := t.TempDir()
	root, project := artifactTestProject(t)
	a := &app{
		settings: Settings{DataDir: dataDir, ProjectsRoot: root}, artifacts: map[string][]Artifact{},
		agents:     map[string][]Agent{project.ID: {{ID: "designer", ProjectID: project.ID}}},
		blackboard: map[string][]AgentBlackboardEntry{}, runtimeHooks: map[string]bool{}, runtimeWatchers: map[string]map[chan RuntimeEvent]bool{},
	}
	content := []byte("<!doctype html><html><body>Preview</body></html>")
	full, err := a.safeWorkspacePath(project.ID, "designer", "preview.html")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0644); err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(project.Path, ".karoz", "artifacts", "designer")
	if !strings.HasPrefix(full, wantRoot+string(os.PathSeparator)) {
		t.Fatalf("artifact path %q is outside project-local root %q", full, wantRoot)
	}
	artifact, err := a.registerWorkspaceArtifact(project.ID, "designer", "run-1", "preview.html", "mockup_html", "Preview", "", content)
	if err != nil {
		t.Fatal(err)
	}
	reviewingBody := bytes.NewBufferString(`{"status":"reviewing","actor_agent_id":"designer"}`)
	reviewingRecorder := httptest.NewRecorder()
	a.handleArtifacts(reviewingRecorder, httptest.NewRequest(http.MethodPatch, "/", reviewingBody), project, []string{artifact.ID})
	if reviewingRecorder.Code != http.StatusOK || !strings.Contains(reviewingRecorder.Body.String(), `"status":"reviewing"`) {
		t.Fatalf("reviewing response code=%d body=%s", reviewingRecorder.Code, reviewingRecorder.Body.String())
	}
	approvedBody := bytes.NewBufferString(`{"status":"approved","actor_agent_id":"user","note":"Looks good"}`)
	approvedRecorder := httptest.NewRecorder()
	a.handleArtifacts(approvedRecorder, httptest.NewRequest(http.MethodPatch, "/", approvedBody), project, []string{artifact.ID})
	if approvedRecorder.Code != http.StatusOK || !strings.Contains(approvedRecorder.Body.String(), `"status":"approved"`) {
		t.Fatalf("approved response code=%d body=%s", approvedRecorder.Code, approvedRecorder.Body.String())
	}
	previewRecorder := httptest.NewRecorder()
	a.handleArtifacts(previewRecorder, httptest.NewRequest(http.MethodGet, "/", nil), project, []string{artifact.ID, "preview"})
	if previewRecorder.Code != http.StatusOK || !strings.Contains(previewRecorder.Body.String(), "Preview") {
		t.Fatalf("preview response code=%d body=%s", previewRecorder.Code, previewRecorder.Body.String())
	}
}

func TestArtifactsPersistAndLegacyWorkspaceFilesAreRegistered(t *testing.T) {
	dataDir := t.TempDir()
	root, project := artifactTestProject(t)
	projectID := project.ID
	agentID := "designer"
	a := &app{
		settings:  Settings{DataDir: dataDir, ProjectsRoot: root},
		agents:    map[string][]Agent{projectID: {{ID: agentID, ProjectID: projectID}}},
		artifacts: map[string][]Artifact{},
	}
	full, err := a.safeWorkspacePath(projectID, agentID, "legacy.svg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcileWorkspaceArtifacts(); err != nil {
		t.Fatal(err)
	}
	items := a.artifactsForProject(projectID, agentID, "", "")
	if len(items) != 1 || items[0].Kind != "mockup_svg" || items[0].Revision != 1 {
		t.Fatalf("reconciled artifacts = %+v", items)
	}

	reloaded := &app{settings: Settings{DataDir: dataDir, ProjectsRoot: root}, artifacts: map[string][]Artifact{}}
	if err := reloaded.loadArtifacts(); err != nil {
		t.Fatal(err)
	}
	persisted, ok := reloaded.artifactByID(projectID, items[0].ID)
	if !ok || persisted.Path != "legacy.svg" || len(persisted.Revisions) != 1 {
		t.Fatalf("persisted artifact = %+v ok=%v", persisted, ok)
	}
}

func TestConcurrentArtifactWritesPreserveEveryRevision(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	root, project := artifactTestProject(t)
	a := &app{
		settings: Settings{DataDir: t.TempDir(), ProjectsRoot: root}, artifacts: map[string][]Artifact{},
		blackboard: map[string][]AgentBlackboardEntry{}, runtimeHooks: map[string]bool{}, runtimeWatchers: map[string]map[chan RuntimeEvent]bool{},
	}
	const writes = 10
	var wait sync.WaitGroup
	wait.Add(writes)
	for index := 0; index < writes; index++ {
		go func() {
			defer wait.Done()
			if _, err := a.registerWorkspaceArtifact(project.ID, "designer", "run", "concurrent.html", "mockup_html", "Concurrent", "", []byte(randomID())); err != nil {
				t.Errorf("register artifact: %v", err)
			}
		}()
	}
	wait.Wait()
	items := a.artifactsForProject(project.ID, "designer", "", "")
	if len(items) != 1 || items[0].Revision != writes || len(items[0].Revisions) != writes {
		t.Fatalf("concurrent artifact revisions = %+v", items)
	}
}
