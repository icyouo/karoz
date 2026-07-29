package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
	processdomain "github.com/karoz/karoz/internal/process"
)

func TestRuntimeMonitorFreezesAndDeliversBlackboardAction(t *testing.T) {
	root := t.TempDir()
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	path := filepath.Join(root, "p1")
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: projectID(path), Name: "p1", Path: path}
	a.agents[project.ID] = []Agent{{ID: "owner", ProjectID: project.ID, Nickname: "Owner"}}
	item, err := a.createMonitor(project, Monitor{ID: "m1", AgentID: "owner", Name: "task updates", Trigger: monitordomain.Trigger{Kind: monitordomain.TriggerRuntimeEvent, EventKinds: []string{"task_changed"}}, Action: monitordomain.Action{Kind: monitordomain.ActionBlackboard, Topic: "task update", Template: "task changed"}})
	if err != nil {
		t.Fatal(err)
	}
	a.emitRuntimeStateChanged(RuntimeEvent{ID: "event-1", ProjectID: project.ID, Kind: "task_changed", EntityID: "task-1", To: "done", CreatedAt: time.Now().UTC()})
	if got := a.monitorsForProject(project.ID)[0].PendingFires; len(got) != 0 {
		t.Fatalf("pending fire was not delivered: %+v", got)
	}
	entries := a.blackboardFor(project.ID, 20)
	var delivered int
	for _, entry := range entries {
		if entry.SourceType == "monitor" && entry.SourceID == monitordomain.StableFireID(item.ID, "event-1") {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("monitor delivery count=%d entries=%+v", delivered, entries)
	}
	for _, entry := range entries {
		if entry.SourceType == "monitor" && entry.Detail != "task changed" {
			t.Fatalf("configured action template was not delivered: %+v", entry)
		}
	}
	// Stable fire identity makes a duplicate runtime emission idempotent.
	a.emitRuntimeStateChanged(RuntimeEvent{ID: "event-1", ProjectID: project.ID, Kind: "task_changed", EntityID: "task-1", To: "done", CreatedAt: time.Now().UTC()})
	entries = a.blackboardFor(project.ID, 20)
	delivered = 0
	for _, entry := range entries {
		if entry.SourceType == "monitor" {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("duplicate monitor delivery: %+v", entries)
	}
}

func TestProcessExitMonitorMatchesOnlyTerminalProcess(t *testing.T) {
	root := t.TempDir()
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	path := filepath.Join(root, "p1")
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: projectID(path), Name: "p1", Path: path}
	a.agents[project.ID] = []Agent{{ID: "owner", ProjectID: project.ID, Nickname: "Owner"}}
	_, err := a.createMonitor(project, Monitor{ID: "m-exit", AgentID: "owner", Name: "exit", Trigger: monitordomain.Trigger{Kind: monitordomain.TriggerProcessExit, ProcessID: "proc-1", FailureOnly: true}, Action: monitordomain.Action{Kind: monitordomain.ActionBlackboard, Topic: "process failed"}})
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	a.emitRuntimeStateChanged(RuntimeEvent{ID: "ordinary", ProjectID: project.ID, Kind: "process_changed", EntityID: "proc-1", ExitCode: &zero, To: "exited", CreatedAt: time.Now().UTC()})
	for _, entry := range a.blackboardFor(project.ID, 10) {
		if entry.SourceType == "monitor" {
			t.Fatalf("non-terminal process matched: %+v", entry)
		}
	}
	code := 1
	a.emitRuntimeStateChanged(RuntimeEvent{ID: "terminal", ProjectID: project.ID, Kind: processTerminalEventKind, EntityID: "proc-1", ExitCode: &code, To: "failed", Reason: "process_terminal", CreatedAt: time.Now().UTC()})
	matched := false
	for _, entry := range a.blackboardFor(project.ID, 10) {
		matched = matched || entry.SourceType == "monitor"
	}
	if !matched {
		t.Fatalf("terminal process did not match")
	}
}

func TestDeletingOwnerDisablesAndClearsMonitorWork(t *testing.T) {
	root := t.TempDir()
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	path := filepath.Join(root, "p1")
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: projectID(path), Name: "p1", Path: path}
	a.agents[project.ID] = []Agent{{ID: "karoz", ProjectID: project.ID}, {ID: "owner", ProjectID: project.ID}}
	if _, err := a.createMonitor(project, Monitor{ID: "m-delete", AgentID: "owner", Name: "owner", Trigger: monitordomain.Trigger{Kind: monitordomain.TriggerRuntimeEvent, EventKinds: []string{"task_changed"}}, Action: monitordomain.Action{Kind: monitordomain.ActionBlackboard, Topic: "task"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.deleteProjectAgent(project, "owner"); err != nil {
		t.Fatal(err)
	}
	got := a.monitorsForProject(project.ID)
	if len(got) != 1 || got[0].State != monitordomain.StateDisabled || got[0].ErrorCode != "owner_deleted" || len(got[0].PendingFires) != 0 {
		t.Fatalf("deleted owner monitor = %+v", got)
	}
	// Recreating the ID must not reactivate a monitor created by the deleted
	// owner identity.
	a.agents[project.ID] = append(a.agents[project.ID], Agent{ID: "owner", ProjectID: project.ID})
	if _, err := a.setMonitorState(project, "m-delete", monitordomain.StateActive); err == nil {
		t.Fatal("recreated owner resumed owner-deleted monitor")
	}
}

func TestGate3CreateMonitorRejectsUnavailableTriggerAndMissingTarget(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	project := Project{ID: "p1", Name: "p1", Path: t.TempDir()}
	a.agents[project.ID] = []Agent{{ID: "owner", ProjectID: project.ID}}
	if _, err := a.createMonitor(project, Monitor{AgentID: "owner", Name: "output", Trigger: monitordomain.Trigger{Kind: monitordomain.TriggerProcessOutput, ProcessID: "p", Pattern: "x"}, Action: monitordomain.Action{Kind: monitordomain.ActionBlackboard, Topic: "x"}}); err == nil {
		t.Fatal("process_output was accepted in Gate3")
	}
	if _, err := a.createMonitor(project, Monitor{AgentID: "owner", Name: "target", Trigger: monitordomain.Trigger{Kind: monitordomain.TriggerRuntimeEvent, EventKinds: []string{"task_changed"}}, Action: monitordomain.Action{Kind: monitordomain.ActionNotifyAgent, AgentID: "missing", TurnType: "ask"}}); err == nil {
		t.Fatal("missing notify target was accepted")
	}
}

func TestProcessOutputMonitorUsesRedactedCompleteSequence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "p1")
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	projectID := projectID(path)
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	a.agents[projectID] = []Agent{{ID: "owner", ProjectID: projectID}}
	now := time.Now().UTC()
	a.monitors[projectID] = []Monitor{{ID: "output", ProjectID: projectID, AgentID: "owner", Name: "output", Revision: 1, State: monitordomain.StateActive, Trigger: monitordomain.Trigger{Kind: monitordomain.TriggerProcessOutput, Revision: 1, ProcessID: "proc", Pattern: "Authorization"}, Action: monitordomain.Action{Revision: 1, Kind: monitordomain.ActionBlackboard, Topic: "output", Template: "matched output"}, CreatedAt: now, UpdatedAt: now}}
	a.evaluateProcessOutput(processOutputObservation{ProjectID: projectID, ProcessID: "proc", Line: processdomain.OutputLine{Sequence: 1, Stream: "stderr", Text: "Authorization: Bearer secret-token"}})
	entries := a.blackboardFor(projectID, 10)
	for _, entry := range entries {
		if strings.Contains(entry.Detail, "secret-token") {
			t.Fatalf("raw process secret leaked: %+v", entry)
		}
	}
	if len(entries) != 1 || entries[0].Detail != "matched output" {
		t.Fatalf("output monitor result = %+v", entries)
	}
	// The supervisor sequence is the event identity: duplicate handoff must not fire again.
	a.evaluateProcessOutput(processOutputObservation{ProjectID: projectID, ProcessID: "proc", Line: processdomain.OutputLine{Sequence: 1, Text: "token"}})
	if got := a.blackboardFor(projectID, 10); len(got) != 1 {
		t.Fatalf("duplicate output sequence fired: %+v", got)
	}
}
