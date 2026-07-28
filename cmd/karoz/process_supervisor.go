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

const (
	defaultProcessLifetime = time.Hour
	maxProcessLifetime     = 24 * time.Hour
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

type processBoundary interface {
	AfterStart(*exec.Cmd) error
	Signal(os.Signal) error
	Close() error
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
	DefaultLifetime time.Duration
	MaxLifetime     time.Duration
	TerminalRetry   time.Duration
	Fail            func(processFailpoint) error
	GuardExecutable string
	GuardArgsPrefix []string
}

type processStartRequest struct {
	ID, ProjectID, AgentID, RunID string
	Command, Workdir, Description string
	Lifetime                      time.Duration
}

type processRecoveryFault struct {
	ProcessID string
	Stage     string
	Error     string
}

type processSupervisor struct {
	ctx    context.Context
	cancel context.CancelFunc

	store        processKernelStore
	reservations processReservationBoundary
	openLog      processLogOpener
	config       processSupervisorConfig

	startMu sync.Mutex
	mu      sync.Mutex
	closed  bool
	handles map[string]*supervisedProcess

	recoveryMu sync.Mutex
	recovery   []processRecoveryFault
}

type supervisedProcess struct {
	supervisor *processSupervisor
	record     processdomain.Process
	cmd        *exec.Cmd
	boundary   processBoundary
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
	early     []byte
	stateMu   sync.Mutex
	cause     processTerminalCause
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
	if config.DefaultLifetime <= 0 {
		config.DefaultLifetime = defaultProcessLifetime
	}
	if config.MaxLifetime <= 0 {
		config.MaxLifetime = maxProcessLifetime
	}
	if config.DefaultLifetime > config.MaxLifetime {
		return nil, errors.New("default process lifetime exceeds maximum")
	}
	if config.TerminalRetry <= 0 {
		config.TerminalRetry = 10 * time.Millisecond
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
	lifetime, err := supervisor.validateStartRequest(request)
	if err != nil {
		return processdomain.Process{}, err
	}

	// Shutdown takes the same lock before closing admission. A spawned process is
	// therefore either fully registered and signalable, or fully rolled back.
	supervisor.startMu.Lock()
	defer supervisor.startMu.Unlock()

	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		return processdomain.Process{}, errors.New("process supervisor is closed")
	}
	if _, exists := supervisor.handles[request.ID]; exists {
		supervisor.mu.Unlock()
		return processdomain.Process{}, errors.New("process already exists")
	}
	supervisor.mu.Unlock()

	now := time.Now().UTC()
	record := processdomain.Process{
		ID: request.ID, ProjectID: request.ProjectID, AgentID: request.AgentID,
		RunID: request.RunID, Command: request.Command, Workdir: request.Workdir,
		Description: request.Description, State: processdomain.StateStarting,
		LifetimeMS: lifetime.Milliseconds(), StartedAt: now, UpdatedAt: now,
	}
	if err := supervisor.reservations.Reserve(record); err != nil {
		return processdomain.Process{}, err
	}
	if err := supervisor.store.CreateStarting(record); err != nil {
		abortErr := supervisor.reservations.Abort(record)
		if abortErr != nil {
			supervisor.recordRecovery(record.ID, "abort_after_create_failure", abortErr)
			return processdomain.Process{}, errors.Join(err, abortErr)
		}
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
	supervisor.armCollectors(handle)
	if err := supervisor.fail(processFailCollectorArm); err != nil {
		return supervisor.rollbackSpawn(handle, err, false)
	}
	if err := supervisor.fail(processFailWaiterArm); err != nil {
		return supervisor.rollbackSpawn(handle, err, false)
	}
	supervisor.armWaiter(handle)
	if err := supervisor.fail(processFailRegistration); err != nil {
		return supervisor.rollbackSpawn(handle, err, true)
	}

	handle.collectMu.Lock()
	handle.stateMu.Lock()
	handle.record.State = processdomain.StateRunning
	handle.record.PID = handle.cmd.Process.Pid
	handle.record.GuardPID = handle.cmd.Process.Pid
	handle.record.PGID = handle.cmd.Process.Pid
	handle.record.UpdatedAt = time.Now().UTC()
	running := handle.record
	handle.stateMu.Unlock()
	if err := supervisor.store.MarkRunning(running); err != nil {
		handle.collectMu.Unlock()
		return supervisor.rollbackSpawn(handle, err, true)
	}
	if err := writeAll(handle.log, handle.early); err != nil {
		handle.collectMu.Unlock()
		return supervisor.rollbackSpawn(handle, fmt.Errorf("process log write failed: %w", err), true)
	}
	handle.early = nil
	handle.stateMu.Lock()
	handle.registered = true
	handle.stateMu.Unlock()
	supervisor.mu.Lock()
	supervisor.handles[record.ID] = handle
	supervisor.mu.Unlock()
	handle.collectMu.Unlock()
	close(handle.gate)

	go supervisor.enforceLifetime(handle, lifetime)
	select {
	case <-handle.exitObserved:
		<-handle.terminalDone
		return handle.snapshot(), nil
	default:
		return handle.snapshot(), nil
	}
}

func (supervisor *processSupervisor) validateStartRequest(request processStartRequest) (time.Duration, error) {
	if request.ID == "" || request.ProjectID == "" || request.AgentID == "" ||
		request.Command == "" || request.Workdir == "" {
		return 0, errors.New("invalid background process request")
	}
	if request.Lifetime < 0 {
		return 0, errors.New("process lifetime must not be negative")
	}
	lifetime := request.Lifetime
	if lifetime == 0 {
		lifetime = supervisor.config.DefaultLifetime
	}
	if lifetime > supervisor.config.MaxLifetime {
		return 0, errors.New("process lifetime exceeds maximum")
	}
	return lifetime, nil
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
	supervisor.persistTerminal(record, "pre_spawn_terminal")
	if err := supervisor.reservations.Abort(record); err != nil {
		supervisor.recordRecovery(record.ID, "pre_spawn_abort", err)
		return record, errors.Join(cause, err)
	}
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
	args := append([]string(nil), supervisor.config.GuardArgsPrefix...)
	args = append(args, "process-guard", "--", "bash", "-lc", record.Command)
	cmd := exec.CommandContext(supervisor.ctx, executable, args...)
	cmd.Dir = record.Workdir
	boundary, err := newBackgroundProcessBoundary(cmd)
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = boundary.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = boundary.Close()
		_ = stdout.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = boundary.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, err
	}
	if err := boundary.AfterStart(cmd); err != nil {
		_ = boundary.Signal(os.Kill)
		_ = cmd.Wait()
		_ = boundary.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, err
	}
	return &supervisedProcess{
		supervisor: supervisor, record: record, cmd: cmd, boundary: boundary,
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
	chunk := make([]byte, 4096)
	for {
		count, readErr := reader.Read(chunk)
		if count > 0 {
			handle.collectMu.Lock()
			_, accepted := handle.buffer.AppendStream(stream, string(chunk[:count]))
			handle.stateMu.Lock()
			registered := handle.registered
			handle.stateMu.Unlock()
			if accepted > 0 {
				if registered {
					if err := writeAll(handle.log, chunk[:accepted]); err != nil {
						handle.collectMu.Unlock()
						handle.setCause(processdomain.StateFailed, "process log write failed: "+err.Error())
						_ = handle.boundary.Signal(os.Kill)
						return
					}
				} else {
					handle.early = append(handle.early, chunk[:accepted]...)
				}
			}
			handle.collectMu.Unlock()
		}
		if readErr != nil {
			return
		}
	}
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		count, err := writer.Write(value)
		if err != nil {
			return err
		}
		if count <= 0 {
			return io.ErrShortWrite
		}
		value = value[count:]
	}
	return nil
}

func (supervisor *processSupervisor) armWaiter(handle *supervisedProcess) {
	armed := make(chan struct{})
	go func() {
		defer close(handle.waitDone)
		close(armed)
		waitErr := handle.cmd.Wait()
		close(handle.exitObserved)
		// The guard may die before its command tree. Contain the residual group
		// before draining pipes or committing the terminal record.
		_ = handle.boundary.Signal(os.Kill)
		supervisor.waitForCollectors(handle)
		<-handle.gate
		handle.stateMu.Lock()
		registered := handle.registered
		handle.stateMu.Unlock()
		if !registered {
			return
		}
		handle.collectMu.Lock()
		_, _ = handle.buffer.FlushStream("stdout")
		_, _ = handle.buffer.FlushStream("stderr")
		_ = handle.log.Close()
		handle.collectMu.Unlock()
		_ = handle.boundary.Close()
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
	supervisor.persistTerminal(record, "live_terminal")
	handle.stateMu.Lock()
	handle.record = record
	handle.stateMu.Unlock()
	supervisor.mu.Lock()
	delete(supervisor.handles, record.ID)
	supervisor.mu.Unlock()
	close(handle.terminalDone)
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
	_ = handle.boundary.Signal(os.Kill)
	if waiterArmed {
		<-handle.waitDone
	} else {
		_ = handle.cmd.Wait()
		supervisor.waitForCollectors(handle)
	}
	_ = handle.stdout.Close()
	_ = handle.stderr.Close()
	_ = handle.boundary.Close()
	handle.collectMu.Lock()
	handle.early = nil
	_ = handle.log.Close()
	handle.collectMu.Unlock()
	record := handle.snapshot()
	record.State = processdomain.StateFailed
	record.Error = cause.Error()
	record.UpdatedAt = time.Now().UTC()
	record.EndedAt = timePointer(record.UpdatedAt)
	supervisor.persistTerminal(record, "rollback_terminal")
	if err := supervisor.reservations.Abort(record); err != nil {
		supervisor.recordRecovery(record.ID, "rollback_abort", err)
		return record, errors.Join(cause, err)
	}
	return record, cause
}

func (supervisor *processSupervisor) persistTerminal(record processdomain.Process, stage string) {
	for {
		if err := supervisor.store.MarkTerminal(record); err == nil {
			return
		} else {
			supervisor.recordRecovery(record.ID, stage, err)
		}
		// Do not claim terminal completion or release the handle while the
		// durable terminal/outbox transaction remains unavailable. Shutdown
		// deliberately waits for this retry loop before canceling supervisorCtx.
		time.Sleep(supervisor.config.TerminalRetry)
	}
}

func (supervisor *processSupervisor) recordRecovery(id, stage string, err error) {
	supervisor.recoveryMu.Lock()
	supervisor.recovery = append(supervisor.recovery, processRecoveryFault{
		ProcessID: id, Stage: stage, Error: err.Error(),
	})
	supervisor.recoveryMu.Unlock()
}

func (supervisor *processSupervisor) RecoveryFaults() []processRecoveryFault {
	supervisor.recoveryMu.Lock()
	defer supervisor.recoveryMu.Unlock()
	result := make([]processRecoveryFault, len(supervisor.recovery))
	copy(result, supervisor.recovery)
	return result
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
	if err := handle.boundary.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	timer := time.NewTimer(supervisor.config.StopGrace)
	defer timer.Stop()
	select {
	case <-handle.terminalDone:
		return nil
	case <-timer.C:
		_ = handle.boundary.Signal(os.Kill)
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
		_ = handle.boundary.Signal(os.Kill)
	}
}

func (supervisor *processSupervisor) Shutdown(ctx context.Context) error {
	supervisor.startMu.Lock()
	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		supervisor.startMu.Unlock()
		return nil
	}
	supervisor.closed = true
	handles := make([]*supervisedProcess, 0, len(supervisor.handles))
	for _, handle := range supervisor.handles {
		handles = append(handles, handle)
	}
	supervisor.mu.Unlock()
	supervisor.startMu.Unlock()

	for _, handle := range handles {
		select {
		case <-handle.exitObserved:
			continue
		default:
		}
		handle.setCause(processdomain.StateInterrupted, "server shutting down")
		_ = handle.boundary.Signal(syscall.SIGTERM)
	}
	for _, handle := range handles {
		grace := time.NewTimer(supervisor.config.StopGrace)
		select {
		case <-handle.terminalDone:
			if !grace.Stop() {
				<-grace.C
			}
		case <-grace.C:
			_ = handle.boundary.Signal(os.Kill)
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
			_ = handle.boundary.Signal(os.Kill)
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

func timePointer(value time.Time) *time.Time {
	copy := value
	return &copy
}

func (request processStartRequest) String() string {
	return fmt.Sprintf("%s/%s", request.ProjectID, request.ID)
}
