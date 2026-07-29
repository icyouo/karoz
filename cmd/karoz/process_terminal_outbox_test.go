//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
	processdomain "github.com/karoz/karoz/internal/process"
)

func persistPendingTerminalProcess(
	t *testing.T,
	runtime *processRuntimePersistence,
	project Project,
	id string,
	state processdomain.State,
	exitCode int,
	now time.Time,
) processdomain.Process {
	t.Helper()
	record := processdomain.Process{
		ID: id, ProjectID: project.ID, AgentID: "agent",
		RunID: "run-" + id, Command: "true", Workdir: project.Path,
		State: processdomain.StateStarting, LifetimeMS: 60_000,
		StartedAt: now.Add(-2 * time.Second),
		UpdatedAt: now.Add(-2 * time.Second),
	}
	prepared, err := runtime.PrepareRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	admitRuntimeRecord(t, runtime, prepared)
	prepared.State = processdomain.StateRunning
	prepared.UpdatedAt = now.Add(-time.Second)
	if err := runtime.MarkRunning(prepared); err != nil {
		t.Fatal(err)
	}
	prepared.State = state
	prepared.ExitCode = exitCode
	prepared.UpdatedAt = now
	prepared.EndedAt = timePointer(now)
	if err := runtime.MarkTerminal(prepared); err != nil {
		t.Fatal(err)
	}
	return prepared
}

func persistRunningProcessForOutboxRestart(
	t *testing.T,
	runtime *processRuntimePersistence,
	project Project,
	id string,
	now time.Time,
) processdomain.Process {
	t.Helper()
	record := processdomain.Process{
		ID: id, ProjectID: project.ID, AgentID: "agent",
		RunID: "run-" + id, Command: "sleep 30", Workdir: project.Path,
		State: processdomain.StateStarting, LifetimeMS: 60_000,
		StartedAt: now.Add(-time.Second), UpdatedAt: now.Add(-time.Second),
	}
	prepared, err := runtime.PrepareRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	admitRuntimeRecord(t, runtime, prepared)
	prepared.State = processdomain.StateRunning
	prepared.PID, prepared.GuardPID, prepared.PGID = 101, 101, 101
	prepared.UpdatedAt = now
	if err := runtime.MarkRunning(prepared); err != nil {
		t.Fatal(err)
	}
	return prepared
}

func processTerminalDurableRecord(
	t *testing.T,
	runtime *processRuntimePersistence,
	projectID, processID string,
) (durableProcessRecord, int) {
	t.Helper()
	project := runtime.projectRuntime(projectID)
	if project == nil {
		t.Fatalf("project runtime %s is unavailable", projectID)
	}
	runtime.authorityMu.Lock()
	partition := runtime.authority.Projects[project.identity.SafeProjectKey]
	record, exists := partition.Records[processID]
	runtime.authorityMu.Unlock()
	if !exists {
		t.Fatalf("process %s durable record is unavailable", processID)
	}
	project.ledgerMu.Lock()
	slots := len(project.ledger.Slots)
	project.ledgerMu.Unlock()
	return record, slots
}

func assertProcessTerminalPending(
	t *testing.T,
	runtime *processRuntimePersistence,
	projectID, processID string,
) {
	t.Helper()
	record, slots := processTerminalDurableRecord(
		t,
		runtime,
		projectID,
		processID,
	)
	if record.Event == nil ||
		record.Event.ID != processTerminalEventID(processID) ||
		record.Reservation == nil ||
		record.Reservation.State !=
			monitordomain.ReservationTerminalUnacknowledged ||
		slots != 1 {
		t.Fatalf("pending terminal state = %+v slots=%d", record, slots)
	}
}

func assertProcessTerminalReleased(
	t *testing.T,
	runtime *processRuntimePersistence,
	projectID, processID string,
) {
	t.Helper()
	record, slots := processTerminalDurableRecord(
		t,
		runtime,
		projectID,
		processID,
	)
	if record.Event != nil ||
		record.Reservation != nil ||
		record.AcknowledgedEventID != processTerminalEventID(processID) ||
		slots != 0 {
		t.Fatalf("released terminal state = %+v slots=%d", record, slots)
	}
	project := runtime.projectRuntime(projectID)
	project.journalMu.Lock()
	_, releaseExists := project.journal.Operations["process/"+processID+"/release"]
	project.journalMu.Unlock()
	if releaseExists {
		t.Fatalf("release operation remained for %s", processID)
	}
}

func bootstrapProcessTerminalOutboxApp(
	t *testing.T,
	dataDir, projectsRoot string,
	fail func(processPersistenceFailpoint) error,
) *app {
	t.Helper()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: projectsRoot})
	a.processPersistenceFail = fail
	projects, err := a.scanProjects()
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range projects {
		a.agents[project.ID] = []Agent{
			{ID: "karoz", ProjectID: project.ID, Name: "Karoz"},
			{ID: "agent", ProjectID: project.ID, Name: "Agent"},
		}
	}
	if err := a.loadAgentMessages(); err != nil {
		t.Fatal(err)
	}
	if err := a.bootstrapProcessRuntime(); err != nil {
		t.Fatal(err)
	}
	return a
}

func stopOutboxAppForCrash(a *app) {
	if a != nil && a.supervisorCancel != nil {
		a.supervisorCancel()
	}
}

func TestProcessTerminalOutboxPersistsMessageBeforeAcknowledging(t *testing.T) {
	dataDir := t.TempDir()
	projectsRoot, projects := processRetentionRestartProjects(t, "project")
	seed, err := newProcessRuntimePersistence(dataDir, projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	persistPendingTerminalProcess(
		t,
		seed,
		projects[0],
		"zero-watchers",
		processdomain.StateSucceeded,
		0,
		time.Now().UTC(),
	)
	a := bootstrapProcessTerminalOutboxApp(
		t,
		dataDir,
		projectsRoot,
		nil,
	)
	t.Cleanup(func() { stopOutboxAppForCrash(a) })
	if len(a.runtimeWatchers) != 0 {
		t.Fatal("test unexpectedly has runtime watchers")
	}
	messages := a.agentMessagesFor(projects[0].ID, "agent")
	if len(messages) != 1 || messages[0].ID != processTerminalEventID("zero-watchers") ||
		messages[0].Intent != "process_terminal" ||
		messages[0].Body != "Background process zero-watchers reached succeeded (exit code 0; run run-zero-watchers)." {
		t.Fatalf("durable terminal message = %+v", messages)
	}
	assertProcessTerminalReleased(
		t,
		a.processRuntime,
		projects[0].ID,
		"zero-watchers",
	)
}

func TestProcessTerminalOutboxRetriesSavedMessageBeforeAcknowledging(t *testing.T) {
	dataDir := t.TempDir()
	projectsRoot, projects := processRetentionRestartProjects(t, "project")
	seed, err := newProcessRuntimePersistence(dataDir, projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	persistPendingTerminalProcess(t, seed, projects[0], "retry", processdomain.StateFailed, 9, time.Now().UTC())
	var armed atomic.Bool
	armed.Store(true)
	first := bootstrapProcessTerminalOutboxApp(t, dataDir, projectsRoot, func(point processPersistenceFailpoint) error {
		if armed.Load() && point == processPersistAfterTerminalMessage {
			return errors.New("crash after terminal message save")
		}
		return nil
	})
	assertProcessTerminalPending(t, first.processRuntime, projects[0].ID, "retry")
	if messages := first.agentMessagesFor(projects[0].ID, "agent"); len(messages) != 1 || messages[0].ID != processTerminalEventID("retry") {
		t.Fatalf("message was not durable before crash: %+v", messages)
	}
	stopOutboxAppForCrash(first)
	armed.Store(false)
	second := bootstrapProcessTerminalOutboxApp(t, dataDir, projectsRoot, nil)
	t.Cleanup(func() { stopOutboxAppForCrash(second) })
	assertProcessTerminalReleased(t, second.processRuntime, projects[0].ID, "retry")
	if messages := second.agentMessagesFor(projects[0].ID, "agent"); len(messages) != 1 || messages[0].ID != processTerminalEventID("retry") {
		t.Fatalf("retry duplicated terminal message: %+v", messages)
	}
}

func TestProcessTerminalOutboxUsesKarozAfterOwnerDeletion(t *testing.T) {
	dataDir := t.TempDir()
	projectsRoot, projects := processRetentionRestartProjects(t, "project")
	runtime, err := newProcessRuntimePersistence(dataDir, projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	persistPendingTerminalProcess(t, runtime, projects[0], "deleted-owner", processdomain.StateKilled, 1, time.Now().UTC())
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: projectsRoot})
	a.agents[projects[0].ID] = []Agent{{ID: "karoz", ProjectID: projects[0].ID, Name: "Karoz"}}
	if err := a.bootstrapProcessRuntime(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopOutboxAppForCrash(a) })
	if messages := a.agentMessagesFor(projects[0].ID, "karoz"); len(messages) != 1 || messages[0].ID != processTerminalEventID("deleted-owner") {
		t.Fatalf("Karoz terminal notification = %+v", messages)
	}
	assertProcessTerminalReleased(t, a.processRuntime, projects[0].ID, "deleted-owner")
}

func TestProcessTerminalMessageAdmissionIsIdempotentAndExact(t *testing.T) {
	project := runtimeTestProject(t, "project")
	a := newApp(Settings{DataDir: t.TempDir()})
	a.agents[project.ID] = []Agent{{ID: "agent", ProjectID: project.ID}}
	exitCode := 4
	event := RuntimeEvent{
		ID: processTerminalEventID("exact"), ProjectID: project.ID,
		Kind: processTerminalEventKind, EntityID: "exact", AgentID: "agent",
		RunID: "run-exact", To: string(processdomain.StateFailed), ExitCode: &exitCode,
		Reason: "process_terminal", CreatedAt: time.Now().UTC(),
	}
	if accepted, err := a.admitProcessTerminalMessage(event); err != nil || !accepted {
		t.Fatalf("first terminal admission accepted=%v err=%v", accepted, err)
	}
	if accepted, err := a.admitProcessTerminalMessage(event); err != nil || accepted {
		t.Fatalf("retry terminal admission accepted=%v err=%v", accepted, err)
	}
	changed := event
	changedExitCode := 5
	changed.ExitCode = &changedExitCode
	if _, err := a.admitProcessTerminalMessage(changed); err == nil {
		t.Fatal("changed terminal payload was accepted under stable event ID")
	}
}

func TestProcessTerminalOutboxCyclesReleaseSlotsAndApplyRetention(t *testing.T) {
	dataDir := t.TempDir()
	projectsRoot, projects := processRetentionRestartProjects(t, "project")
	setProcessRetentionEnv(t, 2, 24*time.Hour, 1<<20)
	a := bootstrapProcessTerminalOutboxApp(
		t,
		dataDir,
		projectsRoot,
		nil,
	)
	t.Cleanup(func() { stopOutboxAppForCrash(a) })
	const cycles = 20
	for index := 0; index < cycles; index++ {
		processID := fmt.Sprintf("cycle-%02d", index)
		persistPendingTerminalProcess(
			t,
			a.processRuntime,
			projects[0],
			processID,
			processdomain.StateSucceeded,
			0,
			time.Now().UTC().Add(time.Duration(index)*time.Millisecond),
		)
		a.drainProcessTerminalOutbox()
		record, slots := processTerminalDurableRecord(
			t,
			a.processRuntime,
			projects[0].ID,
			processID,
		)
		if record.Event != nil || record.Reservation != nil || slots != 0 {
			t.Fatalf(
				"cycle %d leaked terminal state: %+v slots=%d",
				index,
				record,
				slots,
			)
		}
	}
	if items := a.processRuntime.List(projects[0].ID); len(items) > 2 {
		t.Fatalf("configured retention retained %d cycles: %+v", len(items), items)
	}
	if messages := a.agentMessagesFor(projects[0].ID, "agent"); len(messages) != cycles {
		t.Fatalf("durable terminal messages retained %d events, want %d", len(messages), cycles)
	}
	project := a.processRuntime.projects[projects[0].ID]
	project.ledgerMu.Lock()
	slots := len(project.ledger.Slots)
	project.ledgerMu.Unlock()
	if slots != 0 {
		t.Fatalf("terminal cycles leaked %d reservation slots", slots)
	}
}

func TestProcessTerminalOutboxHasNoSeparateTerminalCapacityStore(t *testing.T) {
	dataDir := t.TempDir()
	projectsRoot, projects := processRetentionRestartProjects(t, "project")
	a := bootstrapProcessTerminalOutboxApp(t, dataDir, projectsRoot, nil)
	t.Cleanup(func() { stopOutboxAppForCrash(a) })
	key := projectAgentKey(projects[0].ID, "agent")
	a.mu.Lock()
	for index := 0; index < 4096; index++ {
		a.agentMessages[key] = append(a.agentMessages[key], AgentMessage{
			ID: fmt.Sprintf("prior-%04d", index), ProjectID: projects[0].ID,
			AgentID: "agent", Role: "system", Intent: "note", Body: "prior",
			Seq: int64(index + 1), CreatedAt: time.Now().UTC(),
		})
	}
	a.mu.Unlock()
	persistPendingTerminalProcess(t, a.processRuntime, projects[0], "over-4096", processdomain.StateSucceeded, 0, time.Now().UTC())
	a.drainProcessTerminalOutbox()
	assertProcessTerminalReleased(t, a.processRuntime, projects[0].ID, "over-4096")
	if messages := a.agentMessagesFor(projects[0].ID, "agent"); len(messages) != 4097 || messages[len(messages)-1].ID != processTerminalEventID("over-4096") {
		t.Fatalf("terminal message admission kept a separate capacity: %d %+v", len(messages), messages[len(messages)-1])
	}
}

func TestProcessAbortAcceptsTerminalAlreadyReleasedByOutbox(t *testing.T) {
	project := runtimeTestProject(t, "project")
	runtime, err := newProcessRuntimePersistence(t.TempDir(), []Project{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := persistPendingTerminalProcess(
		t,
		runtime,
		project,
		"released-before-abort",
		processdomain.StateFailed,
		1,
		time.Now().UTC(),
	)

	eventID := processTerminalEventID(record.ID)
	if err := runtime.AcknowledgeTerminal(project.ID, record.ID, eventID); err != nil {
		t.Fatalf("acknowledge terminal process before abort: %v", err)
	}
	if err := runtime.Abort(record); err != nil {
		t.Fatalf("abort terminal process already released by outbox: %v", err)
	}
	assertProcessTerminalReleased(t, runtime, project.ID, record.ID)
}

func TestProcessTerminalOutboxPersistsPendingOutputGapBeforeRelease(t *testing.T) {
	dataDir := t.TempDir()
	project := runtimeTestProject(t, "project")
	runtime, err := newProcessRuntimePersistence(
		dataDir,
		[]Project{project},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	record := persistPendingTerminalProcess(
		t,
		runtime,
		project,
		"terminal-gap",
		processdomain.StateSucceeded,
		0,
		time.Now().UTC(),
	)
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: project.Path})
	a.processRuntime = runtime
	a.agents[project.ID] = []Agent{{
		ID: "agent", ProjectID: project.ID, Name: "Agent",
	}}
	t.Cleanup(a.supervisorCancel)
	a.recordProcessOutputGap(project.ID, record.ID, 7)

	a.drainProcessTerminalOutbox()

	assertProcessTerminalReleased(t, runtime, project.ID, record.ID)
	got := runtime.List(project.ID)
	if len(got) != 1 ||
		got[0].OutputLostLines != 1 ||
		got[0].OutputGapCount != 1 ||
		got[0].OutputGapOldestSeq != 7 ||
		got[0].OutputGapNewestSeq != 7 ||
		len(got[0].OutputGaps) != 1 ||
		got[0].OutputGaps[0] != (processdomain.SeqRange{Start: 7, End: 7}) {
		t.Fatalf("terminal release lost pending output gap: %+v", got)
	}
}
