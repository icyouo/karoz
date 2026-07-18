package main

import (
	"errors"
	"net/http"
	"strings"
)

func (a *app) handleGroups(w http.ResponseWriter, r *http.Request, project Project, parts []string) {
	if len(parts) == 0 && r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"groups": a.groupsForProject(project.ID)})
		return
	}
	if len(parts) == 2 && parts[1] == "coordinator" && r.Method == http.MethodPatch {
		var req struct {
			CoordinatorAgentID string `json:"coordinator_agent_id"`
		}
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		group, err := a.transferGroupCoordinator(project, parts[0], strings.TrimSpace(req.CoordinatorAgentID))
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, group)
		return
	}
	if len(parts) == 2 && parts[1] == "inbox" && r.Method == http.MethodGet {
		a.mu.Lock()
		items := append([]GroupInboxMessage{}, a.groupInbox[project.ID]...)
		a.mu.Unlock()
		filtered := make([]GroupInboxMessage, 0)
		for _, item := range items {
			if item.GroupID == parts[0] {
				filtered = append(filtered, item)
			}
		}
		writeJSON(w, map[string]any{"messages": filtered})
		return
	}
	http.NotFound(w, r)
}

func (a *app) handlePlans(w http.ResponseWriter, r *http.Request, project Project, parts []string) {
	if len(parts) == 0 && r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"plans": a.plansForProject(project.ID)})
		return
	}
	if len(parts) == 0 && r.Method == http.MethodPost {
		var req struct {
			AuthorAgentID string `json:"author_agent_id"`
			WorkPlanDraftRequest
		}
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		author, ok := a.projectAgent(project, strings.TrimSpace(req.AuthorAgentID))
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("author agent not found"))
			return
		}
		plan, err := a.createPlanDraft(project, author, author.ID, req.WorkPlanDraftRequest)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, plan)
		return
	}
	if len(parts) == 0 {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	planID := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		plan, ok := a.planByID(project.ID, planID)
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("plan not found"))
			return
		}
		writeJSON(w, plan)
		return
	}
	if len(parts) == 2 && parts[1] == "submit" && r.Method == http.MethodPost {
		var req struct {
			ActorID         string `json:"actor_id"`
			ExpectedVersion int64  `json:"expected_version"`
		}
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		plan, err := a.submitPlan(project.ID, planID, firstNonEmpty(strings.TrimSpace(req.ActorID), "user"), req.ExpectedVersion)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, plan)
		return
	}
	if len(parts) == 2 && parts[1] == "activate" && r.Method == http.MethodPost {
		var req struct {
			ApprovedBy      string `json:"approved_by"`
			ExpectedVersion int64  `json:"expected_version"`
		}
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		plan, err := a.activatePlan(project, planID, firstNonEmpty(strings.TrimSpace(req.ApprovedBy), "user"), req.ExpectedVersion)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, plan)
		return
	}
	if len(parts) == 2 && parts[1] == "actions" && r.Method == http.MethodPost {
		var req struct {
			ActorAgentID string `json:"actor_agent_id"`
			PlanActionRequest
		}
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		actor, ok := a.projectAgent(project, strings.TrimSpace(req.ActorAgentID))
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("actor agent not found"))
			return
		}
		plan, err := a.advancePlan(project, actor, planID, req.PlanActionRequest)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, plan)
		return
	}
	http.NotFound(w, r)
}
