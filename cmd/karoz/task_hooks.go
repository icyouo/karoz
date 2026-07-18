package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

func (a *app) notifyTaskRuntimeHooks(project Project, task Task) {
	if plan, changed := a.markPlanTaskTerminal(project.ID, task); changed {
		a.schedulePlanEvent(project.ID, plan.OwnerAgentID, plan.ID, task.PlanStepID, "task_terminal", task.ID)
	}
	key := project.ID + "/" + task.ID
	success := task.Status == "done"
	summary := task.Result
	if strings.TrimSpace(summary) == "" {
		summary = task.FailureSummary
	}
	if strings.TrimSpace(summary) == "" {
		summary = "task status: " + task.Status
	}
	now := time.Now().UTC()
	var deliveries []TaskRuntimeHook
	a.mu.Lock()
	hooks := a.taskHooks[key]
	for i := range hooks {
		if hooks[i].Status != "pending" || hooks[i].HookType != "resident_task_completion" {
			continue
		}
		hooks[i].Status = "delivered"
		hooks[i].DeliveredAt = &now
		hooks[i].ResponsePayload = map[string]any{"success": success, "summary": summary, "task_id": task.ID, "status": task.Status}
		deliveries = append(deliveries, hooks[i])
	}
	a.taskHooks[key] = hooks
	a.mu.Unlock()
	for _, hook := range deliveries {
		hint := fmt.Sprintf("[task hook] task_id=%s success=%t summary=%s", task.ID, success, strings.TrimSpace(summary))
		a.appendAgentMessage(project.ID, hook.AgentID, "system", "task_hook", hint)
		a.appendTaskLog(project.ID, task.ID, "resident hook delivered to agent "+hook.AgentID+": "+hook.ID)
		a.triggerAgentTaskEvent(project, task, hook)
	}
	if len(deliveries) > 0 {
		if err := a.saveTaskHooks(); err != nil {
			log.Printf("save task hooks: %v", err)
		}
	}
}

func (a *app) triggerAgentTaskEvent(project Project, task Task, hook TaskRuntimeHook) {
	if task.PlanID != "" {
		return
	}
	if strings.EqualFold(os.Getenv("KAROZ_AGENT_AUTO_RESPOND"), "0") || strings.EqualFold(os.Getenv("KAROZ_AGENT_AUTO_RESPOND"), "false") {
		return
	}
	agent, ok := a.projectAgent(project, hook.AgentID)
	if !ok {
		return
	}
	job, err := newScheduledRun(
		ScheduledRunTaskEvent,
		AgentRunInput{
			ProjectID: project.ID,
			AgentID:   agent.ID,
			Trigger:   RunTriggerTaskEvent,
			TurnType:  "ask",
			SourceID:  task.ID,
			MessageID: hook.ID,
		},
		"task_event/"+project.ID+"/"+task.ID+"/"+hook.ID,
		TaskEventRunPayload{TaskID: task.ID, HookID: hook.ID},
		3*time.Minute,
	)
	if err != nil {
		log.Printf("create task event scheduled run project=%s agent=%s task=%s hook=%s: %v", project.ID, agent.ID, task.ID, hook.ID, err)
		return
	}
	if _, scheduled := a.scheduleAgentRun(job); !scheduled {
		log.Printf("schedule task event run rejected project=%s agent=%s task=%s hook=%s", project.ID, agent.ID, task.ID, hook.ID)
	}
}

func (a *app) registerTaskRuntimeHook(projectID, agentID, taskID string, payload map[string]any) TaskRuntimeHook {
	if payload == nil {
		payload = map[string]any{}
	}
	session := a.ensureAgentSession(projectID, agentID)
	now := time.Now().UTC()
	hook := TaskRuntimeHook{
		ID:             randomID(),
		TaskID:         taskID,
		ProjectID:      projectID,
		AgentID:        agentID,
		SessionID:      session.SessionID,
		HookType:       "resident_task_completion",
		Status:         "pending",
		RequestPayload: payload,
		CreatedAt:      now,
	}
	a.mu.Lock()
	if a.taskHooks == nil {
		a.taskHooks = map[string][]TaskRuntimeHook{}
	}
	key := projectID + "/" + taskID
	var updated []TaskRuntimeHook
	for _, existing := range a.taskHooks[key] {
		if existing.AgentID == agentID && existing.HookType == hook.HookType {
			continue
		}
		updated = append(updated, existing)
	}
	updated = append(updated, hook)
	a.taskHooks[key] = updated
	a.mu.Unlock()
	if err := a.saveTaskHooks(); err != nil {
		log.Printf("save task hooks: %v", err)
	}
	return hook
}

func taskStatusIsTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "failed", "deploy_failed", "cancelled", "canceled":
		return true
	default:
		return false
	}
}
