package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	processdomain "github.com/karoz/karoz/internal/process"
)

type processKernelStore interface {
	CreateStarting(processdomain.Process) error
	MarkRunning(processdomain.Process) error
	MarkTerminal(processdomain.Process) error
}

type processReservationBoundary interface {
	Reserve(processdomain.Process) error
	Abort(processdomain.Process) error
}

type processLogOpener func(processdomain.Process) (io.WriteCloser, error)

type processFailpoint string

const (
	processFailLogOpen      processFailpoint = "log_open"
	processFailWatchdog     processFailpoint = "watchdog"
	processFailSpawn        processFailpoint = "spawn"
	processFailCollectorArm processFailpoint = "collector_arm"
	processFailWaiterArm    processFailpoint = "waiter_arm"
	processFailRegistration processFailpoint = "registration"
)

type processSupervisorConfig struct {
	LogBytes        int64
	TailLines       int
	ExitDrain       time.Duration
	StopGrace       time.Duration
	Fail            func(processFailpoint) error
	GuardExecutable string
	GuardArgsPrefix []string
}

type processStartRequest struct {
	ID, ProjectID, AgentID, RunID string
	Command, Workdir, Description string
	Lifetime                      time.Duration
}

type processSupervisor struct {
	ctx    context.Context
	cancel context.CancelFunc

	store        processKernelStore
	reservations processReservationBoundary
	openLog      processLogOpener
	config       processSupervisorConfig

	mu      sync.Mutex
	closed  bool
	handles map[string]*supervisedProcess
}

type supervisedProcess struct {
	supervisor *processSupervisor
	record     processdomain.Process
	cmd        *exec.Cmd
	watchdog   *os.File
	stdout     io.ReadCloser
	stderr     io.ReadCloser
	log        io.WriteCloser
	buffer     *processdomain.OutputBuffer

	gate         chan struct{}
	registered   bool
	exitObserved chan struct{}
	waitDone     chan struct{}
	terminalDone chan struct{}
	stdoutDone   chan struct{}
	stderrDone   chan struct{}

	collectMu sync.Mutex
	stateMu   sync.Mutex
	cause     processTerminalCause
	terminal  sync.Once
}

type processTerminalCause struct {
	state processdomain.State
	err   string
}

func newProcessSupervisor(
	serverCtx context.Context,
	store processKernelStore,
	reservations processReservationBoundary,
	openLog processLogOpener,
	config processSupervisorConfig,
) (*processSupervisor, error) {
	if serverCtx == nil || store == nil || reservations == nil || openLog == nil {
		return nil, errors.New("process supervisor dependencies are incomplete")
	}
	if !backgroundProcessSupported {
		return nil, errors.New("background processes are unsupported on this platform")
	}
	if config.LogBytes <= 0 {
		config.LogBytes = 8 << 20
	}
	if config.TailLines <= 0 {
		config.TailLines = 200
	}
	if config.ExitDrain <= 0 {
		config.ExitDrain = 250 * time.Millisecond
	}
	if config.StopGrace <= 0 {
		config.StopGrace = 500 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(serverCtx)
	return &processSupervisor{
		ctx: ctx, cancel: cancel, store: store, reservations: reservations,
		openLog: openLog, config: config, handles: make(map[string]*supervisedProcess),
	}, nil
}

// Start deliberately ignores creatorCtx. A request, Run, turn, or SSE
// subscriber may record provenance but has no cancellation authority.
func (supervisor *processSupervisor) Start(_ context.Context, request processStartRequest) (processdomain.Process, error) {
	if request.ID == "" || request.ProjectID == "" || request.AgentID == "" ||
		request.Command == "" || request.Workdir == "" {
		return processdomain.Process{}, errors.New("invalid background process request")
	}
	select {
	case <-supervisor.ctx.Done():
		return processdomain.Process{}, errors.New("process supervisor is shutting down")
	default:
	}
	now := time.Now().UTC()
	record := processdomain.Process{
		ID: request.ID, ProjectID: request.ProjectID, AgentID: request.AgentID,
		RunID: request.RunID, Command: request.Command, Workdir: request.Workdir,
		Description: request.Description, State: processdomain.StateStarting,
		LifetimeMS: request.Lifetime.Milliseconds(), StartedAt: now, UpdatedAt: now,
	}
	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		return processdomain.Process{}, errors.New("process supervisor is closed")
	}
	if _, exists := supervisor.handles[record.ID]; exists {
		supervisor.mu.Unlock()
		return processdomain.Process{}, errors.New("process already exists")
	}
	supervisor.mu.Unlock()

	if err := supervisor.reservations.Reserve(record); err != nil {
		return processdomain.Process{}, err
	}
	if err := supervisor.store.CreateStarting(record); err != nil {
		_ = supervisor.reservations.Abort(record)
		return processdomain.Process{}, err
	}
	if err := supervisor.fail(processFailLogOpen); err != nil {
		return supervisor.failBeforeSpawn(record, err)
	}
	logWriter, err := supervisor.openLog(record)
	if err != nil {
		return supervisor.failBeforeSpawn(record, err)
	}
	if err := supervisor.fail(processFailWatchdog); err != nil {
		_ = logWriter.Close()
		return supervisor.failBeforeSpawn(record, err)
	}
	handle, err := supervisor.launch(record, logWriter)
	if err != nil {
		_ = logWriter.Close()
		return supervisor.failBeforeSpawn(record, err)
	}
	supervisor.mu.Lock()
	supervisor.handles[record.ID] = handle
	supervisor.mu.Unlock()

	if err := supervisor.fail(processFailCollectorArm); err != nil {
		return supervisor.rollbackSpawn(handle, err, false)
	}
	supervisor.armCollectors(handle)
	if err := supervisor.fail(processFailWaiterArm); err != nil {
		return supervisor.rollbackSpawn(handle, err, false)
	}
	supervisor.armWaiter(handle)

	if err := supervisor.fail(processFailRegistration); err != nil {
		return supervisor.rollbackSpawn(handle, err, true)
	}
	handle.record.State = processdomain.StateRunning
	handle.record.PID = handle.cmd.Process.Pid
	handle.record.GuardPID = handle.cmd.Process.Pid
	handle.record.PGID = handle.cmd.Process.Pid
	handle.record.UpdatedAt = time.Now().UTC()
	if err := supervisor.store.MarkRunning(handle.record); err != nil {
		return supervisor.rollbackSpawn(handle, err, true)
	}
	handle.stateMu.Lock()
	handle.registered = true
	handle.stateMu.Unlock()
	close(handle.gate)
	if request.Lifetime > 0 {
		go supervisor.enforceLifetime(handle, request.Lifetime)
	}
	select {
	case <-handle.exitObserved:
		<-handle.terminalDone
		return handle.snapshot(), nil
	default:
		return handle.snapshot(), nil
	}
}

func (supervisor *processSupervisor) fail(point processFailpoint) error {
	if supervisor.config.Fail == nil {
		return nil
	}
	return supervisor.config.Fail(point)
}

func (supervisor *processSupervisor) failBeforeSpawn(record processdomain.Process, cause error) (processdomain.Process, error) {
	record.State = processdomain.StateFailed
	record.Error = cause.Error()
	record.UpdatedAt = time.Now().UTC()
	record.EndedAt = timePointer(record.UpdatedAt)
	_ = supervisor.store.MarkTerminal(record)
	_ = supervisor.reservations.Abort(record)
	return record, cause
}

func (supervisor *processSupervisor) launch(record processdomain.Process, logWriter io.WriteCloser) (*supervisedProcess, error) {
	if err := supervisor.fail(processFailSpawn); err != nil {
		return nil, err
	}
	executable := supervisor.config.GuardExecutable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	watchdogRead, watchdogWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	args := append([]string(nil), supervisor.config.GuardArgsPrefix...)
	args = append(args, "process-guard", "--", "bash", "-lc", record.Command)
	cmd := exec.CommandContext(supervisor.ctx, executable, args...)
	cmd.Dir = record.Workdir
	cmd.ExtraFiles = []*os.File{watchdogRead}
	prepareBackgroundGuardProcess(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		watchdogRead.Close()
		watchdogWrite.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		watchdogRead.Close()
		watchdogWrite.Close()
		stdout.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		watchdogRead.Close()
		watchdogWrite.Close()
		stdout.Close()
		stderr.Close()
		return nil, err
	}
	_ = watchdogRead.Close()
	return &supervisedProcess{
		supervisor: supervisor, record: record, cmd: cmd, watchdog: watchdogWrite,
		stdout: stdout, stderr: stderr, log: logWriter,
		buffer: processdomain.NewOutputBuffer(supervisor.config.TailLines, supervisor.config.LogBytes),
		gate:   make(chan struct{}), exitObserved: make(chan struct{}),
		waitDone: make(chan struct{}), terminalDone: make(chan struct{}),
		stdoutDone: make(chan struct{}), stderrDone: make(chan struct{}),
	}, nil
}

func (supervisor *processSupervisor) armCollectors(handle *supervisedProcess) {
	stdoutArmed := make(chan struct{})
	stderrArmed := make(chan struct{})
	go supervisor.collect(handle, "stdout", handle.stdout, handle.stdoutDone, stdoutArmed)
	go supervisor.collect(handle, "stderr", handle.stderr, handle.stderrDone, stderrArmed)
	<-stdoutArmed
	<-stderrArmed
}

func (supervisor *processSupervisor) collect(
	handle *supervisedProcess,
	stream string,
	reader io.Reader,
	done, armed chan struct{},
) {
	defer close(done)
	close(armed)
	<-handle.gate
	handle.stateMu.Lock()
	registered := handle.registered
	handle.stateMu.Unlock()
	if !registered {
		return
	}
	chunk := make([]byte, 4096)
	for {
		count, err := reader.Read(chunk)
		if count > 0 {
			handle.collectMu.Lock()
			_, accepted := handle.buffer.AppendStream(stream, string(chunk[:count]))
			if accepted > 0 {
				written, writeErr := handle.log.Write(chunk[:accepted])
				if writeErr == nil && int64(written) != accepted {
					writeErr = io.ErrShortWrite
				}
				if writeErr != nil {
					handle.collectMu.Unlock()
					handle.setCause(processdomain.StateFailed, "process log write failed: "+writeErr.Error())
					_ = signalBackgroundProcessGroup(handle.pgid(), syscall.SIGKILL)
					return
				}
			}
			handle.collectMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (supervisor *processSupervisor) armWaiter(handle *supervisedProcess) {
	armed := make(chan struct{})
	go func() {
		defer close(handle.waitDone)
		close(armed)
		waitErr := handle.cmd.Wait()
		close(handle.exitObserved)
		<-handle.gate
		handle.stateMu.Lock()
		registered := handle.registered
		handle.stateMu.Unlock()
		if !registered {
			return
		}
		supervisor.waitForCollectors(handle)
		handle.collectMu.Lock()
		_ = handle.buffer.FlushAllSequenced()
		_ = handle.log.Close()
		handle.collectMu.Unlock()
		if handle.watchdog != nil {
			_ = handle.watchdog.Close()
		}
		supervisor.finish(handle, waitErr)
	}()
	<-armed
}

func (supervisor *processSupervisor) waitForCollectors(handle *supervisedProcess) {
	timer := time.NewTimer(supervisor.config.ExitDrain)
	defer timer.Stop()
	stdoutDone, stderrDone := handle.stdoutDone, handle.stderrDone
	for stdoutDone != nil || stderrDone != nil {
		select {
		case <-stdoutDone:
			stdoutDone = nil
		case <-stderrDone:
			stderrDone = nil
		case <-timer.C:
			_ = handle.stdout.Close()
			_ = handle.stderr.Close()
			return
		}
	}
}

func (supervisor *processSupervisor) finish(handle *supervisedProcess, waitErr error) {
	handle.terminal.Do(func() {
		defer close(handle.terminalDone)
		record := handle.snapshot()
		handle.stateMu.Lock()
		cause := handle.cause
		handle.stateMu.Unlock()
		exitCode := 0
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if waitErr != nil {
			exitCode = -1
		}
		if cause.state != "" {
			record.State, record.Error = cause.state, cause.err
		} else if waitErr == nil {
			record.State = processdomain.StateSucceeded
		} else {
			record.State = processdomain.StateFailed
			record.Error = waitErr.Error()
		}
		record.ExitCode = exitCode
		record.UpdatedAt = time.Now().UTC()
		record.EndedAt = timePointer(record.UpdatedAt)
		record.LogBytes, record.LogLines, record.LogTruncated = handle.buffer.Stats()
		_ = supervisor.store.MarkTerminal(record)
		handle.stateMu.Lock()
		handle.record = record
		handle.stateMu.Unlock()
		supervisor.mu.Lock()
		delete(supervisor.handles, record.ID)
		supervisor.mu.Unlock()
	})
}

func (supervisor *processSupervisor) rollbackSpawn(handle *supervisedProcess, cause error, waiterArmed bool) (processdomain.Process, error) {
	handle.setCause(processdomain.StateFailed, cause.Error())
	handle.stateMu.Lock()
	handle.registered = false
	handle.stateMu.Unlock()
	select {
	case <-handle.gate:
	default:
		close(handle.gate)
	}
	_ = signalBackgroundProcessGroup(handle.cmd.Process.Pid, syscall.SIGKILL)
	if handle.watchdog != nil {
		_ = handle.watchdog.Close()
	}
	if waiterArmed {
		<-handle.waitDone
	} else {
		_ = handle.cmd.Wait()
	}
	_ = handle.stdout.Close()
	_ = handle.stderr.Close()
	_ = handle.log.Close()
	record := handle.snapshot()
	record.State = processdomain.StateFailed
	record.Error = cause.Error()
	record.UpdatedAt = time.Now().UTC()
	record.EndedAt = timePointer(record.UpdatedAt)
	_ = supervisor.store.MarkTerminal(record)
	_ = supervisor.reservations.Abort(record)
	supervisor.mu.Lock()
	delete(supervisor.handles, record.ID)
	supervisor.mu.Unlock()
	return record, cause
}

func (supervisor *processSupervisor) Stop(id string) error {
	supervisor.mu.Lock()
	handle := supervisor.handles[id]
	supervisor.mu.Unlock()
	if handle == nil {
		return errors.New("process not found")
	}
	select {
	case <-handle.exitObserved:
		<-handle.terminalDone
		return nil
	default:
	}
	handle.setCause(processdomain.StateKilled, "process stopped")
	if err := signalBackgroundProcessGroup(handle.pgid(), syscall.SIGTERM); err != nil {
		return err
	}
	timer := time.NewTimer(supervisor.config.StopGrace)
	defer timer.Stop()
	select {
	case <-handle.terminalDone:
		return nil
	case <-timer.C:
		_ = signalBackgroundProcessGroup(handle.pgid(), syscall.SIGKILL)
		<-handle.terminalDone
		return nil
	}
}

func (supervisor *processSupervisor) enforceLifetime(handle *supervisedProcess, lifetime time.Duration) {
	timer := time.NewTimer(lifetime)
	defer timer.Stop()
	select {
	case <-handle.terminalDone:
		return
	case <-handle.exitObserved:
		return
	case <-timer.C:
		handle.setCause(processdomain.StateFailed, "lifetime exceeded")
		_ = signalBackgroundProcessGroup(handle.pgid(), syscall.SIGKILL)
	}
}

func (supervisor *processSupervisor) Shutdown(ctx context.Context) error {
	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		return nil
	}
	supervisor.closed = true
	handles := make([]*supervisedProcess, 0, len(supervisor.handles))
	for _, handle := range supervisor.handles {
		handles = append(handles, handle)
	}
	supervisor.mu.Unlock()
	for _, handle := range handles {
		select {
		case <-handle.exitObserved:
			continue
		default:
		}
		handle.setCause(processdomain.StateInterrupted, "server shutting down")
		_ = signalBackgroundProcessGroup(handle.pgid(), syscall.SIGTERM)
	}
	for _, handle := range handles {
		grace := time.NewTimer(supervisor.config.StopGrace)
		select {
		case <-handle.terminalDone:
			if !grace.Stop() {
				<-grace.C
			}
		case <-grace.C:
			_ = signalBackgroundProcessGroup(handle.pgid(), syscall.SIGKILL)
			select {
			case <-handle.terminalDone:
			case <-ctx.Done():
				supervisor.cancel()
				return ctx.Err()
			}
		case <-ctx.Done():
			if !grace.Stop() {
				<-grace.C
			}
			_ = signalBackgroundProcessGroup(handle.pgid(), syscall.SIGKILL)
			supervisor.cancel()
			return ctx.Err()
		}
	}
	supervisor.cancel()
	return nil
}

func (handle *supervisedProcess) setCause(state processdomain.State, message string) {
	handle.stateMu.Lock()
	if handle.cause.state == "" {
		handle.cause = processTerminalCause{state: state, err: message}
	}
	handle.stateMu.Unlock()
}

func (handle *supervisedProcess) snapshot() processdomain.Process {
	handle.stateMu.Lock()
	defer handle.stateMu.Unlock()
	return handle.record
}

func (handle *supervisedProcess) pgid() int {
	handle.stateMu.Lock()
	defer handle.stateMu.Unlock()
	return handle.record.PGID
}

func timePointer(value time.Time) *time.Time {
	copy := value
	return &copy
}

func (request processStartRequest) String() string {
	return fmt.Sprintf("%s/%s", request.ProjectID, request.ID)
}
