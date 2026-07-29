//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"errors"
	"fmt"
	"sync"
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

func setProcessTerminalSink(
	a *app,
	sink func(RuntimeEvent) error,
) {
	a.processTerminalSinkMu.Lock()
	a.processTerminalSink = sink
	a.processTerminalSinkMu.Unlock()
}

func setProcessTerminalAfterDeliveryHook(
	a *app,
	hook func(RuntimeEvent) error,
) {
	a.processTerminalSinkMu.Lock()
	a.processTerminalAfterDeliveryHook = hook
	a.processTerminalSinkMu.Unlock()
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

type idempotentRuntimeEventSink struct {
	mu       sync.Mutex
	attempts []RuntimeEvent
	visible  map[string]RuntimeEvent
}

func newIdempotentRuntimeEventSink() *idempotentRuntimeEventSink {
	return &idempotentRuntimeEventSink{
		visible: map[string]RuntimeEvent{},
	}
}

func (sink *idempotentRuntimeEventSink) Accept(event RuntimeEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.attempts = append(sink.attempts, event)
	key := processTerminalDeliveryKey(event.ProjectID, event.ID)
	if _, exists := sink.visible[key]; !exists {
		sink.visible[key] = event
	}
	return nil
}

func (sink *idempotentRuntimeEventSink) Counts() (int, int) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return len(sink.attempts), len(sink.visible)
}

func bootstrapProcessTerminalOutboxApp(
	t *testing.T,
	dataDir, projectsRoot string,
	sink func(RuntimeEvent) error,
	fail func(processPersistenceFailpoint) error,
	afterDelivery func(RuntimeEvent) error,
) *app {
	t.Helper()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: projectsRoot})
	a.processPersistenceFail = fail
	setProcessTerminalSink(a, sink)
	setProcessTerminalAfterDeliveryHook(a, afterDelivery)
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
	persistPendingTerminalProcess(
		t, seed, projects[0], "blocked", processdomain.StateFailed, 7, now,
	)
	persistPendingTerminalProcess(
		t, seed, projects[1], "accepted", processdomain.StateSucceeded, 0, now,
	)

	var delivered []RuntimeEvent
	sinkErr := errors.New("runtime sink backpressure")
	a := bootstrapProcessTerminalOutboxApp(
		t,
		dataDir,
		projectsRoot,
		func(event RuntimeEvent) error {
			if event.ProjectID == projects[0].ID {
				return sinkErr
			}
			delivered = append(delivered, event)
			return nil
		},
		nil,
		nil,
	)
	t.Cleanup(func() { stopOutboxAppForCrash(a) })
	assertProcessTerminalPending(
		t, a.processRuntime, projects[0].ID, "blocked",
	)
	assertProcessTerminalReleased(
		t, a.processRuntime, projects[1].ID, "accepted",
	)
	if len(delivered) != 1 ||
		delivered[0].ProjectID != projects[1].ID ||
		delivered[0].EntityID != "accepted" {
		t.Fatalf("isolated delivery = %+v", delivered)
	}

	persistPendingTerminalProcess(
		t,
		a.processRuntime,
		projects[1],
		"accepted-after-corruption",
		processdomain.StateSucceeded,
		0,
		now.Add(time.Second),
	)
	projectA := a.processRuntime.projects[projects[0].ID]
	a.processRuntime.authorityMu.Lock()
	partitionA := a.processRuntime.authority.Projects[projectA.identity.SafeProjectKey]
	corrupt := partitionA.Records["blocked"]
	corrupt.Event.ExitCode++
	partitionA.Records["blocked"] = corrupt
	a.processRuntime.authority.Projects[projectA.identity.SafeProjectKey] = partitionA
	a.processRuntime.authorityMu.Unlock()
	setProcessTerminalSink(a, func(event RuntimeEvent) error {
		if event.ProjectID == projects[0].ID {
			t.Fatalf("corrupt project event was delivered: %+v", event)
		}
		delivered = append(delivered, event)
		return nil
	})
	a.drainProcessTerminalOutbox()
	assertProcessTerminalPending(
		t, a.processRuntime, projects[0].ID, "blocked",
	)
	assertProcessTerminalReleased(
		t,
		a.processRuntime,
		projects[1].ID,
		"accepted-after-corruption",
	)
	if !a.processRuntime.projectDisabled(projectA.identity.SafeProjectKey) {
		t.Fatal("corrupt terminal outbox project was not disabled")
	}
	if len(delivered) != 2 ||
		delivered[1].EntityID != "accepted-after-corruption" {
		t.Fatalf("corrupt-project isolation delivery = %+v", delivered)
	}
}

func TestProcessTerminalOutboxStartupRecoversInterruptedProcess(t *testing.T) {
	dataDir := t.TempDir()
	projectsRoot, projects := processRetentionRestartProjects(t, "project")
	seed, err := newProcessRuntimePersistence(dataDir, projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	persistRunningProcessForOutboxRestart(
		t,
		seed,
		projects[0],
		"interrupted",
		now,
	)
	var events []RuntimeEvent
	a := bootstrapProcessTerminalOutboxApp(
		t,
		dataDir,
		projectsRoot,
		func(event RuntimeEvent) error {
			events = append(events, event)
			return nil
		},
		nil,
		nil,
	)
	t.Cleanup(func() { stopOutboxAppForCrash(a) })
	if len(events) != 1 {
		t.Fatalf("startup terminal events = %+v", events)
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
}

func TestProcessTerminalOutboxCrashRecoveryBoundaries(t *testing.T) {
	tests := []struct {
		name              string
		failpoint         processPersistenceFailpoint
		failDelivery      bool
		failAfterDelivery bool
		wantAttempts      int
	}{
		{
			name: "before delivery", failDelivery: true,
			wantAttempts: 1,
		},
		{
			name:              "after delivery before acknowledgement",
			failAfterDelivery: true,
			wantAttempts:      2,
		},
		{
			name:         "after acknowledgement",
			failpoint:    processPersistAfterAck,
			wantAttempts: 1,
		},
		{
			name:         "during release",
			failpoint:    processPersistAfterAuthorityDetach,
			wantAttempts: 1,
		},
		{
			name:         "after ledger release",
			failpoint:    processPersistAfterLedgerRelease,
			wantAttempts: 1,
		},
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
			sink := newIdempotentRuntimeEventSink()
			sinkFn := sink.Accept
			if test.failDelivery {
				sinkFn = func(RuntimeEvent) error {
					return errors.New("delivery unavailable")
				}
			}
			var fail func(processPersistenceFailpoint) error
			if test.failpoint != "" {
				fail = func(point processPersistenceFailpoint) error {
					if point == test.failpoint {
						return errors.New("injected release crash")
					}
					return nil
				}
			}
			var afterDelivery func(RuntimeEvent) error
			if test.failAfterDelivery {
				afterDelivery = func(RuntimeEvent) error {
					return errors.New("crash after accepted delivery")
				}
			}
			first := bootstrapProcessTerminalOutboxApp(
				t,
				dataDir,
				projectsRoot,
				sinkFn,
				fail,
				afterDelivery,
			)
			if test.failDelivery || test.failAfterDelivery {
				assertProcessTerminalPending(
					t,
					first.processRuntime,
					projects[0].ID,
					processID,
				)
			}
			stopOutboxAppForCrash(first)

			second := bootstrapProcessTerminalOutboxApp(
				t,
				dataDir,
				projectsRoot,
				sink.Accept,
				nil,
				nil,
			)
			t.Cleanup(func() { stopOutboxAppForCrash(second) })
			assertProcessTerminalReleased(
				t,
				second.processRuntime,
				projects[0].ID,
				processID,
			)
			attempts, visible := sink.Counts()
			if attempts != test.wantAttempts || visible != 1 {
				t.Fatalf(
					"delivery attempts=%d visible=%d, want %d/1",
					attempts,
					visible,
					test.wantAttempts,
				)
			}
			sink.mu.Lock()
			event := sink.visible[processTerminalDeliveryKey(
				projects[0].ID,
				processTerminalEventID(processID),
			)]
			sink.mu.Unlock()
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
	sink := newIdempotentRuntimeEventSink()
	failFirstPostDelivery := true
	a := bootstrapProcessTerminalOutboxApp(
		t,
		dataDir,
		projectsRoot,
		sink.Accept,
		nil,
		func(RuntimeEvent) error {
			if failFirstPostDelivery {
				failFirstPostDelivery = false
				return errors.New("pause after first accepted delivery")
			}
			return nil
		},
	)
	t.Cleanup(func() { stopOutboxAppForCrash(a) })
	attempts, visible := sink.Counts()
	if attempts != 2 || visible != 2 {
		t.Fatalf(
			"project-scoped initial delivery attempts=%d visible=%d, want 2/2",
			attempts,
			visible,
		)
	}
	setProcessTerminalAfterDeliveryHook(a, nil)
	a.drainProcessTerminalOutbox()
	attempts, visible = sink.Counts()
	if attempts != 2 || visible != 2 {
		t.Fatalf(
			"project-scoped retry attempts=%d visible=%d, want 2/2",
			attempts,
			visible,
		)
	}
	for _, project := range projects {
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
	sink := newIdempotentRuntimeEventSink()
	a := bootstrapProcessTerminalOutboxApp(
		t,
		dataDir,
		projectsRoot,
		sink.Accept,
		nil,
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
	attempts, visible := sink.Counts()
	if attempts != cycles || visible != cycles {
		t.Fatalf(
			"cycle delivery attempts=%d visible=%d, want %d",
			attempts,
			visible,
			cycles,
		)
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
