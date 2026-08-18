package main

import (
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

func (a *app) createMemory(projectID, agentID, layer string, args map[string]any, priority int, metadata map[string]any) (string, error) {
	summary := toolStringArg(args, "summary", 1000)
	detail := toolStringArg(args, "detail", 12000)
	if summary == "" || detail == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "summary and detail are required"}), nil
	}
	scope := strings.ToLower(toolStringArg(args, "scope", 32))
	if scope == "" {
		scope = "agent"
	}
	if scope != "agent" && scope != "project" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "scope must be agent or project"}), nil
	}
	if scope == "project" && layer != "fact" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "project scope is only supported for facts"}), nil
	}
	supersedesID := toolStringArg(args, "supersedes_id", 128)
	if supersedesID != "" && layer != "decision" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "only decisions can supersede another memory"}), nil
	}
	now := time.Now().UTC()
	entry := AgentMemoryEntry{
		ID:           randomID(),
		ProjectID:    projectID,
		AgentID:      agentID,
		SessionID:    residentSessionID(projectID, agentID),
		Layer:        layer,
		Scope:        scope,
		State:        "active",
		Priority:     priority,
		Summary:      summary,
		Detail:       detail,
		Metadata:     metadata,
		SupersedesID: supersedesID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	store := a.memoryStoreLocked()
	current := append([]AgentMemoryEntry{}, store.entries[key]...)
	if supersedesID != "" {
		predecessorIndex, validationResult := a.validateMemorySupersessionLocked(projectID, agentID, supersedesID, current)
		if validationResult != "" {
			a.mu.Unlock()
			return validationResult, nil
		}
		current[predecessorIndex].State = "archived"
		current[predecessorIndex].ArchivedAt = &now
		current[predecessorIndex].UpdatedAt = now
		current[predecessorIndex].SupersededByID = entry.ID
	} else {
		if scope == "project" {
			for _, entries := range store.entries {
				for _, existing := range entries {
					if existing.ProjectID == projectID && existing.Layer == "fact" &&
						memoryEntryScope(existing) == "project" && existing.State == "active" &&
						existing.ArchivedAt == nil && existing.SupersededByID == "" &&
						memoriesExactlyEqual(existing, entry) {
						a.mu.Unlock()
						return toolJSON(map[string]any{"entry": memorySummary(existing), "deduplicated": true}), nil
					}
				}
			}
		} else {
			for _, existing := range current {
				if existing.Layer == layer && existing.State == "active" && existing.ArchivedAt == nil &&
					existing.SupersededByID == "" && memoriesExactlyEqual(existing, entry) {
					a.mu.Unlock()
					return toolJSON(map[string]any{"entry": memorySummary(existing), "deduplicated": true}), nil
				}
			}
		}
	}
	current = append(current, entry)
	proposed := make(map[string][]AgentMemoryEntry, len(store.entries)+1)
	for existingKey, entries := range store.entries {
		proposed[existingKey] = entries
	}
	proposed[key] = current
	if err := a.saveJSON("agent-memory.json", proposed, 0644); err != nil {
		a.mu.Unlock()
		return "", err
	}
	store.entries[key] = current
	a.mu.Unlock()
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
	return toolJSON(map[string]any{"entry": memorySummary(entry), "deduplicated": false}), nil
}

func normalizeMemoryExactValue(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func memoryRationale(entry AgentMemoryEntry) string {
	if entry.Metadata == nil {
		return ""
	}
	value, _ := entry.Metadata["rationale"].(string)
	return value
}

func memoriesExactlyEqual(left, right AgentMemoryEntry) bool {
	if memoryEntryScope(left) != memoryEntryScope(right) {
		return false
	}
	if normalizeMemoryExactValue(left.Summary) != normalizeMemoryExactValue(right.Summary) ||
		normalizeMemoryExactValue(left.Detail) != normalizeMemoryExactValue(right.Detail) {
		return false
	}
	if left.Layer == "decision" {
		return normalizeMemoryExactValue(memoryRationale(left)) == normalizeMemoryExactValue(memoryRationale(right))
	}
	if left.Layer == "pending" {
		// Priority is part of pending queue identity. A caller asking for the
		// same text at a different priority creates a distinct active item
		// instead of silently discarding the requested scheduling semantics.
		return left.Priority == right.Priority
	}
	return true
}

func (a *app) validateMemorySupersessionLocked(projectID, agentID, supersedesID string, current []AgentMemoryEntry) (int, string) {
	for i, existing := range current {
		if existing.ID != supersedesID {
			continue
		}
		if existing.ProjectID != projectID || existing.AgentID != agentID {
			return -1, toolJSON(map[string]any{"error": "invalid_supersession", "message": "supersedes_id belongs to another project or agent"})
		}
		if existing.Layer != "decision" {
			return -1, toolJSON(map[string]any{"error": "invalid_supersession", "message": "supersedes_id must reference a decision"})
		}
		if existing.State != "active" || existing.ArchivedAt != nil || existing.SupersededByID != "" {
			return -1, toolJSON(map[string]any{"error": "invalid_supersession", "message": "supersedes_id must reference an active decision"})
		}
		return i, ""
	}
	for existingKey, entries := range a.memoryStoreLocked().entries {
		if existingKey == projectAgentKey(projectID, agentID) {
			continue
		}
		for _, existing := range entries {
			if existing.ID == supersedesID {
				return -1, toolJSON(map[string]any{"error": "invalid_supersession", "message": "supersedes_id belongs to another project or agent"})
			}
		}
	}
	return -1, toolJSON(map[string]any{"error": "invalid_supersession", "message": "supersedes_id was not found"})
}

func (a *app) dropPendingMemory(projectID, agentID, id string) string {
	if strings.TrimSpace(id) == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "id is required"})
	}
	key := projectAgentKey(projectID, agentID)
	now := time.Now().UTC()
	a.mu.Lock()
	store := a.memoryStoreLocked()
	found := false
	for i := range store.entries[key] {
		if store.entries[key][i].ID == id && store.entries[key][i].Layer == "pending" && store.entries[key][i].State == "active" {
			store.entries[key][i].State = "archived"
			store.entries[key][i].ArchivedAt = &now
			store.entries[key][i].UpdatedAt = now
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
	corpus := a.snapshotMemorySearchCorpus(projectID, agentID)
	lexicalQuery := newMemoryLexicalQuery(query)
	type memoryMatch struct {
		entry AgentMemoryEntry
		score int
	}
	var matchedMemories []memoryMatch
	for _, entry := range corpus.memories {
		if score := corpus.scorer.score(lexicalQuery, entry.Summary+"\n"+entry.Detail); score > 0 {
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
	for _, msg := range corpus.archives {
		if score := corpus.scorer.score(lexicalQuery, msg.Body); score > 0 {
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

type memoryLexicalQuery struct {
	phrase     string
	asciiTerms []string
	cjkBigrams []string
}

const (
	memoryIDFScale            = 1000
	memoryCJKInfoFloor        = 500
	memoryCJKMaxEvidence      = 4
	memoryCJKScoreCap         = 999999
	memoryTermScoreBase       = 2000000
	memoryTermScoreCap        = 999999
	memoryPhraseScoreBase     = 4000000
	memoryPhraseScoreCap      = 999999
	memoryProactiveLayerLimit = 3
)

type memorySearchScorer struct {
	documentCount int
	cjkDocumentDF map[string]int
}

type memorySearchCorpus struct {
	memories []AgentMemoryEntry
	archives []AgentArchiveMessage
	scorer   memorySearchScorer
}

// snapshotMemorySearchCorpus takes one consistent view of both searchable
// stores. Every memory entry and every non-tool archive message contributes
// one document to CJK document frequency, even when a caller later filters
// which result kinds it returns.
func (a *app) snapshotMemorySearchCorpus(projectID, agentID string) memorySearchCorpus {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	store := a.memoryStoreLocked()
	memories := make([]AgentMemoryEntry, 0, len(store.entries[key]))
	for _, entries := range store.entries {
		for _, entry := range entries {
			if entry.ProjectID != projectID {
				continue
			}
			if entry.AgentID == agentID {
				memories = append(memories, entry)
				continue
			}
			if entry.Layer == "fact" && memoryEntryScope(entry) == "project" &&
				(entry.State == "active" || entry.State == "archived") {
				memories = append(memories, entry)
			}
		}
	}
	allArchives := a.conversationServiceLocked().ArchivedMessagesFor(key)
	a.mu.Unlock()

	archives := make([]AgentArchiveMessage, 0, len(allArchives))
	documents := make([]string, 0, len(memories)+len(allArchives))
	for _, entry := range memories {
		documents = append(documents, entry.Summary+"\n"+entry.Detail)
	}
	for _, msg := range allArchives {
		// Tool payloads are already represented by the surrounding assistant or
		// system message. Searching them recursively injects old JSON blobs into
		// the current model loop, so they are neither results nor DF documents.
		if msg.Role == "tool_call" || msg.Role == "tool_result" {
			continue
		}
		archives = append(archives, msg)
		documents = append(documents, msg.Body)
	}
	return memorySearchCorpus{
		memories: memories,
		archives: archives,
		scorer:   newMemorySearchScorer(documents),
	}
}

func newMemorySearchScorer(documents []string) memorySearchScorer {
	scorer := memorySearchScorer{
		documentCount: len(documents),
		cjkDocumentDF: map[string]int{},
	}
	for _, document := range documents {
		for bigram := range memoryCJKBigramSet(document) {
			scorer.cjkDocumentDF[bigram]++
		}
	}
	return scorer
}

func newMemoryLexicalQuery(query string) memoryLexicalQuery {
	query = strings.ToLower(strings.TrimSpace(query))
	result := memoryLexicalQuery{phrase: query}
	var asciiRun, cjkRun []rune
	seenASCII := map[string]bool{}
	seenCJK := map[string]bool{}
	flushASCII := func() {
		term := string(asciiRun)
		asciiRun = asciiRun[:0]
		if len([]rune(term)) >= 2 && !seenASCII[term] {
			seenASCII[term] = true
			result.asciiTerms = append(result.asciiTerms, term)
		}
	}
	flushCJK := func() {
		for i := 0; i+1 < len(cjkRun); i++ {
			term := string(cjkRun[i : i+2])
			if !seenCJK[term] {
				seenCJK[term] = true
				result.cjkBigrams = append(result.cjkBigrams, term)
			}
		}
		cjkRun = cjkRun[:0]
	}
	for _, r := range []rune(query) {
		switch {
		case isMemoryCJKRune(r):
			flushASCII()
			cjkRun = append(cjkRun, r)
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			flushCJK()
			asciiRun = append(asciiRun, r)
		default:
			flushASCII()
			flushCJK()
		}
	}
	flushASCII()
	flushCJK()
	return result
}

func isMemoryCJKRune(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

func memoryCJKBigramSet(text string) map[string]bool {
	returned := map[string]bool{}
	var run []rune
	flush := func() {
		for i := 0; i+1 < len(run); i++ {
			returned[string(run[i:i+2])] = true
		}
		run = run[:0]
	}
	for _, r := range []rune(strings.ToLower(text)) {
		if isMemoryCJKRune(r) {
			run = append(run, r)
		} else {
			flush()
		}
	}
	flush()
	return returned
}

func memoryUnicodeTermSet(text string) map[string]bool {
	returned := map[string]bool{}
	var run []rune
	flush := func() {
		if len(run) >= 2 {
			returned[string(run)] = true
		}
		run = run[:0]
	}
	for _, r := range []rune(strings.ToLower(text)) {
		if !isMemoryCJKRune(r) && (unicode.IsLetter(r) || unicode.IsNumber(r)) {
			run = append(run, r)
		} else {
			flush()
		}
	}
	flush()
	return returned
}

func (s memorySearchScorer) cjkIDFFixedPoint(bigram string) int {
	if s.documentCount <= 0 {
		return 0
	}
	df := s.cjkDocumentDF[bigram]
	var idf float64
	if s.documentCount <= 2 {
		// Rarity is not identifiable in a one-document corpus or when the same
		// entry is represented once in memory and once in the archive. Preserve
		// retrieval there with the positive BM25 form.
		idf = math.Log(1 + (float64(s.documentCount-df)+0.5)/(float64(df)+0.5))
	} else {
		// Clamped Robertson/Sparck-Jones weight makes terms present in at least
		// half the corpus contribute no evidence, regardless of query length.
		ratio := (float64(s.documentCount-df) + 0.5) / (float64(df) + 0.5)
		if ratio <= 1 {
			return 0
		}
		idf = math.Log(ratio)
	}
	return int(math.Round(idf * memoryIDFScale))
}

// score uses disjoint bands: an exact phrase always outranks any collection of
// whole Unicode terms, which always outranks an accepted CJK-only match.
func (s memorySearchScorer) score(query memoryLexicalQuery, text string) int {
	text = strings.ToLower(text)
	if query.phrase != "" && strings.Contains(text, query.phrase) {
		bonus := len([]rune(query.phrase))
		return memoryPhraseScoreBase + min(bonus, memoryPhraseScoreCap)
	}
	textTerms := memoryUnicodeTermSet(text)
	termMatches := 0
	for _, term := range query.asciiTerms {
		if textTerms[term] {
			termMatches++
		}
	}
	if termMatches > 0 {
		return memoryTermScoreBase + min(termMatches*memoryIDFScale, memoryTermScoreCap)
	}

	textBigrams := memoryCJKBigramSet(text)
	rawMatches := 0
	idfContributions := make([]int, 0, min(len(query.cjkBigrams), memoryCJKMaxEvidence))
	for _, term := range query.cjkBigrams {
		if textBigrams[term] {
			rawMatches++
			idfContributions = append(idfContributions, s.cjkIDFFixedPoint(term))
		}
	}
	if rawMatches < 2 {
		return 0
	}
	sort.Sort(sort.Reverse(sort.IntSlice(idfContributions)))
	information := 0
	for i := 0; i < min(len(idfContributions), memoryCJKMaxEvidence); i++ {
		information += idfContributions[i]
	}
	if information < memoryCJKInfoFloor {
		return 0
	}
	return min(information, memoryCJKScoreCap)
}

// relevantMemoriesFor scores active decisions and facts against the shared
// memory/archive corpus, but selects each layer independently. Done and pending
// memories remain available through their explicit tools and archive search;
// neither is proactively injected.
func (a *app) relevantMemoriesFor(projectID, agentID, query string, limit int) []AgentMemoryEntry {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" || limit <= 0 {
		return []AgentMemoryEntry{}
	}
	lexicalQuery := newMemoryLexicalQuery(query)
	corpus := a.snapshotMemorySearchCorpus(projectID, agentID)
	type memoryMatch struct {
		entry AgentMemoryEntry
		score int
	}
	var matchedDecisions []memoryMatch
	var matchedFacts []memoryMatch
	for _, entry := range corpus.memories {
		if entry.State != "active" || entry.ArchivedAt != nil {
			continue
		}
		if entry.Layer != "decision" && entry.Layer != "fact" {
			continue
		}
		if entry.Layer == "decision" && (entry.AgentID != agentID || memoryEntryScope(entry) != "agent") {
			continue
		}
		if entry.Layer == "fact" && entry.AgentID != agentID && memoryEntryScope(entry) != "project" {
			continue
		}
		if score := corpus.scorer.score(lexicalQuery, entry.Summary+"\n"+entry.Detail); score >= 1 {
			match := memoryMatch{entry: entry, score: score}
			if entry.Layer == "decision" {
				matchedDecisions = append(matchedDecisions, match)
			} else {
				matchedFacts = append(matchedFacts, match)
			}
		}
	}
	sortMatches := func(matched []memoryMatch) {
		sort.SliceStable(matched, func(i, j int) bool {
			if matched[i].score == matched[j].score {
				left := matched[i].entry.UpdatedAt
				if left.IsZero() {
					left = matched[i].entry.CreatedAt
				}
				right := matched[j].entry.UpdatedAt
				if right.IsZero() {
					right = matched[j].entry.CreatedAt
				}
				return left.After(right)
			}
			return matched[i].score > matched[j].score
		})
	}
	sortMatches(matchedDecisions)
	sortMatches(matchedFacts)

	out := make([]AgentMemoryEntry, 0, limit)
	decisionLimit := min(memoryProactiveLayerLimit, limit)
	for i := 0; i < min(decisionLimit, len(matchedDecisions)); i++ {
		out = append(out, matchedDecisions[i].entry)
	}
	factLimit := min(memoryProactiveLayerLimit, limit-len(out))
	for i := 0; i < min(factLimit, len(matchedFacts)); i++ {
		out = append(out, matchedFacts[i].entry)
	}
	if out == nil {
		return []AgentMemoryEntry{}
	}
	return out
}

func (a *app) getArchivedMessages(projectID, agentID string, startSeq, endSeq int64, limit int) string {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	messages := a.conversationServiceLocked().MessagesFor(key)
	a.mu.Unlock()
	var out []map[string]any
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
