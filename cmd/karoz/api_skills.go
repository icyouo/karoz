package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/karoz/karoz/internal/skillfetch"
)

func (a *app) installedSkillsDir() string {
	base := strings.TrimSpace(a.settings.DataDir)
	if base == "" {
		return ""
	}
	return filepath.Join(base, "skills")
}

func (a *app) handleSkills(w http.ResponseWriter, r *http.Request) {
	dir := a.installedSkillsDir()
	if dir == "" {
		writeError(w, http.StatusInternalServerError, errors.New("data_dir is not configured"))
		return
	}
	suffix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/skills"), "/")

	if suffix == "" {
		switch r.Method {
		case http.MethodGet:
			skills, err := skillfetch.ListInstalled(dir)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, map[string]any{"skills": skills})
		case http.MethodPost:
			var req struct {
				Source string `json:"source"`
			}
			if err := readJSON(r, &req); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			installed, err := skillfetch.Install(r.Context(), nil, dir, req.Source)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, installed)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	if strings.Contains(suffix, "/") {
		http.NotFound(w, r)
		return
	}

	if r.Method == http.MethodDelete {
		if err := skillfetch.RemoveInstalled(dir, suffix); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, err)
				return
			}
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"removed": true, "name": suffix})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}
