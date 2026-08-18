// Package execution owns the process boundary used by Karoz's application
// services.  Keeping command construction and output capture here gives the
// rest of the application one seam for cancellation, limits, and tests.
package execution

import (
	"context"
	"errors"
	"io"
	osexec "os/exec"
	"strings"
	"sync"
	"time"
)

const defaultWaitDelay = 2 * time.Second

// CommandRequest is the intentionally small contract between application
// services and the host process adapter. Configure and Finalize are kept as
// hooks so platform-specific process-group handling can remain outside this
// package while all callers still share the same execution lifecycle.
type CommandRequest struct {
	Name           string
	Args           []string
	Dir            string
	Stdin          io.Reader
	CombinedOutput bool
	MaxOutputBytes int
	WaitDelay      time.Duration
	Configure      func(*osexec.Cmd)
	Finalize       func(*osexec.Cmd) error
	Sandbox        SandboxPolicy
}

// CommandResult contains captured command output and the process outcome.
// When CombinedOutput is requested, Stdout contains the combined stream and
// Stderr is empty. This mirrors exec.Cmd.CombinedOutput while remaining safe
// for commands that write stdout and stderr concurrently.
type CommandResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Duration  time.Duration
	Truncated bool
}

func (result CommandResult) Output() string {
	if result.Stderr == "" {
		return result.Stdout
	}
	return result.Stdout + result.Stderr
}

// Runner is the application-facing execution port. Tests can provide a fake
// implementation without touching the host or spawning a child process.
type Runner interface {
	Run(context.Context, CommandRequest) (CommandResult, error)
}

// HostRunner executes commands on the local machine. It deliberately contains
// no Karoz policy decisions; authorization and worktree checks stay in the
// owning application service, while this adapter enforces lifecycle and
// output-capture invariants consistently.
type HostRunner struct{}

func NewHostRunner() Runner { return HostRunner{} }

func (HostRunner) Run(ctx context.Context, request CommandRequest) (CommandResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		return CommandResult{ExitCode: -1}, errors.New("command name is required")
	}
	if err := (UnsupportedSandboxEnforcer{}).Enforce(request.Sandbox); err != nil {
		return CommandResult{ExitCode: -1}, err
	}

	started := time.Now()
	cmd := osexec.CommandContext(ctx, name, request.Args...)
	cmd.Dir = request.Dir
	cmd.Stdin = request.Stdin
	if request.Configure != nil {
		request.Configure(cmd)
	}
	if request.WaitDelay > 0 {
		cmd.WaitDelay = request.WaitDelay
	} else {
		cmd.WaitDelay = defaultWaitDelay
	}

	var stdout, stderr *limitedBuffer
	if request.CombinedOutput {
		combined := newLimitedBuffer(request.MaxOutputBytes)
		cmd.Stdout = combined
		cmd.Stderr = combined
		stdout = combined
	} else {
		stdout = newLimitedBuffer(request.MaxOutputBytes)
		stderr = newLimitedBuffer(request.MaxOutputBytes)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
	}

	err := cmd.Run()
	if request.Finalize != nil {
		_ = request.Finalize(cmd)
	}
	result := CommandResult{
		Stdout:    stdout.String(),
		ExitCode:  -1,
		Duration:  time.Since(started),
		Truncated: stdout.Truncated(),
	}
	if stderr != nil && stderr != stdout {
		result.Stderr = stderr.String()
		result.Truncated = result.Truncated || stderr.Truncated()
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

// limitedBuffer is a concurrency-safe bounded writer. os/exec may copy
// stdout and stderr from separate goroutines, including when both are wired to
// the same combined stream.
type limitedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func newLimitedBuffer(limit int) *limitedBuffer {
	if limit < 0 {
		limit = 0
	}
	return &limitedBuffer{limit: limit}
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	originalLen := len(data)
	if buffer.limit == 0 {
		buffer.data = append(buffer.data, data...)
		return originalLen, nil
	}
	remaining := buffer.limit - len(buffer.data)
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		buffer.data = append(buffer.data, data...)
	}
	if originalLen > remaining {
		buffer.truncated = true
	}
	return originalLen, nil
}

func (buffer *limitedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return strings.ToValidUTF8(string(buffer.data), "�")
}

func (buffer *limitedBuffer) Truncated() bool {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.truncated
}

var _ Runner = HostRunner{}
