package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// agentTranscriptAppendMetadata preserves the wire-level facts that the
// visible AgentMessage API intentionally does not expose. It is only used
// while the app mutex is held.
type agentTranscriptAppendMetadata struct {
	RunID         string
	Kind          string
	ToolCallID    string
	ToolName      string
	ToolArguments string
	ToolResult    string
	ToolSuccess   *bool
	Visible       *bool
	ModelOnly     bool
}

func transcriptKindForMessage(role, intent string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	intent = strings.ToLower(strings.TrimSpace(intent))
	switch {
	case role == "tool_call":
		return "tool_call"
	case role == "tool_result":
		return "tool_result"
	case intent == "interrupt":
		return "interrupt"
	case intent == "status" || intent == "error" || role == "status":
		return "status"
	default:
		return "message"
	}
}

func transcriptItemForAgentMessage(message AgentMessage, metadata agentTranscriptAppendMetadata) AgentTranscriptItem {
	visible := true
	if metadata.Visible != nil {
		visible = *metadata.Visible
	}
	return AgentTranscriptItem{
		ID:            message.ID,
		MessageID:     message.ID,
		ProjectID:     message.ProjectID,
		AgentID:       message.AgentID,
		SessionID:     message.SessionID,
		Seq:           message.Seq,
		RunID:         strings.TrimSpace(metadata.RunID),
		Role:          message.Role,
		Kind:          firstNonEmpty(strings.TrimSpace(metadata.Kind), transcriptKindForMessage(message.Role, message.Intent)),
		Intent:        message.Intent,
		Body:          message.Body,
		ToolCallID:    strings.TrimSpace(metadata.ToolCallID),
		ToolName:      strings.TrimSpace(metadata.ToolName),
		ToolArguments: strings.TrimSpace(metadata.ToolArguments),
		ToolResult:    strings.TrimSpace(metadata.ToolResult),
		ToolSuccess:   metadata.ToolSuccess,
		Visible:       visible,
		ModelOnly:     metadata.ModelOnly,
		CreatedAt:     message.CreatedAt,
	}
}

// appendAgentModelOnlyTranscriptForRun records the exact instruction handed to
// a scheduled resident Run without adding a visible chat card. Scheduled work
// has no user POST to persist for it, so this is the durable first item that
// ties its input, tool calls/results, and final answer to one Run trajectory.
//
// The append is idempotent for a Run/input pair. A transient save failure can
// therefore be retried without duplicating the model instruction in context.
func (a *app) appendAgentModelOnlyTranscriptForRun(projectID, agentID, runID, intent, body string) (AgentTranscriptItem, bool, error) {
	projectID = strings.TrimSpace(projectID)
	agentID = strings.TrimSpace(agentID)
	runID = strings.TrimSpace(runID)
	intent = firstNonEmpty(strings.TrimSpace(intent), "scheduled_input")
	body = strings.TrimSpace(body)
	if projectID == "" || agentID == "" || runID == "" || body == "" {
		return AgentTranscriptItem{}, false, fmt.Errorf("scheduled transcript input requires project, agent, run, and body")
	}
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	runtime := a.agentRuntimeLocked()
	run, active := runtime.runs[key]
	if !active || !run.State.Active() || run.ID != runID || runtime.cancelling[key] == runID {
		a.mu.Unlock()
		return AgentTranscriptItem{}, false, fmt.Errorf("scheduled transcript Run %s is no longer active: %w", runID, context.Canceled)
	}
	for _, item := range a.conversationServiceLocked().TranscriptsFor(key) {
		if item.ModelOnly && item.RunID == runID && item.Role == "user" && item.Intent == intent && item.Body == body {
			a.mu.Unlock()
			if err := a.saveAgentSessionEvents(); err != nil {
				return item, true, err
			}
			return item, true, nil
		}
	}
	session := a.ensureAgentSessionLocked(projectID, agentID)
	item := AgentTranscriptItem{
		ID:        messageID(),
		ProjectID: projectID,
		AgentID:   agentID,
		SessionID: session.SessionID,
		Seq:       a.nextAgentTranscriptSequenceLocked(projectID, agentID),
		RunID:     runID,
		Role:      "user",
		Kind:      "message",
		Intent:    intent,
		Body:      body,
		Visible:   false,
		ModelOnly: true,
		CreatedAt: time.Now().UTC(),
	}
	a.appendAgentSessionEventLocked(newAgentModelInputSessionEvent(item))
	a.mu.Unlock()
	if err := a.saveAgentSessionEvents(); err != nil {
		return item, true, err
	}
	return item, true, nil
}

// nextAgentTranscriptSequenceLocked gives visible messages and hidden model
// inputs one shared order. Gaps in the visible AgentMessage sequence are
// intentional: they preserve the true ordering without exposing model-only
// runtime instructions through the stable chat API.
func (a *app) nextAgentTranscriptSequenceLocked(projectID, agentID string) int64 {
	key := projectAgentKey(projectID, agentID)
	var maxSeq int64
	for index, message := range a.conversationServiceLocked().MessagesFor(key) {
		seq := message.Seq
		if seq <= 0 {
			seq = int64(index + 1)
		}
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	for _, item := range a.conversationServiceLocked().TranscriptsFor(key) {
		if item.Seq > maxSeq {
			maxSeq = item.Seq
		}
	}
	return maxSeq + 1
}

func transcriptItemFromLegacyMessage(message AgentMessage) AgentTranscriptItem {
	return AgentTranscriptItem{
		ID:        message.ID,
		MessageID: message.ID,
		ProjectID: message.ProjectID,
		AgentID:   message.AgentID,
		SessionID: message.SessionID,
		Seq:       message.Seq,
		Role:      message.Role,
		Kind:      transcriptKindForMessage(message.Role, message.Intent),
		Intent:    message.Intent,
		Body:      message.Body,
		Visible:   true,
		CreatedAt: message.CreatedAt,
	}
}

// agentTranscriptForModel returns the provider-neutral projection rebuilt from
// the canonical session event stream.
func (a *app) agentTranscriptForModel(projectID, agentID string) []AgentTranscriptItem {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	stored := a.conversationServiceLocked().TranscriptsFor(key)
	a.mu.Unlock()
	sort.SliceStable(stored, func(i, j int) bool {
		if stored[i].Seq == stored[j].Seq {
			return stored[i].CreatedAt.Before(stored[j].CreatedAt)
		}
		return stored[i].Seq < stored[j].Seq
	})
	if stored == nil {
		return []AgentTranscriptItem{}
	}
	return stored
}

func (a *app) appendAgentToolCallForRun(projectID, agentID, runID string, call codexToolCall) (AgentMessage, bool) {
	return a.appendAgentMessageForRunWithTranscript(projectID, agentID, runID, "tool_call", call.Name, call.Arguments, agentTranscriptAppendMetadata{
		RunID:         runID,
		Kind:          "tool_call",
		ToolCallID:    firstNonEmpty(call.CallID, call.ID),
		ToolName:      call.Name,
		ToolArguments: call.Arguments,
	})
}

func (a *app) appendAgentToolResultForRun(projectID, agentID, runID string, call codexToolCall, result string, success bool) (AgentMessage, bool) {
	succeeded := success
	return a.appendAgentMessageForRunWithTranscript(projectID, agentID, runID, "tool_result", call.Name, result, agentTranscriptAppendMetadata{
		RunID:       runID,
		Kind:        "tool_result",
		ToolCallID:  firstNonEmpty(call.CallID, call.ID),
		ToolName:    call.Name,
		ToolResult:  result,
		ToolSuccess: &succeeded,
	})
}
