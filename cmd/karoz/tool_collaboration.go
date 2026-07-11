package main

import (
	"strings"
	"time"
)

func (a *app) sendToAgent(projectID, sourceAgentID, parentRunID string, args map[string]any) string {
	targetAgentID := toolStringArg(args, "target_agent_id", 128)
	body := toolStringArg(args, "body", 20000)
	if targetAgentID == "" || body == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "target_agent_id and body are required"})
	}
	project, err := a.projectByID(projectID)
	if err != nil {
		return toolJSON(map[string]any{"error": "project_not_found", "message": err.Error()})
	}
	target, ok := a.projectAgent(project, targetAgentID)
	if !ok || target.ID == sourceAgentID {
		return toolJSON(map[string]any{"error": "invalid_target", "message": "target agent must exist and differ from source"})
	}
	intent := toolStringArg(args, "intent", 64)
	if intent == "" {
		intent = "request"
	}
	if !validAgentIntent(intent) {
		return toolJSON(map[string]any{"error": "validation_error", "message": "invalid intent"})
	}
	if !a.agentRouteAllowed(projectID, sourceAgentID, targetAgentID, intent) {
		return toolJSON(map[string]any{"error": "route_denied", "message": "send_to is outside the configured agent acceptance range"})
	}
	subject := toolStringArg(args, "subject", 500)
	if subject == "" {
		subject = "Agent handoff"
	}
	threadKey := toolStringArg(args, "thread_key", 256)
	if threadKey == "" {
		threadKey = residentSessionID(projectID, sourceAgentID)
	}
	now := time.Now().UTC()
	correlationID := firstNonEmpty(toolStringArg(args, "correlation_id", 256), randomID())
	if a.collaborationCorrelationMessageCount(projectID, correlationID) >= maxCollaborationMessagesPerCorrelation {
		return toolJSON(map[string]any{"error": "correlation_limit_exceeded", "message": "collaboration message limit reached; start a new handoff only if more work is required", "correlation_id": correlationID})
	}
	artifactIDs := toolStringSliceArg(args, "artifact_ids", 100)
	if _, err := a.validateArtifactRefs(projectID, artifactIDs); err != nil {
		return toolJSON(map[string]any{"error": "invalid_artifact", "message": err.Error()})
	}
	msg := AgentInboxMessage{
		ID:             randomID(),
		ProjectID:      projectID,
		SourceAgentID:  sourceAgentID,
		TargetAgentID:  targetAgentID,
		CorrelationID:  correlationID,
		ParentRunID:    strings.TrimSpace(parentRunID),
		MessageType:    "handoff",
		Intent:         intent,
		Subject:        subject,
		Body:           body,
		Objective:      firstNonEmpty(toolStringArg(args, "objective", 4000), subject),
		ExpectedOutput: firstNonEmpty(toolStringArg(args, "expected_output", 4000), "A concise response or concrete result that closes this handoff."),
		ArtifactIDs:    artifactIDs,
		ThreadKey:      threadKey,
		Priority:       clampToolInt(args, "priority", 0, 0, 100),
		Status:         HandoffQueued,
		CreatedAt:      now,
	}
	if err := a.queueInboxMessage(projectID, msg); err != nil {
		return toolJSON(map[string]any{"error": "save_failed", "message": err.Error()})
	}
	a.appendAgentMessage(projectID, targetAgentID, "system", intent, "Handoff from "+sourceAgentID+": "+subject+"\n\n"+body)
	a.triggerAgentHandoffResponse(project, target, msg)
	return toolJSON(map[string]any{"message_id": msg.ID, "correlation_id": msg.CorrelationID, "parent_run_id": msg.ParentRunID, "status": HandoffDelivered, "target": map[string]any{"agent_id": targetAgentID}, "delivery": map[string]any{"state": HandoffDelivered, "retry_required": false, "auto_response": true}})
}

func (a *app) replyToInboxMessage(projectID, sourceAgentID, parentRunID string, args map[string]any) string {
	a.handoffReplyMu.Lock()
	defer a.handoffReplyMu.Unlock()
	inboxMessageID := toolStringArg(args, "inbox_message_id", 128)
	body := toolStringArg(args, "body", 20000)
	if inboxMessageID == "" || body == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "inbox_message_id and body are required"})
	}
	original, ok := a.inboxMessage(projectID, sourceAgentID, inboxMessageID)
	if !ok || original.TargetAgentID != sourceAgentID {
		return toolJSON(map[string]any{"error": "not_found", "message": "inbox message not found for this agent"})
	}
	if !handoffStatusOpen(original.Status) {
		return toolJSON(map[string]any{"error": "already_closed", "message": "handoff is no longer open", "status": original.Status})
	}
	if handoffMessageIsReply(original) {
		a.markInboxAcked(projectID, sourceAgentID, original.ID)
		return toolJSON(map[string]any{"error": "reply_is_terminal", "message": "a reply closes the current handoff and cannot be replied to; use send_to with a new handoff only when additional work is required", "status": HandoffClosed})
	}
	if a.collaborationCorrelationMessageCount(projectID, original.CorrelationID) >= maxCollaborationMessagesPerCorrelation {
		a.markInboxAcked(projectID, sourceAgentID, original.ID)
		return toolJSON(map[string]any{"error": "correlation_limit_exceeded", "message": "collaboration message limit reached and the handoff was closed", "correlation_id": original.CorrelationID, "status": HandoffClosed})
	}
	if original.SourceAgentID == "" || original.SourceAgentID == sourceAgentID {
		return toolJSON(map[string]any{"error": "invalid_target", "message": "inbox message has no different source agent"})
	}
	project, err := a.projectByID(projectID)
	if err != nil {
		return toolJSON(map[string]any{"error": "project_not_found", "message": err.Error()})
	}
	target, ok := a.projectAgent(project, original.SourceAgentID)
	if !ok {
		return toolJSON(map[string]any{"error": "invalid_target", "message": "source agent no longer exists"})
	}
	subject := toolStringArg(args, "subject", 500)
	if subject == "" {
		subject = "Re: " + firstNonEmpty(original.Subject, "Agent handoff")
	}
	threadKey := original.ThreadKey
	if threadKey == "" {
		threadKey = residentSessionID(projectID, original.SourceAgentID)
	}
	now := time.Now().UTC()
	msg := AgentInboxMessage{
		ID:             randomID(),
		ProjectID:      projectID,
		SourceAgentID:  sourceAgentID,
		TargetAgentID:  original.SourceAgentID,
		CorrelationID:  original.CorrelationID,
		ParentRunID:    strings.TrimSpace(parentRunID),
		MessageType:    "reply",
		Intent:         "reply",
		Subject:        subject,
		Body:           body,
		Objective:      "Deliver the result for " + original.Objective,
		ExpectedOutput: "Acknowledge the result or take the next concrete action.",
		ArtifactIDs:    append([]string{}, original.ArtifactIDs...),
		ThreadKey:      threadKey,
		ReplyToID:      original.ID,
		Priority:       original.Priority,
		Status:         HandoffQueued,
		CreatedAt:      now,
	}
	if err := a.queueInboxMessage(projectID, msg); err != nil {
		return toolJSON(map[string]any{"error": "save_failed", "message": err.Error()})
	}
	a.markInboxReplied(projectID, sourceAgentID, original.ID, body, msg.ID)
	a.appendAgentMessage(projectID, original.SourceAgentID, "system", "reply", "Reply from "+sourceAgentID+" to "+original.ID+": "+subject+"\n\n"+body)
	a.triggerAgentHandoffResponse(project, target, msg)
	return toolJSON(map[string]any{"message_id": msg.ID, "reply_to_id": original.ID, "correlation_id": msg.CorrelationID, "parent_run_id": msg.ParentRunID, "status": HandoffDelivered, "target": map[string]any{"agent_id": original.SourceAgentID}, "delivery": map[string]any{"state": HandoffDelivered, "retry_required": false, "auto_response": true}})
}

func (a *app) declineInboxHandoff(projectID, agentID string, args map[string]any) string {
	messageID := toolStringArg(args, "inbox_message_id", 128)
	reason := toolStringArg(args, "reason", 4000)
	if messageID == "" || reason == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "inbox_message_id and reason are required"})
	}
	msg, ok := a.inboxMessage(projectID, agentID, messageID)
	if !ok || msg.TargetAgentID != agentID {
		return toolJSON(map[string]any{"error": "not_found", "message": "inbox message not found for this agent"})
	}
	if _, ok := a.transitionHandoff(projectID, agentID, messageID, HandoffDeclined, reason); !ok {
		return toolJSON(map[string]any{"error": "already_closed", "message": "handoff is no longer open", "status": msg.Status})
	}
	project, err := a.projectByID(projectID)
	if err == nil {
		if target, exists := a.projectAgent(project, msg.SourceAgentID); exists {
			notification := AgentInboxMessage{
				ID: randomID(), ProjectID: projectID, SourceAgentID: agentID, TargetAgentID: msg.SourceAgentID,
				CorrelationID: msg.CorrelationID, MessageType: "decline", Intent: "reply",
				Subject: "Declined: " + firstNonEmpty(msg.Subject, "Agent handoff"), Body: reason,
				Objective:      "Review the declined handoff and choose another path.",
				ExpectedOutput: "Acknowledge the decline or take the next concrete action.",
				ArtifactIDs:    append([]string{}, msg.ArtifactIDs...), ThreadKey: msg.ThreadKey,
				ReplyToID: msg.ID, Priority: msg.Priority, Status: HandoffQueued, CreatedAt: time.Now().UTC(),
			}
			if err := a.queueInboxMessage(projectID, notification); err == nil {
				a.updateInboxMessage(projectID, agentID, messageID, func(item *AgentInboxMessage) { item.ResultMessageID = notification.ID })
				a.triggerAgentHandoffResponse(project, target, notification)
			}
		}
	}
	a.appendAgentMessage(projectID, msg.SourceAgentID, "system", "handoff_declined", "Handoff "+messageID+" declined by "+agentID+": "+reason)
	return toolJSON(map[string]any{"inbox_message_id": messageID, "correlation_id": msg.CorrelationID, "status": HandoffDeclined, "reason": reason})
}

func (a *app) ackInboxMessage(projectID, agentID string, args map[string]any) string {
	inboxMessageID := toolStringArg(args, "inbox_message_id", 128)
	if inboxMessageID == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "inbox_message_id is required"})
	}
	msg, ok := a.inboxMessage(projectID, agentID, inboxMessageID)
	if !ok || msg.TargetAgentID != agentID {
		return toolJSON(map[string]any{"error": "not_found", "message": "inbox message not found for this agent"})
	}
	if !a.markInboxAcked(projectID, agentID, inboxMessageID) {
		return toolJSON(map[string]any{"error": "not_found", "message": "inbox message not found for this agent"})
	}
	note := toolStringArg(args, "note", 1000)
	if note != "" {
		a.appendAgentMessage(projectID, agentID, "system", "ack", "Acked inbox "+inboxMessageID+": "+note)
	}
	if msg.SourceAgentID != "" && msg.SourceAgentID != agentID {
		body := "Ack from " + agentID + " for " + inboxMessageID
		if note != "" {
			body += ": " + note
		}
		a.appendAgentMessage(projectID, msg.SourceAgentID, "system", "ack", body)
	}
	return toolJSON(map[string]any{"inbox_message_id": inboxMessageID, "correlation_id": msg.CorrelationID, "status": HandoffClosed})
}

func (a *app) reportActivity(projectID string, agent Agent, args map[string]any) string {
	summary := toolStringArg(args, "summary", 1000)
	if summary == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "summary is required"})
	}
	kind := toolStringArg(args, "activity_kind", 64)
	if kind == "" {
		kind = "progress"
	}
	if !validActivityKind(kind) {
		return toolJSON(map[string]any{"error": "validation_error", "message": "invalid activity_kind"})
	}
	entry := a.appendBlackboardEntry(projectID, agent, kind, summary, toolStringArg(args, "detail", 8000), toolStringArg(args, "inbox_message_id", 128))
	return toolJSON(map[string]any{"entry": entry})
}

func (a *app) markBlackboardActivity(projectID string, agent Agent, args map[string]any) string {
	activityID := toolStringArg(args, "activity_id", 128)
	result := toolStringArg(args, "handling_result", 64)
	if activityID == "" || result == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "activity_id and handling_result are required"})
	}
	if !validActivityHandlingResult(result) {
		return toolJSON(map[string]any{"error": "validation_error", "message": "invalid handling_result"})
	}
	now := time.Now().UTC()
	var updated AgentBlackboardEntry
	found := false
	derived := false
	a.mu.Lock()
	items := a.blackboard[projectID]
	for i := range items {
		if items[i].ID != activityID {
			continue
		}
		if items[i].Derived {
			derived = true
			break
		}
		items[i].HandledAt = &now
		items[i].UpdatedAt = now
		items[i].HandledBy = agent.ID
		items[i].HandlingResult = result
		items[i].RoutedToAgentID = toolStringArg(args, "routed_to_agent_id", 128)
		items[i].CreatedTaskID = toolStringArg(args, "created_task_id", 128)
		items[i].RequiresAction = toolBoolArg(args, "requires_action", false)
		switch result {
		case "expired":
			items[i].Status = "expired"
		case "ignored", "no_action":
			items[i].Status = "ignored"
		default:
			items[i].Status = "handled"
		}
		updated = items[i]
		found = true
		break
	}
	a.blackboard[projectID] = items
	a.mu.Unlock()
	if derived {
		return toolJSON(map[string]any{"error": "derived_projection", "message": "derived blackboard entries are read-only; act on the source Run, Handoff, or Task"})
	}
	if !found {
		return toolJSON(map[string]any{"error": "not_found", "message": "activity not found"})
	}
	if err := a.saveBlackboard(); err != nil {
		return toolJSON(map[string]any{"error": "save_failed", "message": err.Error()})
	}
	a.emitRuntimeStateChanged(RuntimeEvent{
		ID:        randomID(),
		ProjectID: projectID,
		Kind:      "blackboard_changed",
		EntityID:  activityID,
		To:        updated.Status,
		Reason:    "blackboard_activity_marked",
		CreatedAt: now,
	})
	return toolJSON(map[string]any{"entry": updated})
}

func validActivityHandlingResult(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "routed_to_inbox", "created_task", "asked_user", "ignored", "expired", "no_action":
		return true
	default:
		return false
	}
}
