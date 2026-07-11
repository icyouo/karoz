package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (a *app) handleCLI2API(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req CLI2APIRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := a.invokeCLI2API(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, res)
}

func (a *app) handleFolderDialog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Prompt string `json:"prompt"`
	}
	_ = readJSON(r, &req)
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = "Choose a folder"
	}
	script := `POSIX path of (choose folder with prompt ` + strconvQuoteAppleScript(prompt) + `)`
	out, err := exec.CommandContext(r.Context(), "osascript", "-e", script).Output()
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("choose folder: %w", err))
		return
	}
	path := filepath.Clean(strings.TrimSpace(string(out)))
	if path == "." || path == "" {
		writeError(w, http.StatusBadRequest, errors.New("no folder selected"))
		return
	}
	writeJSON(w, map[string]string{"path": path})
}

func strconvQuoteAppleScript(value string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
}

func (a *app) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, a.settings)
	case http.MethodPut:
		var req SettingsUpdateRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		root := filepath.Clean(expandHome(strings.TrimSpace(req.ProjectsRoot)))
		if root == "" {
			writeError(w, http.StatusBadRequest, errors.New("projects_root is required"))
			return
		}
		if err := os.MkdirAll(root, 0755); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("create projects root: %w", err))
			return
		}
		extraRoots := normalizeWorkspaceRoots(req.ExtraProjectsRoots, root)
		for _, extraRoot := range extraRoots {
			if err := os.MkdirAll(extraRoot, 0755); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("create extra projects root %s: %w", extraRoot, err))
				return
			}
		}
		a.mu.Lock()
		a.settings.ProjectsRoot = root
		a.settings.ExtraProjectsRoots = extraRoots
		if req.MCPServers != nil {
			a.settings.MCPServers = normalizeMCPServers(*req.MCPServers)
		}
		a.mu.Unlock()
		if err := a.saveSettings(); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("save settings: %w", err))
			return
		}
		writeJSON(w, a.settings)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *app) handleAgentTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, residentAgentTemplates())
}

func (a *app) handleAgentTeamTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, residentAgentTeams())
}

func (a *app) handleAgentTeams(w http.ResponseWriter, r *http.Request, project Project, parts []string) {
	if len(parts) != 0 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req AgentTeamCreateRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	resp, err := a.createAgentTeam(project, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, resp)
}

func (a *app) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	_, err := os.Stat(a.settings.ProjectsRoot)
	writeJSON(w, Diagnostics{
		CodexCLI:       toolStatus("codex"),
		ClaudeCLI:      toolStatus("claude"),
		ProjectsRootOK: err == nil,
	})
}

func (a *app) handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		projects, err := a.scanProjects()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, projects)
	case http.MethodPost:
		var req ProjectCreateRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		project, err := a.createProject(req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, project)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
}
