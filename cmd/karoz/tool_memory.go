package main

import (
	"sort"
	"strings"
	"time"
)

func (a *app) createMemory(projectID, agentID, layer string, args map[string]any, priority int, metadata map[string]any) (string, error) {
	summary := toolStringArg(args, "summary", 1000)
	detail := toolStringArg(args, "detail", 12000)
	if summary == "" || detail == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "summary and detail are required"}), nil
	}
	now := time.Now().UTC()
	session := a.ensureAgentSession(projectID, agentID)
	entry := AgentMemoryEntry{
		ID:        randomID(),
		ProjectID: projectID,
		AgentID:   agentID,
		SessionID: session.SessionID,
		Layer:     layer,
		State:     "active",
		Priority:  priority,
		Summary:   summary,
		Detail:    detail,
		Metadata:  metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	key := agentMessageKey(projectID, agentID)
	a.mu.Lock()
	a.memories[key] = append(a.memories[key], entry)
	a.mu.Unlock()
	if err := a.saveMemories(); err != nil {
		return "", err
	}
	if layer == "pending" {
		a.emitRuntimeStateChanged(RuntimeEvent{
			ID:        randomID(),
			ProjectID: projectID,
			Kind:      "memory_changed",
			EntityID:  entry.ID,
			To:        "active",
			Reason:    "pending_memory_created",
			CreatedAt: time.Now().UTC(),
		})
	}
	return toolJSON(map[string]any{"entry": memorySummary(entry)}), nil
}

func (a *app) dropPendingMemory(projectID, agentID, id string) string {
	if strings.TrimSpace(id) == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "id is required"})
	}
	key := agentMessageKey(projectID, agentID)
	now := time.Now().UTC()
	a.mu.Lock()
	found := false
	for i := range a.memories[key] {
		if a.memories[key][i].ID == id && a.memories[key][i].Layer == "pending" && a.memories[key][i].State == "active" {
			a.memories[key][i].State = "archived"
			a.memories[key][i].ArchivedAt = &now
			a.memories[key][i].UpdatedAt = now
			found = true
			break
		}
	}
	a.mu.Unlock()
	if found {
		if err := a.saveMemories(); err != nil {
			return toolJSON(map[string]any{"error": "save_failed", "message": err.Error()})
		}
		return toolJSON(map[string]any{"id": id, "state": "archived"})
	}
	return toolJSON(map[string]any{"error": "not_found", "message": "pending memory not found"})
}

func (a *app) searchArchive(projectID, agentID, query string, limit int) string {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "query is required"})
	}
	key := agentMessageKey(projectID, agentID)
	a.mu.Lock()
	memories := append([]AgentMemoryEntry{}, a.memories[key]...)
	archives := append([]AgentArchiveMessage{}, a.archives[key]...)
	a.mu.Unlock()
	terms := strings.Fields(query)
	matchScore := func(text string) int {
		text = strings.ToLower(text)
		if strings.Contains(text, query) {
			return len(terms) + 4
		}
		score := 0
		for _, term := range terms {
			if len([]rune(term)) < 2 {
				continue
			}
			if strings.Contains(text, term) {
				score++
			}
		}
		return score
	}
	type memoryMatch struct {
		entry AgentMemoryEntry
		score int
	}
	var matchedMemories []memoryMatch
	for _, entry := range memories {
		if score := matchScore(entry.Summary + "\n" + entry.Detail); score > 0 {
			matchedMemories = append(matchedMemories, memoryMatch{entry: entry, score: score})
		}
	}
	sort.SliceStable(matchedMemories, func(i, j int) bool {
		if matchedMemories[i].score == matchedMemories[j].score {
			return matchedMemories[i].entry.UpdatedAt.After(matchedMemories[j].entry.UpdatedAt)
		}
		return matchedMemories[i].score > matchedMemories[j].score
	})
	memoryResults := make([]map[string]any, 0, min(limit, len(matchedMemories)))
	for _, match := range matchedMemories {
		memoryResults = append(memoryResults, memorySummary(match.entry))
		if len(memoryResults) >= limit {
			break
		}
	}
	type messageMatch struct {
		message AgentArchiveMessage
		score   int
	}
	var matchedMessages []messageMatch
	for _, msg := range archives {
		// Tool payloads are already represented by the surrounding assistant or
		// system message. Returning them here recursively injects old JSON blobs
		// into the current model loop.
		if msg.Role == "tool_call" || msg.Role == "tool_result" {
			continue
		}
		if score := matchScore(msg.Body); score > 0 {
			matchedMessages = append(matchedMessages, messageMatch{message: msg, score: score})
		}
	}
	sort.SliceStable(matchedMessages, func(i, j int) bool {
		if matchedMessages[i].score == matchedMessages[j].score {
			return matchedMessages[i].message.Seq > matchedMessages[j].message.Seq
		}
		return matchedMessages[i].score > matchedMessages[j].score
	})
	messageResults := make([]map[string]any, 0, min(limit, len(matchedMessages)))
	for _, match := range matchedMessages {
		msg := match.message
		messageResults = append(messageResults, compactArchivedMessageForTool(msg.Seq, msg.Role, msg.Intent, msg.Body, msg.CreatedAt))
		if len(messageResults) >= limit {
			break
		}
	}
	return toolJSON(map[string]any{"memory_entries": memoryResults, "messages": messageResults})
}

func (a *app) getArchivedMessages(projectID, agentID string, startSeq, endSeq int64, limit int) string {
	key := agentMessageKey(projectID, agentID)
	a.mu.Lock()
	archives := append([]AgentArchiveMessage{}, a.archives[key]...)
	messages := append([]AgentMessage{}, a.agentMessages[key]...)
	a.mu.Unlock()
	var out []map[string]any
	for _, msg := range archives {
		if msg.Seq >= startSeq && msg.Seq <= endSeq {
			out = append(out, compactArchivedMessageForTool(msg.Seq, msg.Role, msg.Intent, msg.Body, msg.CreatedAt))
		}
		if len(out) >= limit {
			return toolJSON(map[string]any{"messages": out})
		}
	}
	for _, msg := range messages {
		if msg.Seq >= startSeq && msg.Seq <= endSeq {
			out = append(out, compactArchivedMessageForTool(msg.Seq, msg.Role, msg.Intent, msg.Body, msg.CreatedAt))
		}
		if len(out) >= limit {
			break
		}
	}
	return toolJSON(map[string]any{"messages": out})
}

func compactArchivedMessageForTool(seq int64, role, intent, body string, createdAt time.Time) map[string]any {
	originalChars := len(body)
	compactBody := ""
	if role == "tool_call" || role == "tool_result" {
		compactBody = "[historical tool payload omitted; inspect the surrounding assistant/system result or call the current domain tool]"
	} else {
		compactBody = promptAgentMessageBody(AgentMessage{Role: role, Intent: intent, Body: body})
	}
	return map[string]any{
		"seq":            seq,
		"role":           role,
		"intent":         intent,
		"body":           compactBody,
		"original_chars": originalChars,
		"created_at":     createdAt,
	}
}
