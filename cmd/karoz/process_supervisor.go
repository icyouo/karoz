package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	processdomain "github.com/karoz/karoz/internal/process"
)

const (
	defaultProcessLifetime = time.Hour
	maxProcessLifetime     = 24 * time.Hour
	defaultTerminalRetries = 3
	maxTerminalRetries     = 5
	maxTerminalRetryDelay  = 250 * time.Millisecond
	maxRecoveryFaults      = 64
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
	ProveContained(time.Duration) error
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
	LogBytes          int64
	TailLines         int
	MaxConcurrent     int
	OutputEventBytes  int64
	ExitDrain         time.Duration
	StopGrace         time.Duration
	DefaultLifetime   time.Duration
	MaxLifetime       time.Duration
	TerminalRetry     time.Duration
	TerminalRetries   int
	Fail              func(processFailpoint) error
	GuardExecutable   string
	GuardArgsPrefix   []string
	BoundaryFactory   func(*exec.Cmd) (processBoundary, error)
	ProcessKill       func(*os.Process) error
	ProcessWait       func(*exec.Cmd) error
	BeforeFinalize    func()
	PrepareRecord     func(processdomain.Process) (processdomain.Process, error)
	TerminalPersisted func(processdomain.Process)
	OutputLine        func(processdomain.Process, processdomain.OutputLine)
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
	Count     uint64
}

type processSupervisor struct {
	ctx    context.Context
	cancel context.CancelFunc

	store        processKernelStore
	reservations processReservationBoundary
	openLog      processLogOpener
	config       processSupervisorConfig

	startGate        chan struct{}
	shutdownGate     chan struct{}
	mu               sync.Mutex
	closed           bool
	shutdownComplete bool
	shutdownErr      error
	handles          map[string]*supervisedProcess
	faulted          map[string]*processFaultedOwnership

	recoveryMu sync.Mutex
	recovery   []processRecoveryFault
}

type processFaultedOwnership struct {
	mu          sync.Mutex
	record      processdomain.Process
	handle      *supervisedProcess
	waiterArmed bool
	cause       error
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

	gate           chan struct{}
	registered     bool
	exitObserved   chan struct{}
	waitDone       chan struct{}
	bareWaitOnce   sync.Once
	waitErr        error
	terminalResult chan struct{}
	terminalOnce   sync.Once
	terminalErr    error
	stdoutDone     chan struct{}
	stderrDone     chan struct{}

	collectMu    sync.Mutex
	finalizeMu   sync.Mutex
	logCloseOnce sync.Once
	logCloseErr  error
	early        []byte
	stateMu      sync.Mutex
	cause        processTerminalCause
}

type processTerminalCause struct {
	state processdomain.State
	err   string
}

type processCleanupError struct {
	err error
}

func (failure *processCleanupError) Error() string {
	return failure.err.Error()
}

func (failure *processCleanupError) Unwrap() error {
	return failure.err
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
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = defaultProcessMaxConcurrent
	}
	if config.OutputEventBytes <= 0 {
		config.OutputEventBytes = defaultProcessOutputEventMax
	}
	if config.OutputEventBytes > maxProcessOutputEventMax {
		return nil, errors.New("process output event limit exceeds product ceiling")
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
	if config.MaxLifetime > maxProcessLifetime {
		return nil, errors.New("maximum process lifetime exceeds product ceiling")
	}
	if config.DefaultLifetime > config.MaxLifetime {
		return nil, errors.New("default process lifetime exceeds maximum")
	}
	if config.TerminalRetry <= 0 {
		config.TerminalRetry = 10 * time.Millisecond
	}
	if config.TerminalRetry > maxTerminalRetryDelay {
		return nil, errors.New("terminal retry delay exceeds maximum")
	}
	if config.TerminalRetries <= 0 {
		config.TerminalRetries = defaultTerminalRetries
	}
	if config.TerminalRetries > maxTerminalRetries {
		return nil, errors.New("terminal retry count exceeds maximum")
	}
	if config.BoundaryFactory == nil {
		config.BoundaryFactory = newBackgroundProcessBoundary
	}
	if config.ProcessKill == nil {
		config.ProcessKill = func(process *os.Process) error { return process.Kill() }
	}
	if config.ProcessWait == nil {
		config.ProcessWait = func(cmd *exec.Cmd) error { return cmd.Wait() }
	}
	ctx, cancel := context.WithCancel(serverCtx)
	return &processSupervisor{
		ctx: ctx, cancel: cancel, store: store, reservations: reservations,
		openLog: openLog, config: config, startGate: make(chan struct{}, 1),
		shutdownGate: make(chan struct{}, 1),
		handles:      make(map[string]*supervisedProcess), faulted: make(map[string]*processFaultedOwnership),
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
	if err := supervisor.acquireStart(supervisor.ctx); err != nil {
		return processdomain.Process{}, err
	}
	defer supervisor.releaseStart()

	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		return processdomain.Process{}, errors.New("process supervisor is closed")
	}
	if _, exists := supervisor.handles[request.ID]; exists {
		supervisor.mu.Unlock()
		return processdomain.Process{}, errors.New("process already exists")
	}
	if _, exists := supervisor.faulted[request.ID]; exists {
		supervisor.mu.Unlock()
		return processdomain.Process{}, errors.New("process has unresolved durable state")
	}
	activeForProject := 0
	for _, handle := range supervisor.handles {
		if handle.snapshot().ProjectID == request.ProjectID {
			activeForProject++
		}
	}
	for _, faulted := range supervisor.faulted {
		if faulted.handle != nil && faulted.record.ProjectID == request.ProjectID {
			activeForProject++
		}
	}
	if activeForProject >= supervisor.config.MaxConcurrent {
		supervisor.mu.Unlock()
		return processdomain.Process{}, errors.New("project background process concurrency limit reached")
	}
	supervisor.mu.Unlock()

	now := time.Now().UTC()
	record := processdomain.Process{
		ID: request.ID, ProjectID: request.ProjectID, AgentID: request.AgentID,
		RunID: request.RunID, Command: request.Command, Workdir: request.Workdir,
		Description: request.Description, State: processdomain.StateStarting,
		LifetimeMS: lifetime.Milliseconds(), StartedAt: now, UpdatedAt: now,
	}
	if supervisor.config.PrepareRecord != nil {
		record, err = supervisor.config.PrepareRecord(record)
		if err != nil {
			return processdomain.Process{}, err
		}
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
		var cleanupFailure *processCleanupError
		if errors.As(err, &cleanupFailure) {
			supervisor.recordRecovery(record.ID, "post_spawn_cleanup", cleanupFailure)
			supervisor.retainFaultedHandle(record, handle, false, err)
			return record, err
		}
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
		ctx, cancel := context.WithTimeout(context.Background(), 2*supervisor.config.StopGrace)
		defer cancel()
		err := supervisor.recoverExited(ctx, handle)
		return handle.snapshot(), err
	default:
		return handle.snapshot(), nil
	}
}

func (supervisor *processSupervisor) acquireStart(ctx context.Context) error {
	select {
	case supervisor.startGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (supervisor *processSupervisor) releaseStart() {
	<-supervisor.startGate
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
	if err := supervisor.persistTerminal(record, "pre_spawn_terminal"); err != nil {
		supervisor.retainFaultedRecord(record, err)
		return record, errors.Join(cause, err)
	}
	if err := supervisor.reservations.Abort(record); err != nil {
		supervisor.recordRecovery(record.ID, "pre_spawn_abort", err)
		supervisor.retainFaultedRecord(record, err)
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
	args = append(args, "process-guard", "--")
	args = append(args, backgroundShellCommand(record.Command)...)
	cmd := exec.CommandContext(supervisor.ctx, executable, args...)
	cmd.Dir = record.Workdir
	boundary, err := supervisor.config.BoundaryFactory(cmd)
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
	handle := &supervisedProcess{
		supervisor: supervisor, record: record, cmd: cmd, boundary: boundary,
		stdout: stdout, stderr: stderr, log: logWriter,
		buffer: processdomain.NewOutputBuffer(supervisor.config.TailLines, supervisor.config.LogBytes),
		gate:   make(chan struct{}), exitObserved: make(chan struct{}),
		waitDone: make(chan struct{}), terminalResult: make(chan struct{}),
		stdoutDone: make(chan struct{}), stderrDone: make(chan struct{}),
	}
	if err := boundary.AfterStart(cmd); err != nil {
		cleanupErr := supervisor.cleanupUnregistered(handle, false)
		if cleanupErr != nil {
			return handle, &processCleanupError{err: errors.Join(err, cleanupErr)}
		}
		return nil, err
	}
	return handle, nil
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
			lines, accepted := handle.buffer.AppendStream(stream, string(chunk[:count]))
			handle.stateMu.Lock()
			registered := handle.registered
			handle.stateMu.Unlock()
			if accepted > 0 {
				if registered {
					if err := writeAll(handle.log, chunk[:accepted]); err != nil {
						handle.collectMu.Unlock()
						handle.setCause(processdomain.StateFailed, "process log write failed: "+err.Error())
						if containErr := supervisor.hardContainLive(handle); containErr != nil {
							supervisor.recordRecovery(handle.processID(), "log_write_containment", containErr)
						}
						return
					}
				} else {
					handle.early = append(handle.early, chunk[:accepted]...)
				}
			}
			if registered && supervisor.config.OutputLine != nil {
				record := handle.snapshot()
				for _, line := range lines {
					supervisor.config.OutputLine(record, line)
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
		waitErr := supervisor.config.ProcessWait(handle.cmd)
		handle.stateMu.Lock()
		handle.waitErr = waitErr
		handle.stateMu.Unlock()
		close(handle.exitObserved)
		if supervisor.config.BeforeFinalize != nil {
			supervisor.config.BeforeFinalize()
		}
		supervisor.waitForCollectors(handle)
		<-handle.gate
		handle.stateMu.Lock()
		registered := handle.registered
		handle.stateMu.Unlock()
		if !registered {
			return
		}
		_ = supervisor.finalizeExited(handle)
	}()
	<-armed
}

func (supervisor *processSupervisor) finalizeExited(handle *supervisedProcess) error {
	handle.finalizeMu.Lock()
	defer handle.finalizeMu.Unlock()
	select {
	case <-handle.terminalResult:
		return handle.terminalResultError()
	default:
	}
	handle.stateMu.Lock()
	waitErr := handle.waitErr
	handle.stateMu.Unlock()
	containErr := supervisor.containAfterWait(handle)
	logErr := supervisor.closeLogAndFlush(handle)
	if err := errors.Join(containErr, logErr, cleanupWaitError(waitErr)); err != nil {
		supervisor.recordRecovery(handle.processID(), "containment", err)
		return fmt.Errorf("process containment failed: %w", err)
	}
	supervisor.finish(handle, waitErr)
	return handle.terminalResultError()
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
	if err := supervisor.persistTerminal(record, "live_terminal"); err != nil {
		handle.stateMu.Lock()
		handle.record = record
		handle.stateMu.Unlock()
		handle.setTerminalResult(err)
		return
	}
	handle.stateMu.Lock()
	handle.record = record
	handle.stateMu.Unlock()
	supervisor.mu.Lock()
	delete(supervisor.handles, record.ID)
	supervisor.mu.Unlock()
	handle.setTerminalResult(nil)
}

func (supervisor *processSupervisor) rollbackSpawn(handle *supervisedProcess, cause error, waiterArmed bool) (processdomain.Process, error) {
	handle.setCause(processdomain.StateFailed, cause.Error())
	handle.stateMu.Lock()
	handle.registered = false
	handle.stateMu.Unlock()
	record := handle.snapshot()
	if cleanupErr := supervisor.cleanupUnregistered(handle, waiterArmed); cleanupErr != nil {
		supervisor.recordRecovery(record.ID, "rollback_cleanup", cleanupErr)
		supervisor.retainFaultedHandle(record, handle, waiterArmed, errors.Join(cause, cleanupErr))
		return record, errors.Join(cause, cleanupErr)
	}
	record.State = processdomain.StateFailed
	record.Error = cause.Error()
	record.UpdatedAt = time.Now().UTC()
	record.EndedAt = timePointer(record.UpdatedAt)
	if err := supervisor.persistTerminal(record, "rollback_terminal"); err != nil {
		supervisor.retainFaultedRecord(record, err)
		return record, errors.Join(cause, err)
	}
	if err := supervisor.reservations.Abort(record); err != nil {
		supervisor.recordRecovery(record.ID, "rollback_abort", err)
		supervisor.retainFaultedRecord(record, err)
		return record, errors.Join(cause, err)
	}
	return record, cause
}

func (supervisor *processSupervisor) cleanupUnregistered(handle *supervisedProcess, waiterArmed bool) error {
	select {
	case <-handle.gate:
	default:
		close(handle.gate)
	}

	signalErr := handle.retryHardSignal()
	closeErr := handle.boundary.Close()
	_ = handle.stdout.Close()
	_ = handle.stderr.Close()
	waitErr := supervisor.boundedReap(handle, waiterArmed)
	proofErr := handle.boundary.ProveContained(supervisor.config.StopGrace)

	handle.collectMu.Lock()
	handle.early = nil
	handle.collectMu.Unlock()
	logErr := handle.closeLog()

	// Only a platform boundary proof can discharge a failed group/job signal.
	// Killing or reaping the guard alone says nothing about Unix descendants.
	if signalErr != nil && proofErr == nil {
		supervisor.recordRecovery(handle.processID(), "signal_fallback", signalErr)
		signalErr = nil
	}
	return errors.Join(signalErr, closeErr, waitErr, proofErr, logErr)
}

func (supervisor *processSupervisor) closeLogAndFlush(handle *supervisedProcess) error {
	handle.collectMu.Lock()
	stdout, _ := handle.buffer.FlushStream("stdout")
	stderr, _ := handle.buffer.FlushStream("stderr")
	registered := handle.registered
	handle.collectMu.Unlock()
	if registered && supervisor.config.OutputLine != nil {
		record := handle.snapshot()
		for _, line := range append(stdout, stderr...) {
			supervisor.config.OutputLine(record, line)
		}
	}
	return handle.closeLog()
}

func (handle *supervisedProcess) closeLog() error {
	handle.logCloseOnce.Do(func() {
		handle.collectMu.Lock()
		handle.logCloseErr = handle.log.Close()
		handle.collectMu.Unlock()
	})
	return handle.logCloseErr
}

func (supervisor *processSupervisor) boundedReap(handle *supervisedProcess, waiterArmed bool) error {
	done := handle.waitDone
	if !waiterArmed {
		handle.bareWaitOnce.Do(func() {
			go func() {
				waitErr := supervisor.config.ProcessWait(handle.cmd)
				handle.stateMu.Lock()
				handle.waitErr = waitErr
				handle.stateMu.Unlock()
				close(handle.waitDone)
			}()
		})
	}
	if waitForChannel(done, supervisor.config.StopGrace) {
		handle.stateMu.Lock()
		waitErr := handle.waitErr
		handle.stateMu.Unlock()
		return cleanupWaitError(waitErr)
	}
	killErr := normalizeProcessDone(supervisor.config.ProcessKill(handle.cmd.Process))
	if !waitForChannel(done, supervisor.config.StopGrace) {
		return errors.Join(killErr, errors.New("process reap timed out"))
	}
	handle.stateMu.Lock()
	waitErr := handle.waitErr
	handle.stateMu.Unlock()
	return errors.Join(killErr, cleanupWaitError(waitErr))
}

func waitForChannel(done <-chan struct{}, limit time.Duration) bool {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func cleanupWaitError(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return fmt.Errorf("process wait was not proven: %w", err)
}

func normalizeProcessDone(err error) error {
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (supervisor *processSupervisor) containAfterWait(handle *supervisedProcess) error {
	signalErr := handle.retryHardSignal()
	closeErr := handle.boundary.Close()
	proofErr := handle.boundary.ProveContained(supervisor.config.StopGrace)
	if signalErr != nil && proofErr == nil {
		supervisor.recordRecovery(handle.processID(), "signal_fallback", signalErr)
		signalErr = nil
	}
	return errors.Join(signalErr, closeErr, proofErr)
}

func (supervisor *processSupervisor) persistTerminal(record processdomain.Process, stage string) error {
	var lastErr error
	for attempt := 0; attempt < supervisor.config.TerminalRetries; attempt++ {
		if err := supervisor.store.MarkTerminal(record); err == nil {
			if supervisor.config.TerminalPersisted != nil {
				supervisor.config.TerminalPersisted(record)
			}
			return nil
		} else {
			lastErr = err
			supervisor.recordRecovery(record.ID, stage, err)
		}
		if attempt+1 < supervisor.config.TerminalRetries {
			timer := time.NewTimer(supervisor.config.TerminalRetry)
			select {
			case <-timer.C:
			case <-supervisor.ctx.Done():
				timer.Stop()
				return fmt.Errorf("terminal state is not durable: %w", lastErr)
			}
		}
	}
	return fmt.Errorf("terminal state is not durable: %w", lastErr)
}

func (supervisor *processSupervisor) recordRecovery(id, stage string, err error) {
	supervisor.recoveryMu.Lock()
	for index := range supervisor.recovery {
		fault := &supervisor.recovery[index]
		if fault.ProcessID == id && fault.Stage == stage {
			fault.Error = err.Error()
			fault.Count++
			supervisor.recoveryMu.Unlock()
			return
		}
	}
	if len(supervisor.recovery) == maxRecoveryFaults {
		copy(supervisor.recovery, supervisor.recovery[1:])
		supervisor.recovery = supervisor.recovery[:maxRecoveryFaults-1]
	}
	supervisor.recovery = append(supervisor.recovery, processRecoveryFault{
		ProcessID: id, Stage: stage, Error: err.Error(), Count: 1,
	})
	supervisor.recoveryMu.Unlock()
}

func (supervisor *processSupervisor) retainFaultedRecord(record processdomain.Process, cause error) {
	supervisor.mu.Lock()
	supervisor.faulted[record.ID] = &processFaultedOwnership{record: record, cause: cause}
	supervisor.mu.Unlock()
}

func (supervisor *processSupervisor) retainFaultedHandle(
	record processdomain.Process,
	handle *supervisedProcess,
	waiterArmed bool,
	cause error,
) {
	supervisor.mu.Lock()
	supervisor.faulted[record.ID] = &processFaultedOwnership{
		record: record, handle: handle, waiterArmed: waiterArmed, cause: cause,
	}
	supervisor.mu.Unlock()
}

func (supervisor *processSupervisor) RecoveryFaults() []processRecoveryFault {
	supervisor.recoveryMu.Lock()
	defer supervisor.recoveryMu.Unlock()
	result := make([]processRecoveryFault, len(supervisor.recovery))
	copy(result, supervisor.recovery)
	return result
}

func (supervisor *processSupervisor) Stop(id string) error {
	return supervisor.StopWithCause(
		id,
		processdomain.StateKilled,
		"process stopped",
	)
}

func (supervisor *processSupervisor) StopWithCause(
	id string,
	state processdomain.State,
	message string,
) error {
	if !state.Terminal() || strings.TrimSpace(message) == "" {
		return errors.New("process stop terminal cause is invalid")
	}
	supervisor.mu.Lock()
	handle := supervisor.handles[id]
	faulted := supervisor.faulted[id]
	supervisor.mu.Unlock()
	if handle == nil {
		if faulted != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*supervisor.config.StopGrace)
			defer cancel()
			return supervisor.recoverFaulted(ctx, faulted)
		}
		return errors.New("process not found")
	}
	select {
	case <-handle.exitObserved:
		ctx, cancel := context.WithTimeout(context.Background(), 2*supervisor.config.StopGrace)
		defer cancel()
		return supervisor.recoverExited(ctx, handle)
	default:
	}
	handle.setCause(state, message)
	if err := handle.signal(syscall.SIGTERM); err != nil {
		if containErr := supervisor.hardContainLive(handle); containErr != nil {
			return containErr
		}
	}
	timer := time.NewTimer(supervisor.config.StopGrace)
	defer timer.Stop()
	select {
	case <-handle.terminalResult:
		return handle.terminalResultError()
	case <-timer.C:
		if err := supervisor.hardContainLive(handle); err != nil {
			return err
		}
		select {
		case <-handle.terminalResult:
			return handle.terminalResultError()
		case <-time.After(supervisor.config.StopGrace):
			return errors.New("process terminal state is unresolved")
		}
	}
}

func (supervisor *processSupervisor) LiveSnapshot(
	id string,
) (processdomain.Process, bool) {
	supervisor.mu.Lock()
	handle := supervisor.handles[id]
	supervisor.mu.Unlock()
	if handle == nil {
		return processdomain.Process{}, false
	}
	record := handle.snapshot()
	record.LogBytes, record.LogLines, record.LogTruncated = handle.buffer.Stats()
	tail := handle.buffer.TailSequenced(1)
	if len(tail) > 0 {
		record.OutputSeq = tail[len(tail)-1].Sequence
	}
	return record, true
}

func (supervisor *processSupervisor) LiveTail(
	projectID, id string,
	limit int,
) ([]processdomain.OutputLine, bool) {
	supervisor.mu.Lock()
	handle := supervisor.handles[id]
	supervisor.mu.Unlock()
	if handle == nil {
		return nil, false
	}
	if handle.snapshot().ProjectID != projectID {
		return nil, false
	}
	return handle.buffer.TailSequenced(limit), true
}

func (supervisor *processSupervisor) enforceLifetime(handle *supervisedProcess, lifetime time.Duration) {
	timer := time.NewTimer(lifetime)
	defer timer.Stop()
	select {
	case <-handle.terminalResult:
		return
	case <-handle.exitObserved:
		return
	case <-timer.C:
		handle.setCause(processdomain.StateFailed, "lifetime exceeded")
		if err := supervisor.hardContainLive(handle); err != nil {
			supervisor.recordRecovery(handle.processID(), "lifetime_containment", err)
		}
	}
}

func (supervisor *processSupervisor) Shutdown(ctx context.Context) error {
	if err := supervisor.acquireShutdown(ctx); err != nil {
		return err
	}
	defer supervisor.releaseShutdown()
	if err := supervisor.acquireStart(ctx); err != nil {
		return err
	}
	supervisor.mu.Lock()
	if supervisor.shutdownComplete {
		err := supervisor.shutdownErr
		supervisor.mu.Unlock()
		supervisor.releaseStart()
		return err
	}
	supervisor.closed = true
	handles := make([]*supervisedProcess, 0, len(supervisor.handles))
	for _, handle := range supervisor.handles {
		handles = append(handles, handle)
	}
	faulted := make([]*processFaultedOwnership, 0, len(supervisor.faulted))
	for _, ownership := range supervisor.faulted {
		faulted = append(faulted, ownership)
	}
	supervisor.mu.Unlock()
	supervisor.releaseStart()

	for _, ownership := range faulted {
		if err := supervisor.recoverFaulted(ctx, ownership); err != nil {
			return err
		}
	}
	for _, handle := range handles {
		select {
		case <-handle.exitObserved:
			if err := supervisor.recoverExited(ctx, handle); err != nil {
				return err
			}
			continue
		default:
		}
		handle.setCause(processdomain.StateInterrupted, "server shutting down")
		if err := handle.signal(syscall.SIGTERM); err != nil {
			if containErr := supervisor.hardContainLive(handle); containErr != nil {
				supervisor.recordRecovery(handle.processID(), "shutdown_containment", containErr)
			}
		}
	}
	for _, handle := range handles {
		grace := time.NewTimer(supervisor.config.StopGrace)
		select {
		case <-handle.terminalResult:
			if !grace.Stop() {
				<-grace.C
			}
			if err := handle.terminalResultError(); err != nil {
				return err
			}
		case <-grace.C:
			if err := supervisor.hardContainLive(handle); err != nil {
				return err
			}
			select {
			case <-handle.exitObserved:
				if err := supervisor.recoverExited(ctx, handle); err != nil {
					return err
				}
			default:
			}
			select {
			case <-handle.terminalResult:
				if err := handle.terminalResultError(); err != nil {
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-ctx.Done():
			if !grace.Stop() {
				<-grace.C
			}
			if err := supervisor.hardContainLive(handle); err != nil {
				supervisor.recordRecovery(handle.processID(), "shutdown_containment", err)
			}
			return ctx.Err()
		}
	}
	supervisor.mu.Lock()
	if len(supervisor.handles) != 0 || len(supervisor.faulted) != 0 {
		supervisor.mu.Unlock()
		return errors.New("process supervisor has unresolved ownership")
	}
	supervisor.shutdownComplete = true
	supervisor.shutdownErr = nil
	supervisor.mu.Unlock()
	supervisor.cancel()
	return nil
}

func (supervisor *processSupervisor) recoverExited(ctx context.Context, handle *supervisedProcess) error {
	select {
	case <-handle.waitDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-handle.terminalResult:
		return handle.terminalResultError()
	default:
		return supervisor.finalizeExited(handle)
	}
}

func (supervisor *processSupervisor) acquireShutdown(ctx context.Context) error {
	select {
	case supervisor.shutdownGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (supervisor *processSupervisor) releaseShutdown() {
	<-supervisor.shutdownGate
}

func (supervisor *processSupervisor) recoverFaulted(
	ctx context.Context,
	ownership *processFaultedOwnership,
) error {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	record := ownership.record
	if ownership.handle != nil {
		if err := supervisor.cleanupUnregistered(ownership.handle, ownership.waiterArmed); err != nil {
			supervisor.recordRecovery(record.ID, "faulted_cleanup", err)
			return fmt.Errorf("process cleanup remains unresolved: %w", err)
		}
		record.State = processdomain.StateFailed
		if ownership.cause != nil {
			record.Error = ownership.cause.Error()
		}
		record.UpdatedAt = time.Now().UTC()
		record.EndedAt = timePointer(record.UpdatedAt)
	}
	if !record.State.Terminal() {
		record.State = processdomain.StateFailed
		if ownership.cause != nil {
			record.Error = ownership.cause.Error()
		}
		record.UpdatedAt = time.Now().UTC()
		record.EndedAt = timePointer(record.UpdatedAt)
	}
	if err := supervisor.persistTerminal(record, "faulted_terminal"); err != nil {
		return err
	}
	if err := supervisor.reservations.Abort(record); err != nil {
		supervisor.recordRecovery(record.ID, "faulted_abort", err)
		return err
	}
	supervisor.mu.Lock()
	if supervisor.faulted[record.ID] == ownership {
		delete(supervisor.faulted, record.ID)
	}
	supervisor.mu.Unlock()
	return nil
}

func (handle *supervisedProcess) setCause(state processdomain.State, message string) {
	handle.stateMu.Lock()
	if handle.cause.state == "" {
		handle.cause = processTerminalCause{state: state, err: message}
	}
	handle.stateMu.Unlock()
}

func (handle *supervisedProcess) signal(signal os.Signal) error {
	return handle.boundary.Signal(signal)
}

func (handle *supervisedProcess) retryHardSignal() error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = handle.signal(os.Kill)
		if err == nil {
			return nil
		}
		if attempt < 2 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	return err
}

func (supervisor *processSupervisor) hardContainLive(handle *supervisedProcess) error {
	signalErr := handle.retryHardSignal()
	if signalErr == nil {
		if proofErr := handle.boundary.ProveContained(supervisor.config.StopGrace); proofErr == nil {
			return nil
		} else {
			signalErr = errors.Join(signalErr, proofErr)
		}
	}
	closeErr := handle.boundary.Close()
	proofErr := handle.boundary.ProveContained(supervisor.config.StopGrace)
	if proofErr == nil {
		if closeErr != nil {
			err := fmt.Errorf("process boundary close failed: %w", closeErr)
			supervisor.recordRecovery(handle.processID(), "containment", err)
			return err
		}
		supervisor.recordRecovery(handle.processID(), "signal_fallback", signalErr)
		return nil
	}
	killErr := normalizeProcessDone(supervisor.config.ProcessKill(handle.cmd.Process))
	retryProofErr := handle.boundary.ProveContained(supervisor.config.StopGrace)
	if retryProofErr == nil {
		if closeErr != nil {
			err := fmt.Errorf("process boundary close failed: %w", closeErr)
			supervisor.recordRecovery(handle.processID(), "containment", err)
			return err
		}
		supervisor.recordRecovery(handle.processID(), "signal_fallback", errors.Join(signalErr, proofErr))
		return nil
	}
	err := errors.Join(signalErr, closeErr, proofErr, killErr, retryProofErr)
	supervisor.recordRecovery(handle.processID(), "containment", err)
	return err
}

func (handle *supervisedProcess) setTerminalResult(err error) {
	handle.terminalOnce.Do(func() {
		handle.stateMu.Lock()
		handle.terminalErr = err
		handle.stateMu.Unlock()
		close(handle.terminalResult)
	})
}

func (handle *supervisedProcess) terminalResultError() error {
	handle.stateMu.Lock()
	defer handle.stateMu.Unlock()
	return handle.terminalErr
}

func (handle *supervisedProcess) waitTerminalResult() error {
	<-handle.terminalResult
	return handle.terminalResultError()
}

func (handle *supervisedProcess) snapshot() processdomain.Process {
	handle.stateMu.Lock()
	defer handle.stateMu.Unlock()
	return handle.record
}

func (handle *supervisedProcess) processID() string {
	handle.stateMu.Lock()
	defer handle.stateMu.Unlock()
	return handle.record.ID
}

func timePointer(value time.Time) *time.Time {
	copy := value
	return &copy
}

func (request processStartRequest) String() string {
	return fmt.Sprintf("%s/%s", request.ProjectID, request.ID)
}
