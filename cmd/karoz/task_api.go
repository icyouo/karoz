package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

func (a *app) handleTasks(w http.ResponseWriter, r *http.Request, project Project, parts []string) {
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, a.tasksForProject(project.ID))
		case http.MethodPost:
			var req TaskCreateRequest
			if err := readJSON(r, &req); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			artifactIDs, err := a.validateTaskArtifactRefs(project.ID, req.ArtifactIDs)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			req.ArtifactIDs = artifactIDs
			task, err := a.createTask(project, req)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			a.startTaskAsync(project, task, "manual_create")
			writeJSON(w, task)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	taskID := parts[0]
	task, ok := a.findTask(project.ID, taskID)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		writeJSON(w, task)
		return
	}
	if len(parts) == 2 && parts[1] == "run" && r.Method == http.MethodPost {
		updated := a.runTaskAsync(project, task, "manual_run")
		writeJSON(w, updated)
		return
	}
	if len(parts) == 2 && parts[1] == "merge" && r.Method == http.MethodPost {
		updated, err := a.retryTaskMerge(project, task)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, updated)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		updated, err := a.cancelTask(project, task)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, updated)
		return
	}
	if len(parts) == 2 && parts[1] == "cleanup" && r.Method == http.MethodPost {
		updated, err := a.cleanupTaskWorktree(project, task)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, updated)
		return
	}
	if len(parts) == 2 && parts[1] == "logs" && r.Method == http.MethodGet {
		logs, err := a.readTaskLog(project.ID, task.ID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(logs)
		return
	}
	if len(parts) == 2 && (parts[1] == "runtime-log" || parts[1] == "deployment-log") && r.Method == http.MethodGet {
		logs, err := a.readTaskLog(project.ID, task.ID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, TaskLogResponse{Content: string(logs)})
		return
	}
	if len(parts) == 2 && parts[1] == "diff" && r.Method == http.MethodGet {
		diff, err := a.readTaskDiff(r.Context(), project, task)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, TaskLogResponse{Content: diff})
		return
	}
	http.NotFound(w, r)
}

func (a *app) readTaskDiff(ctx context.Context, project Project, task Task) (string, error) {
	dir := strings.TrimSpace(task.WorktreePath)
	if dir != "" {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			dir = ""
		}
	}
	if dir == "" {
		dir = strings.TrimSpace(project.Path)
	}
	if dir == "" {
		return "", nil
	}
	const maxDiffBytes = 256 << 10
	runGitDiff := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "diff"}, args...)...)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return "", err
		}
		raw := out.Bytes()
		if len(raw) > maxDiffBytes {
			return string(raw[:maxDiffBytes]) + "\n... (diff truncated)\n", nil
		}
		return string(raw), nil
	}
	if out, err := runGitDiff("HEAD"); err == nil {
		return out, nil
	}
	if out, err := runGitDiff(); err == nil {
		return out, nil
	}
	return "", nil
}
