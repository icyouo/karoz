//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
	processdomain "github.com/karoz/karoz/internal/process"
)

func runtimeTestProject(t *testing.T, id string) Project {
	t.Helper()
	path := t.TempDir()
	return Project{ID: id, Name: id, Path: path}
}

func runtimeStartingRecord(
	t *testing.T,
	runtime *processRuntimePersistence,
	projectID, id string,
) processdomain.Process {
	t.Helper()
	now := time.Now().UTC()
	record := processdomain.Process{
		ID: id, ProjectID: projectID, AgentID: "agent", Command: "true",
		Workdir: t.TempDir(), State: processdomain.StateStarting,
		LifetimeMS: 60_000, StartedAt: now, UpdatedAt: now,
	}
	prepared, err := runtime.PrepareRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func admitRuntimeRecord(
	t *testing.T,
	runtime *processRuntimePersistence,
	record processdomain.Process,
) {
	t.Helper()
	if err := runtime.Reserve(record); err != nil {
		t.Fatal(err)
	}
	if err := runtime.CreateStarting(record); err != nil {
		t.Fatal(err)
	}
}

func TestProcessRuntimeAdmissionLifecycleAndProjectIsolation(t *testing.T) {
	dataDir := t.TempDir()
	projectA := runtimeTestProject(t, "project-a")
	projectB := runtimeTestProject(t, "project-b")
	runtime, err := newProcessRuntimePersistence(dataDir, []Project{projectA, projectB}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []Project{projectA, projectB} {
		record := runtimeStartingRecord(t, runtime, project.ID, "same-id")
		admitRuntimeRecord(t, runtime, record)
		record.State = processdomain.StateRunning
		record.PID, record.GuardPID, record.PGID = 100, 100, 100
		record.UpdatedAt = record.UpdatedAt.Add(time.Second)
		if err := runtime.MarkRunning(record); err != nil {
			t.Fatal(err)
		}
		record.State = processdomain.StateSucceeded
		record.ExitCode = 0
		record.PID, record.GuardPID, record.PGID = 0, 0, 0
		record.UpdatedAt = record.UpdatedAt.Add(time.Second)
		record.EndedAt = timePointer(record.UpdatedAt)
		if err := runtime.MarkTerminal(record); err != nil {
			t.Fatal(err)
		}
	}
	for _, project := range []Project{projectA, projectB} {
		items := runtime.List(project.ID)
		if len(items) != 1 || items[0].ID != "same-id" || items[0].State != processdomain.StateSucceeded {
			t.Fatalf("%s records = %+v", project.ID, items)
		}
		state := runtime.projects[project.ID]
		if len(state.ledger.Slots) != 1 {
			t.Fatalf("%s ledger occupancy = %d", project.ID, len(state.ledger.Slots))
		}
	}
	if runtime.projects[projectA.ID].identity.SafeProjectKey ==
		runtime.projects[projectB.ID].identity.SafeProjectKey {
		t.Fatal("projects shared a safe key")
	}
}

func TestProcessRuntimeAdmissionCrashRecoveryEveryStep(t *testing.T) {
	points := []processPersistenceFailpoint{
		processPersistAfterIntent,
		processPersistAfterTokenSelection,
		processPersistAfterLedgerAllocate,
		processPersistAfterAllocatedOperation,
		processPersistAfterAuthorityCreate,
		processPersistAfterAuthorityOperation,
		processPersistAfterLedgerActivate,
		processPersistAfterActiveOperation,
		processPersistAfterAuthorityActive,
		processPersistAfterCommit,
	}
	for _, point := range points {
		t.Run(string(point), func(t *testing.T) {
			dataDir := t.TempDir()
			project := runtimeTestProject(t, "project-"+string(point))
			var fired atomic.Bool
			runtime, err := newProcessRuntimePersistence(
				dataDir,
				[]Project{project},
				func(candidate processPersistenceFailpoint) error {
					if candidate == point && fired.CompareAndSwap(false, true) {
						return errors.New("crash")
					}
					return nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			record := runtimeStartingRecord(t, runtime, project.ID, "process")
			if err := runtime.Reserve(record); err == nil {
				t.Fatal("failpoint did not interrupt admission")
			}
			recovered, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
			if err != nil {
				t.Fatalf("recover %s: %v", point, err)
			}
			items := recovered.List(project.ID)
			if point == processPersistAfterIntent ||
				point == processPersistAfterTokenSelection ||
				point == processPersistAfterLedgerAllocate ||
				point == processPersistAfterAllocatedOperation {
				if len(items) != 0 || len(recovered.projects[project.ID].ledger.Slots) != 0 {
					t.Fatalf("pre-authority crash leaked admission: items=%+v slots=%d", items, len(recovered.projects[project.ID].ledger.Slots))
				}
			} else {
				if len(items) != 1 || items[0].State != processdomain.StateInterrupted {
					t.Fatalf("post-authority recovery = %+v", items)
				}
				assertOneInterruptedEvent(t, recovered, project.ID, "process")
			}
		})
	}
}

func TestProcessRuntimeInitializationCrashRecovery(t *testing.T) {
	for _, point := range []processPersistenceFailpoint{
		processPersistAfterIndexInitializing,
		processPersistAfterLedgerInitialize,
		processPersistAfterJournalInitialize,
		processPersistAfterAuthorityInitialize,
		processPersistAfterIndexReady,
	} {
		t.Run(string(point), func(t *testing.T) {
			dataDir := t.TempDir()
			project := runtimeTestProject(t, "init-"+string(point))
			var fired atomic.Bool
			_, err := newProcessRuntimePersistence(
				dataDir, []Project{project},
				func(candidate processPersistenceFailpoint) error {
					if candidate == point && fired.CompareAndSwap(false, true) {
						return errors.New("crash")
					}
					return nil
				},
			)
			if err == nil {
				t.Fatal("initialization failpoint did not fire")
			}
			recovered, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
			if err != nil {
				t.Fatalf("recover initialization %s: %v", point, err)
			}
			if recovered.projects[project.ID] == nil {
				t.Fatal("recovered runtime project is missing")
			}
		})
	}
}

func TestProcessRuntimeBootstrapInterruptsExactlyOnce(t *testing.T) {
	dataDir := t.TempDir()
	project := runtimeTestProject(t, "interrupt-project")
	runtime, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := runtimeStartingRecord(t, runtime, project.ID, "running")
	admitRuntimeRecord(t, runtime, record)
	record.State = processdomain.StateRunning
	record.PID, record.GuardPID, record.PGID = 91, 92, 93
	record.UpdatedAt = record.UpdatedAt.Add(time.Second)
	if err := runtime.MarkRunning(record); err != nil {
		t.Fatal(err)
	}
	first, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertOneInterruptedEvent(t, first, project.ID, record.ID)
	got := first.List(project.ID)[0]
	if got.PID != 0 || got.PGID != 0 || got.GuardPID != 0 || got.State != processdomain.StateInterrupted {
		t.Fatalf("normalized process = %+v", got)
	}
	second, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertOneInterruptedEvent(t, second, project.ID, record.ID)
}

func TestProcessRuntimeTerminalCrashRecoveryExactEvent(t *testing.T) {
	for _, point := range []processPersistenceFailpoint{
		processPersistAfterTerminal,
		processPersistAfterLedgerTerminal,
	} {
		t.Run(string(point), func(t *testing.T) {
			dataDir := t.TempDir()
			project := runtimeTestProject(t, "terminal-"+string(point))
			var enabled atomic.Bool
			runtime, err := newProcessRuntimePersistence(
				dataDir, []Project{project},
				func(candidate processPersistenceFailpoint) error {
					if enabled.Load() && candidate == point {
						enabled.Store(false)
						return errors.New("crash")
					}
					return nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			record := runtimeStartingRecord(t, runtime, project.ID, "terminal-crash")
			admitRuntimeRecord(t, runtime, record)
			record.State = processdomain.StateFailed
			record.Error = "failed"
			record.ExitCode = 9
			record.UpdatedAt = record.UpdatedAt.Add(time.Second)
			record.EndedAt = timePointer(record.UpdatedAt)
			enabled.Store(true)
			if err := runtime.MarkTerminal(record); err == nil {
				t.Fatal("terminal failpoint did not fire")
			}
			recovered, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
			if err != nil {
				t.Fatal(err)
			}
			recovered.authorityMu.Lock()
			durable := recovered.authority.Projects[recovered.projects[project.ID].identity.SafeProjectKey].Records[record.ID]
			recovered.authorityMu.Unlock()
			if durable.Event == nil || durable.Event.ID != processTerminalEventID(record.ID) ||
				durable.Event.ExitCode != 9 || durable.Reservation == nil ||
				durable.Reservation.State != monitordomain.ReservationTerminalUnacknowledged {
				t.Fatalf("terminal recovery = %+v", durable)
			}
			if len(recovered.projects[project.ID].ledger.Slots) != 1 {
				t.Fatal("terminal reservation was lost")
			}
		})
	}
}

func assertOneInterruptedEvent(
	t *testing.T,
	runtime *processRuntimePersistence,
	projectID, processID string,
) {
	t.Helper()
	project := runtime.projects[projectID]
	runtime.authorityMu.Lock()
	record := runtime.authority.Projects[project.identity.SafeProjectKey].Records[processID]
	runtime.authorityMu.Unlock()
	if record.Event == nil || record.Event.ID != processTerminalEventID(processID) ||
		record.Event.State != processdomain.StateInterrupted ||
		record.Reservation == nil ||
		record.Reservation.State != monitordomain.ReservationTerminalUnacknowledged {
		t.Fatalf("recovery event = %+v record=%+v", record.Event, record)
	}
}

func TestProcessRuntimeTerminalAckReleaseCrashWindows(t *testing.T) {
	points := []processPersistenceFailpoint{
		processPersistAfterReleaseIntent,
		processPersistAfterAck,
		processPersistAfterReleaseOperation,
		processPersistAfterReleaseMark,
		processPersistAfterAuthorityDetach,
		processPersistAfterDetachedOperation,
		processPersistAfterLedgerRelease,
		processPersistAfterAdmissionDelete,
	}
	for _, point := range points {
		t.Run(string(point), func(t *testing.T) {
			dataDir := t.TempDir()
			project := runtimeTestProject(t, "ack-"+string(point))
			var failEnabled atomic.Bool
			runtime, err := newProcessRuntimePersistence(
				dataDir, []Project{project},
				func(candidate processPersistenceFailpoint) error {
					if failEnabled.Load() && candidate == point {
						failEnabled.Store(false)
						return errors.New("crash")
					}
					return nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			record := runtimeStartingRecord(t, runtime, project.ID, "terminal")
			admitRuntimeRecord(t, runtime, record)
			record.State = processdomain.StateFailed
			record.ExitCode = 2
			record.Error = "failed"
			record.UpdatedAt = record.UpdatedAt.Add(time.Second)
			record.EndedAt = timePointer(record.UpdatedAt)
			if err := runtime.MarkTerminal(record); err != nil {
				t.Fatal(err)
			}
			failEnabled.Store(true)
			if err := runtime.AcknowledgeTerminal(
				project.ID, record.ID, processTerminalEventID(record.ID),
			); err == nil {
				t.Fatal("ack failpoint did not fire")
			}
			recovered, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
			if err != nil {
				t.Fatalf("recover ack %s: %v", point, err)
			}
			state := recovered.projects[project.ID]
			if len(state.ledger.Slots) != 0 {
				t.Fatalf("released ledger occupancy = %d", len(state.ledger.Slots))
			}
			recovered.authorityMu.Lock()
			durable := recovered.authority.Projects[state.identity.SafeProjectKey].Records[record.ID]
			recovered.authorityMu.Unlock()
			if durable.Event != nil || durable.Reservation != nil ||
				durable.AcknowledgedEventID != processTerminalEventID(record.ID) {
				t.Fatalf("released authority record = %+v", durable)
			}
		})
	}
}

func TestProcessRuntimeTerminalAcknowledgementIsExactAndIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	project := runtimeTestProject(t, "ack-exact")
	runtime, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := runtimeStartingRecord(t, runtime, project.ID, "terminal")
	admitRuntimeRecord(t, runtime, record)
	record.State = processdomain.StateFailed
	record.ExitCode = 3
	record.EndedAt = timePointer(record.UpdatedAt)
	if err := runtime.MarkTerminal(record); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AcknowledgeTerminal(project.ID, record.ID, "wrong"); err == nil {
		t.Fatal("wrong terminal event id was accepted")
	}
	state := runtime.projects[project.ID]
	if _, exists := state.journal.Operations["process/"+record.ID+"/release"]; exists {
		t.Fatal("wrong acknowledgement persisted a release intent")
	}
	eventID := processTerminalEventID(record.ID)
	if err := runtime.AcknowledgeTerminal(project.ID, record.ID, eventID); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AcknowledgeTerminal(project.ID, record.ID, eventID); err != nil {
		t.Fatalf("idempotent acknowledgement failed: %v", err)
	}
}

func TestAdmissionRecoveryStateMatrix(t *testing.T) {
	project := runtimeTestProject(t, "admission-matrix")
	identity := monitordomain.RuntimeProjectIdentity{
		ProjectID: project.ID, CanonicalProjectPath: project.Path,
		CanonicalPathSHA256: monitordomain.CanonicalProjectPathSHA256(project.Path),
		SafeProjectKey:      monitordomain.SafeProjectKey(project.ID),
	}
	operation := monitordomain.RuntimeMutationOperation{
		ID: "process/entity/admit", Project: identity, Kind: "process_admission",
		State: "allocated", ReservationToken: "token",
		AuthorityID: processAuthorityID, EntityID: "entity",
	}
	reservation := monitordomain.TerminalReservation{
		Slot: 1, Token: "token", ProjectID: identity.ProjectID,
		ProjectIdentitySHA256: identity.CanonicalPathSHA256,
		OperationID:           operation.ID, AuthorityID: processAuthorityID,
		EntityID: "entity", EventKind: processTerminalEventKind,
		State: monitordomain.ReservationAllocating,
	}
	authority := durableProcessRecord{
		Process: processdomain.Process{
			ID: "entity", ProjectID: identity.ProjectID,
			State: processdomain.StateStarting,
		},
		Reservation: &reservation,
	}
	tests := []struct {
		name            string
		state           string
		ledgerState     monitordomain.TerminalReservationState
		ledgerExists    bool
		authorityState  monitordomain.TerminalReservationState
		authorityExists bool
		valid           bool
	}{
		{name: "intent", state: "intent", valid: true},
		{name: "token-selected-no-ledger", state: "token_selected", valid: true},
		{name: "token-selected-ledger", state: "token_selected", ledgerExists: true, ledgerState: monitordomain.ReservationAllocating, valid: true},
		{name: "allocated-no-authority", state: "allocated", ledgerExists: true, ledgerState: monitordomain.ReservationAllocating, valid: true},
		{name: "allocated-authority", state: "allocated", ledgerExists: true, ledgerState: monitordomain.ReservationAllocating, authorityExists: true, authorityState: monitordomain.ReservationAllocating, valid: true},
		{name: "authority-saved-after-ledger", state: "authority_saved", ledgerExists: true, ledgerState: monitordomain.ReservationActive, authorityExists: true, authorityState: monitordomain.ReservationAllocating, valid: true},
		{name: "active-before-authority", state: "active", ledgerExists: true, ledgerState: monitordomain.ReservationActive, authorityExists: true, authorityState: monitordomain.ReservationAllocating, valid: true},
		{name: "committed", state: "committed", ledgerExists: true, ledgerState: monitordomain.ReservationActive, authorityExists: true, authorityState: monitordomain.ReservationActive, valid: true},
		{name: "allocated-lost-ledger", state: "allocated"},
		{name: "intent-with-ledger", state: "intent", ledgerExists: true, ledgerState: monitordomain.ReservationAllocating},
		{name: "token-selected-with-authority", state: "token_selected", ledgerExists: true, ledgerState: monitordomain.ReservationAllocating, authorityExists: true, authorityState: monitordomain.ReservationAllocating},
		{name: "allocated-active-ledger", state: "allocated", ledgerExists: true, ledgerState: monitordomain.ReservationActive, authorityExists: true, authorityState: monitordomain.ReservationAllocating},
		{name: "authority-saved-active-authority", state: "authority_saved", ledgerExists: true, ledgerState: monitordomain.ReservationActive, authorityExists: true, authorityState: monitordomain.ReservationActive},
		{name: "active-allocating-ledger", state: "active", ledgerExists: true, ledgerState: monitordomain.ReservationAllocating, authorityExists: true, authorityState: monitordomain.ReservationAllocating},
		{name: "committed-allocating-authority", state: "committed", ledgerExists: true, ledgerState: monitordomain.ReservationActive, authorityExists: true, authorityState: monitordomain.ReservationAllocating},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidateOperation := operation
			candidateOperation.State = test.state
			if test.state == "intent" {
				candidateOperation.ReservationToken = ""
			}
			candidateLedger := reservation
			candidateLedger.State = test.ledgerState
			candidateAuthority := authority
			candidateReservation := reservation
			candidateReservation.State = test.authorityState
			candidateAuthority.Reservation = &candidateReservation
			err := validateAdmissionRecoveryState(
				identity, candidateOperation, candidateLedger, test.ledgerExists,
				candidateAuthority, test.authorityExists,
			)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, err=%v", test.valid, err)
			}
		})
	}
}

func TestAdmissionRecoveryRejectsPersistedImpossibleState(t *testing.T) {
	dataDir := t.TempDir()
	project := runtimeTestProject(t, "admission-persisted-matrix")
	runtime, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := runtimeStartingRecord(t, runtime, project.ID, "entity")
	admitRuntimeRecord(t, runtime, record)
	state := runtime.projects[project.ID]
	state.journalMu.Lock()
	operation := state.journal.Operations["process/"+record.ID+"/admit"]
	state.journalMu.Unlock()
	operation.State = "allocated"
	if err := runtime.putOperation(state, operation); err != nil {
		t.Fatal(err)
	}

	recovered, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.projectRuntime(project.ID) != nil ||
		recovered.ProjectError(project.ID) == nil {
		t.Fatal("impossible admission state did not fail closed for its project")
	}
}

func TestTerminalReleaseRecoveryRejectsTokenEventAndAckMismatch(t *testing.T) {
	for _, mutation := range []string{
		"missing-release", "operation-token", "event-token", "ack-id",
	} {
		t.Run(mutation, func(t *testing.T) {
			dataDir := t.TempDir()
			project := runtimeTestProject(t, "release-binding-"+mutation)
			var failPoint processPersistenceFailpoint
			if mutation == "ack-id" || mutation == "missing-release" {
				failPoint = processPersistAfterAck
			} else {
				failPoint = processPersistAfterReleaseIntent
			}
			var enabled atomic.Bool
			runtime, err := newProcessRuntimePersistence(
				dataDir, []Project{project},
				func(point processPersistenceFailpoint) error {
					if enabled.Load() && point == failPoint {
						enabled.Store(false)
						return errors.New("crash")
					}
					return nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			record := runtimeStartingRecord(t, runtime, project.ID, "terminal")
			admitRuntimeRecord(t, runtime, record)
			record.State = processdomain.StateFailed
			record.EndedAt = timePointer(record.UpdatedAt)
			if err := runtime.MarkTerminal(record); err != nil {
				t.Fatal(err)
			}
			enabled.Store(true)
			if err := runtime.AcknowledgeTerminal(
				project.ID, record.ID, processTerminalEventID(record.ID),
			); err == nil {
				t.Fatal("release failpoint did not fire")
			}
			state := runtime.projects[project.ID]
			switch mutation {
			case "missing-release":
				err = runtime.deleteOperation(
					state, "process/"+record.ID+"/release",
				)
			case "operation-token":
				state.journalMu.Lock()
				operation := state.journal.Operations["process/"+record.ID+"/release"]
				state.journalMu.Unlock()
				operation.ReservationToken = "wrong-token"
				err = runtime.putOperation(state, operation)
			case "event-token":
				runtime.authorityMu.Lock()
				partition := runtime.authority.Projects[state.identity.SafeProjectKey]
				durable := partition.Records[record.ID]
				durable.Event.Reservation.Token = "wrong-token"
				partition.Records[record.ID] = durable
				runtime.authority.Projects[state.identity.SafeProjectKey] = partition
				err = runtime.store.saveJSON("processes.json", runtime.authority)
				runtime.authorityMu.Unlock()
			case "ack-id":
				runtime.authorityMu.Lock()
				partition := runtime.authority.Projects[state.identity.SafeProjectKey]
				durable := partition.Records[record.ID]
				durable.AcknowledgedEventID = "process/other/terminal"
				partition.Records[record.ID] = durable
				runtime.authority.Projects[state.identity.SafeProjectKey] = partition
				err = runtime.store.saveJSON("processes.json", runtime.authority)
				runtime.authorityMu.Unlock()
			}
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.projectRuntime(project.ID) != nil ||
				recovered.ProjectError(project.ID) == nil {
				t.Fatal("corrupt terminal release binding did not disable its project")
			}
		})
	}
}

func TestProcessSnapshotRejectsCrossProjectLogAndUnsafeTombstone(t *testing.T) {
	project := runtimeTestProject(t, "snapshot-integrity")
	identity := monitordomain.RuntimeProjectIdentity{
		ProjectID: project.ID, CanonicalProjectPath: project.Path,
		CanonicalPathSHA256: monitordomain.CanonicalProjectPathSHA256(project.Path),
		SafeProjectKey:      monitordomain.SafeProjectKey(project.ID),
	}
	now := time.Now().UTC()
	snapshot := processAuthoritySnapshot{
		SchemaVersion: processSnapshotSchemaVersion,
		Projects: map[string]processAuthorityProject{
			identity.SafeProjectKey: {
				Project: identity,
				Records: map[string]durableProcessRecord{
					"done": {
						Process: processdomain.Process{
							ID: "done", ProjectID: project.ID, State: processdomain.StateFailed,
							LogPath: processLogRelativePath(identity, "done"),
						},
						AcknowledgedEventID: processTerminalEventID("done"),
					},
				},
				Tombstones: map[string]processTombstone{},
			},
		},
	}
	if err := validateProcessAuthoritySnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	partition := snapshot.Projects[identity.SafeProjectKey]
	record := partition.Records["done"]
	record.Process.LogPath = filepath.Join("process-logs", "other-project", "done.log")
	partition.Records["done"] = record
	snapshot.Projects[identity.SafeProjectKey] = partition
	if err := validateProcessAuthoritySnapshot(snapshot); err == nil {
		t.Fatal("cross-project log path was accepted")
	}
	record.Process.LogPath = processLogRelativePath(identity, "done")
	partition.Records["done"] = record
	partition.Tombstones["done"] = processTombstone{
		ProcessID: "done", LogPath: "processes.json", CreatedAt: now,
	}
	snapshot.Projects[identity.SafeProjectKey] = partition
	if err := validateProcessAuthoritySnapshot(snapshot); err == nil {
		t.Fatal("unsafe retention tombstone was accepted")
	}
}

func TestProcessRuntimeStrictMissingCorruptAndSymlinkRejection(t *testing.T) {
	t.Run("missing-established", func(t *testing.T) {
		dataDir := t.TempDir()
		project := runtimeTestProject(t, "missing")
		if _, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dataDir, "processes.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil); err == nil {
			t.Fatal("missing established process store loaded as empty")
		}
	})
	for _, name := range []string{"terminal-reservations.json", "runtime-mutations.json"} {
		t.Run("missing-"+name, func(t *testing.T) {
			dataDir := t.TempDir()
			project := runtimeTestProject(t, "missing-"+name)
			runtime, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
			if err != nil {
				t.Fatal(err)
			}
			key := runtime.projects[project.ID].identity.SafeProjectKey
			if err := os.Remove(filepath.Join(dataDir, "project-runtime", key, name)); err != nil {
				t.Fatal(err)
			}
			reloaded, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.projectErrs[project.ID] == nil || reloaded.projects[project.ID] != nil {
				t.Fatalf("missing established %s did not disable its project", name)
			}
		})
	}
	t.Run("corrupt-established", func(t *testing.T) {
		dataDir := t.TempDir()
		project := runtimeTestProject(t, "corrupt")
		if _, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "processes.json"), []byte("{broken"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil); err == nil {
			t.Fatal("corrupt established process store loaded as empty")
		}
	})
	t.Run("corrupt-project-journal", func(t *testing.T) {
		dataDir := t.TempDir()
		project := runtimeTestProject(t, "corrupt-journal")
		runtime, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
		if err != nil {
			t.Fatal(err)
		}
		key := runtime.projects[project.ID].identity.SafeProjectKey
		path := filepath.Join(dataDir, "project-runtime", key, "runtime-mutations.json")
		if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
			t.Fatal(err)
		}
		reloaded, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if reloaded.projectErrs[project.ID] == nil || reloaded.projects[project.ID] != nil {
			t.Fatal("corrupt project journal did not disable its project")
		}
	})
	t.Run("symlink-ledger", func(t *testing.T) {
		dataDir := t.TempDir()
		project := runtimeTestProject(t, "symlink")
		runtime, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
		if err != nil {
			t.Fatal(err)
		}
		key := runtime.projects[project.ID].identity.SafeProjectKey
		ledgerPath := filepath.Join(dataDir, "project-runtime", key, "terminal-reservations.json")
		target := filepath.Join(t.TempDir(), "target.json")
		if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(ledgerPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, ledgerPath); err != nil {
			t.Fatal(err)
		}
		reloaded, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if reloaded.projectErrs[project.ID] == nil || reloaded.projects[project.ID] != nil {
			t.Fatal("symlinked ledger did not disable its project")
		}
		content, err := os.ReadFile(target)
		if err != nil || string(content) != "{}" {
			t.Fatalf("symlink target mutated: %q %v", content, err)
		}
	})
	t.Run("permission-drift", func(t *testing.T) {
		dataDir := t.TempDir()
		project := runtimeTestProject(t, "permissions")
		if _, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dataDir, "processes.json"), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil); err == nil {
			t.Fatal("group-readable runtime state was accepted")
		}
	})
	t.Run("unknown-schema-field", func(t *testing.T) {
		dataDir := t.TempDir()
		project := runtimeTestProject(t, "unknown-field")
		if _, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dataDir, "processes.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		body["unexpected"] = true
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil); err == nil {
			t.Fatal("unknown process schema field was accepted")
		}
	})
}

func TestRuntimeProjectIdentityRejectsCollisionsAndNestedData(t *testing.T) {
	project := runtimeTestProject(t, "project-a")
	duplicate := project
	duplicate.ID = "project-b"
	if _, err := resolveRuntimeProjectIdentities(t.TempDir(), []Project{project, duplicate}); err == nil {
		t.Fatal("duplicate canonical project path was accepted")
	}
	nestedData := filepath.Join(project.Path, ".runtime")
	if _, err := resolveRuntimeProjectIdentities(nestedData, []Project{project}); err == nil {
		t.Fatal("runtime data nested under project was accepted")
	}
}

func TestProcessRuntimeReadyPartitionDeletionDisablesOnlyAffectedProject(t *testing.T) {
	dataDir := t.TempDir()
	projectA := runtimeTestProject(t, "partition-a")
	projectB := runtimeTestProject(t, "partition-b")
	runtime, err := newProcessRuntimePersistence(dataDir, []Project{projectA, projectB}, nil)
	if err != nil {
		t.Fatal(err)
	}
	keyA := runtime.projects[projectA.ID].identity.SafeProjectKey
	runtime.authorityMu.Lock()
	delete(runtime.authority.Projects, keyA)
	runtime.authority.Generation++
	if err := runtime.store.saveJSON("processes.json", runtime.authority); err != nil {
		runtime.authorityMu.Unlock()
		t.Fatal(err)
	}
	runtime.authorityMu.Unlock()

	reloaded, err := newProcessRuntimePersistence(dataDir, []Project{projectA, projectB}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.projectErrs[projectA.ID] == nil || reloaded.projects[projectA.ID] != nil {
		t.Fatal("missing ready authority partition was silently recreated")
	}
	if reloaded.projects[projectB.ID] == nil {
		t.Fatal("healthy project was disabled with the missing partition")
	}
}

func TestProcessRuntimeRejectsOrphanStoresWithoutGlobalSentinels(t *testing.T) {
	dataDir := t.TempDir()
	project := runtimeTestProject(t, "orphan-store")
	if _, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dataDir, "project-runtime", "index.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dataDir, "processes.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil); err == nil {
		t.Fatal("orphan ledger/journal were adopted without global sentinels")
	}
}

func TestProcessRuntimeExactAdmissionOperationAndProjectIsolation(t *testing.T) {
	for _, mutation := range []string{"missing-operation", "token-mismatch"} {
		t.Run(mutation, func(t *testing.T) {
			dataDir := t.TempDir()
			projectA := runtimeTestProject(t, "exact-a-"+mutation)
			projectB := runtimeTestProject(t, "exact-b-"+mutation)
			runtime, err := newProcessRuntimePersistence(
				dataDir, []Project{projectA, projectB}, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			record := runtimeStartingRecord(t, runtime, projectA.ID, "active")
			admitRuntimeRecord(t, runtime, record)
			stateA := runtime.projects[projectA.ID]
			operationID := "process/" + record.ID + "/admit"
			stateA.journalMu.Lock()
			if mutation == "missing-operation" {
				delete(stateA.journal.Operations, operationID)
			} else {
				operation := stateA.journal.Operations[operationID]
				operation.ReservationToken = "mismatched-token"
				stateA.journal.Operations[operationID] = operation
			}
			stateA.journal.Generation++
			if err := runtime.store.saveJSON(
				filepath.Join(
					"project-runtime",
					stateA.identity.SafeProjectKey,
					"runtime-mutations.json",
				),
				stateA.journal,
			); err != nil {
				stateA.journalMu.Unlock()
				t.Fatal(err)
			}
			stateA.journalMu.Unlock()

			reloaded, err := newProcessRuntimePersistence(
				dataDir, []Project{projectA, projectB}, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.projectErrs[projectA.ID] == nil || reloaded.projects[projectA.ID] != nil {
				t.Fatalf("%s did not fail closed for affected project", mutation)
			}
			if reloaded.projects[projectB.ID] == nil {
				t.Fatalf("%s disabled healthy project", mutation)
			}
			recordB := runtimeStartingRecord(t, reloaded, projectB.ID, "healthy")
			admitRuntimeRecord(t, reloaded, recordB)
		})
	}
}

func TestProcessRuntimeCorruptProjectStoreDoesNotDisableHealthyProject(t *testing.T) {
	dataDir := t.TempDir()
	projectA := runtimeTestProject(t, "corrupt-a")
	projectB := runtimeTestProject(t, "corrupt-b")
	runtime, err := newProcessRuntimePersistence(dataDir, []Project{projectA, projectB}, nil)
	if err != nil {
		t.Fatal(err)
	}
	keyA := runtime.projects[projectA.ID].identity.SafeProjectKey
	path := filepath.Join(dataDir, "project-runtime", keyA, "terminal-reservations.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newProcessRuntimePersistence(dataDir, []Project{projectA, projectB}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.projectErrs[projectA.ID] == nil || reloaded.projects[projectA.ID] != nil {
		t.Fatal("corrupt project ledger did not disable its project")
	}
	if reloaded.projects[projectB.ID] == nil {
		t.Fatal("corrupt project ledger disabled healthy project")
	}
	recordB := runtimeStartingRecord(t, reloaded, projectB.ID, "healthy-after-corruption")
	admitRuntimeRecord(t, reloaded, recordB)
}

func TestProcessRuntimeStoredPathIdentityMismatchIsProjectScoped(t *testing.T) {
	dataDir := t.TempDir()
	projectA := runtimeTestProject(t, "path-a")
	projectB := runtimeTestProject(t, "path-b")
	if _, err := newProcessRuntimePersistence(dataDir, []Project{projectA, projectB}, nil); err != nil {
		t.Fatal(err)
	}
	reboundPath := t.TempDir()
	projectA.Path = reboundPath
	reloaded, err := newProcessRuntimePersistence(dataDir, []Project{projectA, projectB}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ProjectError(projectA.ID) == nil || reloaded.projectRuntime(projectA.ID) != nil {
		t.Fatal("stored canonical path mismatch did not disable affected project")
	}
	if reloaded.projectRuntime(projectB.ID) == nil {
		t.Fatal("stored canonical path mismatch disabled healthy project")
	}
}

func TestApplicationBootstrapKeepsHealthyProjectWhenPeerStoreIsCorrupt(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"project-a", "project-b"} {
		if err := os.MkdirAll(filepath.Join(root, name, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := t.TempDir()
	first := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	if err := first.bootstrap(); err != nil {
		t.Fatal(err)
	}
	projects, err := first.scanProjects()
	if err != nil {
		t.Fatal(err)
	}
	projectA, projectB := projects[0], projects[1]
	keyA := first.processRuntime.projectRuntime(projectA.ID).identity.SafeProjectKey
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := first.shutdownProcessRuntime(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if err := os.WriteFile(
		filepath.Join(dataDir, "project-runtime", keyA, "runtime-mutations.json"),
		[]byte("{broken"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	second := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	if err := second.bootstrap(); err != nil {
		t.Fatalf("healthy project did not survive peer corruption: %v", err)
	}
	if second.processRuntime.ProjectError(projectA.ID) == nil {
		t.Fatal("corrupt project was not disabled")
	}
	if second.processRuntime.projectRuntime(projectB.ID) == nil {
		t.Fatal("healthy project runtime is unavailable")
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := second.shutdownProcessRuntime(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}

func TestProcessRuntimeStateSurvivesWorkspaceGitClean(t *testing.T) {
	dataDir := t.TempDir()
	project := runtimeTestProject(t, "git-clean")
	if output, err := exec.Command("git", "init", project.Path).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	runtime, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := runtimeStartingRecord(t, runtime, project.ID, "survivor")
	admitRuntimeRecord(t, runtime, record)
	workspaceState := filepath.Join(project.Path, ".karoz", "throwaway")
	if err := os.MkdirAll(filepath.Dir(workspaceState), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspaceState, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "clean", "-fdx")
	command.Dir = project.Path
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git clean: %v: %s", err, output)
	}
	recovered, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if items := recovered.List(project.ID); len(items) != 1 ||
		items[0].State != processdomain.StateInterrupted {
		t.Fatalf("runtime state lost after git clean: %+v", items)
	}
}

func TestProcessRuntimeConcurrentAdmissionAndRetentionBounds(t *testing.T) {
	dataDir := t.TempDir()
	project := runtimeTestProject(t, "concurrent")
	runtime, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime.retention = processdomain.RetentionPolicy{
		MaxRecords: 200, MaxAge: 0, MaxTotalBytes: 256 << 20,
	}
	const count = 24
	var wait sync.WaitGroup
	errorsOut := make(chan error, count)
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			record := processdomain.Process{
				ID: fmt.Sprintf("p-%02d", index), ProjectID: project.ID,
				AgentID: "agent", Command: "true", Workdir: project.Path,
				State: processdomain.StateStarting, LifetimeMS: 1000,
				StartedAt: time.Unix(int64(index+1), 0).UTC(),
				UpdatedAt: time.Unix(int64(index+1), 0).UTC(),
			}
			record, runErr := runtime.PrepareRecord(record)
			if runErr == nil {
				runErr = runtime.Reserve(record)
			}
			if runErr == nil {
				runErr = runtime.CreateStarting(record)
			}
			if runErr == nil {
				record.State = processdomain.StateRunning
				runErr = runtime.MarkRunning(record)
			}
			if runErr == nil {
				record.State = processdomain.StateSucceeded
				record.EndedAt = timePointer(record.UpdatedAt)
				runErr = runtime.MarkTerminal(record)
			}
			if runErr == nil {
				runErr = runtime.AcknowledgeTerminal(
					project.ID, record.ID, processTerminalEventID(record.ID),
				)
			}
			errorsOut <- runErr
		}(index)
	}
	wait.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.ApplyRetention(
		project.ID,
		processdomain.RetentionPolicy{MaxRecords: 5, MaxAge: 0, MaxTotalBytes: 1 << 20},
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if items := runtime.List(project.ID); len(items) != 5 {
		t.Fatalf("retained records = %d, want 5", len(items))
	}
}

func TestProcessRetentionNeverPrunesActiveOrUnacknowledged(t *testing.T) {
	dataDir := t.TempDir()
	project := runtimeTestProject(t, "retention-pins")
	runtime, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	active := runtimeStartingRecord(t, runtime, project.ID, "active")
	admitRuntimeRecord(t, runtime, active)
	terminal := runtimeStartingRecord(t, runtime, project.ID, "unacknowledged")
	admitRuntimeRecord(t, runtime, terminal)
	terminal.State = processdomain.StateFailed
	terminal.Error = "failed"
	terminal.EndedAt = timePointer(terminal.UpdatedAt)
	if err := runtime.MarkTerminal(terminal); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ApplyRetention(
		project.ID,
		processdomain.RetentionPolicy{MaxRecords: 0, MaxTotalBytes: 0},
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if items := runtime.List(project.ID); len(items) != 2 {
		t.Fatalf("retention pruned pinned records: %+v", items)
	}
}

func TestApplicationBootstrapsDormantProcessRuntime(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	if err := a.bootstrap(); err != nil {
		t.Fatal(err)
	}
	if !a.processRuntimeReady() {
		t.Fatal("Application did not own the dormant process runtime")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.shutdownProcessRuntime(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestApplicationRegistersNewProjectWithoutRebuildingLiveSupervisor(t *testing.T) {
	root := t.TempDir()
	existingPath := filepath.Join(root, "existing")
	if err := os.MkdirAll(filepath.Join(existingPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := projectFromPath(existingPath, root, "main")
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	if err := a.bootstrap(); err != nil {
		t.Fatal(err)
	}
	supervisor := a.processSupervisor
	request := startRequest("live-during-registration", "sleep 30", existingPath)
	request.ProjectID = existing.ID
	if _, err := supervisor.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	created, err := a.createProject(ProjectCreateRequest{Name: "new-runtime-project"})
	if err != nil {
		t.Fatal(err)
	}
	if a.processSupervisor != supervisor {
		t.Fatal("project registration rebuilt the live supervisor")
	}
	if a.processRuntime.projectRuntime(created.ID) == nil {
		t.Fatal("new project was not added to the live process runtime")
	}
	supervisor.mu.Lock()
	live := supervisor.handles[request.ID]
	supervisor.mu.Unlock()
	if live == nil || live.snapshot().State != processdomain.StateRunning {
		t.Fatalf("existing live handle was interrupted by registration: %+v", live)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.shutdownProcessRuntime(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestImportProjectRejectsCanonicalPathAliasWithoutRuntimeWrites(t *testing.T) {
	root := t.TempDir()
	existingPath := filepath.Join(root, "existing")
	if err := os.MkdirAll(filepath.Join(existingPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := projectFromPath(existingPath, root, "main")
	dataDir := t.TempDir()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	if err := a.bootstrap(); err != nil {
		t.Fatal(err)
	}
	if err := a.saveSettings(); err != nil {
		t.Fatal(err)
	}
	if err := a.saveProjectAliases(); err != nil {
		t.Fatal(err)
	}

	existingRuntime := a.processRuntime.projectRuntime(existing.ID)
	if existingRuntime == nil {
		t.Fatal("existing project runtime was not registered")
	}
	safetyPaths := []string{
		filepath.Join(dataDir, "settings.json"),
		filepath.Join(dataDir, "project-aliases.json"),
		filepath.Join(dataDir, "project-runtime", "index.json"),
		filepath.Join(dataDir, "processes.json"),
		filepath.Join(
			dataDir, "project-runtime", existingRuntime.identity.SafeProjectKey,
			"terminal-reservations.json",
		),
		filepath.Join(
			dataDir, "project-runtime", existingRuntime.identity.SafeProjectKey,
			"runtime-mutations.json",
		),
	}
	before := make(map[string][]byte, len(safetyPaths))
	for _, path := range safetyPaths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = body
	}

	makeAlias := func(name string) (string, Project) {
		t.Helper()
		aliasPath := filepath.Join(t.TempDir(), name)
		if err := os.Symlink(existingPath, aliasPath); err != nil {
			t.Fatal(err)
		}
		return aliasPath, projectFromPath(aliasPath, aliasPath, "extra")
	}
	aliasProjects := make([]Project, 0, 4)
	for index, name := range []string{"sequential-a", "sequential-b"} {
		aliasPath, aliasProject := makeAlias(name)
		aliasProjects = append(aliasProjects, aliasProject)
		request := ProjectCreateRequest{Path: aliasPath, Name: name}
		var importErr error
		if index == 0 {
			request.Mode = "import"
			_, importErr = a.createProject(request)
		} else {
			_, importErr = a.importProject(request)
		}
		if importErr == nil {
			t.Fatalf("canonical path alias import %s succeeded", name)
		}
	}

	concurrentResults := make(chan error, 2)
	var concurrentImports sync.WaitGroup
	for _, name := range []string{"concurrent-a", "concurrent-b"} {
		aliasPath, aliasProject := makeAlias(name)
		aliasProjects = append(aliasProjects, aliasProject)
		concurrentImports.Add(1)
		go func(path, alias string) {
			defer concurrentImports.Done()
			_, importErr := a.importProject(ProjectCreateRequest{Path: path, Name: alias})
			concurrentResults <- importErr
		}(aliasPath, name)
	}
	concurrentImports.Wait()
	close(concurrentResults)
	for importErr := range concurrentResults {
		if importErr == nil {
			t.Fatal("concurrent canonical path alias import succeeded")
		}
	}

	for _, path := range safetyPaths {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before[path]) {
			t.Errorf("failed alias registration changed runtime safety state %s", path)
		}
	}
	for _, aliasProject := range aliasProjects {
		if aliasProject.ID == existing.ID {
			t.Fatal("alias fixture unexpectedly produced the primary project ID")
		}
		aliasKey := monitordomain.SafeProjectKey(aliasProject.ID)
		if _, err := os.Stat(filepath.Join(dataDir, "project-runtime", aliasKey)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed alias registration created runtime partition: %v", err)
		}
		if a.processRuntime.projectRuntime(aliasProject.ID) != nil {
			t.Fatal("failed alias registration became runtime-visible")
		}
	}
	if a.processRuntime.projectRuntime(existing.ID) != existingRuntime {
		t.Fatal("failed imports replaced the existing live project runtime")
	}
	if _, err := os.Stat(filepath.Join(existingPath, ".karoz", "ignore-initialized")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected import mutated the project workspace: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := a.shutdownProcessRuntime(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	restarted := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	if err := restarted.loadSettings(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.bootstrap(); err != nil {
		t.Fatalf("clean restart after rejected aliases: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := restarted.shutdownProcessRuntime(ctx); err != nil {
			t.Errorf("shutdown restarted process runtime: %v", err)
		}
	})
	if restarted.processRuntime.projectRuntime(existing.ID) == nil {
		t.Fatal("restart lost the existing project runtime")
	}
	for _, aliasProject := range aliasProjects {
		if restarted.processRuntime.projectRuntime(aliasProject.ID) != nil {
			t.Fatal("restart adopted a rejected canonical alias")
		}
	}
}

func TestImportProjectSettingsFailureAbortsClaimAndRollsBackConfig(t *testing.T) {
	root := t.TempDir()
	existingPath := filepath.Join(root, "existing")
	if err := os.MkdirAll(filepath.Join(existingPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	if err := a.bootstrap(); err != nil {
		t.Fatal(err)
	}
	if err := a.saveSettings(); err != nil {
		t.Fatal(err)
	}
	if err := a.saveProjectAliases(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.shutdownProcessRuntime(ctx); err != nil {
			t.Errorf("shutdown process runtime: %v", err)
		}
	})

	externalPath := filepath.Join(t.TempDir(), "external")
	if err := os.MkdirAll(filepath.Join(externalPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	external := projectFromPath(externalPath, externalPath, "extra")
	paths := []string{
		filepath.Join(dataDir, "settings.json"),
		filepath.Join(dataDir, "project-aliases.json"),
		filepath.Join(dataDir, "project-runtime", "index.json"),
		filepath.Join(dataDir, "processes.json"),
	}
	before := make(map[string][]byte, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = body
	}
	a.projectImportSettingsSave = func() error {
		return errors.New("settings save failed")
	}
	if _, err := a.importProject(ProjectCreateRequest{
		Path: externalPath, Name: "rejected-external",
	}); err == nil {
		t.Fatal("settings failure did not reject import")
	}
	a.projectImportSettingsSave = nil

	for _, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before[path]) {
			t.Errorf("settings failure changed %s", path)
		}
	}
	a.mu.Lock()
	extraRoots := append([]string(nil), a.settings.ExtraProjectsRoots...)
	_, aliasExists := a.projectAliases[external.ID]
	a.mu.Unlock()
	if len(extraRoots) != 0 || aliasExists {
		t.Fatalf("settings failure changed in-memory config: roots=%v alias=%v", extraRoots, aliasExists)
	}
	a.processRuntime.indexMu.Lock()
	_, claimExists := a.processRuntime.index.Projects[monitordomain.SafeProjectKey(external.ID)]
	a.processRuntime.indexMu.Unlock()
	if claimExists {
		t.Fatal("pre-commit settings failure retained its initializing claim")
	}
	if _, err := os.Stat(filepath.Join(
		dataDir, "project-runtime", monitordomain.SafeProjectKey(external.ID),
	)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings failure created a runtime partition: %v", err)
	}
}

func TestImportProjectSettingsFailurePreservesMissingConfigFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "existing", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	if err := a.bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.shutdownProcessRuntime(ctx); err != nil {
			t.Errorf("shutdown process runtime: %v", err)
		}
	})
	externalPath := filepath.Join(t.TempDir(), "external")
	if err := os.MkdirAll(filepath.Join(externalPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	external := projectFromPath(externalPath, externalPath, "extra")
	a.projectImportSettingsSave = func() error {
		return errors.New("settings save failed")
	}
	if _, err := a.importProject(ProjectCreateRequest{Path: externalPath}); err == nil {
		t.Fatal("settings failure did not reject import")
	}
	for _, name := range []string{"settings.json", "project-aliases.json"} {
		if _, err := os.Stat(filepath.Join(dataDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("precommit rollback created %s: %v", name, err)
		}
	}
	a.processRuntime.indexMu.Lock()
	_, exists := a.processRuntime.index.Projects[monitordomain.SafeProjectKey(external.ID)]
	a.processRuntime.indexMu.Unlock()
	if exists {
		t.Fatal("precommit failure retained its import claim")
	}
}

func TestImportProjectClaimCrashRecovery(t *testing.T) {
	tests := []struct {
		point           processPersistenceFailpoint
		committed       bool
		settingsWritten bool
		aliasesWritten  bool
		progress        string
		intentCleared   bool
	}{
		{point: processPersistAfterIndexInitializing, progress: projectImportClaimed},
		{point: processPersistAfterImportWorkspace, progress: projectImportClaimed},
		{point: processPersistAfterImportSettings, committed: true, settingsWritten: true, progress: projectImportClaimed},
		{point: processPersistAfterImportSettingsState, committed: true, settingsWritten: true, progress: projectImportSettingsCommitted},
		{point: processPersistAfterImportAliases, committed: true, settingsWritten: true, aliasesWritten: true, progress: projectImportSettingsCommitted},
		{point: processPersistAfterImportAliasesState, committed: true, settingsWritten: true, aliasesWritten: true, progress: projectImportAliasesCommitted},
		{point: processPersistAfterAuthorityInitialize, committed: true, settingsWritten: true, aliasesWritten: true, progress: projectImportAliasesCommitted},
		{point: processPersistAfterLedgerInitialize, committed: true, settingsWritten: true, aliasesWritten: true, progress: projectImportAliasesCommitted},
		{point: processPersistAfterJournalInitialize, committed: true, settingsWritten: true, aliasesWritten: true, progress: projectImportAliasesCommitted},
		{point: processPersistAfterIndexReady, committed: true, settingsWritten: true, aliasesWritten: true, intentCleared: true},
	}
	for _, test := range tests {
		t.Run(string(test.point), func(t *testing.T) {
			root := t.TempDir()
			existingPath := filepath.Join(root, "existing")
			if err := os.MkdirAll(filepath.Join(existingPath, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			existing := projectFromPath(existingPath, root, "main")
			externalPath := filepath.Join(t.TempDir(), "external")
			if err := os.MkdirAll(filepath.Join(externalPath, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			external := projectFromPath(externalPath, externalPath, "extra")
			dataDir := t.TempDir()
			a := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
			if err := a.bootstrap(); err != nil {
				t.Fatal(err)
			}
			if err := a.saveSettings(); err != nil {
				t.Fatal(err)
			}
			if err := a.saveProjectAliases(); err != nil {
				t.Fatal(err)
			}
			settingsPath := filepath.Join(dataDir, "settings.json")
			aliasesPath := filepath.Join(dataDir, "project-aliases.json")
			settingsBefore, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			aliasesBefore, err := os.ReadFile(aliasesPath)
			if err != nil {
				t.Fatal(err)
			}
			desiredSettings := a.settings
			desiredSettings.ExtraProjectsRoots = normalizeWorkspaceRoots(
				append([]string(nil), externalPath), desiredSettings.ProjectsRoot,
			)
			desiredAliases := cloneProjectAliases(a.projectAliases)
			desiredAliases[external.ID] = "external"
			settingsAfter, err := json.MarshalIndent(desiredSettings, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			settingsAfter = append(settingsAfter, '\n')
			aliasesAfter, err := json.MarshalIndent(desiredAliases, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			aliasesAfter = append(aliasesAfter, '\n')
			var fired atomic.Bool
			a.processRuntime.fail = func(point processPersistenceFailpoint) error {
				if point == test.point && fired.CompareAndSwap(false, true) {
					return errors.New("simulated import crash")
				}
				return nil
			}
			if _, err := a.importProject(ProjectCreateRequest{
				Path: externalPath, Name: "external",
			}); err == nil {
				t.Fatalf("failpoint %s did not interrupt import", test.point)
			}
			if !fired.Load() {
				t.Fatalf("failpoint %s was not reached", test.point)
			}
			settingsAtCrash, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			aliasesAtCrash, err := os.ReadFile(aliasesPath)
			if err != nil {
				t.Fatal(err)
			}
			expectedSettings := settingsBefore
			if test.settingsWritten {
				expectedSettings = settingsAfter
			}
			expectedAliases := aliasesBefore
			if test.aliasesWritten {
				expectedAliases = aliasesAfter
			}
			if string(settingsAtCrash) != string(expectedSettings) ||
				string(aliasesAtCrash) != string(expectedAliases) {
				t.Fatalf(
					"crash bytes settings_match=%v aliases_match=%v",
					string(settingsAtCrash) == string(expectedSettings),
					string(aliasesAtCrash) == string(expectedAliases),
				)
			}
			externalKey := monitordomain.SafeProjectKey(external.ID)
			a.processRuntime.indexMu.Lock()
			crashEntry := a.processRuntime.index.Projects[externalKey]
			a.processRuntime.indexMu.Unlock()
			if test.intentCleared {
				if crashEntry.Import != nil {
					t.Fatalf("ready import retained intent: %+v", crashEntry.Import)
				}
			} else if crashEntry.Import == nil ||
				crashEntry.Import.Progress != test.progress {
				t.Fatalf(
					"crash import progress=%+v, want %q",
					crashEntry.Import, test.progress,
				)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := a.shutdownProcessRuntime(ctx); err != nil {
				cancel()
				t.Fatal(err)
			}
			cancel()

			restarted := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
			if err := restarted.loadSettings(); err != nil {
				t.Fatal(err)
			}
			if err := restarted.bootstrap(); err != nil {
				t.Fatalf("restart after %s: %v", test.point, err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := restarted.shutdownProcessRuntime(ctx); err != nil {
					t.Errorf("shutdown restarted runtime: %v", err)
				}
			})
			if restarted.processRuntime.projectRuntime(existing.ID) == nil {
				t.Fatal("recovery lost the existing project runtime")
			}
			externalRuntime := restarted.processRuntime.projectRuntime(external.ID)
			restarted.processRuntime.indexMu.Lock()
			entry, claimExists := restarted.processRuntime.index.Projects[externalKey]
			restarted.processRuntime.indexMu.Unlock()
			if test.committed {
				if externalRuntime == nil || !claimExists || entry.State != "ready" {
					t.Fatalf(
						"committed import was not completed: runtime=%v claim=%+v exists=%v",
						externalRuntime, entry, claimExists,
					)
				}
			} else if externalRuntime != nil || claimExists {
				t.Fatalf(
					"uncommitted import was not rolled back: runtime=%v claim=%+v exists=%v",
					externalRuntime, entry, claimExists,
				)
			}
			roots := restarted.settings.WorkspaceRoots()
			var configured bool
			for _, candidate := range roots {
				if filepath.Clean(candidate) == filepath.Clean(externalPath) {
					configured = true
				}
			}
			if configured != test.committed {
				t.Fatalf("configured=%v, want %v after %s", configured, test.committed, test.point)
			}
			settingsRecovered, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			aliasesRecovered, err := os.ReadFile(aliasesPath)
			if err != nil {
				t.Fatal(err)
			}
			expectedSettings = settingsBefore
			expectedAliases = aliasesBefore
			if test.committed {
				expectedSettings = settingsAfter
				expectedAliases = aliasesAfter
			}
			if string(settingsRecovered) != string(expectedSettings) ||
				string(aliasesRecovered) != string(expectedAliases) {
				t.Fatalf(
					"recovery bytes settings_match=%v aliases_match=%v",
					string(settingsRecovered) == string(expectedSettings),
					string(aliasesRecovered) == string(expectedAliases),
				)
			}
			restarted.mu.Lock()
			recoveredAlias := restarted.projectAliases[external.ID]
			restarted.mu.Unlock()
			if test.committed && recoveredAlias != "external" {
				t.Fatalf("recovered alias=%q, want external", recoveredAlias)
			}
		})
	}
}

func TestProcessRuntimeConcurrentCanonicalPathRegistrationAllowsOneOwner(t *testing.T) {
	dataDir := t.TempDir()
	runtime, err := newProcessRuntimePersistence(dataDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir()
	projects := []Project{
		{ID: "canonical-owner-a", Name: "a", Path: path},
		{ID: "canonical-owner-b", Name: "b", Path: path},
	}
	start := make(chan struct{})
	results := make(chan error, len(projects))
	var registrations sync.WaitGroup
	for _, project := range projects {
		project := project
		registrations.Add(1)
		go func() {
			defer registrations.Done()
			<-start
			results <- runtime.RegisterProject(project)
		}()
	}
	close(start)
	registrations.Wait()
	close(results)

	var succeeded, rejected int
	for registerErr := range results {
		if registerErr == nil {
			succeeded++
		} else {
			rejected++
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("registration outcomes: succeeded=%d rejected=%d", succeeded, rejected)
	}

	runtime.indexMu.Lock()
	index := cloneRuntimeProjectIndex(runtime.index)
	runtime.indexMu.Unlock()
	if err := validateRuntimeProjectIndex(index); err != nil {
		t.Fatalf("invalid committed index: %v", err)
	}
	if len(index.Projects) != 1 {
		t.Fatalf("committed index has %d projects, want 1", len(index.Projects))
	}
	var visible int
	for _, project := range projects {
		if runtime.projectRuntime(project.ID) != nil {
			visible++
		}
	}
	if visible != 1 {
		t.Fatalf("runtime-visible canonical owners=%d, want 1", visible)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "project-runtime"))
	if err != nil {
		t.Fatal(err)
	}
	var projectPartitions int
	for _, entry := range entries {
		if entry.IsDir() {
			projectPartitions++
		}
	}
	if projectPartitions != 1 {
		t.Fatalf("runtime project partitions=%d, want 1", projectPartitions)
	}
}

func TestProcessRuntimeProjectRegistrationCrashRecovery(t *testing.T) {
	for _, point := range []processPersistenceFailpoint{
		processPersistAfterIndexInitializing,
		processPersistAfterAuthorityInitialize,
		processPersistAfterLedgerInitialize,
		processPersistAfterJournalInitialize,
		processPersistAfterIndexReady,
	} {
		t.Run(string(point), func(t *testing.T) {
			dataDir := t.TempDir()
			projectA := runtimeTestProject(t, "register-base-"+string(point))
			projectB := runtimeTestProject(t, "register-new-"+string(point))
			runtime, err := newProcessRuntimePersistence(dataDir, []Project{projectA}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var fired atomic.Bool
			runtime.fail = func(candidate processPersistenceFailpoint) error {
				if candidate == point && fired.CompareAndSwap(false, true) {
					return errors.New("registration crash")
				}
				return nil
			}
			if err := runtime.RegisterProject(projectB); err == nil {
				t.Fatal("registration failpoint did not fire")
			}
			recovered, err := newProcessRuntimePersistence(
				dataDir, []Project{projectA, projectB}, nil,
			)
			if err != nil {
				t.Fatalf("recover project registration %s: %v", point, err)
			}
			if recovered.projectRuntime(projectA.ID) == nil ||
				recovered.projectRuntime(projectB.ID) == nil {
				t.Fatalf("project registration recovery %s lost a project", point)
			}
		})
	}
}

func TestProcessSupervisorDoesNotSpawnBeforeDurableAdmission(t *testing.T) {
	dataDir := t.TempDir()
	project := runtimeTestProject(t, "no-spawn")
	var failEnabled atomic.Bool
	runtime, err := newProcessRuntimePersistence(
		dataDir, []Project{project},
		func(point processPersistenceFailpoint) error {
			if failEnabled.Load() && point == processPersistAfterIntent {
				return errors.New("persistence failed")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var boundaryCalls atomic.Int32
	supervisor, err := newProcessSupervisor(
		context.Background(), runtime, runtime, runtime.OpenLog,
		processSupervisorConfig{
			PrepareRecord: runtime.PrepareRecord,
			BoundaryFactory: func(*exec.Cmd) (processBoundary, error) {
				boundaryCalls.Add(1)
				return nil, errors.New("must not spawn")
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	failEnabled.Store(true)
	request := startRequest("durability-first", "true", project.Path)
	request.ProjectID = project.ID
	if _, err := supervisor.Start(context.Background(), request); err == nil {
		t.Fatal("durability failure did not reject start")
	}
	if boundaryCalls.Load() != 0 {
		t.Fatalf("spawn boundary called %d times before durable admission", boundaryCalls.Load())
	}
}

func TestProcessSnapshotStrictJSONRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	project := runtimeTestProject(t, "roundtrip")
	runtime, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, "processes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded processAuthoritySnapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := validateProcessAuthoritySnapshot(decoded); err != nil {
		t.Fatal(err)
	}
	if runtime.authority.SchemaVersion != processSnapshotSchemaVersion {
		t.Fatal("runtime authority schema version drift")
	}
}

func TestProcessRuntimeListReturnsIndependentCopies(t *testing.T) {
	dataDir := t.TempDir()
	project := runtimeTestProject(t, "copy")
	runtime, err := newProcessRuntimePersistence(dataDir, []Project{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := runtimeStartingRecord(t, runtime, project.ID, "copy-process")
	record.OutputGaps = []processdomain.SeqRange{{Start: 2, End: 3}}
	admitRuntimeRecord(t, runtime, record)
	record.State = processdomain.StateRunning
	if err := runtime.MarkRunning(record); err != nil {
		t.Fatal(err)
	}
	record.State = processdomain.StateFailed
	record.EndedAt = timePointer(record.UpdatedAt)
	if err := runtime.MarkTerminal(record); err != nil {
		t.Fatal(err)
	}
	first := runtime.List(project.ID)
	first[0].OutputGaps[0].Start = 999
	*first[0].EndedAt = time.Time{}
	second := runtime.List(project.ID)
	if second[0].OutputGaps[0].Start != 2 || second[0].EndedAt.IsZero() {
		t.Fatalf("List exposed mutable process aliases: %+v", second[0])
	}
}
