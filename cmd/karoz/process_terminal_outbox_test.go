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

func TestProcessTerminalOutboxDoesNotAcknowledgeWithoutDurableSink(
	t *testing.T,
) {
	dataDir := t.TempDir()
	_, projects := processRetentionRestartProjects(t, "project")
	runtime, err := newProcessRuntimePersistence(dataDir, projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	persistPendingTerminalProcess(
		t,
		runtime,
		projects[0],
		"no-sink",
		processdomain.StateFailed,
		7,
		time.Now().UTC(),
	)
	a := newApp(Settings{DataDir: dataDir})
	a.processRuntime = runtime
	a.drainProcessTerminalOutbox()
	assertProcessTerminalPending(t, runtime, projects[0].ID, "no-sink")
}

func TestProcessTerminalOutboxBackpressureAndProjectIsolation(t *testing.T) {
	dataDir := t.TempDir()
	projectsRoot, projects := processRetentionRestartProjects(
		t,
		"project-a",
		"project-b",
	)
	seed, err := newProcessRuntimePersistence(dataDir, projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for index, project := range projects {
		persistPendingTerminalProcess(
			t,
			seed,
			project,
			fmt.Sprintf("terminal-%d", index),
			processdomain.StateSucceeded,
			0,
			now,
		)
	}
	var blocked atomic.Bool
	blocked.Store(true)
	a := bootstrapProcessTerminalOutboxApp(
		t,
		dataDir,
		projectsRoot,
		func(point processPersistenceFailpoint) error {
			if blocked.Load() && point == processPersistBeforeRuntimeEventSink {
				return errors.New("runtime sink backpressure")
			}
			return nil
		},
	)
	t.Cleanup(func() { stopOutboxAppForCrash(a) })
	for index, project := range projects {
		assertProcessTerminalPending(
			t,
			a.processRuntime,
			project.ID,
			fmt.Sprintf("terminal-%d", index),
		)
	}
	if events := a.processEventSink.Pending(projects[0].ID); len(events) != 0 {
		t.Fatalf("backpressured sink accepted events: %+v", events)
	}

	projectA := a.processRuntime.projects[projects[0].ID]
	a.processRuntime.authorityMu.Lock()
	partitionA := a.processRuntime.authority.Projects[projectA.identity.SafeProjectKey]
	corrupt := partitionA.Records["terminal-0"]
	corrupt.Event.ExitCode++
	partitionA.Records["terminal-0"] = corrupt
	a.processRuntime.authority.Projects[projectA.identity.SafeProjectKey] = partitionA
	a.processRuntime.authorityMu.Unlock()
	blocked.Store(false)
	a.drainProcessTerminalOutbox()
	assertProcessTerminalPending(
		t,
		a.processRuntime,
		projects[0].ID,
		"terminal-0",
	)
	assertProcessTerminalReleased(
		t,
		a.processRuntime,
		projects[1].ID,
		"terminal-1",
	)
	if !a.processRuntime.projectDisabled(projectA.identity.SafeProjectKey) {
		t.Fatal("corrupt terminal outbox project was not disabled")
	}
	events := a.processEventSink.Pending(projects[1].ID)
	if len(events) != 1 || events[0].EntityID != "terminal-1" {
		t.Fatalf("isolated durable sink events = %+v", events)
	}
}

func TestProcessTerminalOutboxStartupRecoversInterruptedProcess(t *testing.T) {
	dataDir := t.TempDir()
	projectsRoot, projects := processRetentionRestartProjects(t, "project")
	seed, err := newProcessRuntimePersistence(dataDir, projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	persistRunningProcessForOutboxRestart(
		t,
		seed,
		projects[0],
		"interrupted",
		time.Now().UTC(),
	)
	a := bootstrapProcessTerminalOutboxApp(
		t,
		dataDir,
		projectsRoot,
		nil,
	)
	t.Cleanup(func() { stopOutboxAppForCrash(a) })
	events := a.processEventSink.Pending(projects[0].ID)
	if len(events) != 1 {
		t.Fatalf("startup durable terminal events = %+v", events)
	}
	event := events[0]
	if event.ID != processTerminalEventID("interrupted") ||
		event.ProjectID != projects[0].ID ||
		event.AgentID != "agent" ||
		event.EntityID != "interrupted" ||
		event.RunID != "run-interrupted" ||
		event.To != string(processdomain.StateInterrupted) ||
		event.ExitCode == nil ||
		*event.ExitCode != 0 {
		t.Fatalf("startup terminal provenance = %+v", event)
	}
	assertProcessTerminalReleased(
		t,
		a.processRuntime,
		projects[0].ID,
		"interrupted",
	)

	reloaded, err := newProcessRuntimeEventSink(dataDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	reloadedEvents := reloaded.Pending(projects[0].ID)
	if len(reloadedEvents) != 1 ||
		reloadedEvents[0].ID != event.ID {
		t.Fatalf("restarted future-consumer events = %+v", reloadedEvents)
	}
}

func TestProcessTerminalOutboxCrashRecoveryBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		failpoint processPersistenceFailpoint
	}{
		{name: "before durable sink", failpoint: processPersistBeforeRuntimeEventSink},
		{name: "after durable sink", failpoint: processPersistAfterRuntimeEventSink},
		{name: "after acknowledgement", failpoint: processPersistAfterAck},
		{name: "during release", failpoint: processPersistAfterAuthorityDetach},
		{name: "after ledger release", failpoint: processPersistAfterLedgerRelease},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataDir := t.TempDir()
			projectsRoot, projects := processRetentionRestartProjects(t, "project")
			seed, err := newProcessRuntimePersistence(dataDir, projects, nil)
			if err != nil {
				t.Fatal(err)
			}
			processID := "terminal"
			persistPendingTerminalProcess(
				t,
				seed,
				projects[0],
				processID,
				processdomain.StateFailed,
				9,
				time.Now().UTC(),
			)
			var armed atomic.Bool
			armed.Store(true)
			first := bootstrapProcessTerminalOutboxApp(
				t,
				dataDir,
				projectsRoot,
				func(point processPersistenceFailpoint) error {
					if armed.Load() && point == test.failpoint {
						return errors.New("injected terminal delivery crash")
					}
					return nil
				},
			)
			stopOutboxAppForCrash(first)
			armed.Store(false)

			second := bootstrapProcessTerminalOutboxApp(
				t,
				dataDir,
				projectsRoot,
				nil,
			)
			t.Cleanup(func() { stopOutboxAppForCrash(second) })
			assertProcessTerminalReleased(
				t,
				second.processRuntime,
				projects[0].ID,
				processID,
			)
			events := second.processEventSink.Pending(projects[0].ID)
			if len(events) != 1 {
				t.Fatalf("stable durable terminal events = %+v", events)
			}
			event := events[0]
			if event.ProjectID != projects[0].ID ||
				event.AgentID != "agent" ||
				event.EntityID != processID ||
				event.To != string(processdomain.StateFailed) ||
				event.ExitCode == nil ||
				*event.ExitCode != 9 {
				t.Fatalf("stable terminal event = %+v", event)
			}
		})
	}
}

func TestProcessTerminalOutboxScopesDeliveryIdentityByProject(t *testing.T) {
	dataDir := t.TempDir()
	projectsRoot, projects := processRetentionRestartProjects(
		t,
		"project-a",
		"project-b",
	)
	seed, err := newProcessRuntimePersistence(dataDir, projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, project := range projects {
		persistPendingTerminalProcess(
			t,
			seed,
			project,
			"same-process-id",
			processdomain.StateSucceeded,
			0,
			now,
		)
	}
	a := bootstrapProcessTerminalOutboxApp(
		t,
		dataDir,
		projectsRoot,
		nil,
	)
	t.Cleanup(func() { stopOutboxAppForCrash(a) })
	for _, project := range projects {
		events := a.processEventSink.Pending(project.ID)
		if len(events) != 1 ||
			events[0].ID != processTerminalEventID("same-process-id") {
			t.Fatalf("project %s durable events = %+v", project.ID, events)
		}
		assertProcessTerminalReleased(
			t,
			a.processRuntime,
			project.ID,
			"same-process-id",
		)
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
	if events := a.processEventSink.Pending(projects[0].ID); len(events) != cycles {
		t.Fatalf("durable sink retained %d events, want %d", len(events), cycles)
	}
	project := a.processRuntime.projects[projects[0].ID]
	project.ledgerMu.Lock()
	slots := len(project.ledger.Slots)
	project.ledgerMu.Unlock()
	if slots != 0 {
		t.Fatalf("terminal cycles leaked %d reservation slots", slots)
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
