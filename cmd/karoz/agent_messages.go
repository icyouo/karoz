package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	processdomain "github.com/karoz/karoz/internal/process"
)

func (a *app) agentMessagesFor(projectID, agentID string) []AgentMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]AgentMessage{}, a.agentMessages[projectAgentKey(projectID, agentID)]...)
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
	a.backgroundOwnerMu.Lock()
	defer a.backgroundOwnerMu.Unlock()

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
	for key, messages := range a.agentMessages {
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
	previous := append([]AgentMessage{}, a.agentMessages[key]...)
	expected.Seq = a.nextAgentTranscriptSequenceLocked(event.ProjectID, targetID)
	a.agentMessages[key] = append(a.agentMessages[key], expected)
	if err := a.saveJSON("agent-messages.json", a.agentMessages, 0644); err != nil {
		a.agentMessages[key] = previous
		return false, err
	}
	if err := a.processRuntimePersistenceFail(processPersistAfterTerminalMessage); err != nil {
		return true, err
	}
	return true, nil
}

func (a *app) processTerminalMessageTargetLocked(projectID, ownerID string) string {
	key := projectAgentKey(projectID, ownerID)
	if !a.backgroundOwnerDeleting[key] {
		for _, agent := range a.agents[projectID] {
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
	run, ok := a.agentRuns[key]
	if !ok || !run.State.Active() || strings.TrimSpace(runID) == "" || run.ID != runID || a.agentRunCancelling[key] == runID {
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
	items := a.agentMessages[key]
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
	if a.agentMessages == nil {
		a.agentMessages = map[string][]AgentMessage{}
	}
	key := projectAgentKey(projectID, agentID)
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
	a.agentMessages[key] = append(a.agentMessages[key], msg)
	a.appendTranscriptForAgentMessageLocked(msg, metadata)
	return msg
}

func (a *app) persistAppendedAgentMessage(projectID, agentID string) {
	if err := a.saveAgentMessages(); err != nil {
		log.Printf("save agent messages: %v", err)
	}
	if err := a.saveAgentTranscripts(); err != nil {
		log.Printf("save agent transcripts: %v", err)
	}
	a.maybeCheckpointAgentSession(projectID, agentID, false)
}

func (a *app) ensureAgentSession(projectID, agentID string) AgentSessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ensureAgentSessionLocked(projectID, agentID)
}

func (a *app) ensureAgentSessionLocked(projectID, agentID string) AgentSessionState {
	if a.agentSessions == nil {
		a.agentSessions = map[string]AgentSessionState{}
	}
	key := projectAgentKey(projectID, agentID)
	if state, ok := a.agentSessions[key]; ok && strings.TrimSpace(state.SessionID) != "" {
		return state
	}
	state := AgentSessionState{
		SessionID:           residentSessionID(projectID, agentID),
		ProjectID:           projectID,
		AgentID:             agentID,
		ShortWindowStartSeq: 1,
		LastCheckpointAt:    time.Now().UTC(),
	}
	a.agentSessions[key] = state
	return state
}

func (a *app) agentSessionState(projectID, agentID string) AgentSessionState {
	return a.ensureAgentSession(projectID, agentID)
}

func (a *app) updateAgentSessionState(state AgentSessionState) {
	a.mu.Lock()
	a.agentSessions[projectAgentKey(state.ProjectID, state.AgentID)] = state
	a.mu.Unlock()
	if err := a.saveAgentSessions(); err != nil {
		log.Printf("save agent sessions: %v", err)
	}
}

func (a *app) maybeCheckpointAgentSession(projectID, agentID string, force bool) {
	const shortWindowLimit int64 = 50
	const summaryLimit = 24
	state := a.agentSessionState(projectID, agentID)
	messages := a.agentMessagesFor(projectID, agentID)
	if len(messages) == 0 {
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
		return
	}
	boundarySeq := maxSeq - shortWindowLimit
	if boundarySeq <= state.BoundarySeq {
		return
	}
	nextSeq := state.CoveredSeqEnd + 1
	if nextSeq <= 0 {
		nextSeq = 1
	}
	var batch []AgentMessage
	for _, msg := range messages {
		if msg.Seq >= nextSeq && msg.Seq <= boundarySeq {
			batch = append(batch, msg)
			if len(batch) >= summaryLimit {
				break
			}
		}
	}
	if len(batch) == 0 {
		return
	}
	a.archiveAgentMessages(projectID, agentID, messages, state.ShortWindowStartSeq, batch[len(batch)-1].Seq)
	var b strings.Builder
	if previous := normalizeResidentSummary(state.ResidentSummary, 2400); previous != "" {
		b.WriteString("Earlier checkpoint highlights:\n")
		b.WriteString(previous)
		b.WriteString("\n\n")
	}
	b.WriteString("Recent checkpoint highlights:\n")
	for _, msg := range batch {
		line := compactAgentSummaryLine(msg)
		if line == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(line)
		b.WriteString("\n")
		if b.Len() >= 5200 {
			break
		}
	}
	state.ResidentSummary = limitString(b.String(), 6000)
	if state.CoveredSeqStart <= 0 {
		state.CoveredSeqStart = batch[0].Seq
	}
	state.CoveredSeqEnd = batch[len(batch)-1].Seq
	state.BoundarySeq = state.CoveredSeqEnd
	state.ShortWindowStartSeq = state.BoundarySeq + 1
	if minStart := maxSeq - shortWindowLimit + 1; minStart > state.ShortWindowStartSeq {
		state.ShortWindowStartSeq = minStart
	}
	state.LongTermVersion++
	state.LastCheckpointAt = time.Now().UTC()
	a.updateAgentSessionState(state)
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

func (a *app) archiveAgentMessages(projectID, agentID string, messages []AgentMessage, startSeq, endSeq int64) {
	if endSeq < startSeq {
		return
	}
	key := projectAgentKey(projectID, agentID)
	now := time.Now().UTC()
	a.mu.Lock()
	existing := map[int64]bool{}
	for _, archived := range a.archives[key] {
		existing[archived.Seq] = true
	}
	for _, msg := range messages {
		if msg.Seq < startSeq || msg.Seq > endSeq || existing[msg.Seq] {
			continue
		}
		a.archives[key] = append(a.archives[key], AgentArchiveMessage{
			ID:         msg.ID,
			ProjectID:  msg.ProjectID,
			AgentID:    msg.AgentID,
			SessionID:  msg.SessionID,
			Seq:        msg.Seq,
			Role:       msg.Role,
			Intent:     msg.Intent,
			Body:       msg.Body,
			CreatedAt:  msg.CreatedAt,
			ArchivedAt: now,
		})
	}
	sort.SliceStable(a.archives[key], func(i, j int) bool { return a.archives[key][i].Seq < a.archives[key][j].Seq })
	a.mu.Unlock()
	if err := a.saveArchives(); err != nil {
		log.Printf("save archives: %v", err)
	}
}

func compactAgentSummaryLine(msg AgentMessage) string {
	content := promptAgentMessageBody(msg)
	if content == "" {
		return ""
	}
	content = strings.Join(strings.Fields(content), " ")
	return fmt.Sprintf("seq %d %s: %s", msg.Seq, strings.TrimSpace(msg.Role), limitString(content, 280))
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
