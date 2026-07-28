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
	mu          sync.Mutex
	records     map[string]processdomain.Process
	transitions map[string]int
	failRunning bool
}

func newMemoryProcessStore() *memoryProcessStore {
	return &memoryProcessStore{
		records:     make(map[string]processdomain.Process),
		transitions: make(map[string]int),
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
	mu       sync.Mutex
	reserved map[string]bool
	aborted  map[string]bool
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
		supervisor := testSupervisor(t, store, reservations, &synchronizedBuffer{}, processSupervisorConfig{})
		_, err := supervisor.Start(context.Background(), startRequest("registration-save", "sleep 30", t.TempDir()))
		if err == nil {
			t.Fatal("registration save failure did not roll back")
		}
		record := waitTerminal(t, store, "registration-save")
		if record.State != processdomain.StateFailed || !reservations.aborted["registration-save"] {
			t.Fatalf("registration rollback = %+v, %+v", record, reservations.aborted)
		}
	})
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
	record := waitTerminal(t, store, "log-fail")
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
	record = waitTerminal(t, store, "lifetime")
	if record.State != processdomain.StateFailed || record.Error != "lifetime exceeded" {
		t.Fatalf("lifetime result = %+v", record)
	}
	if store.transitions["lifetime"] != 1 {
		t.Fatalf("terminal transition count = %d", store.transitions["lifetime"])
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
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(value)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
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
