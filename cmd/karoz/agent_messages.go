package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	processdomain "github.com/karoz/karoz/internal/process"
)

func (a *app) agentMessagesFor(projectID, agentID string) []AgentMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.conversationServiceLocked().MessagesFor(projectAgentKey(projectID, agentID))
	return out
}

func (a *app) agentMessagesForDisplay(projectID, agentID string) []AgentMessage {
	out := a.agentMessagesFor(projectID, agentID)
	for i := range out {
		if out[i].Role == "tool_result" {
			out[i].Body = compactToolResultForDisplay(out[i].Intent, out[i].Body)
		} else if out[i].Role == "tool_call" {
			out[i].Body = limitString(out[i].Body, 2000)
		}
	}
	if out == nil {
		return []AgentMessage{}
	}
	return out
}

func (a *app) agentMessagesPageForDisplay(projectID, agentID string, beforeSeq int64, limit int) AgentMessagesPage {
	if limit <= 0 {
		limit = 80
	}
	if limit > 200 {
		limit = 200
	}
	messages := a.agentMessagesForDisplay(projectID, agentID)
	if beforeSeq > 0 {
		filtered := messages[:0]
		for _, msg := range messages {
			if msg.Seq > 0 && msg.Seq < beforeSeq {
				filtered = append(filtered, msg)
			}
		}
		messages = filtered
	}
	hasMore := false
	if len(messages) > limit {
		hasMore = true
		messages = messages[len(messages)-limit:]
	}
	page := AgentMessagesPage{Messages: messages, HasMore: hasMore, ModelContext: a.modelContextForCounter(projectID, agentID)}
	if hasMore && len(messages) > 0 {
		page.NextBeforeSeq = messages[0].Seq
	}
	if page.Messages == nil {
		page.Messages = []AgentMessage{}
	}
	if page.ModelContext == nil {
		page.ModelContext = []AgentContextMessage{}
	}
	return page
}

// agentTranscriptDeltaForModel centralizes the session boundary used by the
// resident prompt and local context meter. Seq zero remains eligible for
// legacy records that predate durable sequencing.
func (a *app) agentTranscriptDeltaForModel(projectID, agentID string) []AgentTranscriptItem {
	state := a.agentSessionState(projectID, agentID)
	transcript := a.agentTranscriptForModel(projectID, agentID)
	delta := make([]AgentTranscriptItem, 0, len(transcript))
	for _, item := range transcript {
		if item.Seq >= state.ShortWindowStartSeq || item.Seq == 0 {
			delta = append(delta, item)
		}
	}
	return delta
}

// modelContextForCounter returns a bounded, normalized server projection of
// the actual resident transcript window. It includes hidden scheduled inputs
// only in that projection; regular chat messages remain the visible API.
func (a *app) modelContextForCounter(projectID, agentID string) []AgentContextMessage {
	items := compactTranscriptForContextCounter(a.agentTranscriptDeltaForModel(projectID, agentID))
	if items == nil {
		return []AgentContextMessage{}
	}
	return items
}

func (a *app) appendAgentMessage(projectID, agentID, role, intent, body string) AgentMessage {
	a.mu.Lock()
	msg := a.appendAgentMessageLocked(projectID, agentID, role, intent, body)
	a.mu.Unlock()
	a.persistAppendedAgentMessage(projectID, agentID)
	return msg
}

// admitProcessTerminalMessage puts the process outbox receipt in the existing
// durable agent message stream. The process event ID is the receipt ID: a
// retry must find the identical payload before its terminal reservation can be
// released. Holding backgroundOwnerMu keeps owner deletion and target choice
// ordered without another registry or generation store.
func (a *app) admitProcessTerminalMessage(event RuntimeEvent) (bool, error) {
	if err := validateProcessRuntimeEvent(event); err != nil {
		return false, err
	}
	a.agentRuntimeLocked().backgroundOwnerMu.Lock()
	defer a.agentRuntimeLocked().backgroundOwnerMu.Unlock()

	a.mu.Lock()
	defer a.mu.Unlock()
	targetID := a.processTerminalMessageTargetLocked(event.ProjectID, event.AgentID)
	expected := AgentMessage{
		ID:        event.ID,
		ProjectID: event.ProjectID,
		AgentID:   targetID,
		SessionID: residentSessionID(event.ProjectID, targetID),
		Role:      "system",
		Intent:    "process_terminal",
		Body:      processTerminalMessageBody(event),
		CreatedAt: event.CreatedAt,
	}
	for key, messages := range a.conversationServiceLocked().MessageSnapshot() {
		if !strings.HasPrefix(key, event.ProjectID+"/") {
			continue
		}
		for _, message := range messages {
			if message.ID != event.ID {
				continue
			}
			// The first durable admission fixes the recipient (owner or Karoz).
			// A later retry may observe an owner recreation, so compare against
			// that durable recipient instead of routing the same event again.
			expected.AgentID = message.AgentID
			expected.SessionID = message.SessionID
			if !sameProcessTerminalMessage(message, expected) {
				return false, fmt.Errorf("process terminal message identity collision")
			}
			return false, nil
		}
	}
	key := projectAgentKey(event.ProjectID, targetID)
	previousEvents := a.conversationServiceLocked().EventsFor(key)
	previousMessages := a.conversationServiceLocked().MessagesFor(key)
	previousTranscripts := a.conversationServiceLocked().TranscriptsFor(key)
	expected.Seq = a.nextAgentTranscriptSequenceLocked(event.ProjectID, targetID)
	a.appendAgentSessionEventLocked(newAgentMessageSessionEvent(expected, agentTranscriptAppendMetadata{}))
	if err := a.saveAgentSessionEventsLocked(); err != nil {
		a.conversationServiceLocked().Replace(key, previousEvents)
		a.conversationServiceLocked().ReplaceMessages(key, previousMessages)
		a.conversationServiceLocked().ReplaceTranscripts(key, previousTranscripts)
		return false, err
	}
	if err := a.processRuntimePersistenceFail(processPersistAfterTerminalMessage); err != nil {
		return true, err
	}
	return true, nil
}

func (a *app) processTerminalMessageTargetLocked(projectID, ownerID string) string {
	key := projectAgentKey(projectID, ownerID)
	if !a.agentRuntimeLocked().backgroundOwnerDeleting[key] {
		for _, agent := range a.agentDirectoryLocked().agents[projectID] {
			if agent.ID == ownerID {
				return ownerID
			}
		}
	}
	return "karoz"
}

func sameProcessTerminalMessage(left, right AgentMessage) bool {
	return left.ID == right.ID &&
		left.ProjectID == right.ProjectID &&
		left.AgentID == right.AgentID &&
		left.SessionID == right.SessionID &&
		left.Role == right.Role &&
		left.Intent == right.Intent &&
		left.Body == right.Body &&
		left.CreatedAt.Equal(right.CreatedAt)
}

func processTerminalMessageBody(event RuntimeEvent) string {
	exitCode := 0
	if event.ExitCode != nil {
		exitCode = *event.ExitCode
	}
	return fmt.Sprintf(
		"Background process %s reached %s (exit code %d; run %s).",
		event.EntityID,
		event.To,
		exitCode,
		firstNonEmpty(event.RunID, "unknown"),
	)
}

func validateProcessRuntimeEvent(event RuntimeEvent) error {
	if event.ID != processTerminalEventID(event.EntityID) ||
		strings.TrimSpace(event.ProjectID) == "" ||
		strings.TrimSpace(event.AgentID) == "" ||
		!safeProcessID(event.EntityID) ||
		event.Kind != processTerminalEventKind ||
		event.Reason != "process_terminal" ||
		!processdomain.State(event.To).Terminal() ||
		event.ExitCode == nil ||
		event.CreatedAt.IsZero() {
		return fmt.Errorf("invalid process runtime event")
	}
	return nil
}

func (a *app) appendAgentMessageForRun(projectID, agentID, runID, role, intent, body string) (AgentMessage, bool) {
	return a.appendAgentMessageForRunWithTranscript(projectID, agentID, runID, role, intent, body, agentTranscriptAppendMetadata{RunID: runID})
}

func (a *app) appendAgentMessageForRunWithTranscript(projectID, agentID, runID, role, intent, body string, metadata agentTranscriptAppendMetadata) (AgentMessage, bool) {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	runtime := a.agentRuntimeLocked()
	run, ok := runtime.runs[key]
	if !ok || !run.State.Active() || strings.TrimSpace(runID) == "" || run.ID != runID || runtime.cancelling[key] == runID {
		a.mu.Unlock()
		return AgentMessage{}, false
	}
	metadata.RunID = runID
	msg := a.appendAgentMessageLockedWithTranscript(projectID, agentID, role, intent, body, metadata)
	a.mu.Unlock()
	a.persistAppendedAgentMessage(projectID, agentID)
	return msg, true
}

// latestMatchingAgentMessage returns the durable record just written by a
// streaming callback. Run-scoped tool callbacks persist before publishing
// their ledger event, so including this identity lets reconnecting browsers
// reconcile a replay event with an already-loaded history card.
func (a *app) latestMatchingAgentMessage(projectID, agentID, role, intent, body string) (AgentMessage, bool) {
	key := projectAgentKey(projectID, agentID)
	wantBody := strings.TrimSpace(body)
	a.mu.Lock()
	defer a.mu.Unlock()
	items := a.conversationServiceLocked().MessagesFor(key)
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		if item.Role == role && item.Intent == intent && item.Body == wantBody {
			return item, true
		}
	}
	return AgentMessage{}, false
}

func (a *app) appendAgentMessageLocked(projectID, agentID, role, intent, body string) AgentMessage {
	return a.appendAgentMessageLockedWithTranscript(projectID, agentID, role, intent, body, agentTranscriptAppendMetadata{})
}

func (a *app) appendAgentMessageLockedWithTranscript(projectID, agentID, role, intent, body string, metadata agentTranscriptAppendMetadata) AgentMessage {
	session := a.ensureAgentSessionLocked(projectID, agentID)
	msg := AgentMessage{
		ID:        messageID(),
		ProjectID: projectID,
		AgentID:   agentID,
		SessionID: session.SessionID,
		Seq:       a.nextAgentTranscriptSequenceLocked(projectID, agentID),
		Role:      role,
		Intent:    firstNonEmpty(intent, "note"),
		Body:      strings.TrimSpace(body),
		CreatedAt: time.Now().UTC(),
	}
	a.appendAgentSessionEventLocked(newAgentMessageSessionEvent(msg, metadata))
	return msg
}

func (a *app) persistAppendedAgentMessage(projectID, agentID string) {
	if err := a.saveAgentSessionEvents(); err != nil {
		log.Printf("save agent session events: %v", err)
		return
	}
	a.maybeCheckpointAgentSession(projectID, agentID, false)
}

func (a *app) ensureAgentSession(projectID, agentID string) AgentSessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ensureAgentSessionLocked(projectID, agentID)
}

func (a *app) ensureAgentSessionLocked(projectID, agentID string) AgentSessionState {
	key := projectAgentKey(projectID, agentID)
	conversations := a.conversationServiceLocked()
	if state, ok := conversations.Session(key); ok && strings.TrimSpace(state.SessionID) != "" {
		return state
	}
	state := AgentSessionState{
		SessionID:           residentSessionID(projectID, agentID),
		ProjectID:           projectID,
		AgentID:             agentID,
		ShortWindowStartSeq: 1,
		LastCheckpointAt:    time.Now().UTC(),
	}
	conversations.SetSession(key, state)
	return state
}

func (a *app) agentSessionState(projectID, agentID string) AgentSessionState {
	return a.ensureAgentSession(projectID, agentID)
}

func (a *app) updateAgentSessionState(state AgentSessionState) {
	a.mu.Lock()
	a.conversationServiceLocked().SetSession(projectAgentKey(state.ProjectID, state.AgentID), state)
	a.appendAgentSessionEventLocked(newAgentCheckpointSessionEvent(state))
	a.mu.Unlock()
	if err := a.saveAgentSessionEvents(); err != nil {
		log.Printf("save agent session event: %v", err)
	}
}

func (a *app) maybeCheckpointAgentSession(projectID, agentID string, force bool) {
	const shortWindowLimit int64 = 50
	const summaryLimit = 24
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	state, stateExists := a.conversationServiceLocked().Session(key)
	if !stateExists || strings.TrimSpace(state.SessionID) == "" {
		state = AgentSessionState{
			SessionID:           residentSessionID(projectID, agentID),
			ProjectID:           projectID,
			AgentID:             agentID,
			ShortWindowStartSeq: 1,
		}
	}
	messages := a.conversationServiceLocked().MessagesFor(key)
	if len(messages) == 0 {
		a.mu.Unlock()
		return
	}
	maxSeq := messages[len(messages)-1].Seq
	if maxSeq == 0 {
		for i := range messages {
			messages[i].Seq = int64(i + 1)
		}
		maxSeq = messages[len(messages)-1].Seq
	}
	if !force && maxSeq-state.ShortWindowStartSeq+1 <= shortWindowLimit {
		a.mu.Unlock()
		return
	}
	boundarySeq := maxSeq - shortWindowLimit
	if boundarySeq <= state.BoundarySeq {
		a.mu.Unlock()
		return
	}
	nextSeq := state.CoveredSeqEnd + 1
	if nextSeq <= 0 {
		nextSeq = 1
	}
	var batch []AgentMessage
	for _, msg := range messages {
		if strings.TrimSpace(msg.SessionID) != "" && msg.SessionID != state.SessionID {
			continue
		}
		if msg.Seq >= nextSeq && msg.Seq <= boundarySeq {
			batch = append(batch, msg)
			if len(batch) >= summaryLimit {
				break
			}
		}
	}
	if len(batch) == 0 {
		a.mu.Unlock()
		return
	}
	claim := agentCheckpointClaim{
		ProjectID:             projectID,
		AgentID:               agentID,
		SessionID:             state.SessionID,
		CapturedVersion:       state.LongTermVersion,
		CapturedCoveredSeqEnd: state.CoveredSeqEnd,
		SeqStart:              batch[0].Seq,
		SeqEnd:                batch[len(batch)-1].Seq,
		PreviousSummary:       normalizeResidentSummary(state.ResidentSummary, checkpointPreviousSummaryMaxChars),
		Messages:              append([]AgentMessage{}, batch...),
		Agent:                 a.checkpointAgentConfigLocked(projectID, agentID),
	}
	claimKey := checkpointClaimKey(claim)
	runtime := a.agentRuntimeLocked()
	if runtime.checkpointClaims == nil {
		runtime.checkpointClaims = map[string]agentCheckpointClaim{}
	}
	if runtime.checkpointRetryNotBefore == nil {
		runtime.checkpointRetryNotBefore = map[string]time.Time{}
	}
	if _, active := runtime.checkpointClaims[claimKey]; active {
		a.mu.Unlock()
		return
	}
	if retryAt := runtime.checkpointRetryNotBefore[claimKey]; time.Now().Before(retryAt) {
		a.mu.Unlock()
		return
	}
	runtime.checkpointClaims[claimKey] = claim
	a.mu.Unlock()

	go a.runAgentCheckpoint(claim)
}

const (
	checkpointPreviousSummaryMaxChars = 2400
	checkpointMessageBodyMaxChars     = 1000
	checkpointPromptMaxChars          = 14000
	checkpointOutputMaxChars          = 6000
	checkpointDefaultTimeout          = 30 * time.Second
	checkpointDefaultRetryDelay       = 500 * time.Millisecond
)

type agentCheckpointClaim struct {
	ProjectID             string
	AgentID               string
	SessionID             string
	CapturedVersion       int64
	CapturedCoveredSeqEnd int64
	SeqStart              int64
	SeqEnd                int64
	PreviousSummary       string
	Messages              []AgentMessage
	Agent                 Agent
}

func checkpointClaimKey(claim agentCheckpointClaim) string {
	return projectAgentKey(claim.ProjectID, claim.AgentID) + "/" + claim.SessionID
}

func sameCheckpointClaim(left, right agentCheckpointClaim) bool {
	return left.ProjectID == right.ProjectID &&
		left.AgentID == right.AgentID &&
		left.SessionID == right.SessionID &&
		left.CapturedVersion == right.CapturedVersion &&
		left.CapturedCoveredSeqEnd == right.CapturedCoveredSeqEnd &&
		left.SeqStart == right.SeqStart &&
		left.SeqEnd == right.SeqEnd
}

func (a *app) checkpointAgentConfigLocked(projectID, agentID string) Agent {
	for _, candidate := range a.agentDirectoryLocked().agents[projectID] {
		if candidate.ID == agentID {
			return normalizeAgentModelConfig(candidate)
		}
	}
	return normalizeAgentModelConfig(Agent{ID: agentID, ProjectID: projectID})
}

func (a *app) runAgentCheckpoint(claim agentCheckpointClaim) {
	started := time.Now()
	base := a.supervisorCtx
	if base == nil {
		base = context.Background()
	}
	timeout := a.checkpointTimeout
	if timeout <= 0 {
		timeout = checkpointDefaultTimeout
	}
	ctx, cancel := context.WithTimeout(base, timeout)
	defer cancel()

	var output strings.Builder
	var outputMu sync.Mutex
	outputChars := 0
	request := CLI2APIRequest{
		Provider:       claim.Agent.Provider,
		Model:          claim.Agent.Model,
		ThinkingEffort: claim.Agent.ThinkingEffort,
		Prompt:         buildAgentCheckpointPrompt(claim),
		Workdir:        a.settings.ProjectsRoot,
		Mode:           "checkpoint",
		NoTools:        true,
	}
	// The provider request is bounded by ctx. Independently cap streamed text
	// at the collector boundary so no provider adapter can enlarge persisted
	// checkpoint state.
	callbacks := AgentStreamCallbacks{OnDelta: func(delta string) {
		outputMu.Lock()
		defer outputMu.Unlock()
		remaining := checkpointOutputMaxChars - outputChars
		if remaining <= 0 {
			return
		}
		runes := []rune(delta)
		if len(runes) > remaining {
			runes = runes[:remaining]
		}
		output.WriteString(string(runes))
		outputChars += len(runes)
	}}
	provider := a.residentModelProvider()
	if capabilities := provider.Capabilities(request); !capabilities.Streaming {
		a.releaseAgentCheckpoint(claim, true)
		logCheckpointResult(claim, started, "unsupported_provider")
		return
	}
	err := provider.Stream(ctx, request, ResidentToolContext{
		Agent: claim.Agent, Workdir: request.Workdir, TurnType: "checkpoint", EnforcePolicy: true,
	}, callbacks)
	if err != nil {
		status := "provider_error"
		if ctx.Err() == context.DeadlineExceeded {
			status = "timeout"
		} else if ctx.Err() != nil {
			status = "cancelled"
		}
		a.releaseAgentCheckpoint(claim, true)
		logCheckpointResult(claim, started, status)
		return
	}
	if ctx.Err() != nil {
		status := "cancelled"
		if ctx.Err() == context.DeadlineExceeded {
			status = "timeout"
		}
		a.releaseAgentCheckpoint(claim, true)
		logCheckpointResult(claim, started, status)
		return
	}
	outputMu.Lock()
	summary := normalizeResidentSummary(output.String(), checkpointOutputMaxChars)
	outputMu.Unlock()
	if summary == "" {
		a.releaseAgentCheckpoint(claim, true)
		logCheckpointResult(claim, started, "empty")
		return
	}
	if isCheckpointProviderBoilerplate(summary) {
		a.releaseAgentCheckpoint(claim, true)
		logCheckpointResult(claim, started, "provider_boilerplate")
		return
	}
	status := a.commitAgentCheckpoint(claim, summary)
	logCheckpointResult(claim, started, status)
	if status == "success" {
		a.maybeCheckpointAgentSession(claim.ProjectID, claim.AgentID, false)
	}
}

// isCheckpointProviderBoilerplate rejects only short, operational provider
// setup responses. It deliberately avoids broad words such as "provider" or
// "unavailable", which may be legitimate facts in a continuity summary.
func isCheckpointProviderBoilerplate(output string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(output), " "))
	if normalized == "" || len(normalized) > 512 {
		return false
	}
	if strings.HasPrefix(normalized, "karoz received the request.") &&
		strings.Contains(normalized, "cli2api is running in stub mode") &&
		strings.Contains(normalized, "set karoz_agent_provider=") {
		return true
	}
	switch strings.TrimRight(normalized, ".; ") {
	case "codex oauth credentials were not found",
		"claude cli is not logged in and anthropic_api_key is not configured":
		return true
	}
	for _, prefix := range []string{
		"provider unavailable;",
		"provider unavailable:",
		"provider unavailable.",
		"provider is unavailable;",
		"provider is unavailable:",
		"provider is unavailable.",
		"selected provider unavailable;",
		"selected provider unavailable:",
		"selected provider unavailable.",
		"selected provider is unavailable;",
		"selected provider is unavailable:",
		"selected provider is unavailable.",
		"provider not configured;",
		"provider not configured:",
		"provider not configured.",
		"provider is not configured;",
		"provider is not configured:",
		"provider is not configured.",
		"no provider configured;",
		"no provider configured:",
		"no provider configured.",
		"no provider is configured;",
		"no provider is configured:",
		"no provider is configured.",
	} {
		if !strings.HasPrefix(normalized, prefix) {
			continue
		}
		remainder := strings.TrimSpace(strings.TrimPrefix(normalized, prefix))
		return strings.HasPrefix(remainder, "configure ") ||
			strings.HasPrefix(remainder, "please configure ") ||
			strings.HasPrefix(remainder, "set ")
	}
	return false
}

func buildAgentCheckpointPrompt(claim agentCheckpointClaim) string {
	var b strings.Builder
	b.WriteString("Checkpoint mode. Return only a compact factual continuity summary for the next model turn.\n")
	b.WriteString("Capture decisions, durable facts, completed work, pending work, and unresolved questions.\n")
	b.WriteString("Preserve uncertainty explicitly. Do not infer or invent facts. Do not request or call tools.\n")
	if claim.PreviousSummary != "" {
		b.WriteString("\nPrevious checkpoint summary:\n")
		b.WriteString(limitString(claim.PreviousSummary, checkpointPreviousSummaryMaxChars))
		b.WriteString("\n")
	}
	b.WriteString("\nExact archived message batch:\n")
	for _, msg := range claim.Messages {
		body := strings.Join(strings.Fields(promptAgentMessageBody(msg)), " ")
		if body == "" {
			continue
		}
		line := fmt.Sprintf("seq=%d role=%s intent=%s body=%s\n",
			msg.Seq, strings.TrimSpace(msg.Role), strings.TrimSpace(msg.Intent),
			limitString(body, checkpointMessageBodyMaxChars))
		remaining := checkpointPromptMaxChars - b.Len()
		if remaining <= 0 {
			break
		}
		b.WriteString(limitString(line, remaining))
	}
	return limitString(b.String(), checkpointPromptMaxChars)
}

func (a *app) releaseAgentCheckpoint(claim agentCheckpointClaim, retry bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := checkpointClaimKey(claim)
	if current, ok := a.agentRuntimeLocked().checkpointClaims[key]; ok && sameCheckpointClaim(current, claim) {
		delete(a.agentRuntimeLocked().checkpointClaims, key)
		if retry {
			a.deferCheckpointRetryLocked(key)
		}
	}
}

func (a *app) commitAgentCheckpoint(claim agentCheckpointClaim, summary string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	claimKey := checkpointClaimKey(claim)
	runtime := a.agentRuntimeLocked()
	active, ok := runtime.checkpointClaims[claimKey]
	if !ok || !sameCheckpointClaim(active, claim) {
		return "stale"
	}
	defer delete(runtime.checkpointClaims, claimKey)

	key := projectAgentKey(claim.ProjectID, claim.AgentID)
	current, exists := a.conversationServiceLocked().Session(key)
	if !exists || strings.TrimSpace(current.SessionID) == "" {
		current = AgentSessionState{
			SessionID:           residentSessionID(claim.ProjectID, claim.AgentID),
			ProjectID:           claim.ProjectID,
			AgentID:             claim.AgentID,
			ShortWindowStartSeq: 1,
		}
	}
	if current.SessionID != claim.SessionID ||
		current.LongTermVersion != claim.CapturedVersion ||
		current.CoveredSeqEnd != claim.CapturedCoveredSeqEnd {
		return "stale"
	}
	nextState := current
	nextState.ResidentSummary = summary
	if nextState.CoveredSeqStart <= 0 {
		nextState.CoveredSeqStart = claim.SeqStart
	}
	nextState.CoveredSeqEnd = claim.SeqEnd
	nextState.BoundarySeq = claim.SeqEnd
	nextState.ShortWindowStartSeq = claim.SeqEnd + 1
	nextState.LongTermVersion++
	nextState.LastCheckpointAt = time.Now().UTC()

	nextSessions := a.conversationServiceLocked().SessionSnapshot()
	nextSessions[key] = nextState
	if err := a.saveCheckpointSessionsSnapshot(nextSessions); err != nil {
		a.deferCheckpointRetryLocked(claimKey)
		return "save_failed"
	}
	previousEvents := a.conversationServiceLocked().EventsFor(key)
	a.appendAgentSessionEventLocked(newAgentCheckpointSessionEvent(nextState))
	if err := a.saveAgentSessionEventsLocked(); err != nil {
		a.conversationServiceLocked().Replace(key, previousEvents)
		a.deferCheckpointRetryLocked(claimKey)
		return "save_failed"
	}
	a.conversationServiceLocked().ReplaceSessions(nextSessions)
	delete(runtime.checkpointRetryNotBefore, claimKey)
	return "success"
}

func (a *app) deferCheckpointRetryLocked(claimKey string) {
	runtime := a.agentRuntimeLocked()
	if runtime.checkpointRetryNotBefore == nil {
		runtime.checkpointRetryNotBefore = map[string]time.Time{}
	}
	delay := a.checkpointRetryDelay
	if delay <= 0 {
		delay = checkpointDefaultRetryDelay
	}
	runtime.checkpointRetryNotBefore[claimKey] = time.Now().Add(delay)
}

func cloneAgentSessions(source map[string]AgentSessionState) map[string]AgentSessionState {
	cloned := make(map[string]AgentSessionState, len(source)+1)
	for key, state := range source {
		cloned[key] = state
	}
	return cloned
}

func (a *app) saveCheckpointSessionsSnapshot(snapshot map[string]AgentSessionState) error {
	if save := a.conversationServiceLocked().checkpointSessionSaveOverride; save != nil {
		return save(snapshot)
	}
	return nil
}

func logCheckpointResult(claim agentCheckpointClaim, started time.Time, status string) {
	log.Printf(
		"agent checkpoint project=%s agent=%s session=%s seq_start=%d seq_end=%d count=%d duration_ms=%d status=%s",
		claim.ProjectID, claim.AgentID, claim.SessionID, claim.SeqStart, claim.SeqEnd,
		len(claim.Messages), time.Since(started).Milliseconds(), status,
	)
}

func normalizeResidentSummary(value string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = 6000
	}
	lines := strings.Split(strings.TrimSpace(value), "\n")
	seen := map[string]bool{}
	keptReversed := make([]string, 0, len(lines))
	used := 0
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		lower := strings.ToLower(line)
		if line == "" || strings.HasPrefix(lower, "previous rolling summary:") ||
			strings.HasPrefix(lower, "earlier checkpoint highlights:") ||
			strings.HasPrefix(lower, "recent checkpoint highlights:") ||
			strings.HasPrefix(lower, "rolling summary:") || lower == "previ..." {
			continue
		}
		if seen[line] {
			continue
		}
		cost := len(line) + 1
		if len(keptReversed) > 0 && used+cost > maxChars {
			break
		}
		seen[line] = true
		keptReversed = append(keptReversed, line)
		used += cost
	}
	kept := make([]string, len(keptReversed))
	for i := range keptReversed {
		kept[len(keptReversed)-1-i] = keptReversed[i]
	}
	return strings.Join(kept, "\n")
}

func emptyAgentOutputMessage(agent Agent) string {
	name := firstNonEmpty(agent.DisplayName, agent.Nickname, agent.Name, agent.ID, "Agent")
	return name + " did not return any visible text. Check the runtime logs or retry the request."
}

type agentPromptLine struct {
	Role string
	Body string
}

func renderAgentPromptDelta(messages []AgentMessage, maxItems, maxChars int) []agentPromptLine {
	if maxItems <= 0 {
		maxItems = 50
	}
	if maxChars <= 0 {
		maxChars = 24000
	}
	var reversed []agentPromptLine
	used := 0
	for i := len(messages) - 1; i >= 0; i-- {
		body := promptAgentMessageBody(messages[i])
		if body == "" {
			continue
		}
		lineCost := len(messages[i].Role) + len(body) + 4
		if len(reversed) > 0 && used+lineCost > maxChars {
			break
		}
		reversed = append(reversed, agentPromptLine{Role: messages[i].Role, Body: body})
		used += lineCost
		if len(reversed) >= maxItems {
			break
		}
	}
	lines := make([]agentPromptLine, len(reversed))
	for i := range reversed {
		lines[len(reversed)-1-i] = reversed[i]
	}
	return lines
}

func promptAgentMessageBody(msg AgentMessage) string {
	body := strings.TrimSpace(msg.Body)
	if body == "" {
		return ""
	}
	switch msg.Role {
	case "tool_result":
		return compactToolResultForPrompt(body)
	case "tool_call":
		return limitString(body, 1400)
	case "assistant":
		return limitString(body, residentTranscriptMessageMaxChars)
	case "user":
		return limitString(body, residentTranscriptMessageMaxChars)
	default:
		return limitString(body, 2400)
	}
}

func compactToolResultForPrompt(body string) string {
	var decoded any
	if err := json.Unmarshal([]byte(body), &decoded); err == nil {
		scrubbed := scrubToolPromptValue("", decoded)
		if data, err := json.Marshal(scrubbed); err == nil {
			return limitString(string(data), 3600)
		}
	}
	return limitString(body, 2200)
}

func compactToolResultForDisplay(toolName, body string) string {
	if strings.EqualFold(strings.TrimSpace(toolName), "request_choice") || toolResultIsChoiceRequest(body) {
		return body
	}
	return compactToolResultForPrompt(body)
}

func toolResultIsChoiceRequest(body string) bool {
	var decoded struct {
		Kind string `json:"kind"`
	}
	return json.Unmarshal([]byte(body), &decoded) == nil && decoded.Kind == "choice_request"
}

func scrubToolPromptValue(key string, value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, v := range typed {
			out[k] = scrubToolPromptValue(k, v)
		}
		return out
	case []any:
		limit := len(typed)
		if limit > 12 {
			limit = 12
		}
		out := make([]any, 0, limit+1)
		for i := 0; i < limit; i++ {
			out = append(out, scrubToolPromptValue(key, typed[i]))
		}
		if len(typed) > limit {
			out = append(out, fmt.Sprintf("[omitted %d additional items]", len(typed)-limit))
		}
		return out
	case string:
		return scrubToolPromptString(key, typed)
	default:
		return typed
	}
}

func scrubToolPromptString(key, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	lowerKey := strings.ToLower(strings.TrimSpace(key))
	if lowerKey == "data" && len(value) > 200 {
		return fmt.Sprintf("[omitted %d chars of data]", len(value))
	}
	if strings.HasPrefix(value, "data:image/") && len(value) > 200 {
		return fmt.Sprintf("[omitted %d chars of image data URL]", len(value))
	}
	if looksLikeLargeBase64(value) {
		return fmt.Sprintf("[omitted %d chars of base64 data]", len(value))
	}
	switch lowerKey {
	case "stdout":
		return limitString(value, 2200)
	case "stderr":
		return limitString(value, 1200)
	case "result", "output", "body", "text":
		return limitString(value, 2600)
	default:
		return limitString(value, 1200)
	}
}

func looksLikeLargeBase64(value string) bool {
	if len(value) < 800 {
		return false
	}
	sample := value
	if len(sample) > 1200 {
		sample = sample[:1200]
	}
	matches := 0
	for _, r := range sample {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=' {
			matches++
		}
	}
	return matches*100/len(sample) > 95
}
