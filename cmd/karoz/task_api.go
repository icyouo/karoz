package main

import (
	"errors"
	"net/http"
	"os"
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
			task := a.createTask(project, req)
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
	http.NotFound(w, r)
}
