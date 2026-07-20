package main

import (
	collaborationdomain "github.com/karoz/karoz/internal/collaboration"
	"strings"
	"time"
)

const (
	HandoffQueued    = string(collaborationdomain.HandoffQueued)
	HandoffDelivered = string(collaborationdomain.HandoffDelivered)
	HandoffClaimed   = string(collaborationdomain.HandoffClaimed)
	HandoffWorking   = string(collaborationdomain.HandoffWorking)
	HandoffReplied   = string(collaborationdomain.HandoffReplied)
	HandoffDeclined  = string(collaborationdomain.HandoffDeclined)
	HandoffFailed    = string(collaborationdomain.HandoffFailed)
	HandoffClosed    = string(collaborationdomain.HandoffClosed)
)

const maxCollaborationMessagesPerCorrelation = 12

func handoffMessageIsReply(msg AgentInboxMessage) bool {
	return strings.EqualFold(strings.TrimSpace(msg.MessageType), "reply")
}

func handoffMessageIsTerminalDelivery(msg AgentInboxMessage) bool {
	switch strings.ToLower(strings.TrimSpace(msg.MessageType)) {
	case "reply", "result", "decline", "failure":
		return true
	default:
		return strings.EqualFold(strings.TrimSpace(msg.Intent), "reply")
	}
}

func (a *app) collaborationCorrelationMessageCount(projectID, correlationID string) int {
	correlationID = strings.TrimSpace(correlationID)
	if correlationID == "" {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	count := 0
	for _, items := range a.inbox {
		for _, item := range items {
			if item.ProjectID == projectID && item.CorrelationID == correlationID {
				count++
			}
		}
	}
	return count
}

func handoffStatusOpen(status string) bool {
	return collaborationdomain.HandoffOpen(status)
}

func normalizeHandoffStatus(status string) string {
	return string(collaborationdomain.NormalizeHandoffStatus(status))
}

func normalizeHandoffMessage(msg AgentInboxMessage) (AgentInboxMessage, bool) {
	changed := false
	now := time.Now().UTC()
	if strings.TrimSpace(msg.ID) == "" {
		msg.ID = randomID()
		changed = true
	}
	if strings.TrimSpace(msg.CorrelationID) == "" {
		msg.CorrelationID = firstNonEmpty(msg.ThreadKey, msg.ReplyToID, msg.ID)
		changed = true
	}
	if strings.TrimSpace(msg.MessageType) == "" {
		msg.MessageType = "handoff"
		changed = true
	}
	if strings.TrimSpace(msg.Objective) == "" {
		msg.Objective = firstNonEmpty(msg.Subject, msg.Body, "Complete the requested handoff")
		changed = true
	}
	if strings.TrimSpace(msg.ExpectedOutput) == "" {
		if msg.MessageType == "reply" {
			msg.ExpectedOutput = "Acknowledge the result or take the next concrete action."
		} else {
			msg.ExpectedOutput = "A concise response or concrete result that closes this handoff."
		}
		changed = true
	}
	normalizedStatus := normalizeHandoffStatus(msg.Status)
	if msg.Status != normalizedStatus {
		msg.Status = normalizedStatus
		changed = true
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = now
		changed = true
	}
	if msg.UpdatedAt.IsZero() {
		msg.UpdatedAt = msg.CreatedAt
		changed = true
	}
	if msg.Status != HandoffQueued && msg.DeliveredAt == nil {
		delivered := msg.CreatedAt
		msg.DeliveredAt = &delivered
		changed = true
	}
	return msg, changed
}

func (a *app) transitionHandoff(projectID, agentID, messageID, next, result string) (AgentInboxMessage, bool) {
	a.handoffOpsMu.Lock()
	defer a.handoffOpsMu.Unlock()
	updated, err := a.handoffService().Transition(projectID, agentID, messageID, next, result)
	return updated, err == nil
}

func (a *app) claimHandoff(projectID, agentID, messageID string) bool {
	if _, ok := a.transitionHandoff(projectID, agentID, messageID, HandoffClaimed, ""); !ok {
		return false
	}
	_, ok := a.transitionHandoff(projectID, agentID, messageID, HandoffWorking, "")
	return ok
}

func (a *app) failHandoff(projectID, agentID, messageID string, runErr error) {
	if runErr == nil {
		return
	}
	a.transitionHandoff(projectID, agentID, messageID, HandoffFailed, runErr.Error())
}

func (a *app) retryHandoff(projectID, agentID, messageID string) {
	a.transitionHandoff(projectID, agentID, messageID, HandoffDelivered, "")
}

func (a *app) closeOriginalHandoffForReply(reply AgentInboxMessage) {
	if (reply.MessageType != "reply" && reply.MessageType != "decline" && reply.MessageType != "failure") || strings.TrimSpace(reply.ReplyToID) == "" || strings.TrimSpace(reply.SourceAgentID) == "" {
		return
	}
	// Closing the notification must not overwrite the substantive result already
	// stored on the original handoff with the notification's "Acknowledged" value.
	a.transitionHandoff(reply.ProjectID, reply.SourceAgentID, reply.ReplyToID, HandoffClosed, "")
}

func (a *app) notifyFailedHandoff(job ScheduledRun) {
	messageID := firstNonEmpty(job.MessageID, handoffMessageID(job))
	original, ok := a.inboxMessage(job.ProjectID, job.AgentID, messageID)
	if !ok || original.Status != HandoffFailed || original.ResultMessageID != "" || original.SourceAgentID == "" {
		return
	}
	project, err := a.projectByID(job.ProjectID)
	if err != nil {
		return
	}
	target, ok := a.projectAgent(project, original.SourceAgentID)
	if !ok {
		return
	}
	now := time.Now().UTC()
	notification := AgentInboxMessage{
		ID:             randomID(),
		ProjectID:      job.ProjectID,
		SourceAgentID:  job.AgentID,
		TargetAgentID:  original.SourceAgentID,
		CorrelationID:  original.CorrelationID,
		ParentRunID:    job.ID,
		MessageType:    "failure",
		Intent:         "reply",
		Subject:        "Failed: " + firstNonEmpty(original.Subject, "Agent handoff"),
		Body:           firstNonEmpty(original.FailureReason, job.Error, "Handoff execution failed"),
		Objective:      "Review the failed handoff and choose a retry or alternate path.",
		ExpectedOutput: "Acknowledge the failure or take the next concrete action.",
		ArtifactIDs:    append([]string{}, original.ArtifactIDs...),
		ThreadKey:      original.ThreadKey,
		ReplyToID:      original.ID,
		Priority:       original.Priority,
		Status:         HandoffQueued,
		CreatedAt:      now,
	}
	if err := a.queueInboxMessage(job.ProjectID, notification); err != nil {
		return
	}
	a.updateInboxMessage(job.ProjectID, job.AgentID, original.ID, func(item *AgentInboxMessage) { item.ResultMessageID = notification.ID })
	a.appendAgentMessage(job.ProjectID, original.SourceAgentID, "system", "handoff_failed", "Handoff "+original.ID+" failed: "+notification.Body)
	a.triggerAgentHandoffResponse(project, target, notification)
}

func toolStringSliceArg(args map[string]any, key string, limit int) []string {
	raw, ok := args[key].([]any)
	if !ok {
		if values, ok := args[key].([]string); ok {
			raw = make([]any, len(values))
			for i := range values {
				raw[i] = values[i]
			}
		}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		item, _ := value.(string)
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}
