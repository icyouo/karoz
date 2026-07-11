package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

func (a *app) agentMessagesFor(projectID, agentID string) []AgentMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]AgentMessage{}, a.agentMessages[agentMessageKey(projectID, agentID)]...)
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
	page := AgentMessagesPage{Messages: messages, HasMore: hasMore}
	if hasMore && len(messages) > 0 {
		page.NextBeforeSeq = messages[0].Seq
	}
	if page.Messages == nil {
		page.Messages = []AgentMessage{}
	}
	return page
}

func (a *app) appendAgentMessage(projectID, agentID, role, intent, body string) AgentMessage {
	now := time.Now().UTC()
	key := agentMessageKey(projectID, agentID)
	session := a.ensureAgentSession(projectID, agentID)
	msg := AgentMessage{
		ID:        messageID(),
		ProjectID: projectID,
		AgentID:   agentID,
		SessionID: session.SessionID,
		Seq:       int64(len(a.agentMessagesFor(projectID, agentID))) + 1,
		Role:      role,
		Intent:    firstNonEmpty(intent, "note"),
		Body:      strings.TrimSpace(body),
		CreatedAt: now,
	}
	a.mu.Lock()
	a.agentMessages[key] = append(a.agentMessages[key], msg)
	a.mu.Unlock()
	if err := a.saveAgentMessages(); err != nil {
		log.Printf("save agent messages: %v", err)
	}
	a.maybeCheckpointAgentSession(projectID, agentID, false)
	return msg
}

func (a *app) ensureAgentSession(projectID, agentID string) AgentSessionState {
	key := agentMessageKey(projectID, agentID)
	a.mu.Lock()
	defer a.mu.Unlock()
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
	a.agentSessions[agentMessageKey(state.ProjectID, state.AgentID)] = state
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
	if strings.TrimSpace(state.ResidentSummary) != "" {
		b.WriteString("Previous rolling summary:\n")
		b.WriteString(limitString(state.ResidentSummary, 2400))
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

func (a *app) archiveAgentMessages(projectID, agentID string, messages []AgentMessage, startSeq, endSeq int64) {
	if endSeq < startSeq {
		return
	}
	key := agentMessageKey(projectID, agentID)
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
		return limitString(body, 5000)
	case "user":
		return limitString(body, 5000)
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
	if strings.EqualFold(strings.TrimSpace(toolName), "request_choice") {
		return body
	}
	return compactToolResultForPrompt(body)
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
