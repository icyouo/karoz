package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

func (a *app) triggerAgentHandoffResponse(project Project, target Agent, msg AgentInboxMessage) {
	if strings.EqualFold(os.Getenv("KAROZ_AGENT_AUTO_RESPOND"), "0") || strings.EqualFold(os.Getenv("KAROZ_AGENT_AUTO_RESPOND"), "false") {
		return
	}
	job, err := newScheduledRun(
		ScheduledRunHandoff,
		AgentRunInput{ProjectID: project.ID, AgentID: target.ID, Trigger: RunTriggerHandoff, TurnType: "dev", SourceID: msg.SourceAgentID, MessageID: msg.ID},
		"handoff/"+project.ID+"/"+target.ID+"/"+msg.ID,
		HandoffRunPayload{InboxMessageID: msg.ID},
		3*time.Minute,
	)
	if err != nil {
		log.Printf("create handoff scheduled run project=%s agent=%s inbox=%s: %v", project.ID, target.ID, msg.ID, err)
		return
	}
	if _, scheduled := a.scheduleAgentRun(job); !scheduled {
		log.Printf("schedule handoff run rejected project=%s agent=%s inbox=%s", project.ID, target.ID, msg.ID)
	}
}

func (a *app) completeUnhandledInboxAfterAutoResponse(project Project, target Agent, msg AgentInboxMessage, out string) {
	current, ok := a.inboxMessage(project.ID, target.ID, msg.ID)
	if !ok || !handoffStatusOpen(current.Status) {
		return
	}
	out = strings.TrimSpace(out)
	if current.SourceAgentID == "karoz" && !handoffMessageIsReply(current) {
		a.appendBlackboardEntry(project.ID, target, "done", firstNonEmpty(out, "Coordinator handoff completed"), "", current.ID)
		if _, err := a.completeCoordinatorHandoff(current, target, "done", firstNonEmpty(out, "Coordinator handoff completed")); err != nil {
			log.Printf("auto report to coordinator failed project=%s target=%s inbox=%s: %v", project.ID, target.ID, current.ID, err)
		}
		return
	}
	if out == "" || handoffMessageIsTerminalDelivery(current) {
		if !a.markInboxAcked(project.ID, target.ID, current.ID) {
			log.Printf("auto ack failed project=%s target=%s inbox=%s", project.ID, target.ID, current.ID)
		}
		return
	}
	source, ok := a.projectAgent(project, current.SourceAgentID)
	if !ok || source.ID == "" || source.ID == target.ID {
		a.transitionHandoff(project.ID, target.ID, current.ID, HandoffClosed, out)
		return
	}
	parentRunID := current.ParentRunID
	if run, active := a.activeAgentRun(project.ID, target.ID); active {
		parentRunID = run.ID
	}
	reply := AgentInboxMessage{
		ID:             randomID(),
		ProjectID:      project.ID,
		SourceAgentID:  target.ID,
		TargetAgentID:  current.SourceAgentID,
		MessageType:    "reply",
		CorrelationID:  current.CorrelationID,
		ParentRunID:    parentRunID,
		Intent:         "reply",
		Subject:        "Re: " + firstNonEmpty(current.Subject, "Agent handoff"),
		Body:           out,
		Objective:      "Deliver the result for " + current.Objective,
		ExpectedOutput: "Acknowledge the result or take the next concrete action.",
		ArtifactIDs:    append([]string{}, current.ArtifactIDs...),
		ThreadKey:      firstNonEmpty(current.ThreadKey, residentSessionID(project.ID, current.SourceAgentID)),
		ReplyToID:      current.ID,
		Priority:       current.Priority,
		Status:         HandoffQueued,
		CreatedAt:      time.Now().UTC(),
	}
	if err := a.queueInboxMessage(project.ID, reply); err != nil {
		log.Printf("auto reply queue failed project=%s source=%s target=%s inbox=%s: %v", project.ID, target.ID, current.SourceAgentID, current.ID, err)
		return
	}
	a.markInboxReplied(project.ID, target.ID, current.ID, out, reply.ID)
	a.appendAgentMessage(project.ID, current.SourceAgentID, "system", "reply", "Reply from "+target.ID+" to "+current.ID+": "+reply.Subject+"\n\n"+out)
	a.triggerAgentHandoffResponse(project, source, reply)
}

func (a *app) markInboxConsumed(projectID, agentID, messageID string) {
	a.transitionHandoff(projectID, agentID, messageID, HandoffClosed, "")
}

func (a *app) updateInboxMessage(projectID, agentID, messageID string, update func(*AgentInboxMessage)) bool {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	updated := false
	for i := range a.inbox[key] {
		if a.inbox[key][i].ID != messageID {
			continue
		}
		update(&a.inbox[key][i])
		updated = true
		break
	}
	a.mu.Unlock()
	if updated {
		if err := a.saveInbox(); err != nil {
			log.Printf("save inbox: %v", err)
		}
	}
	return updated
}

func (a *app) inboxMessage(projectID, agentID, messageID string) (AgentInboxMessage, bool) {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, msg := range a.inbox[key] {
		if msg.ID == messageID {
			return msg, true
		}
	}
	return AgentInboxMessage{}, false
}

func (a *app) markInboxReplied(projectID, agentID, messageID, result, resultMessageID string) {
	if _, ok := a.transitionHandoff(projectID, agentID, messageID, HandoffReplied, result); !ok {
		log.Printf("reply_to referenced missing inbox project=%s agent=%s inbox=%s", projectID, agentID, messageID)
		return
	}
	a.updateInboxMessage(projectID, agentID, messageID, func(msg *AgentInboxMessage) { msg.ResultMessageID = resultMessageID })
}

func (a *app) markInboxAcked(projectID, agentID, messageID string) bool {
	now := time.Now().UTC()
	msg, ok := a.transitionHandoff(projectID, agentID, messageID, HandoffClosed, "Acknowledged")
	if !ok {
		return false
	}
	a.updateInboxMessage(projectID, agentID, messageID, func(item *AgentInboxMessage) { item.AckedAt = &now })
	a.closeOriginalHandoffForReply(msg)
	return true
}

func (a *app) queueInboxMessage(projectID string, msg AgentInboxMessage) error {
	msg.ProjectID = projectID
	msg, _ = normalizeHandoffMessage(msg)
	msg.Status = HandoffQueued
	msg.DeliveredAt = nil
	msg.UpdatedAt = time.Now().UTC()
	key := projectAgentKey(projectID, msg.TargetAgentID)
	a.mu.Lock()
	correlationCount := 0
	for _, items := range a.inbox {
		for _, item := range items {
			if item.ProjectID == projectID && item.CorrelationID == msg.CorrelationID {
				correlationCount++
			}
		}
	}
	if correlationCount >= maxCollaborationMessagesPerCorrelation {
		a.mu.Unlock()
		return fmt.Errorf("collaboration correlation %s reached message limit %d", msg.CorrelationID, maxCollaborationMessagesPerCorrelation)
	}
	a.inbox[key] = append(a.inbox[key], msg)
	a.mu.Unlock()
	if err := a.saveInbox(); err != nil {
		log.Printf("save inbox: %v", err)
		return err
	}
	a.emitRuntimeStateChanged(RuntimeEvent{
		ID:          randomID(),
		ProjectID:   projectID,
		Kind:        "handoff_created",
		EntityID:    msg.ID,
		To:          HandoffQueued,
		FromAgentID: msg.SourceAgentID,
		ToAgentID:   msg.TargetAgentID,
		Reason:      "handoff_created",
		CreatedAt:   time.Now().UTC(),
	})
	if _, ok := a.transitionHandoff(projectID, msg.TargetAgentID, msg.ID, HandoffDelivered, ""); !ok {
		return fmt.Errorf("deliver handoff %s", msg.ID)
	}
	return nil
}

func (a *app) appendBlackboardEntry(projectID string, agent Agent, kind, summary, detail, inboxMessageID string) AgentBlackboardEntry {
	now := time.Now().UTC()
	entry := AgentBlackboardEntry{
		ID:                   randomID(),
		ProjectID:            projectID,
		AgentID:              agent.ID,
		AgentName:            firstNonEmpty(agent.Nickname, agent.DisplayName, agent.Name, agent.ID),
		ActivityKind:         kind,
		Summary:              summary,
		Detail:               detail,
		SourceType:           "agent_report",
		SourceInboxMessageID: inboxMessageID,
		CreatedAt:            now,
		UpdatedAt:            now,
		Status:               "active",
		RequiresAction:       blackboardEntryRequiresAction(kind, summary, detail),
	}
	entry.SourceID = entry.ID
	a.mu.Lock()
	if a.blackboard == nil {
		a.blackboard = map[string][]AgentBlackboardEntry{}
	}
	a.blackboard[projectID] = append(a.blackboard[projectID], entry)
	a.mu.Unlock()
	if err := a.saveBlackboard(); err != nil {
		log.Printf("save blackboard: %v", err)
	}
	a.emitRuntimeStateChanged(RuntimeEvent{
		ID:        randomID(),
		ProjectID: projectID,
		Kind:      "blackboard_changed",
		EntityID:  entry.ID,
		To:        entry.Status,
		Reason:    "blackboard_entry_created",
		CreatedAt: time.Now().UTC(),
	})
	return entry
}
