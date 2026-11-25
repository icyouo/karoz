package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskDiffEndpointReturnsGitUnifiedDiff(t *testing.T) {
	a, project := newHandlerTestApp(t)

	// Reinitialize demo workspace as a real git repo with one committed file and one modified file
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", project.Path}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, out)
		}
	}
	_ = os.RemoveAll(filepath.Join(project.Path, ".git"))
	runGit("init")
	filePath := filepath.Join(project.Path, "main.txt")
	if err := os.WriteFile(filePath, []byte("line one\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "main.txt")
	runGit("commit", "-m", "initial")

	if err := os.WriteFile(filePath, []byte("line one\nline two\n"), 0644); err != nil {
		t.Fatal(err)
	}

	task, err := a.createTask(project, TaskCreateRequest{Type: "feat", Title: "Diff Test", Goal: "Verify task diff"})
	if err != nil {
		t.Fatalf("createTask failed: %v", err)
	}

	rec := httptest.NewRecorder()
	a.httpHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/"+project.ID+"/tasks/"+task.ID+"/diff", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET task diff status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload TaskLogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload.Content, "+line two") {
		t.Fatalf("expected unified diff containing '+line two', got %q", payload.Content)
	}
}
