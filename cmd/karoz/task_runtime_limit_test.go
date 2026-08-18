package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTaskCreateAPIPersistsMaximumRuntime(t *testing.T) {
	a, project := newHandlerTestApp(t)
	path := "/api/projects/" + project.ID + "/tasks"

	response := serveHTTPRequest(a, http.MethodPost, path, `{"type":"feature","title":"long task","max_runtime_ms":0}`)
	if response.Code != http.StatusOK {
		t.Fatalf("create unlimited task status=%d body=%s", response.Code, response.Body.String())
	}
	var task Task
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.MaxRuntimeMS == nil || *task.MaxRuntimeMS != 0 {
		t.Fatalf("created task maximum runtime = %v", task.MaxRuntimeMS)
	}

	invalid := serveHTTPRequest(a, http.MethodPost, path, `{"type":"feature","title":"invalid task","max_runtime_ms":-1}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("negative maximum runtime status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestTaskCreationHTTPAndModelToolPreserveRuntimeAndSandboxContract(t *testing.T) {
	a, project := newHandlerTestApp(t)
	path := "/api/projects/" + project.ID + "/tasks"
	httpResponse := serveHTTPRequest(a, http.MethodPost, path, `{"type":"feature","title":"HTTP isolated unlimited","max_runtime_ms":0,"sandbox_mode":"required"}`)
	if httpResponse.Code != http.StatusOK {
		t.Fatalf("HTTP task create status=%d body=%s", httpResponse.Code, httpResponse.Body.String())
	}
	var viaHTTP Task
	if err := json.Unmarshal(httpResponse.Body.Bytes(), &viaHTTP); err != nil {
		t.Fatal(err)
	}
	viaToolJSON := a.createTaskFromResidentTool(project, Agent{ID: "karoz", ProjectID: project.ID}, map[string]any{
		"type": "feature", "title": "Tool isolated unlimited", "description": "verify creation parity", "max_runtime_ms": int64(0), "sandbox_mode": "required",
	})
	var viaTool struct {
		Task  Task   `json:"task"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(viaToolJSON), &viaTool); err != nil {
		t.Fatal(err)
	}
	if viaTool.Error != "" {
		t.Fatalf("model tool create failed: %s", viaToolJSON)
	}
	for source, task := range map[string]Task{"http": viaHTTP, "tool": viaTool.Task} {
		if task.MaxRuntimeMS == nil || *task.MaxRuntimeMS != 0 || task.SandboxMode != "required" {
			t.Fatalf("%s contract drifted: %+v", source, task)
		}
	}
}

func TestTaskMaximumRuntimePolicy(t *testing.T) {
	defaultValue, err := normalizeTaskMaxRuntimeMS(nil)
	if err != nil || defaultValue == nil || *defaultValue != time.Hour.Milliseconds() {
		t.Fatalf("default maximum runtime = %v, err=%v", defaultValue, err)
	}
	unlimited := int64(0)
	value, err := normalizeTaskMaxRuntimeMS(&unlimited)
	if err != nil || value == nil || *value != 0 {
		t.Fatalf("unlimited maximum runtime = %v, err=%v", value, err)
	}
	negative := int64(-1)
	if _, err := normalizeTaskMaxRuntimeMS(&negative); err == nil {
		t.Fatal("negative maximum runtime was accepted")
	}
}

func TestTaskRunUsesPerTaskMaximumRuntime(t *testing.T) {
	finite := int64(15)
	a := newApp(Settings{DataDir: t.TempDir()})
	a.projectTasksLocked().tasks["project"] = []Task{{
		ID: "finite", ProjectID: "project", Status: "pending", MaxRuntimeMS: &finite,
	}}
	_, ctx, finish, ok := a.claimTaskRun("project", "finite")
	if !ok {
		t.Fatal("finite task run was not claimed")
	}
	defer finish()
	select {
	case <-ctx.Done():
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("finite task context error = %v", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("finite task maximum runtime was not enforced")
	}
}

func TestTaskRunCanBeUnlimited(t *testing.T) {
	unlimited := int64(0)
	a := newApp(Settings{DataDir: t.TempDir()})
	a.projectTasksLocked().tasks["project"] = []Task{{
		ID: "unlimited", ProjectID: "project", Status: "pending", MaxRuntimeMS: &unlimited,
	}}
	_, ctx, finish, ok := a.claimTaskRun("project", "unlimited")
	if !ok {
		t.Fatal("unlimited task run was not claimed")
	}
	select {
	case <-ctx.Done():
		t.Fatalf("unlimited task ended without cancellation: %v", ctx.Err())
	case <-time.After(25 * time.Millisecond):
	}
	finish()
	if ctx.Err() != context.Canceled {
		t.Fatalf("unlimited task did not retain manual cancellation: %v", ctx.Err())
	}
}

func TestRequiredTaskSandboxFailsClosedBeforeWorktreeMutation(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir()})
	project := Project{ID: "project", Path: t.TempDir()}
	task := Task{ID: "sandboxed", ProjectID: project.ID, Status: "pending", SandboxMode: "required"}
	a.projectTasksLocked().tasks[project.ID] = []Task{task}
	result := a.runTask(project, task)
	if result.Status != "failed" || !strings.Contains(result.FailureSummary, "sandbox unavailable") {
		t.Fatalf("required sandbox task did not fail closed: %+v", result)
	}
	if result.WorktreePath != "" || result.CommitSHA != "" {
		t.Fatalf("sandbox rejection mutated task execution state: %+v", result)
	}
}
