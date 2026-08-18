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
	a.agentDirectoryLocked().agents[project.ID] = []Agent{{ID: "owner", ProjectID: project.ID, Nickname: "Owner"}}
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
	a.agentDirectoryLocked().agents[project.ID] = []Agent{{ID: "owner", ProjectID: project.ID, Nickname: "Owner"}}
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
	a.agentDirectoryLocked().agents[project.ID] = []Agent{{ID: "karoz", ProjectID: project.ID}, {ID: "owner", ProjectID: project.ID}}
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
	a.agentDirectoryLocked().agents[project.ID] = append(a.agentDirectoryLocked().agents[project.ID], Agent{ID: "owner", ProjectID: project.ID})
	if _, err := a.setMonitorState(project, "m-delete", monitordomain.StateActive); err == nil {
		t.Fatal("recreated owner resumed owner-deleted monitor")
	}
}

func TestGate3CreateMonitorRejectsUnavailableTriggerAndMissingTarget(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	project := Project{ID: "p1", Name: "p1", Path: t.TempDir()}
	a.agentDirectoryLocked().agents[project.ID] = []Agent{{ID: "owner", ProjectID: project.ID}}
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
	a.agentDirectoryLocked().agents[projectID] = []Agent{{ID: "owner", ProjectID: projectID}}
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

func TestProcessOutputGapAccumulatorKeepsBoundedExactRanges(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	t.Cleanup(a.supervisorCancel)

	a.recordProcessOutputGap("project", "process", 1)
	a.recordProcessOutputGap("project", "process", 2)
	for sequence := uint64(4); sequence <= 68; sequence += 2 {
		a.recordProcessOutputGap("project", "process", sequence)
	}

	pending := a.takeProcessOutputGapDeltas()
	delta := pending[projectAgentKey("project", "process")]
	if delta.LostLines != 35 || delta.GapCount != 34 {
		t.Fatalf("gap totals = lost %d count %d", delta.LostLines, delta.GapCount)
	}
	if delta.OldestSeq != 1 || delta.NewestSeq != 68 {
		t.Fatalf("gap summary = %d..%d", delta.OldestSeq, delta.NewestSeq)
	}
	if len(delta.Recent) != 32 {
		t.Fatalf("recent ranges = %d, want 32", len(delta.Recent))
	}
	if delta.Recent[0] != (processdomain.SeqRange{Start: 6, End: 6}) ||
		delta.Recent[31] != (processdomain.SeqRange{Start: 68, End: 68}) {
		t.Fatalf("recent ranges lost exact boundaries: %+v", delta.Recent)
	}
	for _, gap := range delta.Recent {
		if gap.Start != gap.End {
			t.Fatalf("successful sequence was bridged by a gap: %+v", gap)
		}
	}
}

func TestProcessOutputGapDiagnosticRespectsIndependentBaseline(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	now := time.Now().UTC()
	a.monitors["project"] = []Monitor{
		{
			ID: "old", ProjectID: "project", AgentID: "owner",
			Name: "old", State: monitordomain.StateActive,
			Trigger: monitordomain.Trigger{
				Kind: monitordomain.TriggerProcessOutput, ProcessID: "process",
			},
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "new", ProjectID: "project", AgentID: "owner",
			Name: "new", State: monitordomain.StateActive,
			Trigger: monitordomain.Trigger{
				Kind: monitordomain.TriggerProcessOutput, ProcessID: "process",
			},
			CreatedAt: now, UpdatedAt: now,
		},
	}
	a.processOutputRuntime.baselines[projectAgentKey("project", "old")] = 5
	a.processOutputRuntime.baselines[projectAgentKey("project", "new")] = 20
	delta := processOutputGapDelta{
		ProjectID: "project", ProcessID: "process",
		Recent:    []processdomain.SeqRange{{Start: 8, End: 10}},
		LostLines: 3, GapCount: 1, OldestSeq: 8, NewestSeq: 10,
	}
	if err := a.saveProcessOutputGapDiagnostics(delta); err != nil {
		t.Fatal(err)
	}
	got := a.monitorsForProject("project")
	if got[0].ErrorCode != "output_gap" ||
		!strings.Contains(got[0].LastError, "lost 3 line") {
		t.Fatalf("old subscription diagnostic = %+v", got[0])
	}
	if got[1].ErrorCode != "" || got[1].LastError != "" {
		t.Fatalf("new subscription inherited an old gap: %+v", got[1])
	}
}

func TestProcessOutputGapSaveFailureReturnsDeltaForRetry(t *testing.T) {
	root := t.TempDir()
	dataFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(dataFile, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	a := newApp(Settings{DataDir: dataFile, ProjectsRoot: root})
	t.Cleanup(a.supervisorCancel)
	now := time.Now().UTC()
	a.monitors["project"] = []Monitor{{
		ID: "monitor", ProjectID: "project", AgentID: "owner",
		Name: "monitor", State: monitordomain.StateActive,
		Trigger: monitordomain.Trigger{
			Kind: monitordomain.TriggerProcessOutput, ProcessID: "process",
		},
		CreatedAt: now, UpdatedAt: now,
	}}
	a.recordProcessOutputGap("project", "process", 9)
	a.drainProcessOutputGaps()

	pending := a.takeProcessOutputGapDeltas()
	delta, ok := pending[projectAgentKey("project", "process")]
	if !ok || delta.LostLines != 1 || delta.GapCount != 1 ||
		len(delta.Recent) != 1 || delta.Recent[0].Start != 9 {
		t.Fatalf("failed save did not preserve retry delta: %+v", pending)
	}
	if got := a.monitorsForProject("project")[0]; got.ErrorCode != "" {
		t.Fatalf("failed save published monitor mutation: %+v", got)
	}
}

func TestProcessOutputCursorIsIsolatedByProcess(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	a.agentDirectoryLocked().agents["project"] = []Agent{{ID: "owner", ProjectID: "project"}}
	now := time.Now().UTC()
	for _, target := range []string{"process-a", "process-b"} {
		a.monitors["project"] = append(a.monitors["project"], Monitor{
			ID: target, ProjectID: "project", AgentID: "owner",
			Name: target, Revision: 1, State: monitordomain.StateActive,
			Trigger: monitordomain.Trigger{
				Kind: monitordomain.TriggerProcessOutput, Revision: 1,
				ProcessID: target, Pattern: "matched",
			},
			Action: monitordomain.Action{
				Kind: monitordomain.ActionBlackboard, Revision: 1,
				Topic: target, Template: target,
			},
			CreatedAt: now, UpdatedAt: now,
		})
	}
	a.evaluateProcessOutput(processOutputObservation{
		ProjectID: "project", ProcessID: "process-a",
		Line: processdomain.OutputLine{Sequence: 100, Text: "matched"},
	})
	a.evaluateProcessOutput(processOutputObservation{
		ProjectID: "project", ProcessID: "process-b",
		Line: processdomain.OutputLine{Sequence: 1, Text: "matched"},
	})
	monitors := a.monitorsForProject("project")
	if monitors[0].TriggerCount != 1 || monitors[1].TriggerCount != 1 {
		t.Fatalf("cross-process cursor skipped a monitor: %+v", monitors)
	}
	keyA := projectAgentKey("project", "process-a")
	keyB := projectAgentKey("project", "process-b")
	if a.processOutputRuntime.cursors[keyA] != 100 ||
		a.processOutputRuntime.cursors[keyB] != 1 {
		t.Fatalf("isolated cursors = %v", a.processOutputRuntime.cursors)
	}
}

func TestProcessOutputLaterSuccessDoesNotHideEarlierGap(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	now := time.Now().UTC()
	a.monitors["project"] = []Monitor{{
		ID: "monitor", ProjectID: "project", AgentID: "owner",
		Name: "monitor", Revision: 1, State: monitordomain.StateActive,
		Trigger: monitordomain.Trigger{
			Kind: monitordomain.TriggerProcessOutput, Revision: 1,
			ProcessID: "process", Pattern: "success",
		},
		Action: monitordomain.Action{
			Kind: monitordomain.ActionBlackboard, Revision: 1,
			Topic: "output", Template: "success",
		},
		CreatedAt: now, UpdatedAt: now,
	}}
	key := projectAgentKey("project", "monitor")
	a.processOutputRuntime.baselines[key] = 0
	a.processOutputRuntime.cursors[key] = 0
	a.evaluateProcessOutput(processOutputObservation{
		ProjectID: "project", ProcessID: "process",
		Line: processdomain.OutputLine{Sequence: 2, Text: "success"},
	})
	if a.processOutputRuntime.cursors[key] != 2 ||
		a.processOutputRuntime.baselines[key] != 0 {
		t.Fatalf(
			"cursor/baseline = %d/%d",
			a.processOutputRuntime.cursors[key],
			a.processOutputRuntime.baselines[key],
		)
	}
	if err := a.saveProcessOutputGapDiagnostics(processOutputGapDelta{
		ProjectID: "project", ProcessID: "process",
		Recent:    []processdomain.SeqRange{{Start: 1, End: 1}},
		LostLines: 1, GapCount: 1, OldestSeq: 1, NewestSeq: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if got := a.monitorsForProject("project")[0]; got.ErrorCode != "output_gap" {
		t.Fatalf("later success hid earlier gap: %+v", got)
	}
}

func TestProcessOutputUnmatchedLineDoesNotSaveMonitorRegistry(t *testing.T) {
	root := t.TempDir()
	dataFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(dataFile, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	a := newApp(Settings{DataDir: dataFile, ProjectsRoot: root})
	now := time.Now().UTC()
	a.monitors["project"] = []Monitor{{
		ID: "monitor", ProjectID: "project", AgentID: "owner",
		Name: "monitor", Revision: 1, State: monitordomain.StateActive,
		Trigger: monitordomain.Trigger{
			Kind: monitordomain.TriggerProcessOutput, Revision: 1,
			ProcessID: "process", Pattern: "wanted",
		},
		CreatedAt: now, UpdatedAt: now,
	}}
	a.evaluateProcessOutput(processOutputObservation{
		ProjectID: "project", ProcessID: "process",
		Line: processdomain.OutputLine{Sequence: 1, Text: "unmatched"},
	})
	key := projectAgentKey("project", "monitor")
	if a.processOutputRuntime.cursors[key] != 1 {
		t.Fatalf("unmatched cursor = %d", a.processOutputRuntime.cursors[key])
	}
	if pending := a.takeProcessOutputGapDeltas(); len(pending) != 0 {
		t.Fatalf("unmatched line attempted a monitor save: %+v", pending)
	}
}

func TestProcessOutputMonitorSaveFailureRecordsCoverageGap(t *testing.T) {
	root := t.TempDir()
	dataFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(dataFile, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	a := newApp(Settings{DataDir: dataFile, ProjectsRoot: root})
	t.Cleanup(a.supervisorCancel)
	now := time.Now().UTC()
	a.monitors["project"] = []Monitor{{
		ID: "monitor", ProjectID: "project", AgentID: "owner",
		Name: "monitor", Revision: 1, State: monitordomain.StateActive,
		Trigger: monitordomain.Trigger{
			Kind: monitordomain.TriggerProcessOutput, Revision: 1,
			ProcessID: "process", Pattern: "matched",
		},
		Action: monitordomain.Action{
			Kind: monitordomain.ActionBlackboard, Revision: 1,
			Topic: "output", Template: "matched",
		},
		CreatedAt: now, UpdatedAt: now,
	}}
	a.evaluateProcessOutput(processOutputObservation{
		ProjectID: "project", ProcessID: "process",
		Line: processdomain.OutputLine{Sequence: 4, Text: "matched"},
	})
	pending := a.takeProcessOutputGapDeltas()
	delta, ok := pending[projectAgentKey("project", "process")]
	if !ok || delta.LostLines != 1 || delta.GapCount != 1 ||
		delta.OldestSeq != 4 || delta.NewestSeq != 4 {
		t.Fatalf("monitor save failure coverage = %+v", pending)
	}
	if got := a.monitorsForProject("project")[0]; got.TriggerCount != 0 {
		t.Fatalf("failed monitor save published mutation: %+v", got)
	}
}
