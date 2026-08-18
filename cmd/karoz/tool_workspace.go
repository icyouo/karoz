package main

import (
	"encoding/json"
	"fmt"
	artifactdomain "github.com/karoz/karoz/internal/artifact"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a *app) writeWorkspaceFileFromTool(projectID, agentID, runID string, args map[string]any) string {
	relPath := toolStringArg(args, "path", 500)
	content := toolStringArg(args, "content", 2_000_000)
	if relPath == "" || content == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "path and content are required"})
	}
	full, err := a.safeWorkspacePath(projectID, agentID, relPath)
	if err != nil {
		return toolJSON(map[string]any{"error": "invalid_path", "message": err.Error()})
	}
	kind := toolStringArg(args, "artifact_kind", 64)
	title := toolStringArg(args, "title", 500)
	description := toolStringArg(args, "description", 4000)
	if err := artifactdomain.ValidateRegisterRevisionInput(workspaceArtifactRegistrationInput(projectID, agentID, runID, relPath, kind, title, description, []byte(content))); err != nil {
		return toolJSON(map[string]any{"error": "artifact_metadata_failed", "message": err.Error()})
	}
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return toolJSON(map[string]any{"error": "mkdir_failed", "message": err.Error()})
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		return toolJSON(map[string]any{"error": "write_failed", "message": err.Error()})
	}
	info, _ := os.Stat(full)
	file := WorkspaceFile{Path: filepath.ToSlash(filepath.Clean(relPath)), Filename: filepath.Base(full), MimeType: mimeTypeForPath(full)}
	if info != nil {
		file.SizeBytes = info.Size()
		file.UpdatedAt = info.ModTime()
	}
	artifact, err := a.registerWorkspaceArtifact(projectID, agentID, runID, file.Path, kind, title, description, []byte(content))
	if err != nil {
		return toolJSON(map[string]any{"error": "artifact_metadata_failed", "message": err.Error(), "file": file})
	}
	return toolJSON(map[string]any{"file": file, "artifact": artifact, "artifact_id": artifact.ID, "preview": artifact.Previewable})
}

func (a *app) showWorkspacePreviewFromTool(projectID, agentID string, args map[string]any) string {
	artifactID := toolStringArg(args, "artifact_id", 128)
	path := toolStringArg(args, "path", 500)
	if artifactID == "" && path == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "artifact_id or path is required"})
	}
	var preview WorkspaceFilePreview
	var err error
	if artifactID != "" {
		preview, err = a.artifactPreview(projectID, artifactID)
	} else {
		preview, err = a.getWorkspaceFilePreview(projectID, agentID, path)
	}
	if err != nil {
		return toolJSON(map[string]any{"error": "preview_failed", "message": err.Error()})
	}
	return toolJSON(map[string]any{"preview": preview})
}

func (a *app) getArtifactFromTool(projectID string, args map[string]any) string {
	artifactID := toolStringArg(args, "artifact_id", 128)
	artifact, ok := a.artifactByID(projectID, artifactID)
	if !ok {
		return toolJSON(map[string]any{"error": "not_found", "message": "artifact not found"})
	}
	return toolJSON(map[string]any{"artifact": artifact})
}

func (a *app) submitArtifactFromTool(projectID string, actor Agent, args map[string]any) string {
	artifactID := toolStringArg(args, "artifact_id", 128)
	artifact, ok := a.artifactByID(projectID, artifactID)
	if !ok {
		return toolJSON(map[string]any{"error": "not_found", "message": "artifact not found"})
	}
	if artifact.AgentID != actor.ID && actor.ID != "karoz" {
		return toolJSON(map[string]any{"error": "forbidden", "message": "only the artifact author or Karoz can submit it"})
	}
	updated, err := a.updateArtifactStatus(projectID, artifactID, actor.ID, ArtifactReviewing, toolStringArg(args, "note", 4000))
	if err != nil {
		return toolJSON(map[string]any{"error": "invalid_transition", "message": err.Error()})
	}
	return toolJSON(map[string]any{"artifact": updated, "status": updated.Status})
}

func (a *app) reviewArtifactFromTool(projectID string, actor Agent, args map[string]any) string {
	artifactID := toolStringArg(args, "artifact_id", 128)
	decision := toolStringArg(args, "decision", 64)
	next := ArtifactApproved
	if decision == "changes_requested" {
		next = ArtifactDraft
	} else if decision != ArtifactApproved {
		return toolJSON(map[string]any{"error": "validation_error", "message": "decision must be approved or changes_requested"})
	}
	updated, err := a.updateArtifactStatus(projectID, artifactID, actor.ID, next, toolStringArg(args, "note", 4000))
	if err != nil {
		return toolJSON(map[string]any{"error": "invalid_transition", "message": err.Error()})
	}
	return toolJSON(map[string]any{"artifact": updated, "status": updated.Status, "decision": decision})
}

func (a *app) createTaskFromResidentTool(project Project, agent Agent, args map[string]any) string {
	title := toolStringArg(args, "title", 500)
	description := toolStringArg(args, "description", 20000)
	if title == "" || description == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "title and description are required"})
	}
	taskType := normalizeTaskType(toolStringArg(args, "type", 64))
	goal := toolStringArg(args, "goal", 20000)
	if goal == "" {
		goal = description
	}
	artifactIDs := toolStringSliceArg(args, "artifact_ids", 100)
	artifactIDs, err := a.validateTaskArtifactRefs(project.ID, artifactIDs)
	if err != nil {
		code := "invalid_artifact"
		if strings.Contains(err.Error(), "must be approved") {
			code = "artifact_not_approved"
		}
		return toolJSON(map[string]any{"error": code, "message": err.Error()})
	}
	maxRuntimeMS, err := optionalTaskMaxRuntimeArg(args)
	if err != nil {
		return toolJSON(map[string]any{"error": "validation_error", "message": err.Error()})
	}
	sandboxMode := toolStringArg(args, "sandbox_mode", 32)
	task, err := a.createTask(project, TaskCreateRequest{
		Type:         taskType,
		Title:        title,
		Description:  description,
		Goal:         goal,
		MaxRuntimeMS: maxRuntimeMS,
		SandboxMode:  sandboxMode,
		ArtifactIDs:  artifactIDs,
	})
	if err != nil {
		return toolJSON(map[string]any{"error": "validation_error", "message": err.Error()})
	}
	a.appendTaskLog(project.ID, task.ID, "created by resident agent: "+agent.ID)
	hook := a.registerTaskRuntimeHook(project.ID, agent.ID, task.ID, map[string]any{
		"title":        title,
		"description":  description,
		"type":         taskType,
		"artifact_ids": artifactIDs,
	})
	a.startTaskAsync(project, task, "resident_agent:"+agent.ID)
	return toolJSON(map[string]any{
		"task":        task,
		"task_id":     task.ID,
		"hook_id":     hook.ID,
		"status":      task.Status,
		"hook_status": hook.Status,
		"message":     "Task created and resident_task_completion hook registered. The resident agent will receive a task_hook message when the task completes or fails.",
	})
}

func optionalTaskMaxRuntimeArg(args map[string]any) (*int64, error) {
	raw, exists := args["max_runtime_ms"]
	if !exists {
		return nil, nil
	}
	var value int64
	switch typed := raw.(type) {
	case float64:
		if math.Trunc(typed) != typed || typed < math.MinInt64 || typed > math.MaxInt64 {
			return nil, fmt.Errorf("max_runtime_ms must be an integer")
		}
		value = int64(typed)
	case int:
		value = int64(typed)
	case int64:
		value = typed
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return nil, fmt.Errorf("max_runtime_ms must be an integer")
		}
		value = parsed
	default:
		return nil, fmt.Errorf("max_runtime_ms must be an integer")
	}
	return &value, nil
}

func (a *app) addAgentFromResidentTool(project Project, actor Agent, args map[string]any) string {
	if !capabilitiesForAgent(actor).CanManageAgents {
		return toolJSON(map[string]any{"error": "forbidden", "message": "only the default Karoz agent can add agents"})
	}
	templateID := toolStringArg(args, "template_id", 100)
	nickname := toolStringArg(args, "nickname", 120)
	if templateID == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "template_id is required"})
	}
	agent, err := a.createProjectAgent(project, AgentCreateRequest{TemplateID: templateID, Nickname: nickname})
	if err != nil {
		return toolJSON(map[string]any{"error": "create_failed", "message": err.Error()})
	}
	return toolJSON(map[string]any{"agent": agent})
}

func (a *app) listAgentTemplatesFromResidentTool(actor Agent, args map[string]any) string {
	if !capabilitiesForAgent(actor).CanManageAgents {
		return toolJSON(map[string]any{"error": "forbidden", "message": "only the default Karoz agent can list agent templates"})
	}
	query := strings.ToLower(toolStringArg(args, "query", 200))
	var templates []map[string]any
	for _, template := range residentAgentTemplates() {
		haystack := strings.ToLower(strings.Join([]string{template.ID, template.Name, template.DisplayName, template.ShortName, template.Role, template.Summary}, "\n"))
		if query != "" && !strings.Contains(haystack, query) {
			continue
		}
		templates = append(templates, map[string]any{
			"template_id":  template.ID,
			"display_name": template.DisplayName,
			"short_name":   template.ShortName,
			"role":         template.Role,
			"summary":      template.Summary,
		})
	}
	var teams []map[string]any
	for _, team := range residentAgentTeams() {
		haystack := strings.ToLower(team.ID + "\n" + team.Name + "\n" + team.Description)
		if query != "" && !strings.Contains(haystack, query) {
			continue
		}
		members := make([]map[string]any, 0, len(team.Agents))
		for _, member := range team.Agents {
			members = append(members, map[string]any{
				"id":          member.ID,
				"nickname":    member.Nickname,
				"template_id": member.TemplateID,
				"role":        member.Role,
			})
		}
		teams = append(teams, map[string]any{
			"template_id": team.ID,
			"name":        team.Name,
			"description": team.Description,
			"members":     members,
		})
	}
	return toolJSON(map[string]any{"agent_templates": templates, "team_templates": teams})
}

func (a *app) createAgentTeamFromResidentTool(project Project, actor Agent, args map[string]any) string {
	if !capabilitiesForAgent(actor).CanManageAgents {
		return toolJSON(map[string]any{"error": "forbidden", "message": "only the default Karoz agent can create agent teams"})
	}
	templateID := toolStringArg(args, "template_id", 100)
	if templateID == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "template_id is required"})
	}
	resp, err := a.createAgentTeam(project, AgentTeamCreateRequest{
		TemplateID: templateID,
		Instance:   toolStringArg(args, "instance", 120),
	})
	if err != nil {
		return toolJSON(map[string]any{"error": "create_failed", "message": err.Error()})
	}
	return toolJSON(map[string]any{"team": resp.Team, "agents": resp.Agents, "routes": resp.Routes, "created": resp.Created, "reused": resp.Reused})
}

func (a *app) deleteAgentFromResidentTool(project Project, actor Agent, args map[string]any) string {
	if !capabilitiesForAgent(actor).CanManageAgents {
		return toolJSON(map[string]any{"error": "forbidden", "message": "only the default Karoz agent can delete agents"})
	}
	agentID := toolStringArg(args, "agent_id", 120)
	if agentID == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "agent_id is required"})
	}
	if err := a.deleteProjectAgent(project, agentID); err != nil {
		return toolJSON(map[string]any{"error": "delete_failed", "message": err.Error()})
	}
	return toolJSON(map[string]any{"deleted": true, "agent_id": agentID})
}

func (a *app) updateTaskStatusFromResidentTool(projectID, agentID string, args map[string]any) string {
	taskID := toolStringArg(args, "task_id", 128)
	status := normalizeResidentTaskStatus(toolStringArg(args, "status", 64))
	if taskID == "" || status == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "task_id and status are required"})
	}
	task, err := a.transitionTaskStatusFromResidentTool(projectID, taskID, status, toolStringArg(args, "result", 12000))
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return toolJSON(map[string]any{"error": "not_found", "message": err.Error()})
		}
		return toolJSON(map[string]any{"error": "invalid_task_transition", "message": err.Error()})
	}
	if err := a.saveTasks(); err != nil {
		return toolJSON(map[string]any{"error": "save_failed", "message": err.Error()})
	}
	a.appendTaskLog(projectID, task.ID, "status updated by resident agent "+agentID+": "+status)
	if taskStatusIsTerminal(status) {
		if project, err := a.projectByID(projectID); err == nil {
			a.notifyTaskRuntimeHooks(project, task)
		}
		a.emitRuntimeStateChanged(RuntimeEvent{
			ID:        randomID(),
			ProjectID: projectID,
			Kind:      "task_changed",
			EntityID:  task.ID,
			To:        task.Status,
			Reason:    "resident_status_update",
			CreatedAt: time.Now().UTC(),
		})
	}
	return toolJSON(map[string]any{"task": task, "task_id": task.ID, "status": task.Status})
}

// Resident task updates are deliberately narrower than the executor lifecycle.
// A resident can close a task it created (or correct a previous terminal
// outcome), but it cannot impersonate the executor by entering running,
// verifying, merging, or cancellation states. Those states carry process and
// Git ownership invariants enforced elsewhere in the runtime.
func normalizeResidentTaskStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "canceled" {
		return "cancelled"
	}
	return status
}

func residentTaskStatusTransitionAllowed(from, to string) bool {
	from = normalizeResidentTaskStatus(from)
	to = normalizeResidentTaskStatus(to)
	if from == to {
		return from == "pending" || from == "done" || from == "failed" || from == "deploy_failed"
	}
	switch from {
	case "", "pending", "failed", "deploy_failed":
		switch to {
		case "done", "failed", "deploy_failed":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

// transitionTaskStatusFromResidentTool performs the read/validate/write under
// one mutex. In particular, an executor cannot move a task into an owned
// lifecycle state between a resident's stale read and its update.
func (a *app) transitionTaskStatusFromResidentTool(projectID, taskID, status, result string) (Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.projectTasksLocked()
	list := state.tasks[projectID]
	for i := range list {
		if list[i].ID != taskID {
			continue
		}
		from := normalizeResidentTaskStatus(list[i].Status)
		if !residentTaskStatusTransitionAllowed(from, status) {
			return Task{}, fmt.Errorf("task status transition %q -> %q is not allowed for resident tools", from, status)
		}
		list[i].Status = status
		if result = strings.TrimSpace(result); result != "" {
			list[i].Result = result
		}
		list[i].UpdatedAt = time.Now().UTC()
		state.tasks[projectID] = list
		return list[i], nil
	}
	return Task{}, fmt.Errorf("task not found")
}
