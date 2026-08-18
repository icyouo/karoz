package execution

import (
	"context"
	"errors"
	"io"
	osexec "os/exec"
)

// StreamCommandRequest is the long-lived counterpart to CommandRequest. The
// caller owns the stream consumption and wait policy; this adapter owns
// construction, pipe wiring, and process start.
type StreamCommandRequest struct {
	Name      string
	Args      []string
	Dir       string
	Env       []string
	StdinPipe bool
	Configure func(*osexec.Cmd) error
}

type StreamProcess struct {
	Cmd    *osexec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
}

type StreamRunner interface {
	Start(context.Context, StreamCommandRequest) (StreamProcess, error)
}

type HostStreamRunner struct{}

func NewHostStreamRunner() StreamRunner { return HostStreamRunner{} }

func (HostStreamRunner) Start(ctx context.Context, request StreamCommandRequest) (StreamProcess, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if request.Name == "" {
		return StreamProcess{}, errors.New("command name is required")
	}
	cmd := osexec.CommandContext(ctx, request.Name, request.Args...)
	cmd.Dir = request.Dir
	cmd.Env = request.Env
	cmd.WaitDelay = defaultWaitDelay
	if request.Configure != nil {
		if err := request.Configure(cmd); err != nil {
			return StreamProcess{}, err
		}
	}
	var stdin io.WriteCloser
	var err error
	if request.StdinPipe {
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return StreamProcess{}, err
		}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		if stdin != nil {
			_ = stdin.Close()
		}
		return StreamProcess{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		if stdin != nil {
			_ = stdin.Close()
		}
		_ = stdout.Close()
		return StreamProcess{}, err
	}
	if err := cmd.Start(); err != nil {
		if stdin != nil {
			_ = stdin.Close()
		}
		_ = stdout.Close()
		_ = stderr.Close()
		return StreamProcess{}, err
	}
	return StreamProcess{Cmd: cmd, Stdin: stdin, Stdout: stdout, Stderr: stderr}, nil
}

var _ StreamRunner = HostStreamRunner{}
