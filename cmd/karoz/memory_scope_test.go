package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func projectMemoryTestApp(t *testing.T) (*app, Project, Agent, Agent) {
	t.Helper()
	a, project, author := freshMemoryLifecycleApp(t)
	author.Nickname = "Author"
	peer := Agent{ID: "peer", ProjectID: project.ID, Nickname: "Peer"}
	a.agentDirectoryLocked().agents[project.ID] = []Agent{author, peer}
	return a, project, author, peer
}

func decodeScopedMemoryResult(t *testing.T, raw string) (id, scope, authorID string, deduplicated bool) {
	t.Helper()
	var result struct {
		Entry struct {
			ID            string `json:"id"`
			Scope         string `json:"scope"`
			AuthorAgentID string `json:"author_agent_id"`
		} `json:"entry"`
		Deduplicated bool `json:"deduplicated"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode scoped memory result: %v\n%s", err, raw)
	}
	return result.Entry.ID, result.Entry.Scope, result.Entry.AuthorAgentID, result.Deduplicated
}

func TestProjectFactVisibleThroughRetrievalSearchAndAPI(t *testing.T) {
	a, project, author, peer := projectMemoryTestApp(t)
	authorCtx := ResidentToolContext{Project: project, Agent: author, Workdir: project.Path}
	sharedRaw, err := a.executeResidentTool(context.Background(), authorCtx, codexToolCall{
		Name:      "remember_fact",
		Arguments: `{"summary":"Shared launch region","detail":"The launch region is Singapore.","scope":"project"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	sharedID, scope, authorID, deduplicated := decodeScopedMemoryResult(t, sharedRaw)
	if sharedID == "" || scope != "project" || authorID != author.ID || deduplicated {
		t.Fatalf("shared fact result = %s", sharedRaw)
	}
	privateRaw, err := a.executeResidentTool(context.Background(), authorCtx, codexToolCall{
		Name:      "remember_fact",
		Arguments: `{"summary":"Private launch code","detail":"The private launch code is orchid."}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	privateID, _, _, _ := decodeScopedMemoryResult(t, privateRaw)

	relevant := a.relevantMemoriesFor(project.ID, peer.ID, "What is the shared launch region?", 6)
	if len(relevant) != 1 || relevant[0].ID != sharedID || relevant[0].AgentID != author.ID ||
		memoryEntryScope(relevant[0]) != "project" {
		t.Fatalf("peer relevant memories = %+v", relevant)
	}
	for _, entry := range a.relevantMemoriesFor(project.ID, peer.ID, "Private launch code", 6) {
		if entry.ID == privateID {
			t.Fatalf("author-private fact leaked into peer retrieval: %+v", entry)
		}
	}

	var searchResult struct {
		MemoryEntries []map[string]any `json:"memory_entries"`
		Messages      []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(a.searchArchive(project.ID, peer.ID, "Shared launch region", 10)), &searchResult); err != nil {
		t.Fatal(err)
	}
	if len(searchResult.MemoryEntries) != 1 || searchResult.MemoryEntries[0]["id"] != sharedID ||
		searchResult.MemoryEntries[0]["scope"] != "project" ||
		searchResult.MemoryEntries[0]["author_agent_id"] != author.ID {
		t.Fatalf("peer archive search = %+v", searchResult)
	}
	if err := json.Unmarshal([]byte(a.searchArchive(project.ID, peer.ID, "Private launch code", 10)), &searchResult); err != nil {
		t.Fatal(err)
	}
	for _, entry := range searchResult.MemoryEntries {
		if entry["id"] == privateID {
			t.Fatalf("author-private fact leaked into peer search: %+v", entry)
		}
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/projects/p1/agents/peer/memory", nil)
	a.handleAgents(recorder, request, project, []string{peer.ID, "memory"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("memory API status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var apiEntries []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &apiEntries); err != nil {
		t.Fatal(err)
	}
	if len(apiEntries) != 1 || apiEntries[0]["id"] != sharedID || apiEntries[0]["scope"] != "project" ||
		apiEntries[0]["author_agent_id"] != author.ID {
		t.Fatalf("peer memory API = %+v", apiEntries)
	}
	for _, key := range []string{"project_id", "agent_id", "session_id"} {
		if _, ok := apiEntries[0][key]; !ok {
			t.Fatalf("shared API entry dropped legacy key %q: %+v", key, apiEntries[0])
		}
	}
	if apiEntries[0]["project_id"] != project.ID || apiEntries[0]["agent_id"] != author.ID ||
		apiEntries[0]["session_id"] != residentSessionID(project.ID, author.ID) {
		t.Fatalf("shared API legacy identity changed: %+v", apiEntries[0])
	}

	authorRecorder := httptest.NewRecorder()
	authorRequest := httptest.NewRequest(http.MethodGet, "/api/projects/p1/agents/"+author.ID+"/memory", nil)
	a.handleAgents(authorRecorder, authorRequest, project, []string{author.ID, "memory"})
	var authorEntries []map[string]any
	if err := json.Unmarshal(authorRecorder.Body.Bytes(), &authorEntries); err != nil {
		t.Fatal(err)
	}
	foundPrivate := false
	for _, entry := range authorEntries {
		if entry["id"] != privateID {
			continue
		}
		foundPrivate = true
		if entry["scope"] != "agent" || entry["author_agent_id"] != author.ID {
			t.Fatalf("private API attribution = %+v", entry)
		}
		for _, key := range []string{"project_id", "agent_id", "session_id"} {
			if _, ok := entry[key]; !ok {
				t.Fatalf("private API entry dropped legacy key %q: %+v", key, entry)
			}
		}
		if entry["project_id"] != project.ID || entry["agent_id"] != author.ID ||
			entry["session_id"] != residentSessionID(project.ID, author.ID) {
			t.Fatalf("private API legacy identity changed: %+v", entry)
		}
	}
	if !foundPrivate {
		t.Fatalf("author API missing private fact: %+v", authorEntries)
	}
}

func TestProjectFactPromptIncludesScopeAndAuthorWithoutPrivateLeak(t *testing.T) {
	a, project, author, peer := projectMemoryTestApp(t)
	now := time.Now().UTC()
	authorKey := projectAgentKey(project.ID, author.ID)
	a.memoryStoreLocked().entries[authorKey] = []AgentMemoryEntry{
		{ID: "shared-prompt-fact", ProjectID: project.ID, AgentID: author.ID, SessionID: residentSessionID(project.ID, author.ID), Layer: "fact", Scope: "project", State: "active", Summary: "Launch region shared note", Detail: "The launch region is Singapore.", CreatedAt: now, UpdatedAt: now},
		{ID: "author-private-prompt-fact", ProjectID: project.ID, AgentID: author.ID, SessionID: residentSessionID(project.ID, author.ID), Layer: "fact", Scope: "agent", State: "active", Summary: "Launch region private note", Detail: "This note must remain private.", CreatedAt: now, UpdatedAt: now},
	}
	prompt := a.buildResidentAgentPromptWithMemoryQuery(project, peer, "What is the launch region?", "ask", "launch region")
	section := promptSection(prompt, memorySectionHeading)
	if !strings.Contains(section, "[fact; id: shared-prompt-fact; scope: project; author_agent_id: "+author.ID+"]") {
		t.Fatalf("shared fact attribution missing from prompt:\n%s", section)
	}
	if strings.Contains(section, "author-private-prompt-fact") {
		t.Fatalf("author-private fact leaked into peer prompt:\n%s", section)
	}
}

func TestProjectFactVisibilityNeverCrossesProjectOrPrivateArchive(t *testing.T) {
	a, project, author, peer := projectMemoryTestApp(t)
	otherRaw, err := a.createMemory("other-project", author.ID, "fact", map[string]any{
		"summary": "Other project deployment region", "detail": "The region is Frankfurt.", "scope": "project",
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherID, _, _, _ := decodeScopedMemoryResult(t, otherRaw)
	replaceProjectArchivesForTest(a, projectAgentKey(project.ID, author.ID), []AgentArchiveMessage{{
		ID: "author-private-message", ProjectID: project.ID, AgentID: author.ID, Role: "assistant",
		Body: "author private archive phrase",
	}})
	for _, entry := range a.relevantMemoriesFor(project.ID, peer.ID, "Other project deployment region", 6) {
		if entry.ID == otherID {
			t.Fatalf("cross-project fact leaked into retrieval: %+v", entry)
		}
	}
	var result struct {
		MemoryEntries []map[string]any `json:"memory_entries"`
		Messages      []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(a.searchArchive(project.ID, peer.ID, "Other project deployment region", 10)), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.MemoryEntries) != 0 {
		t.Fatalf("cross-project fact leaked into search: %+v", result.MemoryEntries)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/projects/"+project.ID+"/agents/"+peer.ID+"/memory", nil)
	a.handleAgents(recorder, request, project, []string{peer.ID, "memory"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("memory API status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var apiEntries []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &apiEntries); err != nil {
		t.Fatal(err)
	}
	if len(apiEntries) != 0 {
		t.Fatalf("cross-project fact leaked into memory API: %+v", apiEntries)
	}
	if err := json.Unmarshal([]byte(a.searchArchive(project.ID, peer.ID, "author private archive phrase", 10)), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 0 {
		t.Fatalf("author-private archive message leaked to peer: %+v", result.Messages)
	}
}

func TestArchivedProjectFactRemainsVisibleToPeerArchiveSearch(t *testing.T) {
	a, project, author, peer := projectMemoryTestApp(t)
	now := time.Now().UTC()
	archivedAt := now
	archived := AgentMemoryEntry{
		ID: "archived-project-fact", ProjectID: project.ID, AgentID: author.ID,
		Layer: "fact", Scope: "project", State: "archived",
		Summary: "Archived shared release region", Detail: "The former region was Tokyo.",
		CreatedAt: now, UpdatedAt: now, ArchivedAt: &archivedAt,
	}
	a.memoryStoreLocked().entries[projectAgentKey(project.ID, author.ID)] = []AgentMemoryEntry{archived}
	if got := a.relevantMemoriesFor(project.ID, peer.ID, "Archived shared release region", 6); len(got) != 0 {
		t.Fatalf("archived project fact was proactive: %+v", got)
	}
	var result struct {
		MemoryEntries []map[string]any `json:"memory_entries"`
	}
	if err := json.Unmarshal([]byte(a.searchArchive(project.ID, peer.ID, "Archived shared release region", 10)), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.MemoryEntries) != 1 || result.MemoryEntries[0]["id"] != archived.ID ||
		result.MemoryEntries[0]["state"] != "archived" || result.MemoryEntries[0]["scope"] != "project" {
		t.Fatalf("archived project fact search = %+v", result.MemoryEntries)
	}
}

func TestProjectScopeValidationHasZeroStateDrift(t *testing.T) {
	tests := []struct {
		name  string
		layer string
		scope string
	}{
		{"invalid scope", "fact", "team"},
		{"decision", "decision", "project"},
		{"pending", "pending", "project"},
		{"done", "done", "project"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, project, author, _ := projectMemoryTestApp(t)
			key := projectAgentKey(project.ID, author.ID)
			deleteAgentSessionForTest(a, key)
			beforeMemories := map[string][]AgentMemoryEntry{}
			for memoryKey, entries := range a.memoryStoreLocked().entries {
				if entries == nil {
					beforeMemories[memoryKey] = nil
				} else {
					beforeMemories[memoryKey] = append([]AgentMemoryEntry{}, entries...)
				}
			}
			beforeSessions := map[string]AgentSessionState{}
			for sessionKey, session := range agentSessionsForTest(a) {
				beforeSessions[sessionKey] = session
			}
			result, err := a.createMemory(project.ID, author.ID, tt.layer, map[string]any{
				"summary": "Invalid scoped memory", "detail": "Must not persist.", "scope": tt.scope,
			}, 0, map[string]any{"rationale": "Invalid"})
			if err != nil {
				t.Fatal(err)
			}
			var rejection struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(result), &rejection); err != nil {
				t.Fatal(err)
			}
			if rejection.Error != "validation_error" || !reflect.DeepEqual(a.memoryStoreLocked().entries, beforeMemories) ||
				!reflect.DeepEqual(agentSessionsForTest(a), beforeSessions) {
				t.Fatalf("scope rejection drifted state: result=%s memories_before=%+v memories_after=%+v sessions_before=%+v sessions_after=%+v", result, beforeMemories, a.memoryStoreLocked().entries, beforeSessions, agentSessionsForTest(a))
			}
		})
	}
}

func TestConcurrentCrossAgentProjectFactDedupKeepsOriginalAuthor(t *testing.T) {
	a, project, author, peer := projectMemoryTestApp(t)
	const writers = 32
	var wg sync.WaitGroup
	results := make(chan string, writers)
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		agentID := author.ID
		if i%2 == 1 {
			agentID = peer.ID
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := a.createMemory(project.ID, agentID, "fact", map[string]any{
				"summary": "Shared API endpoint", "detail": "The endpoint is api.example.test.", "scope": "project",
			}, 0, nil)
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var onlyID, originalAuthor string
	newCount := 0
	for raw := range results {
		id, scope, authorID, deduplicated := decodeScopedMemoryResult(t, raw)
		if scope != "project" || authorID == "" {
			t.Fatalf("concurrent scoped result = %s", raw)
		}
		if onlyID == "" {
			onlyID, originalAuthor = id, authorID
		}
		if id != onlyID || authorID != originalAuthor {
			t.Fatalf("cross-agent dedup changed identity: first=%s/%s got=%s/%s", onlyID, originalAuthor, id, authorID)
		}
		if !deduplicated {
			newCount++
		}
	}
	var stored []AgentMemoryEntry
	for _, entries := range a.memoryStoreLocked().entries {
		for _, entry := range entries {
			if entry.ProjectID == project.ID && memoryEntryScope(entry) == "project" {
				stored = append(stored, entry)
			}
		}
	}
	if newCount != 1 || len(stored) != 1 || stored[0].ID != onlyID || stored[0].AgentID != originalAuthor {
		t.Fatalf("cross-agent stored facts new=%d entries=%+v", newCount, stored)
	}
}

func TestProjectFactReloadAndLegacyPrivateDefault(t *testing.T) {
	a, project, author, peer := projectMemoryTestApp(t)
	sharedRaw, err := a.createMemory(project.ID, author.ID, "fact", map[string]any{
		"summary": "Shared persistence fact", "detail": "This fact survives reload.", "scope": "project",
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	sharedID, _, _, _ := decodeScopedMemoryResult(t, sharedRaw)
	now := time.Now().UTC()
	legacy := AgentMemoryEntry{
		ID: "legacy-private", ProjectID: project.ID, AgentID: author.ID, Layer: "fact", State: "active",
		Summary: "Legacy private fact", Detail: "Missing scope stays private.", CreatedAt: now, UpdatedAt: now,
	}
	a.mu.Lock()
	key := projectAgentKey(project.ID, author.ID)
	a.memoryStoreLocked().entries[key] = append(a.memoryStoreLocked().entries[key], legacy)
	a.mu.Unlock()
	if err := a.saveMemories(); err != nil {
		t.Fatal(err)
	}
	a.memoryStoreLocked().entries = map[string][]AgentMemoryEntry{}
	if err := a.loadMemories(); err != nil {
		t.Fatal(err)
	}
	if memoryEntryScope(a.memoryStoreLocked().entries[key][1]) != "agent" || a.memoryStoreLocked().entries[key][1].Scope != "" {
		t.Fatalf("legacy scope was rewritten: %+v", a.memoryStoreLocked().entries[key][1])
	}
	relevant := a.relevantMemoriesFor(project.ID, peer.ID, "Shared persistence fact", 6)
	if len(relevant) != 1 || relevant[0].ID != sharedID {
		t.Fatalf("reloaded shared fact = %+v", relevant)
	}
	for _, entry := range a.relevantMemoriesFor(project.ID, peer.ID, "Legacy private fact", 6) {
		if entry.ID == legacy.ID {
			t.Fatalf("legacy private fact leaked after reload: %+v", entry)
		}
	}
}

func TestProjectFactsShareFactQuotaWithoutDisplacingDecisions(t *testing.T) {
	a, project, author, peer := projectMemoryTestApp(t)
	now := time.Now().UTC()
	for i := 0; i < 8; i++ {
		updated := now.Add(time.Duration(i) * time.Minute)
		a.memoryStoreLocked().entries[projectAgentKey(project.ID, author.ID)] = append(a.memoryStoreLocked().entries[projectAgentKey(project.ID, author.ID)],
			AgentMemoryEntry{ID: "shared-fact-" + string(rune('a'+i)), ProjectID: project.ID, AgentID: author.ID, Layer: "fact", Scope: "project", State: "active", Summary: "Postgres shared fact", Detail: "Postgres shared detail", CreatedAt: updated, UpdatedAt: updated})
		a.memoryStoreLocked().entries[projectAgentKey(project.ID, peer.ID)] = append(a.memoryStoreLocked().entries[projectAgentKey(project.ID, peer.ID)],
			AgentMemoryEntry{ID: "private-decision-" + string(rune('a'+i)), ProjectID: project.ID, AgentID: peer.ID, Layer: "decision", Scope: "agent", State: "active", Summary: "Postgres private decision", Detail: "Postgres decision detail", CreatedAt: updated, UpdatedAt: updated})
	}
	got := a.relevantMemoriesFor(project.ID, peer.ID, "Postgres", 6)
	decisions, facts := 0, 0
	for _, entry := range got {
		switch entry.Layer {
		case "decision":
			decisions++
		case "fact":
			facts++
			if memoryEntryScope(entry) != "project" {
				t.Fatalf("unexpected private fact in quota test: %+v", entry)
			}
		}
	}
	if len(got) != 6 || decisions != 3 || facts != 3 {
		t.Fatalf("project fact quotas = %+v", got)
	}
}
