package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func freshMemoryLifecycleApp(t *testing.T) (*app, Project, Agent) {
	t.Helper()
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	a, project, agent := newMemoryGateTestApp(t)
	a.memoryStoreLocked().entries[projectAgentKey(project.ID, agent.ID)] = nil
	return a, project, agent
}

func decodeMemoryCreateResult(t *testing.T, raw string) (string, bool) {
	t.Helper()
	var result struct {
		Entry struct {
			ID string `json:"id"`
		} `json:"entry"`
		Deduplicated bool `json:"deduplicated"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode create memory result: %v\n%s", err, raw)
	}
	return result.Entry.ID, result.Deduplicated
}

func TestMemoryExactDeduplicationByLayer(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	key := projectAgentKey(project.ID, agent.ID)
	for _, layer := range []string{"fact", "decision", "done", "pending"} {
		t.Run(layer, func(t *testing.T) {
			metadata := map[string]any(nil)
			if layer == "decision" {
				metadata = map[string]any{"rationale": "  Prefer   SIMPLE  behavior "}
			}
			first, err := a.createMemory(project.ID, agent.ID, layer, map[string]any{
				"summary": "  Shared   Summary ",
				"detail":  "Line ONE",
			}, 10, metadata)
			if err != nil {
				t.Fatal(err)
			}
			firstID, deduplicated := decodeMemoryCreateResult(t, first)
			if firstID == "" || deduplicated {
				t.Fatalf("first create = %s", first)
			}
			var firstEntry AgentMemoryEntry
			for _, entry := range a.memoryStoreLocked().entries[key] {
				if entry.ID == firstID {
					firstEntry = entry
				}
			}

			duplicateMetadata := map[string]any(nil)
			if layer == "decision" {
				duplicateMetadata = map[string]any{"rationale": "prefer simple behavior"}
			}
			duplicate, err := a.createMemory(project.ID, agent.ID, layer, map[string]any{
				"summary": "shared summary",
				"detail":  " line   one ",
			}, 10, duplicateMetadata)
			if err != nil {
				t.Fatal(err)
			}
			duplicateID, deduplicated := decodeMemoryCreateResult(t, duplicate)
			if !deduplicated || duplicateID != firstID {
				t.Fatalf("duplicate create = %s, first id = %s", duplicate, firstID)
			}
			var matching []AgentMemoryEntry
			for _, entry := range a.memoryStoreLocked().entries[key] {
				if entry.Layer == layer {
					matching = append(matching, entry)
				}
			}
			if len(matching) != 1 || !matching[0].CreatedAt.Equal(firstEntry.CreatedAt) ||
				!matching[0].UpdatedAt.Equal(firstEntry.UpdatedAt) || matching[0].Priority != 10 {
				t.Fatalf("dedup changed existing entry: before=%+v after=%+v", firstEntry, matching)
			}
		})
	}
}

func TestPendingPriorityIsPartOfExactIdentity(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	args := map[string]any{"summary": "Review release", "detail": "Check the release candidate."}
	first, err := a.createMemory(project.ID, agent.ID, "pending", args, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstID, _ := decodeMemoryCreateResult(t, first)
	second, err := a.createMemory(project.ID, agent.ID, "pending", args, 99, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondID, deduplicated := decodeMemoryCreateResult(t, second)
	if deduplicated || secondID == firstID {
		t.Fatalf("changed priority was silently deduplicated: first=%s second=%s", first, second)
	}
	entries := a.memoryStoreLocked().entries[projectAgentKey(project.ID, agent.ID)]
	if len(entries) != 2 || entries[0].Priority != 10 || entries[1].Priority != 99 {
		t.Fatalf("pending priority identities = %+v", entries)
	}
}

func TestDecisionDedupIncludesRationale(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	args := map[string]any{"summary": "Database choice", "detail": "Use Postgres."}
	if _, err := a.createMemory(project.ID, agent.ID, "decision", args, 0, map[string]any{"rationale": "Durability"}); err != nil {
		t.Fatal(err)
	}
	second, err := a.createMemory(project.ID, agent.ID, "decision", args, 0, map[string]any{"rationale": "Operational familiarity"})
	if err != nil {
		t.Fatal(err)
	}
	_, deduplicated := decodeMemoryCreateResult(t, second)
	if deduplicated {
		t.Fatalf("different rationale was deduplicated: %s", second)
	}
	if got := len(a.memoryStoreLocked().entries[projectAgentKey(project.ID, agent.ID)]); got != 2 {
		t.Fatalf("decision count = %d, want 2", got)
	}
}

func TestMemoryConcurrentExactDeduplication(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	results := make(chan string, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := a.createMemory(project.ID, agent.ID, "fact", map[string]any{
				"summary": "Concurrent fact",
				"detail":  "Only one entry may exist.",
			}, 0, nil)
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Fatal(err)
	}
	var onlyID string
	for result := range results {
		id, _ := decodeMemoryCreateResult(t, result)
		if onlyID == "" {
			onlyID = id
		}
		if id != onlyID {
			t.Fatalf("concurrent dedup returned ids %q and %q", onlyID, id)
		}
	}
	entries := a.memoryStoreLocked().entries[projectAgentKey(project.ID, agent.ID)]
	if len(entries) != 1 || entries[0].ID != onlyID {
		t.Fatalf("concurrent entries = %+v", entries)
	}
}

func TestDedupIgnoresInactiveHistoricalEntries(t *testing.T) {
	t.Run("dropped pending", func(t *testing.T) {
		a, project, agent := freshMemoryLifecycleApp(t)
		args := map[string]any{"summary": "Retry deploy", "detail": "Retry after credentials rotate."}
		first, err := a.createMemory(project.ID, agent.ID, "pending", args, 7, nil)
		if err != nil {
			t.Fatal(err)
		}
		firstID, _ := decodeMemoryCreateResult(t, first)
		if result := a.dropPendingMemory(project.ID, agent.ID, firstID); !json.Valid([]byte(result)) {
			t.Fatalf("drop result = %s", result)
		}
		second, err := a.createMemory(project.ID, agent.ID, "pending", args, 7, nil)
		if err != nil {
			t.Fatal(err)
		}
		secondID, deduplicated := decodeMemoryCreateResult(t, second)
		if deduplicated || secondID == firstID {
			t.Fatalf("dropped pending absorbed re-add: first=%s second=%s", first, second)
		}
		active := a.activeMemoriesFor(project.ID, agent.ID, "pending", 10)
		if len(active) != 1 || active[0].ID != secondID {
			t.Fatalf("active pending after re-add = %+v", active)
		}
	})

	for _, layer := range []string{"fact", "decision"} {
		t.Run("archived "+layer, func(t *testing.T) {
			a, project, agent := freshMemoryLifecycleApp(t)
			now := time.Now().UTC()
			archivedAt := now
			metadata := map[string]any(nil)
			if layer == "decision" {
				metadata = map[string]any{"rationale": "Original rationale"}
			}
			key := projectAgentKey(project.ID, agent.ID)
			a.memoryStoreLocked().entries[key] = []AgentMemoryEntry{{
				ID: "archived-" + layer, ProjectID: project.ID, AgentID: agent.ID, Layer: layer, State: "archived",
				Summary: "Historical entry", Detail: "Historical detail", Metadata: metadata,
				CreatedAt: now, UpdatedAt: now, ArchivedAt: &archivedAt,
			}}
			result, err := a.createMemory(project.ID, agent.ID, layer, map[string]any{
				"summary": "Historical entry", "detail": "Historical detail",
			}, 0, metadata)
			if err != nil {
				t.Fatal(err)
			}
			id, deduplicated := decodeMemoryCreateResult(t, result)
			if deduplicated || id == "archived-"+layer || len(a.memoryStoreLocked().entries[key]) != 2 ||
				a.memoryStoreLocked().entries[key][1].State != "active" {
				t.Fatalf("archived %s absorbed re-add: result=%s entries=%+v", layer, result, a.memoryStoreLocked().entries[key])
			}
		})
	}
}

func TestSupersededPredecessorCannotAbsorbLaterCreate(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	originalArgs := map[string]any{"summary": "Original decision", "detail": "Use SQLite."}
	originalMetadata := map[string]any{"rationale": "Small footprint"}
	first, err := a.createMemory(project.ID, agent.ID, "decision", originalArgs, 0, originalMetadata)
	if err != nil {
		t.Fatal(err)
	}
	firstID, _ := decodeMemoryCreateResult(t, first)
	if _, err := a.createMemory(project.ID, agent.ID, "decision", map[string]any{
		"summary": "Replacement decision", "detail": "Use Postgres.", "supersedes_id": firstID,
	}, 0, map[string]any{"rationale": "Concurrent writes"}); err != nil {
		t.Fatal(err)
	}
	readded, err := a.createMemory(project.ID, agent.ID, "decision", originalArgs, 0, originalMetadata)
	if err != nil {
		t.Fatal(err)
	}
	readdedID, deduplicated := decodeMemoryCreateResult(t, readded)
	if deduplicated || readdedID == firstID {
		t.Fatalf("superseded predecessor absorbed later create: first=%s readded=%s", first, readded)
	}
	active := a.activeMemoriesFor(project.ID, agent.ID, "decision", 10)
	if len(active) != 2 {
		t.Fatalf("active decisions after re-add = %+v", active)
	}
}

func TestDecisionSupersessionIsAtomicAndLinked(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	now := time.Now().UTC().Add(-time.Hour)
	key := projectAgentKey(project.ID, agent.ID)
	a.memoryStoreLocked().entries[key] = []AgentMemoryEntry{{
		ID: "old-decision", ProjectID: project.ID, AgentID: agent.ID, Layer: "decision", State: "active",
		Summary: "Legacy database choice", Detail: "Use SQLite.", Metadata: map[string]any{"rationale": "Small footprint"},
		CreatedAt: now, UpdatedAt: now,
	}}
	result, err := a.createMemory(project.ID, agent.ID, "decision", map[string]any{
		"summary":       "Current database choice",
		"detail":        "Use Postgres.",
		"supersedes_id": "old-decision",
	}, 0, map[string]any{"rationale": "Concurrent writes"})
	if err != nil {
		t.Fatal(err)
	}
	successorID, deduplicated := decodeMemoryCreateResult(t, result)
	if successorID == "" || deduplicated {
		t.Fatalf("successor result = %s", result)
	}
	entries := a.memoryStoreLocked().entries[key]
	if len(entries) != 2 || entries[0].State != "archived" || entries[0].ArchivedAt == nil ||
		entries[0].SupersededByID != successorID || entries[1].SupersedesID != "old-decision" ||
		entries[1].State != "active" {
		t.Fatalf("supersession entries = %+v", entries)
	}
	summary := memorySummary(entries[0])
	if summary["superseded_by_id"] != successorID || memorySummary(entries[1])["supersedes_id"] != "old-decision" {
		t.Fatalf("summary links missing: predecessor=%+v successor=%+v", summary, memorySummary(entries[1]))
	}
}

func TestRecordDecisionToolAcceptsSupersedesID(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	toolCtx := ResidentToolContext{Project: project, Agent: agent, Workdir: project.Path}
	first, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{
		Name:      "record_decision",
		Arguments: `{"summary":"Initial choice","detail":"Use SQLite.","rationale":"Small footprint"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstID, _ := decodeMemoryCreateResult(t, first)
	arguments, err := json.Marshal(map[string]any{
		"summary": "Replacement choice", "detail": "Use Postgres.", "rationale": "Concurrent writes",
		"supersedes_id": firstID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{
		Name: "record_decision", Arguments: string(arguments),
	}); err != nil {
		t.Fatal(err)
	}
	entries := a.memoryStoreLocked().entries[projectAgentKey(project.ID, agent.ID)]
	if len(entries) != 2 || entries[0].State != "archived" || entries[1].SupersedesID != firstID {
		t.Fatalf("record_decision supersession = %+v", entries)
	}
}

func TestDecisionSupersessionSurvivesReload(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	first, err := a.createMemory(project.ID, agent.ID, "decision", map[string]any{
		"summary": "Legacy storage architecture", "detail": "Use SQLite.",
	}, 0, map[string]any{"rationale": "Small footprint"})
	if err != nil {
		t.Fatal(err)
	}
	predecessorID, _ := decodeMemoryCreateResult(t, first)
	second, err := a.createMemory(project.ID, agent.ID, "decision", map[string]any{
		"summary": "Current storage architecture", "detail": "Use Postgres.", "supersedes_id": predecessorID,
	}, 0, map[string]any{"rationale": "Concurrent writes"})
	if err != nil {
		t.Fatal(err)
	}
	successorID, _ := decodeMemoryCreateResult(t, second)

	a.memoryStoreLocked().entries = map[string][]AgentMemoryEntry{}
	if err := a.loadMemories(); err != nil {
		t.Fatal(err)
	}
	entries := a.memoryStoreLocked().entries[projectAgentKey(project.ID, agent.ID)]
	if len(entries) != 2 {
		t.Fatalf("reloaded supersession entries = %+v", entries)
	}
	predecessor, successor := entries[0], entries[1]
	if predecessor.ID != predecessorID || predecessor.State != "archived" || predecessor.ArchivedAt == nil ||
		predecessor.SupersededByID != successorID || successor.ID != successorID || successor.State != "active" ||
		successor.SupersedesID != predecessorID {
		t.Fatalf("reloaded supersession links = %+v", entries)
	}
	if memorySummary(predecessor)["superseded_by_id"] != successorID ||
		memorySummary(successor)["supersedes_id"] != predecessorID {
		t.Fatalf("reloaded memory summaries lost links: predecessor=%+v successor=%+v", memorySummary(predecessor), memorySummary(successor))
	}
	for _, entry := range a.relevantMemoriesFor(project.ID, agent.ID, "Legacy storage architecture", 6) {
		if entry.ID == predecessorID {
			t.Fatalf("reloaded predecessor was proactive: %+v", entry)
		}
	}
	var archiveResult struct {
		MemoryEntries []struct {
			ID string `json:"id"`
		} `json:"memory_entries"`
	}
	if err := json.Unmarshal([]byte(a.searchArchive(project.ID, agent.ID, "Legacy storage architecture", 10)), &archiveResult); err != nil {
		t.Fatal(err)
	}
	foundPredecessor := false
	for _, entry := range archiveResult.MemoryEntries {
		if entry.ID == predecessorID {
			foundPredecessor = true
		}
	}
	if !foundPredecessor {
		t.Fatalf("reloaded predecessor missing from archive search: %+v", archiveResult.MemoryEntries)
	}
}

func TestDecisionSupersessionRejectsInvalidTargetsWithoutMutation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		target string
	}{
		{"cross layer", "fact-target"},
		{"stale", "stale-target"},
		{"cross agent", "foreign-target"},
		{"missing", "missing-target"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, project, agent := freshMemoryLifecycleApp(t)
			now := time.Now().UTC()
			archivedAt := now
			key := projectAgentKey(project.ID, agent.ID)
			a.memoryStoreLocked().entries[key] = []AgentMemoryEntry{
				{ID: "active-target", ProjectID: project.ID, AgentID: agent.ID, Layer: "decision", State: "active", Summary: "Active", Detail: "Active", CreatedAt: now, UpdatedAt: now},
				{ID: "fact-target", ProjectID: project.ID, AgentID: agent.ID, Layer: "fact", State: "active", Summary: "Fact", Detail: "Fact", CreatedAt: now, UpdatedAt: now},
				{ID: "stale-target", ProjectID: project.ID, AgentID: agent.ID, Layer: "decision", State: "archived", Summary: "Stale", Detail: "Stale", CreatedAt: now, UpdatedAt: now, ArchivedAt: &archivedAt},
			}
			foreignKey := projectAgentKey(project.ID, "other-agent")
			a.memoryStoreLocked().entries[foreignKey] = []AgentMemoryEntry{{
				ID: "foreign-target", ProjectID: project.ID, AgentID: "other-agent", Layer: "decision", State: "active",
				Summary: "Foreign", Detail: "Foreign", CreatedAt: now, UpdatedAt: now,
			}}
			deleteAgentSessionForTest(a, key)
			before := map[string][]AgentMemoryEntry{
				key:        append([]AgentMemoryEntry{}, a.memoryStoreLocked().entries[key]...),
				foreignKey: append([]AgentMemoryEntry{}, a.memoryStoreLocked().entries[foreignKey]...),
			}
			beforeSessions := map[string]AgentSessionState{}
			for sessionKey, session := range agentSessionsForTest(a) {
				beforeSessions[sessionKey] = session
			}
			result, err := a.createMemory(project.ID, agent.ID, "decision", map[string]any{
				"summary": "Replacement", "detail": "Replacement", "supersedes_id": tt.target,
			}, 0, map[string]any{"rationale": "Test"})
			if err != nil {
				t.Fatal(err)
			}
			if !json.Valid([]byte(result)) || !reflect.DeepEqual(a.memoryStoreLocked().entries, before) ||
				!reflect.DeepEqual(agentSessionsForTest(a), beforeSessions) {
				t.Fatalf("invalid supersession drifted state: result=%s memories_before=%+v memories_after=%+v sessions_before=%+v sessions_after=%+v", result, before, a.memoryStoreLocked().entries, beforeSessions, agentSessionsForTest(a))
			}
		})
	}
}

func TestDecisionSupersessionRejectsCrossProjectTargetWithoutMutation(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	now := time.Now().UTC()
	currentKey := projectAgentKey(project.ID, agent.ID)
	foreignKey := projectAgentKey("other-project", agent.ID)
	a.memoryStoreLocked().entries[currentKey] = []AgentMemoryEntry{}
	a.memoryStoreLocked().entries[foreignKey] = []AgentMemoryEntry{{
		ID: "foreign-project-decision", ProjectID: "other-project", AgentID: agent.ID,
		Layer: "decision", State: "active", Summary: "Foreign project choice", Detail: "Keep unchanged.",
		CreatedAt: now, UpdatedAt: now,
	}}
	deleteAgentSessionForTest(a, currentKey)
	beforeMemories := map[string][]AgentMemoryEntry{}
	for key, entries := range a.memoryStoreLocked().entries {
		beforeMemories[key] = append([]AgentMemoryEntry{}, entries...)
	}
	beforeSessions := map[string]AgentSessionState{}
	for key, session := range agentSessionsForTest(a) {
		beforeSessions[key] = session
	}
	result, err := a.createMemory(project.ID, agent.ID, "decision", map[string]any{
		"summary": "Invalid replacement", "detail": "Must not be created.",
		"supersedes_id": "foreign-project-decision",
	}, 0, map[string]any{"rationale": "Invalid cross-project request"})
	if err != nil {
		t.Fatal(err)
	}
	var rejection struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(result), &rejection); err != nil {
		t.Fatal(err)
	}
	if rejection.Error != "invalid_supersession" || !reflect.DeepEqual(a.memoryStoreLocked().entries, beforeMemories) ||
		!reflect.DeepEqual(agentSessionsForTest(a), beforeSessions) {
		t.Fatalf("cross-project supersession drifted state: result=%s memories_before=%+v memories_after=%+v sessions_before=%+v sessions_after=%+v", result, beforeMemories, a.memoryStoreLocked().entries, beforeSessions, agentSessionsForTest(a))
	}
}

func TestDecisionSupersessionSaveFailureRollsBack(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	now := time.Now().UTC()
	key := projectAgentKey(project.ID, agent.ID)
	a.memoryStoreLocked().entries[key] = []AgentMemoryEntry{{
		ID: "old-decision", ProjectID: project.ID, AgentID: agent.ID, Layer: "decision", State: "active",
		Summary: "Old", Detail: "Old detail", CreatedAt: now, UpdatedAt: now,
	}}
	deleteAgentSessionForTest(a, key)
	before := append([]AgentMemoryEntry{}, a.memoryStoreLocked().entries[key]...)
	beforeSessions := map[string]AgentSessionState{}
	for sessionKey, session := range agentSessionsForTest(a) {
		beforeSessions[sessionKey] = session
	}
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	a.settings.DataDir = blocked
	if _, err := a.createMemory(project.ID, agent.ID, "decision", map[string]any{
		"summary": "New", "detail": "New detail", "supersedes_id": "old-decision",
	}, 0, map[string]any{"rationale": "New rationale"}); err == nil {
		t.Fatal("supersession unexpectedly survived persistence failure")
	}
	if !reflect.DeepEqual(a.memoryStoreLocked().entries[key], before) {
		t.Fatalf("save failure changed memory: before=%+v after=%+v", before, a.memoryStoreLocked().entries[key])
	}
	if !reflect.DeepEqual(agentSessionsForTest(a), beforeSessions) {
		t.Fatalf("save failure changed sessions: before=%+v after=%+v", beforeSessions, agentSessionsForTest(a))
	}
}

func TestMemoryReloadIsBackwardCompatibleWithoutSupersessionLinks(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	oldJSON := map[string][]map[string]any{
		projectAgentKey(project.ID, agent.ID): {{
			"id": "legacy", "project_id": project.ID, "agent_id": agent.ID, "session_id": "session",
			"layer": "decision", "state": "active", "priority": 0, "summary": "Legacy",
			"detail": "Legacy detail", "created_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
		}},
	}
	if err := writeJSONFileAtomic(filepath.Join(a.settings.DataDir, "agent-memory.json"), oldJSON, 0644); err != nil {
		t.Fatal(err)
	}
	a.memoryStoreLocked().entries = map[string][]AgentMemoryEntry{}
	if err := a.loadMemories(); err != nil {
		t.Fatal(err)
	}
	entry := a.memoryStoreLocked().entries[projectAgentKey(project.ID, agent.ID)][0]
	if entry.SupersedesID != "" || entry.SupersededByID != "" {
		t.Fatalf("legacy reload invented links: %+v", entry)
	}
}

func TestSupersededDecisionIsArchiveSearchableButNotProactive(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	now := time.Now().UTC()
	archivedAt := now
	key := projectAgentKey(project.ID, agent.ID)
	a.memoryStoreLocked().entries[key] = []AgentMemoryEntry{
		{ID: "old-decision", ProjectID: project.ID, AgentID: agent.ID, Layer: "decision", State: "archived", Summary: "Legacy database choice", Detail: "Use SQLite.", SupersededByID: "new-decision", CreatedAt: now, UpdatedAt: now, ArchivedAt: &archivedAt},
		{ID: "new-decision", ProjectID: project.ID, AgentID: agent.ID, Layer: "decision", State: "active", Summary: "Current database choice", Detail: "Use Postgres.", SupersedesID: "old-decision", CreatedAt: now, UpdatedAt: now},
	}
	for _, entry := range a.relevantMemoriesFor(project.ID, agent.ID, "Legacy database choice", 6) {
		if entry.ID == "old-decision" {
			t.Fatalf("superseded decision was proactive: %+v", entry)
		}
	}
	var result struct {
		MemoryEntries []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"memory_entries"`
	}
	if err := json.Unmarshal([]byte(a.searchArchive(project.ID, agent.ID, "Legacy database choice", 10)), &result); err != nil {
		t.Fatal(err)
	}
	foundOld := false
	for _, entry := range result.MemoryEntries {
		if entry.ID == "old-decision" && entry.State == "archived" {
			foundOld = true
		}
	}
	if !foundOld {
		t.Fatalf("archive result = %+v", result.MemoryEntries)
	}
}

func TestRelevantMemoriesUseIndependentLayerQuotasAndExcludeDone(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	now := time.Now().UTC()
	key := projectAgentKey(project.ID, agent.ID)
	for i := 0; i < 8; i++ {
		updated := now.Add(time.Duration(i) * time.Minute)
		a.memoryStoreLocked().entries[key] = append(a.memoryStoreLocked().entries[key],
			AgentMemoryEntry{ID: "decision-" + string(rune('a'+i)), ProjectID: project.ID, AgentID: agent.ID, Layer: "decision", State: "active", Summary: "Postgres choice", Detail: "Postgres decision", CreatedAt: updated, UpdatedAt: updated},
			AgentMemoryEntry{ID: "fact-" + string(rune('a'+i)), ProjectID: project.ID, AgentID: agent.ID, Layer: "fact", State: "active", Priority: 100, Summary: "Postgres fact", Detail: "Postgres fact", CreatedAt: updated, UpdatedAt: updated},
			AgentMemoryEntry{ID: "done-" + string(rune('a'+i)), ProjectID: project.ID, AgentID: agent.ID, Layer: "done", State: "active", Priority: 100, Summary: "Postgres done", Detail: "Postgres done", CreatedAt: updated, UpdatedAt: updated},
		)
	}
	got := a.relevantMemoriesFor(project.ID, agent.ID, "Postgres", 6)
	decisions, facts := 0, 0
	for _, entry := range got {
		switch entry.Layer {
		case "decision":
			decisions++
		case "fact":
			facts++
		case "done":
			t.Fatalf("done memory was proactive: %+v", entry)
		}
	}
	if len(got) != 6 || decisions != 3 || facts != 3 {
		t.Fatalf("layer quotas result = %+v", got)
	}
	var archiveResult struct {
		MemoryEntries []struct {
			Layer string `json:"layer"`
		} `json:"memory_entries"`
	}
	if err := json.Unmarshal([]byte(a.searchArchive(project.ID, agent.ID, "Postgres done", 20)), &archiveResult); err != nil {
		t.Fatal(err)
	}
	foundDone := false
	for _, entry := range archiveResult.MemoryEntries {
		if entry.Layer == "done" {
			foundDone = true
			break
		}
	}
	if !foundDone {
		t.Fatalf("done memory was not archive-searchable: %+v", archiveResult.MemoryEntries)
	}
}

func TestActiveMemoriesSortPendingPriorityBeforeRecencyAndLimit(t *testing.T) {
	a, project, agent := freshMemoryLifecycleApp(t)
	now := time.Now().UTC()
	key := projectAgentKey(project.ID, agent.ID)
	a.memoryStoreLocked().entries[key] = []AgentMemoryEntry{
		{ID: "recent-low", ProjectID: project.ID, AgentID: agent.ID, Layer: "pending", State: "active", Priority: 1, CreatedAt: now, UpdatedAt: now},
		{ID: "older-high", ProjectID: project.ID, AgentID: agent.ID, Layer: "pending", State: "active", Priority: 9, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
		{ID: "newer-high", ProjectID: project.ID, AgentID: agent.ID, Layer: "pending", State: "active", Priority: 9, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)},
	}
	got := a.activeMemoriesFor(project.ID, agent.ID, "pending", 2)
	if len(got) != 2 || got[0].ID != "newer-high" || got[1].ID != "older-high" {
		t.Fatalf("pending priority order = %+v", got)
	}
}

func TestActiveMemoriesIgnorePriorityOutsidePendingLayer(t *testing.T) {
	for _, layer := range []string{"fact", "decision"} {
		t.Run(layer, func(t *testing.T) {
			a, project, agent := freshMemoryLifecycleApp(t)
			now := time.Now().UTC()
			key := projectAgentKey(project.ID, agent.ID)
			a.memoryStoreLocked().entries[key] = []AgentMemoryEntry{
				{ID: "older-high", ProjectID: project.ID, AgentID: agent.ID, Layer: layer, State: "active", Priority: 100, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
				{ID: "newer-low", ProjectID: project.ID, AgentID: agent.ID, Layer: layer, State: "active", Priority: 0, CreatedAt: now, UpdatedAt: now},
			}
			got := a.activeMemoriesFor(project.ID, agent.ID, layer, 2)
			if len(got) != 2 || got[0].ID != "newer-low" || got[1].ID != "older-high" {
				t.Fatalf("%s memories used pending priority: %+v", layer, got)
			}
		})
	}

	t.Run("all layers", func(t *testing.T) {
		a, project, agent := freshMemoryLifecycleApp(t)
		now := time.Now().UTC()
		key := projectAgentKey(project.ID, agent.ID)
		a.memoryStoreLocked().entries[key] = []AgentMemoryEntry{
			{ID: "older-pending-high", ProjectID: project.ID, AgentID: agent.ID, Layer: "pending", State: "active", Priority: 100, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
			{ID: "newer-fact-low", ProjectID: project.ID, AgentID: agent.ID, Layer: "fact", State: "active", Priority: 0, CreatedAt: now, UpdatedAt: now},
		}
		got := a.activeMemoriesFor(project.ID, agent.ID, "", 2)
		if len(got) != 2 || got[0].ID != "newer-fact-low" || got[1].ID != "older-pending-high" {
			t.Fatalf("all-layer memories used pending priority: %+v", got)
		}
	})
}
