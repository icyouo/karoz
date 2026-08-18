package execution

import (
	"context"
	"errors"
	osexec "os/exec"
	"runtime"
	"testing"
	"time"
)

func TestHostRunnerCapturesSeparateStreams(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	result, err := (HostRunner{}).Run(context.Background(), CommandRequest{
		Name: "sh", Args: []string{"-c", "printf out; printf err >&2"},
	})
	if err != nil {
		t.Fatalf("run command: %v", err)
	}
	if result.Stdout != "out" || result.Stderr != "err" {
		t.Fatalf("streams = stdout %q stderr %q", result.Stdout, result.Stderr)
	}
	if result.Output() != "outerr" {
		t.Fatalf("combined output = %q", result.Output())
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}
}

func TestHostRunnerBoundsCombinedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	result, err := (HostRunner{}).Run(context.Background(), CommandRequest{
		Name: "sh", Args: []string{"-c", "printf 123456"}, CombinedOutput: true, MaxOutputBytes: 3,
	})
	if err != nil {
		t.Fatalf("run command: %v", err)
	}
	if result.Output() != "123" || !result.Truncated {
		t.Fatalf("bounded output = %q truncated=%v", result.Output(), result.Truncated)
	}
}

func TestHostRunnerReturnsContextErrorAndRunsFinalize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	finalized := false
	result, err := (HostRunner{}).Run(ctx, CommandRequest{
		Name: "sh", Args: []string{"-c", "sleep 1"},
		Finalize: func(_ *osexec.Cmd) error {
			finalized = true
			return nil
		},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if !finalized {
		t.Fatal("finalize hook was not called")
	}
	if result.ExitCode == 0 {
		t.Fatalf("timed out command reported success: %+v", result)
	}
}
