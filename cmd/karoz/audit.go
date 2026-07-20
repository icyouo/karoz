package main

import (
	"net/http"
	"sort"
	"time"
)

// handleProjectAudit exports the project's auditable record — agents, tasks,
// handoffs in any status, blackboard entries, and active memories — as one
// JSON snapshot.
func (a *app) handleProjectAudit(w http.ResponseWriter, r *http.Request, project Project) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{
		"exported_at": time.Now().UTC().Unix(),
		"project":     project,
		"agents":      a.projectAgents(project),
		"tasks":       a.tasksForProject(project.ID),
		"handoffs":    a.handoffsForProject(project.ID),
		"blackboard":  a.blackboardFor(project.ID, 0),
		"memories":    a.activeMemoriesForProject(project.ID),
	})
}

// handoffsForProject returns every inbox/handoff record for the project in
// any status; the lifecycle is the auditable part.
func (a *app) handoffsForProject(projectID string) []AgentInboxMessage {
	a.mu.Lock()
	var out []AgentInboxMessage
	for _, items := range a.inbox {
		for _, item := range items {
			if item.ProjectID == projectID {
				out = append(out, item)
			}
		}
	}
	a.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if out == nil {
		return []AgentInboxMessage{}
	}
	return out
}

// activeMemoriesForProject returns active-state memories across all agents
// and layers of the project.
func (a *app) activeMemoriesForProject(projectID string) []AgentMemoryEntry {
	a.mu.Lock()
	var out []AgentMemoryEntry
	for _, items := range a.memories {
		for _, item := range items {
			if item.ProjectID != projectID || item.State != "active" || item.ArchivedAt != nil {
				continue
			}
			out = append(out, item)
		}
	}
	a.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if out == nil {
		return []AgentMemoryEntry{}
	}
	return out
}
