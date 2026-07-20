package main

import (
	"fmt"
	"strings"
	"time"
)

const (
	blackboardSourceAgentReport = "agent_report"
	blackboardSourceRun         = "run"
	blackboardSourceHandoff     = "handoff"
	blackboardSourceTask        = "task"
	blackboardSourceArtifact    = "artifact"
)

// projectRuntimeEventToBlackboard maintains a read model. Runtime entities
// remain the source of truth; derived entries never drive retries or delivery.
func (a *app) projectRuntimeEventToBlackboard(event RuntimeEvent) {
	var entry AgentBlackboardEntry
	switch event.Kind {
	case "agent_run_changed":
		entry = a.runBlackboardProjection(event)
	case "handoff_created", "handoff_changed":
		entry = a.handoffBlackboardProjection(event)
	case "task_changed":
		entry = a.taskBlackboardProjection(event)
	case "artifact_changed":
		entry = a.artifactBlackboardProjection(event)
	default:
		return
	}
	if entry.SourceID == "" {
		return
	}
	a.upsertBlackboardProjection(entry)
}

func (a *app) runBlackboardProjection(event RuntimeEvent) AgentBlackboardEntry {
	runID := firstNonEmpty(event.RunID, event.EntityID)
	status := firstNonEmpty(event.To, "active")
	label := a.agentLabel(event.ProjectID, event.EntityID)
	return newDerivedBlackboardEntry(event, blackboardSourceRun, runID, event.EntityID, label,
		"run", label+" run "+status,
		strings.TrimSpace(fmt.Sprintf("trigger=%s reason=%s", event.Trigger, event.Reason)), status, "")
}

func (a *app) handoffBlackboardProjection(event RuntimeEvent) AgentBlackboardEntry {
	msg, ok := a.handoffByID(event.ProjectID, event.EntityID)
	if !ok || msg.MessageType != "handoff" {
		return AgentBlackboardEntry{}
	}
	label := a.agentLabel(event.ProjectID, msg.TargetAgentID)
	detailParts := []string{
		"objective=" + msg.Objective,
		"expected_output=" + msg.ExpectedOutput,
	}
	if msg.Result != "" {
		detailParts = append(detailParts, "result="+limitString(msg.Result, 700))
	}
	if msg.FailureReason != "" {
		detailParts = append(detailParts, "failure="+limitString(msg.FailureReason, 700))
	}
	entry := newDerivedBlackboardEntry(event, blackboardSourceHandoff, msg.ID, msg.TargetAgentID, label,
		"handoff", msg.SourceAgentID+" → "+msg.TargetAgentID+": "+firstNonEmpty(msg.Subject, msg.Objective),
		strings.Join(detailParts, "\n"), msg.Status, msg.CorrelationID)
	entry.SourceInboxMessageID = msg.ID
	entry.TargetAgentID = msg.TargetAgentID
	return entry
}

func (a *app) taskBlackboardProjection(event RuntimeEvent) AgentBlackboardEntry {
	task, ok := a.findTask(event.ProjectID, event.EntityID)
	if !ok {
		return AgentBlackboardEntry{}
	}
	detail := firstNonEmpty(task.FailureSummary, task.Result, task.Goal, task.Description)
	return newDerivedBlackboardEntry(event, blackboardSourceTask, task.ID, "", "Runtime",
		"task", firstNonEmpty(task.Title, task.ID), limitString(detail, 900), task.Status, "")
}

func (a *app) artifactBlackboardProjection(event RuntimeEvent) AgentBlackboardEntry {
	artifact, ok := a.artifactByID(event.ProjectID, event.EntityID)
	if !ok {
		return AgentBlackboardEntry{}
	}
	label := a.agentLabel(event.ProjectID, artifact.AgentID)
	detail := fmt.Sprintf("kind=%s revision=%d path=%s", artifact.Kind, artifact.Revision, artifact.Path)
	if artifact.ReviewNote != "" {
		detail += "\nreview=" + artifact.ReviewNote
	}
	return newDerivedBlackboardEntry(event, blackboardSourceArtifact, artifact.ID, artifact.AgentID, label,
		"artifact", artifact.Title, detail, artifact.Status, "")
}

func newDerivedBlackboardEntry(event RuntimeEvent, sourceType, sourceID, agentID, agentName, kind, summary, detail, status, correlationID string) AgentBlackboardEntry {
	now := event.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return AgentBlackboardEntry{
		ID:             randomID(),
		ProjectID:      event.ProjectID,
		AgentID:        agentID,
		AgentName:      firstNonEmpty(agentName, agentID, "Runtime"),
		ActivityKind:   kind,
		Summary:        strings.TrimSpace(summary),
		Detail:         strings.TrimSpace(detail),
		SourceType:     sourceType,
		SourceID:       sourceID,
		EventKind:      event.Kind,
		Derived:        true,
		CorrelationID:  correlationID,
		RunID:          event.RunID,
		CreatedAt:      now,
		UpdatedAt:      now,
		Status:         firstNonEmpty(status, "active"),
		RequiresAction: false,
	}
}

func (a *app) upsertBlackboardProjection(entry AgentBlackboardEntry) {
	a.mu.Lock()
	if a.blackboard == nil {
		a.blackboard = map[string][]AgentBlackboardEntry{}
	}
	items := a.blackboard[entry.ProjectID]
	for i := range items {
		if !items[i].Derived || items[i].SourceType != entry.SourceType || items[i].SourceID != entry.SourceID {
			continue
		}
		entry.ID = items[i].ID
		entry.CreatedAt = items[i].CreatedAt
		items[i] = entry
		a.blackboard[entry.ProjectID] = items
		a.mu.Unlock()
		a.saveOrLog("blackboard", a.saveBlackboard())
		return
	}
	a.blackboard[entry.ProjectID] = append(items, entry)
	a.mu.Unlock()
	a.saveOrLog("blackboard", a.saveBlackboard())
}

func (a *app) handoffByID(projectID, messageID string) (AgentInboxMessage, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, messages := range a.inbox {
		for _, msg := range messages {
			if msg.ProjectID == projectID && msg.ID == messageID {
				return msg, true
			}
		}
	}
	return AgentInboxMessage{}, false
}

func (a *app) agentLabel(projectID, agentID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, agent := range a.agents[projectID] {
		if agent.ID == agentID {
			return firstNonEmpty(agent.Nickname, agent.DisplayName, agent.Name, agent.ID)
		}
	}
	return firstNonEmpty(agentID, "Runtime")
}

func (a *app) rebuildBlackboardProjections() error {
	a.mu.Lock()
	if a.blackboard == nil {
		a.blackboard = map[string][]AgentBlackboardEntry{}
	}
	for projectID, items := range a.blackboard {
		manual := items[:0]
		for _, entry := range items {
			if !entry.Derived {
				manual = append(manual, entry)
			}
		}
		a.blackboard[projectID] = manual
	}
	var handoffs []AgentInboxMessage
	for _, messages := range a.inbox {
		handoffs = append(handoffs, messages...)
	}
	var tasks []Task
	for _, projectTasks := range a.tasks {
		tasks = append(tasks, projectTasks...)
	}
	var artifacts []Artifact
	for _, projectArtifacts := range a.artifacts {
		artifacts = append(artifacts, projectArtifacts...)
	}
	a.mu.Unlock()
	if err := a.saveBlackboard(); err != nil {
		return err
	}
	for _, msg := range handoffs {
		a.projectRuntimeEventToBlackboard(RuntimeEvent{
			ID: randomID(), ProjectID: msg.ProjectID, Kind: "handoff_changed", EntityID: msg.ID,
			From: msg.Status, To: msg.Status, Reason: "startup_projection", CreatedAt: firstNonZeroTime(msg.UpdatedAt, msg.CreatedAt),
		})
	}
	for _, task := range tasks {
		a.projectRuntimeEventToBlackboard(RuntimeEvent{
			ID: randomID(), ProjectID: task.ProjectID, Kind: "task_changed", EntityID: task.ID,
			From: task.Status, To: task.Status, Reason: "startup_projection", CreatedAt: firstNonZeroTime(task.UpdatedAt, task.CreatedAt),
		})
	}
	for _, artifact := range artifacts {
		a.projectRuntimeEventToBlackboard(RuntimeEvent{
			ID: randomID(), ProjectID: artifact.ProjectID, Kind: "artifact_changed", EntityID: artifact.ID,
			From: artifact.Status, To: artifact.Status, Reason: "startup_projection", CreatedAt: firstNonZeroTime(artifact.UpdatedAt, artifact.CreatedAt),
		})
	}
	return nil
}

func firstNonZeroTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Now().UTC()
}
