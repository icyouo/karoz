package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	agentSessionEventMessageAppended = "message_appended"
	agentSessionEventModelInput      = "model_input_appended"
	agentSessionEventRunStateChanged = "run_state_changed"
	agentSessionEventHandoffChanged  = "handoff_state_changed"
	agentSessionEventApprovalChanged = "approval_changed"
	agentSessionEventCheckpointed    = "session_checkpointed"
)

// AgentSessionEvent is the canonical durable record for model-visible
// conversation facts. Visible chat messages and provider-neutral transcript
// records are projections of this append-only stream, never independent
// persisted sources of truth.
type AgentSessionEvent struct {
	ID         string               `json:"id"`
	Type       string               `json:"type"`
	ProjectID  string               `json:"project_id"`
	AgentID    string               `json:"agent_id"`
	SessionID  string               `json:"session_id"`
	EventSeq   int64                `json:"event_seq"`
	Seq        int64                `json:"seq"`
	CreatedAt  time.Time            `json:"created_at"`
	Message    *AgentMessage        `json:"message,omitempty"`
	Transcript *AgentTranscriptItem `json:"transcript,omitempty"`
	Run        *AgentRun            `json:"run,omitempty"`
	Handoff    *AgentInboxMessage   `json:"handoff,omitempty"`
	Approval   *AgentApprovalEvent  `json:"approval,omitempty"`
	Checkpoint *AgentSessionState   `json:"checkpoint,omitempty"`
	FromState  string               `json:"from_state,omitempty"`
	Reason     string               `json:"reason,omitempty"`
}

// AgentApprovalEvent contains only the identity and outcome of a sensitive
// approval. In particular, it deliberately excludes the command text and
// canonical workdir that remain inside the short-lived approval token.
type AgentApprovalEvent struct {
	ApprovalID    string    `json:"approval_id"`
	Kind          string    `json:"kind"`
	State         string    `json:"state"`
	RequestRunID  string    `json:"request_run_id,omitempty"`
	ResolvedRunID string    `json:"resolved_run_id,omitempty"`
	Operation     string    `json:"operation,omitempty"`
	ProcessID     string    `json:"process_id,omitempty"`
	CommandSHA256 string    `json:"command_sha256,omitempty"`
	ReceiptID     string    `json:"receipt_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
}

func newAgentMonitorProbeApprovalSessionEvent(session AgentSessionState, challenge monitorProbeChallenge, state string) AgentSessionEvent {
	approval := AgentApprovalEvent{
		ApprovalID: challenge.ID, Kind: "monitor_probe", State: strings.TrimSpace(state),
		ResolvedRunID: challenge.ApprovalRunID, Operation: challenge.Language,
		ProcessID: challenge.MonitorID, CommandSHA256: challenge.SourceSHA256,
		ReceiptID: challenge.ConsumedReceiptID, CreatedAt: time.Now().UTC(), ExpiresAt: challenge.ExpiresAt,
	}
	return AgentSessionEvent{
		ID: randomID(), Type: agentSessionEventApprovalChanged,
		ProjectID: challenge.ProjectID, AgentID: challenge.AgentID, SessionID: session.SessionID,
		CreatedAt: time.Now().UTC(), Approval: &approval, Reason: "monitor_probe_" + approval.State,
	}
}

func newAgentCheckpointSessionEvent(state AgentSessionState) AgentSessionEvent {
	createdAt := state.LastCheckpointAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return AgentSessionEvent{
		ID: randomID(), Type: agentSessionEventCheckpointed,
		ProjectID: state.ProjectID, AgentID: state.AgentID, SessionID: state.SessionID,
		CreatedAt: createdAt, Checkpoint: &state, Reason: "checkpoint_committed",
	}
}

func newAgentRunStateSessionEvent(session AgentSessionState, run AgentRun, fromState, reason string) AgentSessionEvent {
	createdAt := run.UpdatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return AgentSessionEvent{
		ID: randomID(), Type: agentSessionEventRunStateChanged,
		ProjectID: run.ProjectID, AgentID: run.AgentID, SessionID: session.SessionID,
		CreatedAt: createdAt, Run: &run, FromState: strings.TrimSpace(fromState), Reason: strings.TrimSpace(reason),
	}
}

func newAgentHandoffSessionEvent(session AgentSessionState, handoff AgentInboxMessage, fromState, reason string) AgentSessionEvent {
	createdAt := handoff.UpdatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return AgentSessionEvent{
		ID: randomID(), Type: agentSessionEventHandoffChanged,
		ProjectID: handoff.ProjectID, AgentID: handoff.TargetAgentID, SessionID: session.SessionID,
		CreatedAt: createdAt, Handoff: &handoff, FromState: strings.TrimSpace(fromState), Reason: strings.TrimSpace(reason),
	}
}

func newAgentBashApprovalSessionEvent(session AgentSessionState, approval ResidentBashApproval, state string) AgentSessionEvent {
	createdAt := time.Now().UTC()
	if !approval.CreatedAt.IsZero() {
		createdAt = approval.CreatedAt
	}
	approvalEvent := AgentApprovalEvent{
		ApprovalID: approval.ID, Kind: "resident_bash", State: strings.TrimSpace(state),
		RequestRunID: approval.RequestRunID, ResolvedRunID: approval.RunID,
		Operation: approval.Subject.Operation, ProcessID: approval.Subject.ProcessID,
		CommandSHA256: approval.Subject.CommandSHA256, CreatedAt: createdAt, ExpiresAt: approval.ExpiresAt,
	}
	return AgentSessionEvent{
		ID: randomID(), Type: agentSessionEventApprovalChanged,
		ProjectID: approval.Subject.ProjectID, AgentID: approval.Subject.AgentID, SessionID: session.SessionID,
		CreatedAt: time.Now().UTC(), Approval: &approvalEvent, Reason: "resident_bash_" + approvalEvent.State,
	}
}

func newAgentMessageSessionEvent(message AgentMessage, metadata agentTranscriptAppendMetadata) AgentSessionEvent {
	transcript := transcriptItemForAgentMessage(message, metadata)
	return AgentSessionEvent{
		ID: message.ID, Type: agentSessionEventMessageAppended,
		ProjectID: message.ProjectID, AgentID: message.AgentID, SessionID: message.SessionID,
		Seq: message.Seq, CreatedAt: message.CreatedAt,
		Message: &message, Transcript: &transcript,
	}
}

func newAgentModelInputSessionEvent(item AgentTranscriptItem) AgentSessionEvent {
	return AgentSessionEvent{
		ID: item.ID, Type: agentSessionEventModelInput,
		ProjectID: item.ProjectID, AgentID: item.AgentID, SessionID: item.SessionID,
		Seq: item.Seq, CreatedAt: item.CreatedAt, Transcript: &item,
	}
}

func (a *app) appendAgentSessionEventLocked(event AgentSessionEvent) {
	if event.EventSeq <= 0 {
		event.EventSeq = a.nextAgentSessionEventSequenceLocked(event.ProjectID, event.AgentID)
	}
	if err := validateAgentSessionEvent(event); err != nil {
		panic("invalid agent session event: " + err.Error())
	}
	store := a.conversationServiceLocked()
	key := projectAgentKey(event.ProjectID, event.AgentID)
	store.Append(key, event)
	a.applyAgentSessionEventProjectionLocked(event)
}

func (a *app) nextAgentSessionEventSequenceLocked(projectID, agentID string) int64 {
	key := projectAgentKey(projectID, agentID)
	var last int64
	for _, event := range a.conversationServiceLocked().EventsFor(key) {
		if event.EventSeq > last {
			last = event.EventSeq
		}
	}
	return last + 1
}

func (a *app) appendAgentRunStateEvent(run AgentRun, fromState, reason string) {
	if strings.TrimSpace(run.ProjectID) == "" || strings.TrimSpace(run.AgentID) == "" || strings.TrimSpace(run.ID) == "" {
		return
	}
	a.mu.Lock()
	session := a.ensureAgentSessionLocked(run.ProjectID, run.AgentID)
	a.appendAgentSessionEventLocked(newAgentRunStateSessionEvent(session, run, fromState, reason))
	a.mu.Unlock()
	if err := a.saveAgentSessionEvents(); err != nil {
		log.Printf("save agent Run event: %v", err)
	}
}

func (a *app) appendAgentHandoffStateEvent(handoff AgentInboxMessage, fromState, reason string) {
	if strings.TrimSpace(handoff.ProjectID) == "" || strings.TrimSpace(handoff.TargetAgentID) == "" || strings.TrimSpace(handoff.ID) == "" {
		return
	}
	a.mu.Lock()
	session := a.ensureAgentSessionLocked(handoff.ProjectID, handoff.TargetAgentID)
	a.appendAgentSessionEventLocked(newAgentHandoffSessionEvent(session, handoff, fromState, reason))
	a.mu.Unlock()
	if err := a.saveAgentSessionEvents(); err != nil {
		log.Printf("save agent handoff event: %v", err)
	}
}

func (a *app) appendAgentBashApprovalEvent(approval ResidentBashApproval, state string) {
	if strings.TrimSpace(approval.ID) == "" || strings.TrimSpace(approval.Subject.ProjectID) == "" || strings.TrimSpace(approval.Subject.AgentID) == "" || strings.TrimSpace(state) == "" {
		return
	}
	a.mu.Lock()
	a.appendAgentBashApprovalEventLocked(approval, state)
	a.mu.Unlock()
	if err := a.saveAgentSessionEvents(); err != nil {
		log.Printf("save resident Bash approval event: %v", err)
	}
}

func (a *app) appendAgentBashApprovalEventLocked(approval ResidentBashApproval, state string) {
	session := a.ensureAgentSessionLocked(approval.Subject.ProjectID, approval.Subject.AgentID)
	a.appendAgentSessionEventLocked(newAgentBashApprovalSessionEvent(session, approval, state))
}

func (a *app) appendAgentMonitorProbeApprovalEventLocked(challenge monitorProbeChallenge, state string) {
	session := a.ensureAgentSessionLocked(challenge.ProjectID, challenge.AgentID)
	a.appendAgentSessionEventLocked(newAgentMonitorProbeApprovalSessionEvent(session, challenge, state))
}

func (a *app) applyAgentSessionEventProjectionLocked(event AgentSessionEvent) {
	key := projectAgentKey(event.ProjectID, event.AgentID)
	conversations := a.conversationServiceLocked()
	if _, exists := conversations.Session(key); !exists {
		conversations.SetSession(key, AgentSessionState{
			SessionID: event.SessionID, ProjectID: event.ProjectID, AgentID: event.AgentID,
			ShortWindowStartSeq: 1, LastCheckpointAt: event.CreatedAt,
		})
	}
	if event.Checkpoint != nil {
		conversations.SetSession(key, *event.Checkpoint)
	}
	if event.Message != nil {
		conversations.AppendMessage(key, *event.Message)
	}
	if event.Transcript != nil {
		a.conversationServiceLocked().AppendTranscript(key, *event.Transcript)
	}
}

func cloneAgentSessionEvent(event AgentSessionEvent) AgentSessionEvent {
	copy := event
	if event.Message != nil {
		message := *event.Message
		copy.Message = &message
	}
	if event.Transcript != nil {
		transcript := *event.Transcript
		if event.Transcript.ToolSuccess != nil {
			success := *event.Transcript.ToolSuccess
			transcript.ToolSuccess = &success
		}
		copy.Transcript = &transcript
	}
	if event.Run != nil {
		run := *event.Run
		copy.Run = &run
	}
	if event.Handoff != nil {
		handoff := *event.Handoff
		handoff.ArtifactIDs = append([]string{}, event.Handoff.ArtifactIDs...)
		copy.Handoff = &handoff
	}
	if event.Approval != nil {
		approval := *event.Approval
		copy.Approval = &approval
	}
	if event.Checkpoint != nil {
		checkpoint := *event.Checkpoint
		copy.Checkpoint = &checkpoint
	}
	return copy
}

func validateAgentSessionEvent(event AgentSessionEvent) error {
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.ProjectID) == "" ||
		strings.TrimSpace(event.AgentID) == "" || strings.TrimSpace(event.SessionID) == "" ||
		event.EventSeq <= 0 || event.CreatedAt.IsZero() {
		return fmt.Errorf("missing event identity")
	}
	if event.Type != agentSessionEventMessageAppended && event.Type != agentSessionEventModelInput && event.Type != agentSessionEventRunStateChanged && event.Type != agentSessionEventHandoffChanged && event.Type != agentSessionEventApprovalChanged && event.Type != agentSessionEventCheckpointed {
		return fmt.Errorf("unsupported event type %q", event.Type)
	}
	if event.Type == agentSessionEventRunStateChanged {
		if event.Seq != 0 || event.Message != nil || event.Transcript != nil || event.Run == nil ||
			event.Run.ID == "" || event.Run.ProjectID != event.ProjectID || event.Run.AgentID != event.AgentID {
			return fmt.Errorf("invalid Run state event")
		}
		return nil
	}
	if event.Type == agentSessionEventHandoffChanged {
		if event.Seq != 0 || event.Message != nil || event.Transcript != nil || event.Run != nil || event.Handoff == nil ||
			event.Handoff.ID == "" || event.Handoff.ProjectID != event.ProjectID || event.Handoff.TargetAgentID != event.AgentID {
			return fmt.Errorf("invalid handoff state event")
		}
		return nil
	}
	if event.Type == agentSessionEventApprovalChanged {
		if event.Seq != 0 || event.Message != nil || event.Transcript != nil || event.Run != nil || event.Handoff != nil || event.Approval == nil ||
			event.Approval.ApprovalID == "" || (event.Approval.Kind != "resident_bash" && event.Approval.Kind != "monitor_probe") || event.Approval.State == "" || event.Approval.CreatedAt.IsZero() {
			return fmt.Errorf("invalid approval event")
		}
		return nil
	}
	if event.Type == agentSessionEventCheckpointed {
		if event.Seq != 0 || event.Message != nil || event.Transcript != nil || event.Run != nil || event.Handoff != nil || event.Approval != nil || event.Checkpoint == nil ||
			event.Checkpoint.ProjectID != event.ProjectID || event.Checkpoint.AgentID != event.AgentID || event.Checkpoint.SessionID != event.SessionID ||
			event.Checkpoint.ShortWindowStartSeq <= 0 || event.Checkpoint.CoveredSeqEnd < event.Checkpoint.BoundarySeq {
			return fmt.Errorf("invalid checkpoint event")
		}
		return nil
	}
	if event.Seq <= 0 {
		return fmt.Errorf("conversation event requires sequence")
	}
	if event.Type == agentSessionEventMessageAppended {
		if event.Message == nil || event.Transcript == nil {
			return fmt.Errorf("message event requires message and transcript")
		}
		if err := validateAgentSessionEventMessage(event, *event.Message); err != nil {
			return err
		}
		if err := validateAgentSessionEventTranscript(event, *event.Transcript); err != nil {
			return err
		}
		if event.Transcript.MessageID != event.Message.ID || event.Transcript.ModelOnly || !event.Transcript.Visible {
			return fmt.Errorf("message event transcript does not match visible message")
		}
		return nil
	}
	if event.Message != nil || event.Transcript == nil {
		return fmt.Errorf("model input event requires transcript only")
	}
	if err := validateAgentSessionEventTranscript(event, *event.Transcript); err != nil {
		return err
	}
	if !event.Transcript.ModelOnly || event.Transcript.Visible || event.Transcript.Role != "user" {
		return fmt.Errorf("model input event transcript is not hidden user input")
	}
	return nil
}

func validateAgentSessionEventMessage(event AgentSessionEvent, message AgentMessage) error {
	if message.ID != event.ID || message.ProjectID != event.ProjectID || message.AgentID != event.AgentID ||
		message.SessionID != event.SessionID || message.Seq != event.Seq || !message.CreatedAt.Equal(event.CreatedAt) {
		return fmt.Errorf("message identity does not match event")
	}
	return nil
}

func validateAgentSessionEventTranscript(event AgentSessionEvent, transcript AgentTranscriptItem) error {
	if transcript.ID != event.ID || transcript.ProjectID != event.ProjectID || transcript.AgentID != event.AgentID ||
		transcript.SessionID != event.SessionID || transcript.Seq != event.Seq || !transcript.CreatedAt.Equal(event.CreatedAt) {
		return fmt.Errorf("transcript identity does not match event")
	}
	return nil
}

func (a *app) loadAgentSessionEvents() error {
	if err := a.rejectSupersededConversationFiles(); err != nil {
		return err
	}
	loaded := map[string][]AgentSessionEvent{}
	found, err := a.loadJSON("agent-session-events.json", &loaded)
	if err != nil {
		return err
	}
	a.conversationServiceLocked().Reset()
	a.conversationServiceLocked().ResetMessages()
	a.conversationServiceLocked().ResetTranscripts()
	a.conversationServiceLocked().ResetSessions()
	if !found {
		return nil
	}
	for key, events := range loaded {
		var previousEventSeq, previousConversationSeq int64
		for _, event := range events {
			if projectAgentKey(event.ProjectID, event.AgentID) != key {
				return fmt.Errorf("agent session event key mismatch for %s", event.ID)
			}
			if err := validateAgentSessionEvent(event); err != nil {
				return fmt.Errorf("invalid agent session event %s: %w", event.ID, err)
			}
			if event.EventSeq <= previousEventSeq {
				return fmt.Errorf("non-append agent session event order for %s", key)
			}
			previousEventSeq = event.EventSeq
			if event.Seq > 0 {
				if event.Seq <= previousConversationSeq {
					return fmt.Errorf("non-append conversation sequence for %s", key)
				}
				previousConversationSeq = event.Seq
			}
			a.appendAgentSessionEventLocked(event)
		}
	}
	return nil
}

// rejectSupersededConversationFiles makes the event-log cutover explicit.
// Reading those files here would restore a second source of truth; ignoring
// them would silently hide a user's chat history. A release migration, if one
// is ever offered, must be an explicit one-time command rather than a runtime
// fallback.
func (a *app) rejectSupersededConversationFiles() error {
	for _, name := range []string{"agent-messages.json", "agent-transcripts.json", "agent-session-state.json", "agent-archive-messages.json"} {
		_, err := os.Stat(filepath.Join(a.settings.DataDir, name))
		if err == nil {
			return fmt.Errorf("unsupported conversation storage %s: expected agent-session-events.json", name)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (a *app) saveAgentSessionEvents() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveAgentSessionEventsLocked()
}

func (a *app) saveAgentSessionEventsLocked() error {
	return a.saveJSON("agent-session-events.json", a.conversationServiceLocked().Snapshot(), 0644)
}
