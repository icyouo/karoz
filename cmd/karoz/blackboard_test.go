package main

import (
	"strings"
	"testing"
	"time"
)

func TestBlackboardProjectsRuntimeFactsWithoutBecomingBacklog(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	now := time.Now().UTC()
	a := &app{
		settings:        Settings{DataDir: t.TempDir()},
		agents:          map[string][]Agent{"p1": {{ID: "karoz", ProjectID: "p1", Nickname: "Karoz"}, {ID: "designer", ProjectID: "p1", Nickname: "Designer"}}},
		tasks:           map[string][]Task{"p1": {{ID: "task-1", ProjectID: "p1", Title: "Build mockup", Status: "running", UpdatedAt: now}}},
		inbox:           map[string][]AgentInboxMessage{},
		blackboard:      map[string][]AgentBlackboardEntry{},
		runtimeHooks:    map[string]bool{},
		runtimeWatchers: map[string]map[chan RuntimeEvent]bool{},
	}

	a.emitRuntimeStateChanged(RuntimeEvent{ID: "e1", ProjectID: "p1", Kind: "agent_run_changed", EntityID: "designer", RunID: "run-1", Trigger: "user_direct", To: string(RunStatePreparingContext), CreatedAt: now})
	a.emitRuntimeStateChanged(RuntimeEvent{ID: "e2", ProjectID: "p1", Kind: "agent_run_changed", EntityID: "designer", RunID: "run-1", Trigger: "user_direct", From: string(RunStatePreparingContext), To: string(RunStateDone), CreatedAt: now.Add(time.Second)})
	a.emitRuntimeStateChanged(RuntimeEvent{ID: "e3", ProjectID: "p1", Kind: "task_changed", EntityID: "task-1", To: "running", CreatedAt: now.Add(2 * time.Second)})

	entries := a.blackboardFor("p1", 10)
	if len(entries) != 2 {
		t.Fatalf("runtime projections = %+v", entries)
	}
	var runProjection, taskProjection AgentBlackboardEntry
	for _, entry := range entries {
		switch entry.SourceType {
		case blackboardSourceRun:
			runProjection = entry
		case blackboardSourceTask:
			taskProjection = entry
		}
		if !entry.Derived || entry.RequiresAction || blackboardEntryActionable(entry) {
			t.Fatalf("derived projection became actionable: %+v", entry)
		}
	}
	if runProjection.SourceID != "run-1" || runProjection.Status != string(RunStateDone) || !runProjection.UpdatedAt.After(runProjection.CreatedAt) {
		t.Fatalf("run projection = %+v", runProjection)
	}
	if taskProjection.SourceID != "task-1" || taskProjection.Status != "running" {
		t.Fatalf("task projection = %+v", taskProjection)
	}
	result := a.markBlackboardActivity("p1", Agent{ID: "karoz"}, map[string]any{"activity_id": runProjection.ID, "handling_result": "ignored"})
	if !strings.Contains(result, "derived_projection") {
		t.Fatalf("derived projection was mutable: %s", result)
	}
}

func TestBlackboardHandoffProjectionUpsertsAndStartupRebuilds(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	now := time.Now().UTC()
	a := &app{
		settings:        Settings{DataDir: t.TempDir()},
		agents:          map[string][]Agent{"p1": {{ID: "product", ProjectID: "p1", Nickname: "Product"}, {ID: "designer", ProjectID: "p1", Nickname: "Designer"}}},
		tasks:           map[string][]Task{"p1": {{ID: "task-1", ProjectID: "p1", Title: "Implement design", Status: "done", Result: "complete", CreatedAt: now, UpdatedAt: now}}},
		inbox:           map[string][]AgentInboxMessage{},
		blackboard:      map[string][]AgentBlackboardEntry{},
		runtimeHooks:    map[string]bool{},
		runtimeWatchers: map[string]map[chan RuntimeEvent]bool{},
	}
	msg := AgentInboxMessage{
		ID: "handoff-1", ProjectID: "p1", SourceAgentID: "product", TargetAgentID: "designer",
		CorrelationID: "corr-1", MessageType: "handoff", Subject: "Create mockup", Body: "Design it",
		Objective: "Create mockup", ExpectedOutput: "HTML", Status: HandoffQueued, CreatedAt: now,
	}
	if err := a.queueInboxMessage("p1", msg); err != nil {
		t.Fatal(err)
	}
	before, _ := a.inboxMessage("p1", "designer", msg.ID)
	if before.Status != HandoffDelivered {
		t.Fatalf("delivered handoff = %+v", before)
	}
	if _, ok := a.transitionHandoff("p1", "designer", msg.ID, HandoffReplied, "mockup ready"); !ok {
		t.Fatal("handoff reply transition failed")
	}
	entries := a.blackboardFor("p1", 10)
	if len(entries) != 1 || entries[0].SourceType != blackboardSourceHandoff || entries[0].Status != HandoffReplied || !strings.Contains(entries[0].Detail, "mockup ready") {
		t.Fatalf("handoff projection = %+v", entries)
	}

	manual := a.appendBlackboardEntry("p1", Agent{ID: "product", Nickname: "Product"}, "decision_needed", "Choose launch date", "Needs user decision", "")
	if err := a.rebuildBlackboardProjections(); err != nil {
		t.Fatal(err)
	}
	entries = a.blackboardFor("p1", 10)
	if len(entries) != 3 {
		t.Fatalf("rebuilt projections = %+v", entries)
	}
	manualFound := false
	for _, entry := range entries {
		if entry.ID == manual.ID && !entry.Derived && entry.SourceType == blackboardSourceAgentReport {
			manualFound = true
		}
	}
	if !manualFound {
		t.Fatalf("manual report was not retained: %+v", entries)
	}
	if signals := a.unhandledBlackboardSignals("p1", 10); len(signals) != 1 || signals[0].ID != manual.ID {
		t.Fatalf("only manual actionable report should be backlog: %+v", signals)
	}
}
