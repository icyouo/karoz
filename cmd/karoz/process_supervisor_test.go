//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	processdomain "github.com/karoz/karoz/internal/process"
)

func TestBackgroundProcessGuardHelper(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "process-guard" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	os.Exit(runBackgroundProcessGuard(os.Args[index+1:]))
}

type memoryProcessStore struct {
	mu               sync.Mutex
	records          map[string]processdomain.Process
	transitions      map[string]int
	terminalAttempts map[string]int
	terminalFailures map[string]int
	failRunning      bool
}

func newMemoryProcessStore() *memoryProcessStore {
	return &memoryProcessStore{
		records:          make(map[string]processdomain.Process),
		transitions:      make(map[string]int),
		terminalAttempts: make(map[string]int),
		terminalFailures: make(map[string]int),
	}
}

func (store *memoryProcessStore) CreateStarting(item processdomain.Process) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.records[item.ID] = item
	return nil
}

func (store *memoryProcessStore) MarkRunning(item processdomain.Process) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failRunning {
		return errors.New("registration save failed")
	}
	store.records[item.ID] = item
	return nil
}

func (store *memoryProcessStore) MarkTerminal(item processdomain.Process) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.terminalAttempts[item.ID]++
	if store.terminalFailures[item.ID] > 0 {
		store.terminalFailures[item.ID]--
		return errors.New("terminal save failed")
	}
	if current, ok := store.records[item.ID]; ok && current.State.Terminal() {
		return nil
	}
	store.records[item.ID] = item
	store.transitions[item.ID]++
	return nil
}

func (store *memoryProcessStore) get(id string) processdomain.Process {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.records[id]
}

type memoryReservationBoundary struct {
	mu        sync.Mutex
	reserved  map[string]bool
	aborted   map[string]bool
	failAbort bool
}

func newMemoryReservationBoundary() *memoryReservationBoundary {
	return &memoryReservationBoundary{
		reserved: make(map[string]bool), aborted: make(map[string]bool),
	}
}

func (boundary *memoryReservationBoundary) Reserve(item processdomain.Process) error {
	boundary.mu.Lock()
	defer boundary.mu.Unlock()
	boundary.reserved[item.ID] = true
	return nil
}

func (boundary *memoryReservationBoundary) Abort(item processdomain.Process) error {
	boundary.mu.Lock()
	defer boundary.mu.Unlock()
	if boundary.failAbort {
		return errors.New("reservation abort failed")
	}
	boundary.aborted[item.ID] = true
	return nil
}

type synchronizedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (buffer *synchronizedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.Write(value)
}

func (buffer *synchronizedBuffer) Close() error { return nil }

func (buffer *synchronizedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.String()
}

func testSupervisor(t *testing.T, store *memoryProcessStore, reservations *memoryReservationBoundary, writer io.WriteCloser, config processSupervisorConfig) *processSupervisor {
	t.Helper()
	config.GuardExecutable = os.Args[0]
	config.GuardArgsPrefix = []string{"-test.run=TestBackgroundProcessGuardHelper", "--"}
	config.ExitDrain = time.Second
	config.StopGrace = 100 * time.Millisecond
	supervisor, err := newProcessSupervisor(
		context.Background(), store, reservations,
		func(processdomain.Process) (io.WriteCloser, error) { return writer, nil },
		config,
	)
	if err != nil {
		t.Fatal(err)
	}
	return supervisor
}

func startRequest(id, command, workdir string) processStartRequest {
	return processStartRequest{
		ID: id, ProjectID: "project-1", AgentID: "agent-1",
		RunID: "run-1", Command: command, Workdir: workdir,
	}
}

func waitTerminal(t *testing.T, store *memoryProcessStore, id string) processdomain.Process {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		record := store.get(id)
		if record.State.Terminal() {
			return record
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %s did not become terminal: %+v", id, store.get(id))
	return processdomain.Process{}
}

func waitTerminalWithSupervisor(t *testing.T, supervisor *processSupervisor, store *memoryProcessStore, id string) processdomain.Process {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		record := store.get(id)
		if record.State.Terminal() {
			return record
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %s did not become terminal: %+v faults=%#v", id, store.get(id), supervisor.RecoveryFaults())
	return processdomain.Process{}
}

func TestProcessSupervisorTrueOneWriteAndLongRun(t *testing.T) {
	cases := []struct {
		id, command, output string
	}{
		{"true", "true", ""},
		{"one-write", "printf 'hello\\n'", "hello\n"},
	}
	for _, test := range cases {
		t.Run(test.id, func(t *testing.T) {
			store := newMemoryProcessStore()
			reservations := newMemoryReservationBoundary()
			log := &synchronizedBuffer{}
			supervisor := testSupervisor(t, store, reservations, log, processSupervisorConfig{})
			if _, err := supervisor.Start(context.Background(), startRequest(test.id, test.command, t.TempDir())); err != nil {
				t.Fatal(err)
			}
			record := waitTerminal(t, store, test.id)
			if record.State != processdomain.StateSucceeded || log.String() != test.output {
				t.Fatalf("record=%+v log=%q", record, log.String())
			}
		})
	}

	store := newMemoryProcessStore()
	reservations := newMemoryReservationBoundary()
	supervisor := testSupervisor(t, store, reservations, &synchronizedBuffer{}, processSupervisorConfig{})
	creator, cancelCreator := context.WithCancel(context.Background())
	record, err := supervisor.Start(creator, startRequest("long", "sleep 30", t.TempDir()))
	if err != nil || record.State != processdomain.StateRunning {
		t.Fatalf("start long process: %+v, %v", record, err)
	}
	cancelCreator()
	time.Sleep(50 * time.Millisecond)
	if got := store.get("long"); got.State != processdomain.StateRunning {
		t.Fatalf("creator cancellation controlled child: %+v", got)
	}
	if err := supervisor.Stop("long"); err != nil {
		t.Fatal(err)
	}
	if got := waitTerminal(t, store, "long"); got.State != processdomain.StateKilled {
		t.Fatalf("stopped process = %+v", got)
	}
}

func TestProcessSupervisorEnforcesPerProjectConcurrencyCap(t *testing.T) {
	store := newMemoryProcessStore()
	reservations := newMemoryReservationBoundary()
	supervisor := testSupervisor(
		t,
		store,
		reservations,
		&synchronizedBuffer{},
		processSupervisorConfig{MaxConcurrent: 1},
	)
	first := startRequest("cap-first", "sleep 30", t.TempDir())
	if _, err := supervisor.Start(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := startRequest("cap-second", "sleep 30", first.Workdir)
	if _, err := supervisor.Start(context.Background(), second); err == nil ||
		!strings.Contains(err.Error(), "concurren") {
		t.Fatalf("second start at cap error = %v", err)
	}
	if err := supervisor.Stop(first.ID); err != nil {
		t.Fatal(err)
	}
	if record := waitTerminal(t, store, first.ID); !record.State.Terminal() {
		t.Fatalf("first process did not terminate: %+v", record)
	}
	if _, err := supervisor.Start(context.Background(), second); err != nil {
		t.Fatalf("capacity was not released: %v", err)
	}
	if err := supervisor.Stop(second.ID); err != nil {
		t.Fatal(err)
	}
}

func TestProcessSupervisorInstantExitReturnsRecoveredTerminalSnapshot(t *testing.T) {
	exitObserved := make(chan struct{})
	releaseFinalize := make(chan struct{})
	var observedOnce sync.Once
	store := newMemoryProcessStore()
	supervisor := testSupervisor(
		t,
		store,
		newMemoryReservationBoundary(),
		&synchronizedBuffer{},
		processSupervisorConfig{
			BeforeFinalize: func() {
				observedOnce.Do(func() { close(exitObserved) })
				<-releaseFinalize
			},
			Fail: func(point processFailpoint) error {
				if point == processFailRegistration {
					<-exitObserved
				}
				return nil
			},
		},
	)
	result := make(chan processdomain.Process, 1)
	startErr := make(chan error, 1)
	workdir := t.TempDir()
	go func() {
		record, err := supervisor.Start(
			context.Background(),
			startRequest("instant-terminal", "exit 7", workdir),
		)
		result <- record
		startErr <- err
	}()
	select {
	case record := <-result:
		t.Fatalf("Start returned before finalization was released: %+v", record)
	case <-exitObserved:
	}
	close(releaseFinalize)
	record := <-result
	if err := <-startErr; err != nil {
		t.Fatalf("instant exit recovery = %v", err)
	}
	if record.State != processdomain.StateFailed || record.ExitCode != 7 || record.EndedAt == nil {
		t.Fatalf("Start returned stale instant-exit snapshot: %+v", record)
	}
	persisted := store.get(record.ID)
	if persisted.State != record.State || persisted.ExitCode != record.ExitCode {
		t.Fatalf("returned snapshot=%+v persisted=%+v", record, persisted)
	}
}

func TestProcessSupervisorStreamOrdering(t *testing.T) {
	store := newMemoryProcessStore()
	supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{})
	command := "printf 'out-1\\n'; printf 'err-1\\n' >&2; printf 'out-2\\n'; printf 'err-2\\n' >&2; sleep 2"
	if _, err := supervisor.Start(context.Background(), startRequest("streams", command, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var lines []processdomain.OutputLine
	for time.Now().Before(deadline) {
		supervisor.mu.Lock()
		handle := supervisor.handles["streams"]
		supervisor.mu.Unlock()
		if handle != nil {
			lines = handle.buffer.TailSequenced(10)
			if len(lines) == 4 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(lines) != 4 {
		t.Fatalf("stream lines = %#v", lines)
	}
	seen := map[string]int{}
	for index, line := range lines {
		if line.Sequence != uint64(index+1) {
			t.Fatalf("non-global sequence: %#v", lines)
		}
		seen[line.Stream]++
	}
	if seen["stdout"] != 2 || seen["stderr"] != 2 {
		t.Fatalf("stream labels lost: %#v", lines)
	}
	_ = supervisor.Stop("streams")
}

func TestProcessSupervisorSpawnFailpointsRollback(t *testing.T) {
	points := []processFailpoint{
		processFailLogOpen, processFailWatchdog, processFailSpawn,
		processFailCollectorArm, processFailWaiterArm, processFailRegistration,
	}
	for _, point := range points {
		t.Run(string(point), func(t *testing.T) {
			store := newMemoryProcessStore()
			reservations := newMemoryReservationBoundary()
			supervisor := testSupervisor(t, store, reservations, &synchronizedBuffer{}, processSupervisorConfig{
				Fail: func(candidate processFailpoint) error {
					if candidate == point {
						return fmt.Errorf("%s failed", point)
					}
					return nil
				},
			})
			_, err := supervisor.Start(context.Background(), startRequest(string(point), "sleep 30", t.TempDir()))
			if err == nil {
				t.Fatal("failpoint did not fail")
			}
			record := waitTerminal(t, store, string(point))
			if record.State != processdomain.StateFailed || !reservations.aborted[string(point)] {
				t.Fatalf("rollback record=%+v reservations=%+v", record, reservations.aborted)
			}
			supervisor.mu.Lock()
			active := len(supervisor.handles)
			supervisor.mu.Unlock()
			if active != 0 {
				t.Fatalf("spawn rollback left %d handles", active)
			}
		})
	}
	t.Run("registration-save", func(t *testing.T) {
		store := newMemoryProcessStore()
		store.failRunning = true
		reservations := newMemoryReservationBoundary()
		log := &synchronizedBuffer{}
		supervisor := testSupervisor(t, store, reservations, log, processSupervisorConfig{})
		_, err := supervisor.Start(context.Background(), startRequest("registration-save", "printf 'uncommitted\\n'", t.TempDir()))
		if err == nil {
			t.Fatal("registration save failure did not roll back")
		}
		record := waitTerminal(t, store, "registration-save")
		if record.State != processdomain.StateFailed || !reservations.aborted["registration-save"] {
			t.Fatalf("registration rollback = %+v, %+v", record, reservations.aborted)
		}
		if got := log.String(); got != "" {
			t.Fatalf("registration rollback published output: %q", got)
		}
	})
}

func TestProcessSupervisorDelayedRegistrationRetainsEarlyOutput(t *testing.T) {
	for _, command := range []string{"true", "printf 'early\\n'"} {
		t.Run(command, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			store := newMemoryProcessStore()
			log := &synchronizedBuffer{}
			supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), log, processSupervisorConfig{
				Fail: func(point processFailpoint) error {
					if point == processFailRegistration {
						close(entered)
						<-release
					}
					return nil
				},
			})
			done := make(chan error, 1)
			workdir := t.TempDir()
			go func() {
				_, err := supervisor.Start(context.Background(), startRequest("delayed", command, workdir))
				done <- err
			}()
			<-entered
			time.Sleep(50 * time.Millisecond)
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			record := waitTerminal(t, store, "delayed")
			if record.State != processdomain.StateSucceeded {
				t.Fatalf("delayed process = %+v", record)
			}
			want := ""
			if command != "true" {
				want = "early\n"
			}
			if got := log.String(); got != want {
				t.Fatalf("early log = %q, want %q", got, want)
			}
		})
	}
}

func TestProcessSupervisorPostSpawnFailuresDiscardEarlyOutput(t *testing.T) {
	for _, point := range []processFailpoint{
		processFailCollectorArm, processFailWaiterArm, processFailRegistration,
	} {
		for _, command := range []string{"true", "printf 'uncommitted\\n'"} {
			t.Run(string(point)+"/"+strings.Fields(command)[0], func(t *testing.T) {
				store := newMemoryProcessStore()
				log := &synchronizedBuffer{}
				supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), log, processSupervisorConfig{
					Fail: func(candidate processFailpoint) error {
						if candidate == point {
							time.Sleep(50 * time.Millisecond)
							return errors.New("post-spawn failure")
						}
						return nil
					},
				})
				_, err := supervisor.Start(context.Background(), startRequest(string(point), command, t.TempDir()))
				if err == nil {
					t.Fatal("expected rollback")
				}
				if got := log.String(); got != "" {
					t.Fatalf("rollback published early output: %q", got)
				}
			})
		}
	}
}

func TestProcessSupervisorRegistrationIsNotSignalableAndSerializesShutdown(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	store := newMemoryProcessStore()
	supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{
		Fail: func(point processFailpoint) error {
			if point == processFailRegistration {
				close(entered)
				<-release
			}
			return nil
		},
	})
	startDone := make(chan error, 1)
	workdir := t.TempDir()
	go func() {
		_, err := supervisor.Start(context.Background(), startRequest("blocked-registration", "sleep 30", workdir))
		startDone <- err
	}()
	<-entered
	if err := supervisor.Stop("blocked-registration"); err == nil {
		t.Fatal("unregistered process was signalable")
	}
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdownDone <- supervisor.Shutdown(ctx)
	}()
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown crossed in-flight registration: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	if got := waitTerminal(t, store, "blocked-registration"); got.State != processdomain.StateInterrupted {
		t.Fatalf("shutdown record = %+v", got)
	}
}

type failingLogWriter struct{}

func (failingLogWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func (failingLogWriter) Close() error              { return nil }

func TestProcessSupervisorLogFailureAndTerminalArbitration(t *testing.T) {
	store := newMemoryProcessStore()
	supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), failingLogWriter{}, processSupervisorConfig{})
	if _, err := supervisor.Start(context.Background(), startRequest("log-fail", "printf x; sleep 30", t.TempDir())); err != nil {
		t.Fatal(err)
	}
	record := waitTerminalWithSupervisor(t, supervisor, store, "log-fail")
	if record.State != processdomain.StateFailed || !strings.Contains(record.Error, "log write failed") {
		t.Fatalf("log failure = %+v", record)
	}

	store = newMemoryProcessStore()
	supervisor = testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{})
	request := startRequest("lifetime", "sleep 30", t.TempDir())
	request.Lifetime = 50 * time.Millisecond
	if _, err := supervisor.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	record = waitTerminalWithSupervisor(t, supervisor, store, "lifetime")
	if record.State != processdomain.StateFailed || record.Error != "lifetime exceeded" {
		t.Fatalf("lifetime result = %+v", record)
	}
	if store.transitions["lifetime"] != 1 {
		t.Fatalf("terminal transition count = %d", store.transitions["lifetime"])
	}
}

func TestProcessSupervisorLifetimeDefaultsAndCeiling(t *testing.T) {
	store := newMemoryProcessStore()
	reservations := newMemoryReservationBoundary()
	supervisor := testSupervisor(t, store, reservations, &synchronizedBuffer{}, processSupervisorConfig{
		DefaultLifetime: 50 * time.Millisecond,
		MaxLifetime:     100 * time.Millisecond,
	})
	record, err := supervisor.Start(context.Background(), startRequest("default-lifetime", "sleep 30", t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if record.LifetimeMS != 50 {
		t.Fatalf("effective lifetime = %d", record.LifetimeMS)
	}
	if got := waitTerminal(t, store, record.ID); got.Error != "lifetime exceeded" {
		t.Fatalf("default lifetime result = %+v", got)
	}
	for _, lifetime := range []time.Duration{-time.Second, 101 * time.Millisecond} {
		request := startRequest(fmt.Sprintf("invalid-%d", lifetime), "true", t.TempDir())
		request.Lifetime = lifetime
		if _, err := supervisor.Start(context.Background(), request); err == nil {
			t.Fatalf("accepted lifetime %s", lifetime)
		}
	}
	if _, err := newProcessSupervisor(
		context.Background(), newMemoryProcessStore(), newMemoryReservationBoundary(),
		func(processdomain.Process) (io.WriteCloser, error) { return &synchronizedBuffer{}, nil },
		processSupervisorConfig{DefaultLifetime: 25 * time.Hour},
	); err != nil {
		t.Fatalf("unbounded server rejected a duration beyond the former product ceiling: %v", err)
	}
	if _, err := newProcessSupervisor(
		context.Background(), newMemoryProcessStore(), newMemoryReservationBoundary(),
		func(processdomain.Process) (io.WriteCloser, error) { return &synchronizedBuffer{}, nil },
		processSupervisorConfig{DefaultLifetime: 15 * time.Minute, MaxLifetime: 30 * time.Minute},
	); err != nil {
		t.Fatalf("valid lifetime overrides rejected: %v", err)
	}
}

func TestProcessSupervisorUnlimitedLifetime(t *testing.T) {
	store := newMemoryProcessStore()
	supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{
		DefaultLifetime: 10 * time.Millisecond,
	})
	request := startRequest("unlimited-lifetime", "sleep 30", t.TempDir())
	request.Unlimited = true
	record, err := supervisor.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if record.LifetimeMS != 0 {
		t.Fatalf("unlimited lifetime persisted as %dms", record.LifetimeMS)
	}
	time.Sleep(30 * time.Millisecond)
	if current := store.get(record.ID); current.State.Terminal() {
		t.Fatalf("unlimited process was stopped by the default timer: %+v", current)
	}
	if err := supervisor.Stop(record.ID); err != nil {
		t.Fatal(err)
	}
	if got := waitTerminal(t, store, record.ID); got.State != processdomain.StateKilled {
		t.Fatalf("unlimited process manual stop = %+v", got)
	}
}

func TestProcessSupervisorTerminalPersistenceRetriesAllPaths(t *testing.T) {
	type pathCase struct {
		name    string
		command string
		run     func(*testing.T, *processSupervisor, processStartRequest) error
	}
	cases := []pathCase{
		{
			name: "pre-spawn",
			run: func(_ *testing.T, supervisor *processSupervisor, request processStartRequest) error {
				supervisor.config.Fail = func(point processFailpoint) error {
					if point == processFailLogOpen {
						return errors.New("pre-spawn failed")
					}
					return nil
				}
				_, err := supervisor.Start(context.Background(), request)
				return err
			},
		},
		{
			name:    "natural-exit",
			command: "true",
			run: func(_ *testing.T, supervisor *processSupervisor, request processStartRequest) error {
				_, err := supervisor.Start(context.Background(), request)
				return err
			},
		},
		{
			name:    "rollback",
			command: "sleep 30",
			run: func(_ *testing.T, supervisor *processSupervisor, request processStartRequest) error {
				supervisor.config.Fail = func(point processFailpoint) error {
					if point == processFailRegistration {
						return errors.New("registration failed")
					}
					return nil
				}
				_, err := supervisor.Start(context.Background(), request)
				return err
			},
		},
		{
			name:    "stop",
			command: "sleep 30",
			run: func(t *testing.T, supervisor *processSupervisor, request processStartRequest) error {
				if _, err := supervisor.Start(context.Background(), request); err != nil {
					return err
				}
				return supervisor.Stop(request.ID)
			},
		},
		{
			name:    "shutdown",
			command: "sleep 30",
			run: func(t *testing.T, supervisor *processSupervisor, request processStartRequest) error {
				if _, err := supervisor.Start(context.Background(), request); err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				return supervisor.Shutdown(ctx)
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := newMemoryProcessStore()
			store.terminalFailures[test.name] = 1
			supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{
				TerminalRetry: time.Millisecond,
			})
			command := test.command
			if command == "" {
				command = "true"
			}
			request := startRequest(test.name, command, t.TempDir())
			err := test.run(t, supervisor, request)
			if test.name == "pre-spawn" || test.name == "rollback" {
				if err == nil {
					t.Fatal("expected initiating failure")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			record := waitTerminal(t, store, test.name)
			if !record.State.Terminal() || store.terminalAttempts[test.name] < 2 {
				t.Fatalf("terminal retry record=%+v attempts=%d", record, store.terminalAttempts[test.name])
			}
			if len(supervisor.RecoveryFaults()) == 0 {
				t.Fatal("terminal failure was not surfaced")
			}
		})
	}
}

func TestProcessSupervisorAbortFailureIsRecoverableEvidence(t *testing.T) {
	store := newMemoryProcessStore()
	reservations := newMemoryReservationBoundary()
	reservations.failAbort = true
	supervisor := testSupervisor(t, store, reservations, &synchronizedBuffer{}, processSupervisorConfig{
		Fail: func(point processFailpoint) error {
			if point == processFailLogOpen {
				return errors.New("log open failed")
			}
			return nil
		},
	})
	if _, err := supervisor.Start(context.Background(), startRequest("abort-fault", "true", t.TempDir())); err == nil ||
		!strings.Contains(err.Error(), "reservation abort failed") {
		t.Fatalf("abort failure = %v", err)
	}
	faults := supervisor.RecoveryFaults()
	if len(faults) != 1 || faults[0].Stage != "pre_spawn_abort" {
		t.Fatalf("recovery faults = %#v", faults)
	}
}

func TestProcessSupervisorPermanentTerminalFailureRetainsOwnership(t *testing.T) {
	store := newMemoryProcessStore()
	store.terminalFailures["permanent"] = 1000
	supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{
		TerminalRetries: 3,
		TerminalRetry:   time.Millisecond,
	})
	if _, err := supervisor.Start(context.Background(), startRequest("permanent", "true", t.TempDir())); err != nil &&
		!strings.Contains(err.Error(), "terminal state is not durable") {
		t.Fatalf("permanent terminal failure = %v", err)
	}
	waitForRecoveryFault(t, supervisor)
	supervisor.mu.Lock()
	retained := supervisor.handles["permanent"]
	supervisor.mu.Unlock()
	if retained == nil {
		t.Fatal("terminal persistence failure released process ownership")
	}
	faults := waitForRecoveryCount(t, supervisor, 3)
	if len(faults) != 1 || faults[0].Count != 3 {
		t.Fatalf("coalesced recovery faults = %#v", faults)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := supervisor.Shutdown(ctx); err == nil || !strings.Contains(err.Error(), "terminal state is not durable") {
		t.Fatalf("shutdown with unresolved terminal state = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("shutdown ignored its deadline: %s", elapsed)
	}
}

func TestProcessSupervisorPermanentPreSpawnFailureRetainsReservation(t *testing.T) {
	store := newMemoryProcessStore()
	store.terminalFailures["pre-permanent"] = 1000
	reservations := newMemoryReservationBoundary()
	supervisor := testSupervisor(t, store, reservations, &synchronizedBuffer{}, processSupervisorConfig{
		TerminalRetries: 2,
		TerminalRetry:   time.Millisecond,
		Fail: func(point processFailpoint) error {
			if point == processFailLogOpen {
				return errors.New("pre-spawn failure")
			}
			return nil
		},
	})
	request := startRequest("pre-permanent", "true", t.TempDir())
	if _, err := supervisor.Start(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "terminal state is not durable") {
		t.Fatalf("pre-spawn permanent failure = %v", err)
	}
	supervisor.mu.Lock()
	_, retained := supervisor.faulted[request.ID]
	supervisor.mu.Unlock()
	if !retained || reservations.aborted[request.ID] {
		t.Fatalf("faulted ownership=%v aborted=%v", retained, reservations.aborted[request.ID])
	}
	if _, err := supervisor.Start(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "unresolved durable state") {
		t.Fatalf("duplicate faulted start = %v", err)
	}
}

func TestProcessSupervisorRecoveryFaultsAreCoalescedAndBounded(t *testing.T) {
	supervisor := testSupervisor(
		t, newMemoryProcessStore(), newMemoryReservationBoundary(), &synchronizedBuffer{},
		processSupervisorConfig{},
	)
	for index := 0; index < 100; index++ {
		supervisor.recordRecovery("same", "terminal", errors.New("failed"))
	}
	faults := supervisor.RecoveryFaults()
	if len(faults) != 1 || faults[0].Count != 100 {
		t.Fatalf("coalesced faults = %#v", faults)
	}
	for index := 0; index < maxRecoveryFaults+10; index++ {
		supervisor.recordRecovery(fmt.Sprintf("process-%d", index), "terminal", errors.New("failed"))
	}
	if faults = supervisor.RecoveryFaults(); len(faults) != maxRecoveryFaults {
		t.Fatalf("recovery fault bound = %d", len(faults))
	}
}

func TestProcessSupervisorShutdownDeadlineWhileStartIsPersisting(t *testing.T) {
	store := newMemoryProcessStore()
	store.terminalFailures["persisting-start"] = 1
	entered := make(chan struct{})
	supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{
		TerminalRetries: 2,
		TerminalRetry:   100 * time.Millisecond,
		Fail: func(point processFailpoint) error {
			if point == processFailLogOpen {
				close(entered)
				return errors.New("pre-spawn failure")
			}
			return nil
		},
	})
	startDone := make(chan error, 1)
	go func() {
		_, err := supervisor.Start(context.Background(), startRequest("persisting-start", "true", t.TempDir()))
		startDone <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := supervisor.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown waiting for Start = %v", err)
	}
	if err := <-startDone; err == nil || !strings.Contains(err.Error(), "pre-spawn failure") {
		t.Fatalf("start result = %v", err)
	}
}

type injectedProcessBoundary struct {
	delegate  processBoundary
	afterErr  error
	signalErr error
	closeErr  error
	proofErr  error
	closed    bool
}

type recoveringProcessBoundary struct {
	delegate    processBoundary
	mu          sync.Mutex
	failSignals int
	failProofs  int
	signals     []os.Signal
}

func (boundary *recoveringProcessBoundary) AfterStart(cmd *exec.Cmd) error {
	return boundary.delegate.AfterStart(cmd)
}

func (boundary *recoveringProcessBoundary) Signal(signal os.Signal) error {
	boundary.mu.Lock()
	boundary.signals = append(boundary.signals, signal)
	if boundary.failSignals > 0 {
		boundary.failSignals--
		boundary.mu.Unlock()
		return errors.New("injected group signal failure")
	}
	boundary.mu.Unlock()
	return boundary.delegate.Signal(signal)
}

func (boundary *recoveringProcessBoundary) Close() error {
	return boundary.delegate.Close()
}

func (boundary *recoveringProcessBoundary) ProveContained(limit time.Duration) error {
	boundary.mu.Lock()
	if boundary.failProofs > 0 {
		boundary.failProofs--
		boundary.mu.Unlock()
		return errors.New("injected containment proof failure")
	}
	boundary.mu.Unlock()
	return boundary.delegate.ProveContained(limit)
}

func (boundary *recoveringProcessBoundary) signalCounts() (term, kill int) {
	boundary.mu.Lock()
	defer boundary.mu.Unlock()
	for _, signal := range boundary.signals {
		switch signal {
		case syscall.SIGTERM:
			term++
		case os.Kill:
			kill++
		}
	}
	return term, kill
}

func (boundary *injectedProcessBoundary) AfterStart(cmd *exec.Cmd) error {
	if boundary.afterErr != nil {
		_ = boundary.delegate.AfterStart(cmd)
		return boundary.afterErr
	}
	return boundary.delegate.AfterStart(cmd)
}

func (boundary *injectedProcessBoundary) Signal(signal os.Signal) error {
	if boundary.signalErr != nil {
		return boundary.signalErr
	}
	return boundary.delegate.Signal(signal)
}

func (boundary *injectedProcessBoundary) Close() error {
	boundary.closed = true
	delegateErr := boundary.delegate.Close()
	return errors.Join(delegateErr, boundary.closeErr)
}

func (boundary *injectedProcessBoundary) ProveContained(limit time.Duration) error {
	if boundary.proofErr != nil {
		return boundary.proofErr
	}
	return boundary.delegate.ProveContained(limit)
}

func TestProcessSupervisorAfterStartFailureClosesAndKillsGuard(t *testing.T) {
	var injected *injectedProcessBoundary
	supervisor := testSupervisor(
		t, newMemoryProcessStore(), newMemoryReservationBoundary(), &synchronizedBuffer{},
		processSupervisorConfig{
			BoundaryFactory: func(cmd *exec.Cmd) (processBoundary, error) {
				delegate, err := newBackgroundProcessBoundary(cmd)
				if err != nil {
					return nil, err
				}
				injected = &injectedProcessBoundary{
					delegate: delegate, afterErr: errors.New("assignment failed"),
				}
				return injected, nil
			},
			StopGrace: 100 * time.Millisecond,
		},
	)
	start := time.Now()
	if _, err := supervisor.Start(context.Background(), startRequest("after-start", "sleep 30", t.TempDir())); err == nil ||
		!strings.Contains(err.Error(), "assignment failed") {
		t.Fatalf("AfterStart failure = %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("AfterStart rollback blocked: %s", elapsed)
	}
	if injected == nil || !injected.closed {
		t.Fatal("failed boundary was not closed")
	}
}

func TestProcessSupervisorAfterStartUnresolvedCleanupRetainsOwnership(t *testing.T) {
	store := newMemoryProcessStore()
	reservations := newMemoryReservationBoundary()
	supervisor := testSupervisor(
		t, store, reservations, &synchronizedBuffer{},
		processSupervisorConfig{
			BoundaryFactory: func(cmd *exec.Cmd) (processBoundary, error) {
				delegate, err := newBackgroundProcessBoundary(cmd)
				if err != nil {
					return nil, err
				}
				return &injectedProcessBoundary{
					delegate:  delegate,
					afterErr:  errors.New("assignment failed"),
					signalErr: errors.New("signal failed"),
					closeErr:  errors.New("close failed"),
				}, nil
			},
			ProcessKill: func(*os.Process) error { return errors.New("direct kill failed") },
			ProcessWait: func(cmd *exec.Cmd) error {
				_ = cmd.Wait()
				return errors.New("wait failed")
			},
		},
	)
	request := startRequest("after-start-unresolved", "sleep 30", t.TempDir())
	if _, err := supervisor.Start(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "assignment failed") {
		t.Fatalf("AfterStart unresolved cleanup = %v", err)
	}
	supervisor.mu.Lock()
	_, retained := supervisor.faulted[request.ID]
	supervisor.mu.Unlock()
	if !retained || reservations.aborted[request.ID] {
		t.Fatalf("unresolved ownership=%v aborted=%v", retained, reservations.aborted[request.ID])
	}
	if record := store.get(request.ID); record.State != processdomain.StateStarting {
		t.Fatalf("unresolved cleanup terminalized record: %+v", record)
	}
}

func TestProcessSupervisorRollbackCleanupFailuresRetainOwnership(t *testing.T) {
	cases := []struct {
		name  string
		point processFailpoint
		mode  string
	}{
		{name: "collector-signal", point: processFailCollectorArm, mode: "signal"},
		{name: "collector-close", point: processFailCollectorArm, mode: "close"},
		{name: "waiter-proof", point: processFailWaiterArm, mode: "proof"},
		{name: "waiter-wait", point: processFailWaiterArm, mode: "wait"},
		{name: "registration-close", point: processFailRegistration, mode: "close"},
		{name: "registration-wait", point: processFailRegistration, mode: "wait"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := newMemoryProcessStore()
			reservations := newMemoryReservationBoundary()
			config := processSupervisorConfig{
				Fail: func(point processFailpoint) error {
					if point == test.point {
						return errors.New("transaction failed")
					}
					return nil
				},
				BoundaryFactory: func(cmd *exec.Cmd) (processBoundary, error) {
					delegate, err := newBackgroundProcessBoundary(cmd)
					if err != nil {
						return nil, err
					}
					injected := &injectedProcessBoundary{delegate: delegate}
					if test.mode == "signal" || test.mode == "proof" {
						injected.signalErr = errors.New("signal failed")
					}
					if test.mode == "signal" || test.mode == "close" {
						injected.closeErr = errors.New("close failed")
					}
					if test.mode == "proof" {
						injected.proofErr = errors.New("containment not proven")
					}
					return injected, nil
				},
			}
			if test.mode == "wait" {
				config.ProcessWait = func(cmd *exec.Cmd) error {
					_ = cmd.Wait()
					return errors.New("wait failed")
				}
			}
			supervisor := testSupervisor(t, store, reservations, &synchronizedBuffer{}, config)
			request := startRequest(test.name, "sleep 30", t.TempDir())
			if _, err := supervisor.Start(context.Background(), request); err == nil {
				t.Fatal("cleanup failure was not returned")
			}
			supervisor.mu.Lock()
			_, retained := supervisor.faulted[request.ID]
			supervisor.mu.Unlock()
			if !retained || reservations.aborted[request.ID] {
				t.Fatalf("retained=%v aborted=%v", retained, reservations.aborted[request.ID])
			}
			if record := store.get(request.ID); record.State.Terminal() {
				t.Fatalf("cleanup failure terminalized record: %+v", record)
			}
		})
	}
}

func TestProcessSupervisorRetriesFaultedHandleCleanupOnShutdown(t *testing.T) {
	dir := t.TempDir()
	shellPath := filepath.Join(dir, "shell.pid")
	childPath := filepath.Join(dir, "child.pid")
	var boundary *recoveringProcessBoundary
	store := newMemoryProcessStore()
	reservations := newMemoryReservationBoundary()
	supervisor := testSupervisor(t, store, reservations, &synchronizedBuffer{}, processSupervisorConfig{
		BoundaryFactory: func(cmd *exec.Cmd) (processBoundary, error) {
			delegate, err := newBackgroundProcessBoundary(cmd)
			if err != nil {
				return nil, err
			}
			boundary = &recoveringProcessBoundary{
				delegate: delegate, failSignals: 3, failProofs: 1,
			}
			return boundary, nil
		},
		Fail: func(point processFailpoint) error {
			if point != processFailRegistration {
				return nil
			}
			waitForParseablePID(t, shellPath)
			waitForParseablePID(t, childPath)
			return errors.New("registration failed")
		},
	})
	command := fmt.Sprintf(
		"echo $$ > %q; sleep 30 & echo $! > %q; wait",
		shellPath, childPath,
	)
	request := startRequest("recover-cleanup", command, dir)
	if _, err := supervisor.Start(context.Background(), request); err == nil {
		t.Fatal("registration rollback unexpectedly succeeded")
	}
	supervisor.mu.Lock()
	ownership := supervisor.faulted[request.ID]
	supervisor.mu.Unlock()
	if ownership == nil || ownership.handle == nil {
		t.Fatal("unresolved cleanup did not retain its process handle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := supervisor.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown did not recover faulted cleanup: %v", err)
	}
	waitProcessGone(t, readPID(t, shellPath))
	waitProcessGone(t, readPID(t, childPath))
	if !reservations.aborted[request.ID] {
		t.Fatal("recovered rollback did not release its reservation")
	}
}

func TestProcessSupervisorUnixWatchdogFallbackContainsTrees(t *testing.T) {
	cases := []struct {
		name        string
		failSignals int
		start       func(*testing.T, *processSupervisor, processStartRequest) error
	}{
		{
			name:        "rollback",
			failSignals: 3,
			start: func(t *testing.T, supervisor *processSupervisor, request processStartRequest) error {
				supervisor.config.Fail = func(point processFailpoint) error {
					if point == processFailRegistration {
						waitForParseablePID(t, filepath.Join(request.Workdir, "shell.pid"))
						waitForParseablePID(t, filepath.Join(request.Workdir, "child.pid"))
						return errors.New("registration failed")
					}
					return nil
				}
				_, err := supervisor.Start(context.Background(), request)
				return err
			},
		},
		{
			name:        "stop",
			failSignals: 4,
			start: func(t *testing.T, supervisor *processSupervisor, request processStartRequest) error {
				if _, err := supervisor.Start(context.Background(), request); err != nil {
					return err
				}
				waitForParseablePID(t, filepath.Join(request.Workdir, "child.pid"))
				return supervisor.Stop(request.ID)
			},
		},
		{
			name:        "lifetime",
			failSignals: 3,
			start: func(t *testing.T, supervisor *processSupervisor, request processStartRequest) error {
				request.Lifetime = 300 * time.Millisecond
				_, err := supervisor.Start(context.Background(), request)
				if err != nil {
					return err
				}
				waitTerminalWithSupervisor(t, supervisor, supervisor.store.(*memoryProcessStore), request.ID)
				return nil
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			shellPath := filepath.Join(dir, "shell.pid")
			childPath := filepath.Join(dir, "child.pid")
			var boundary *recoveringProcessBoundary
			store := newMemoryProcessStore()
			supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{
				BoundaryFactory: func(cmd *exec.Cmd) (processBoundary, error) {
					delegate, err := newBackgroundProcessBoundary(cmd)
					if err != nil {
						return nil, err
					}
					boundary = &recoveringProcessBoundary{delegate: delegate, failSignals: test.failSignals}
					return boundary, nil
				},
			})
			command := fmt.Sprintf(
				"echo $$ > %q; sleep 30 & echo $! > %q; wait",
				shellPath, childPath,
			)
			request := startRequest("watchdog-"+test.name, command, dir)
			err := test.start(t, supervisor, request)
			if test.name == "rollback" {
				if err == nil || !strings.Contains(err.Error(), "registration failed") {
					t.Fatalf("rollback result = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			waitForParseablePID(t, shellPath)
			waitForParseablePID(t, childPath)
			waitProcessGone(t, readPID(t, shellPath))
			waitProcessGone(t, readPID(t, childPath))
			if boundary == nil {
				t.Fatal("boundary was not created")
			}
		})
	}
}

func TestProcessSupervisorGuardExitContainmentCanBeRetried(t *testing.T) {
	dir := t.TempDir()
	shellPath := filepath.Join(dir, "shell.pid")
	childPath := filepath.Join(dir, "child.pid")
	var boundary *recoveringProcessBoundary
	store := newMemoryProcessStore()
	supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{
		BoundaryFactory: func(cmd *exec.Cmd) (processBoundary, error) {
			delegate, err := newBackgroundProcessBoundary(cmd)
			if err != nil {
				return nil, err
			}
			boundary = &recoveringProcessBoundary{delegate: delegate, failSignals: 3}
			return boundary, nil
		},
	})
	command := fmt.Sprintf(
		"echo $$ > %q; sleep 30 & echo $! > %q; wait",
		shellPath, childPath,
	)
	record, err := supervisor.Start(context.Background(), startRequest("guard-exit-retry", command, dir))
	if err != nil {
		t.Fatal(err)
	}
	waitForParseablePID(t, shellPath)
	waitForParseablePID(t, childPath)
	if err := syscall.Kill(record.GuardPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitForRecoveryFault(t, supervisor)
	if got := store.get(record.ID); got.State.Terminal() {
		t.Fatalf("unproven containment was terminalized: %+v", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := supervisor.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown retry did not recover guard exit: %v", err)
	}
	waitProcessGone(t, readPID(t, shellPath))
	waitProcessGone(t, readPID(t, childPath))
	if got := store.get(record.ID); !got.State.Terminal() {
		t.Fatalf("recovered guard exit was not terminalized: %+v", got)
	}
	if boundary == nil {
		t.Fatal("boundary was not created")
	}
}

func TestProcessSupervisorLogWriteFailureUsesVerifiedContainment(t *testing.T) {
	for _, failProofs := range []int{0, 1} {
		name := "fallback"
		if failProofs > 0 {
			name = "unresolved-then-retry"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			shellPath := filepath.Join(dir, "shell.pid")
			childPath := filepath.Join(dir, "child.pid")
			var boundary *recoveringProcessBoundary
			store := newMemoryProcessStore()
			supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), failingLogWriter{}, processSupervisorConfig{
				BoundaryFactory: func(cmd *exec.Cmd) (processBoundary, error) {
					delegate, err := newBackgroundProcessBoundary(cmd)
					if err != nil {
						return nil, err
					}
					boundary = &recoveringProcessBoundary{
						delegate: delegate, failSignals: 3, failProofs: failProofs,
					}
					return boundary, nil
				},
			})
			command := fmt.Sprintf(
				"echo $$ > %q; sleep 30 & echo $! > %q; sleep 0.05; printf x; wait",
				shellPath, childPath,
			)
			request := startRequest("log-containment-"+name, command, dir)
			if _, err := supervisor.Start(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			waitForParseablePID(t, shellPath)
			waitForParseablePID(t, childPath)
			if failProofs == 0 {
				record := waitTerminalWithSupervisor(t, supervisor, store, request.ID)
				if record.State != processdomain.StateFailed || !strings.Contains(record.Error, "log write failed") {
					t.Fatalf("log failure record = %+v", record)
				}
			} else {
				waitForRecoveryFault(t, supervisor)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := supervisor.Shutdown(ctx); err != nil {
					t.Fatalf("log containment recovery = %v", err)
				}
			}
			waitProcessGone(t, readPID(t, shellPath))
			waitProcessGone(t, readPID(t, childPath))
			if boundary == nil {
				t.Fatal("boundary was not created")
			}
		})
	}
}

func TestProcessSupervisorConcurrentShutdownHonorsCallerContext(t *testing.T) {
	releaseWait := make(chan struct{})
	enteredWait := make(chan struct{})
	var waitOnce sync.Once
	supervisor := testSupervisor(t, newMemoryProcessStore(), newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{
		ProcessWait: func(cmd *exec.Cmd) error {
			err := cmd.Wait()
			waitOnce.Do(func() { close(enteredWait) })
			<-releaseWait
			return err
		},
		StopGrace: 20 * time.Millisecond,
	})
	if _, err := supervisor.Start(context.Background(), startRequest("shutdown-join", "sleep 30", t.TempDir())); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		firstDone <- supervisor.Shutdown(ctx)
	}()
	<-enteredWait
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	err := supervisor.Shutdown(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second shutdown = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("second shutdown ignored its deadline: %s", elapsed)
	}
	close(releaseWait)
	if err := <-firstDone; err != nil {
		t.Fatalf("first shutdown = %v", err)
	}
}

func TestProcessSupervisorStopAndLifetimeEscalateSignals(t *testing.T) {
	for _, mode := range []string{"stop", "lifetime"} {
		t.Run(mode, func(t *testing.T) {
			var boundary *recoveringProcessBoundary
			store := newMemoryProcessStore()
			supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{
				BoundaryFactory: func(cmd *exec.Cmd) (processBoundary, error) {
					delegate, err := newBackgroundProcessBoundary(cmd)
					if err != nil {
						return nil, err
					}
					boundary = &recoveringProcessBoundary{delegate: delegate}
					return boundary, nil
				},
				StopGrace: 20 * time.Millisecond,
			})
			request := startRequest(
				"signal-"+mode,
				"trap '' TERM; while :; do sleep 1; done",
				t.TempDir(),
			)
			if mode == "lifetime" {
				request.Lifetime = 50 * time.Millisecond
			}
			if _, err := supervisor.Start(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if mode == "stop" {
				if err := supervisor.Stop(request.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				waitTerminalWithSupervisor(t, supervisor, store, request.ID)
			}
			term, kill := boundary.signalCounts()
			if mode == "stop" && term == 0 {
				t.Fatal("Stop did not attempt graceful termination")
			}
			if kill == 0 {
				t.Fatalf("%s did not escalate to hard containment (term=%d kill=%d)", mode, term, kill)
			}
		})
	}
}

func TestProcessSupervisorContainmentFallbackAndFailure(t *testing.T) {
	for _, test := range []struct {
		name      string
		signalErr error
		closeErr  error
	}{
		{name: "signal", signalErr: errors.New("kill failed")},
		{name: "close", closeErr: errors.New("close failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newMemoryProcessStore()
			supervisor := testSupervisor(
				t, store, newMemoryReservationBoundary(), &synchronizedBuffer{},
				processSupervisorConfig{
					BoundaryFactory: func(cmd *exec.Cmd) (processBoundary, error) {
						delegate, err := newBackgroundProcessBoundary(cmd)
						if err != nil {
							return nil, err
						}
						return &injectedProcessBoundary{
							delegate: delegate, signalErr: test.signalErr, closeErr: test.closeErr,
						}, nil
					},
				},
			)
			_, err := supervisor.Start(context.Background(), startRequest("containment-"+test.name, "true", t.TempDir()))
			if err != nil && !strings.Contains(err.Error(), "containment failed") {
				t.Fatalf("containment failure = %v", err)
			}
			id := "containment-" + test.name
			waitForRecoveryFault(t, supervisor)
			record := store.get(id)
			supervisor.mu.Lock()
			retained := supervisor.handles[id]
			supervisor.mu.Unlock()
			if test.name == "signal" {
				if record.State != processdomain.StateSucceeded || retained != nil {
					t.Fatalf("signal fallback record=%+v retained=%v", record, retained != nil)
				}
			} else if record.State != processdomain.StateRunning || retained == nil {
				t.Fatalf("close failure record=%+v retained=%v", record, retained != nil)
			}
		})
	}
}

func waitForRecoveryFault(t *testing.T, supervisor *processSupervisor) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(supervisor.RecoveryFaults()) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("recovery fault did not surface")
}

func waitForRecoveryCount(t *testing.T, supervisor *processSupervisor, count uint64) []processRecoveryFault {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		faults := supervisor.RecoveryFaults()
		if len(faults) > 0 && faults[0].Count >= count {
			return faults
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("recovery fault count did not reach %d: %#v", count, supervisor.RecoveryFaults())
	return nil
}

func TestProcessSupervisorTerminalErrorBroadcastsToRepeatedStops(t *testing.T) {
	store := newMemoryProcessStore()
	store.terminalFailures["broadcast"] = 1000
	supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{
		TerminalRetries: 2,
		TerminalRetry:   time.Millisecond,
	})
	if _, err := supervisor.Start(context.Background(), startRequest("broadcast", "sleep 30", t.TempDir())); err != nil {
		t.Fatal(err)
	}
	firstErr := supervisor.Stop("broadcast")
	if firstErr == nil || !strings.Contains(firstErr.Error(), "terminal state is not durable") {
		t.Fatalf("first stop = %v", firstErr)
	}
	results := make(chan error, 8)
	for index := 0; index < cap(results); index++ {
		go func() { results <- supervisor.Stop("broadcast") }()
	}
	for index := 0; index < cap(results); index++ {
		select {
		case err := <-results:
			if err == nil || err.Error() != firstErr.Error() {
				t.Fatalf("broadcast stop = %v, want %v", err, firstErr)
			}
		case <-time.After(time.Second):
			t.Fatal("repeated Stop blocked after terminal error")
		}
	}
}

func TestProcessSupervisorShutdownTimeoutCanBeRetried(t *testing.T) {
	releaseWait := make(chan struct{})
	store := newMemoryProcessStore()
	supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{
		ProcessWait: func(cmd *exec.Cmd) error {
			err := cmd.Wait()
			<-releaseWait
			return err
		},
		StopGrace: 20 * time.Millisecond,
	})
	if _, err := supervisor.Start(context.Background(), startRequest("shutdown-retry", "sleep 30", t.TempDir())); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		err := supervisor.Shutdown(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown attempt %d = %v", attempt+1, err)
		}
	}
	close(releaseWait)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown retry after recovery = %v", err)
	}
}

func TestProcessSupervisorStopExitLifetimeRaceCommitsOnce(t *testing.T) {
	for index := 0; index < 5; index++ {
		store := newMemoryProcessStore()
		supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{})
		id := fmt.Sprintf("race-%d", index)
		request := startRequest(id, "sleep 0.05", t.TempDir())
		request.Lifetime = 55 * time.Millisecond
		if _, err := supervisor.Start(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(45 * time.Millisecond)
			_ = supervisor.Stop(id)
		}()
		record := waitTerminal(t, store, id)
		if !record.State.Terminal() || store.transitions[id] != 1 {
			t.Fatalf("race result=%+v transitions=%d", record, store.transitions[id])
		}
	}
}

func TestProcessSupervisorGracefulShutdown(t *testing.T) {
	store := newMemoryProcessStore()
	supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{})
	if _, err := supervisor.Start(context.Background(), startRequest("shutdown", "sleep 30", t.TempDir())); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := supervisor.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if record := waitTerminal(t, store, "shutdown"); record.State != processdomain.StateInterrupted {
		t.Fatalf("shutdown result = %+v", record)
	}
}

func TestSupervisorHardCrashHelper(t *testing.T) {
	if os.Getenv("KAROZ_HARD_CRASH_HELPER") != "1" {
		return
	}
	dir := os.Getenv("KAROZ_HARD_CRASH_DIR")
	store := newMemoryProcessStore()
	supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{})
	command := fmt.Sprintf(
		"echo $$ > %q; sleep 30 & echo $! > %q; wait",
		filepath.Join(dir, "child.pid"), filepath.Join(dir, "grandchild.pid"),
	)
	if _, err := supervisor.Start(context.Background(), startRequest("hard-crash", command, dir)); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0o600); err != nil {
		os.Exit(4)
	}
	select {}
}

func TestProcessWatchdogKillsDescendantsAfterParentSIGKILL(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestSupervisorHardCrashHelper")
	cmd.Env = append(os.Environ(), "KAROZ_HARD_CRASH_HELPER=1", "KAROZ_HARD_CRASH_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	waitForFile(t, filepath.Join(dir, "ready"))
	childPath := filepath.Join(dir, "child.pid")
	grandchildPath := filepath.Join(dir, "grandchild.pid")
	waitForFile(t, childPath)
	waitForFile(t, grandchildPath)
	childPID := readPID(t, childPath)
	grandchildPID := readPID(t, grandchildPath)
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	waitProcessGone(t, childPID)
	waitProcessGone(t, grandchildPID)
}

func TestProcessSupervisorKillsResidualGroupAfterGuardExit(t *testing.T) {
	t.Run("guard-killed", func(t *testing.T) {
		dir := t.TempDir()
		store := newMemoryProcessStore()
		supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{})
		path := filepath.Join(dir, "shell.pid")
		record, err := supervisor.Start(context.Background(), startRequest("guard-killed", fmt.Sprintf("echo $$ > %q; sleep 30", path), dir))
		if err != nil {
			t.Fatal(err)
		}
		waitForFile(t, path)
		shellPID := readPID(t, path)
		if err := syscall.Kill(record.GuardPID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		if got := waitTerminal(t, store, record.ID); got.State != processdomain.StateFailed {
			t.Fatalf("guard kill result = %+v", got)
		}
		waitProcessGone(t, shellPID)
	})

	t.Run("background-descendant", func(t *testing.T) {
		dir := t.TempDir()
		store := newMemoryProcessStore()
		supervisor := testSupervisor(t, store, newMemoryReservationBoundary(), &synchronizedBuffer{}, processSupervisorConfig{})
		path := filepath.Join(dir, "descendant.pid")
		command := fmt.Sprintf("sleep 30 & echo $! > %q; exit 0", path)
		if _, err := supervisor.Start(context.Background(), startRequest("background-descendant", command, dir)); err != nil {
			t.Fatal(err)
		}
		waitForFile(t, path)
		descendantPID := readPID(t, path)
		if got := waitTerminal(t, store, "background-descendant"); got.State != processdomain.StateSucceeded {
			t.Fatalf("background descendant result = %+v", got)
		}
		waitProcessGone(t, descendantPID)
	})
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s did not appear", path)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		value, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(value)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s did not contain a complete positive PID", path)
	return 0
}

func waitForParseablePID(t *testing.T, path string) {
	t.Helper()
	_ = readPID(t, path)
}

func TestUnixProcessBoundaryRejectsNonPositivePGID(t *testing.T) {
	for _, pgid := range []int{0, -1} {
		boundary := &unixProcessBoundary{pgid: pgid}
		if err := boundary.Signal(syscall.SIGKILL); err == nil {
			t.Fatalf("signaled non-positive pgid %d", pgid)
		}
	}
}

func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d survived parent crash", pid)
}
