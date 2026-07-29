package main

import (
	"net/http"
	"strings"

	monitordomain "github.com/karoz/karoz/internal/monitor"
)

func (a *app) handleMonitors(w http.ResponseWriter, r *http.Request, project Project, parts []string) {
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]any{"monitors": a.monitorsForProject(project.ID)})
		case http.MethodPost:
			var item Monitor
			if err := readJSON(r, &item); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			created, err := a.createMonitor(project, item)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, created)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := a.deleteMonitor(project, strings.TrimSpace(parts[0])); err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, map[string]any{"deleted": parts[0]})
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id, operation := strings.TrimSpace(parts[0]), parts[1]
	if r.Method == http.MethodPost && (operation == "pause" || operation == "resume") {
		state := monitordomain.StateDisabled
		if operation == "resume" {
			state = monitordomain.StateActive
		}
		item, err := a.setMonitorState(project, id, state)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, item)
		return
	}
	if r.Method == http.MethodDelete && operation == "delete" {
		if err := a.deleteMonitor(project, id); err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, map[string]any{"deleted": id})
		return
	}
	http.NotFound(w, r)
}
