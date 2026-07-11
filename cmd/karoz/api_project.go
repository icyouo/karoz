package main

import (
	"errors"
	"net/http"
	"strings"
)

func (a *app) handleProjectScoped(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/api/projects/"))
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	project, err := a.projectByID(parts[0])
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if len(parts) == 1 {
		writeJSON(w, project)
		return
	}
	switch parts[1] {
	case "agents":
		a.handleAgents(w, r, project, parts[2:])
	case "skills":
		a.handleProjectSkills(w, r, project)
	case "agent-teams":
		a.handleAgentTeams(w, r, project, parts[2:])
	case "agent-blackboard":
		a.handleAgentBlackboard(w, r, project)
	case "artifacts":
		a.handleArtifacts(w, r, project, parts[2:])
	case "agent-routes":
		a.handleAgentRoutes(w, r, project)
	case "runtime-events":
		a.handleRuntimeEvents(w, r, project)
	case "tasks":
		a.handleTasks(w, r, project, parts[2:])
	default:
		http.NotFound(w, r)
	}
}

func (a *app) handleArtifacts(w http.ResponseWriter, r *http.Request, project Project, parts []string) {
	if len(parts) == 0 && r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"artifacts": a.artifactsForProject(project.ID, r.URL.Query().Get("agent_id"), r.URL.Query().Get("kind"), r.URL.Query().Get("status"))})
		return
	}
	if len(parts) == 0 {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	artifactID := parts[0]
	artifact, ok := a.artifactByID(project.ID, artifactID)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("artifact not found"))
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		writeJSON(w, artifact)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPatch {
		var req ArtifactStatusUpdateRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		actorID := firstNonEmpty(strings.TrimSpace(req.ActorAgentID), "user")
		updated, err := a.updateArtifactStatus(project.ID, artifactID, actorID, req.Status, req.Note)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, updated)
		return
	}
	if len(parts) == 2 && parts[1] == "preview" && r.Method == http.MethodGet {
		preview, err := a.artifactPreview(project.ID, artifactID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, preview)
		return
	}
	http.NotFound(w, r)
}

func (a *app) handleProjectSkills(w http.ResponseWriter, r *http.Request, project Project) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	skills := a.discoverSkills(project)
	if query == "" {
		writeJSON(w, skills)
		return
	}
	var filtered []Skill
	for _, skill := range skills {
		name := strings.ToLower(skill.Name)
		if strings.HasPrefix(name, query) || strings.Contains(strings.ToLower(skill.Description), query) || strings.Contains(strings.ToLower(skill.ShortDescription), query) {
			filtered = append(filtered, skill)
		}
	}
	writeJSON(w, filtered)
}

func (a *app) handleAgentRoutes(w http.ResponseWriter, r *http.Request, project Project) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, a.routesForProject(project.ID))
	case http.MethodPut:
		var req AgentRoutesUpdateRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		routes, err := a.updateAgentRoutes(project, req.Routes)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, routes)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *app) handleAgentBlackboard(w http.ResponseWriter, r *http.Request, project Project) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, a.blackboardFor(project.ID, 50))
}
