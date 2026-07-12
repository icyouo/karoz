package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (a *app) executeHandoffScheduledRun(ctx context.Context, job ScheduledRun) error {
	var payload HandoffRunPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return err
	}
	messageID := firstNonEmpty(payload.InboxMessageID, job.MessageID)
	project, err := a.projectByID(job.ProjectID)
	if err != nil {
		return err
	}
	target, ok := a.projectAgent(project, job.AgentID)
	if !ok {
		return fmt.Errorf("handoff target agent %s not found", job.AgentID)
	}
	msg, ok := a.inboxMessage(project.ID, target.ID, messageID)
	if !ok || !handoffStatusOpen(msg.Status) {
		return nil
	}
	if !a.claimHandoff(project.ID, target.ID, msg.ID) {
		return nil
	}
	closeInstruction := fmt.Sprintf("Before ending this turn, close handoff %s exactly once with reply_to when the original request needs an answer/result, decline_handoff when it cannot be completed, or ack_inbox only when there is no useful detail.", msg.ID)
	if handoffMessageIsTerminalDelivery(msg) {
		closeInstruction = fmt.Sprintf("This is a substantive peer delivery, not a new request. Review it, then close inbox %s with ack_inbox. Do not call reply_to. If the sender genuinely needs another review or action, create one new send_to handoff to their unique nickname before acking.", msg.ID)
	}
	if msg.SourceAgentID == "karoz" {
		closeInstruction = fmt.Sprintf("Karoz is the coordinator, not a conversation peer. Do not call reply_to for this handoff. Report progress with report_activity and finish handoff %s with report_activity using activity_kind done or error and inbox_message_id %s. Reports never trigger a coordinator response.", msg.ID, msg.ID)
	}
	sourceKind := "peer"
	if msg.SourceAgentID == "karoz" {
		sourceKind = "coordinator"
	}
	userText := strings.TrimSpace(fmt.Sprintf("[%s]<from:%s> You received an asynchronous %s.\n\nInbox message id: %s\nSubject: %s\nObjective: %s\nExpected output: %s\n\n%s\n\nRespond as %s. %s Never send a greeting or receipt-only reply. Send decisions/conflicts to Karoz only as reports. If another peer owns the next step, hand off directly using their unique nickname. If this handoff requires tracked coding or deployment work, create a task.", sourceKind, a.agentNickname(project, msg.SourceAgentID), msg.Intent, msg.ID, msg.Subject, msg.Objective, msg.ExpectedOutput, msg.Body, firstNonEmpty(target.Nickname, target.DisplayName, target.Name, target.ID), closeInstruction))
	out, err := a.runResidentAgentTurn(ctx, project, target, userText, "ask", nil)
	if err != nil {
		a.failHandoff(project.ID, target.ID, msg.ID, err)
		return err
	}
	if strings.TrimSpace(out) == "" {
		out = firstNonEmpty(target.DisplayName, target.Name, target.ID) + " acknowledged the handoff."
	}
	a.appendAgentMessage(project.ID, target.ID, "assistant", "result", out)
	a.completeUnhandledInboxAfterAutoResponse(project, target, msg, out)
	return nil
}

func (a *app) executeTaskEventScheduledRun(ctx context.Context, job ScheduledRun) error {
	var payload TaskEventRunPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return err
	}
	taskID := firstNonEmpty(payload.TaskID, job.SourceID)
	project, err := a.projectByID(job.ProjectID)
	if err != nil {
		return err
	}
	agent, ok := a.projectAgent(project, job.AgentID)
	if !ok {
		return fmt.Errorf("task event agent %s not found", job.AgentID)
	}
	task, ok := a.findTask(project.ID, taskID)
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	success := task.Status == "done"
	summary := firstNonEmpty(task.Result, task.FailureSummary, "task status: "+task.Status)
	prompt := fmt.Sprintf("[task hook] task_id=%s success=%t summary=%s\n\nA tracked task you created or own has reached a terminal state. Interpret the result for your role, update durable project state when useful, and identify the next concrete step. Do not create a duplicate task for work that is already complete.", task.ID, success, strings.TrimSpace(summary))
	out, err := a.runResidentAgentTurn(ctx, project, agent, prompt, "ask", nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		a.appendAgentMessage(project.ID, agent.ID, "assistant", "task_result", out)
	}
	return nil
}

func (a *app) executeIdleReconcileScheduledRun(ctx context.Context, job ScheduledRun) error {
	defer a.endRuntimeHook(job.ProjectID, karozIdleReconcileHook)
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	if !a.projectRuntimeQuiescentIgnoring(job.ProjectID, karozIdleReconcileHook, job.AgentID) || !a.projectBacklogNotEmpty(job.ProjectID) {
		return nil
	}
	project, err := a.projectByID(job.ProjectID)
	if err != nil {
		return nil
	}
	karoz, ok := a.projectAgent(project, job.AgentID)
	if !ok {
		return nil
	}
	a.appendAgentMessage(project.ID, karoz.ID, "system", karozIdleReconcileHook, "Runtime triggered Karoz idle reconciliation after project became idle.")
	userText := "Runtime idle reconciliation requested. The project runtime is quiescent and there is unresolved backlog.\n\n" + a.renderProjectBacklogForKaroz(project.ID) + "\n\nRules:\n- Process backlog, do not merely summarize it.\n- Pending inbox items should be routed to the target agent if that agent is idle, or answered/acked if they belong to Karoz.\n- Pending tasks should be started only through task tools when appropriate; failed tasks require review, retry planning, or user escalation.\n- Unhandled blackboard signals must be consumed exactly once: route to an agent with send_to, create a task, ask the user, ignore with reason, or expire if stale. Then call mark_activity with handling_result.\n- Do not create duplicate handoffs if a matching pending inbox already exists."
	out, err := a.runResidentAgentTurn(ctx, project, karoz, userText, "ask", nil)
	if err != nil {
		a.appendAgentMessage(project.ID, karoz.ID, "assistant", "error", "Karoz idle reconciliation failed: "+err.Error())
		return err
	}
	if strings.TrimSpace(out) != "" {
		a.appendAgentMessage(project.ID, karoz.ID, "assistant", "result", out)
	}
	return nil
}
