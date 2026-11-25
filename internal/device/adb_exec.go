package device

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	AndroidADBDefaultOutputBytes = 16000
	AndroidADBMaxOutputBytes     = 64000
)

type AndroidADBExecRequest struct {
	Args           []string `json:"args"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	MaxOutputBytes int      `json:"max_output_bytes,omitempty"`
}

type AndroidADBExecResult struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	TimedOut  bool   `json:"timed_out"`
	Cancelled bool   `json:"cancelled"`
	Truncated bool   `json:"truncated"`
}

// ValidateAndroidADBExecRequest allows only device-local ADB command families.
// Host-side file operations and ADB server or transport management are rejected
// because they cannot be scoped safely to a single project device.
func ValidateAndroidADBExecRequest(req AndroidADBExecRequest) error {
	if len(req.Args) == 0 || len(req.Args) > 64 {
		return fmt.Errorf("args must contain 1 to 64 ADB subcommand arguments")
	}
	total := 0
	for _, arg := range req.Args {
		total += len(arg)
		if strings.IndexByte(arg, 0) >= 0 || !utf8.ValidString(arg) {
			return fmt.Errorf("invalid ADB argument")
		}
	}
	if total > 16000 {
		return fmt.Errorf("ADB arguments exceed 16000 bytes")
	}
	switch req.Args[0] {
	case "shell", "exec-out":
		if len(req.Args) < 2 || req.Args[1] == "" {
			return fmt.Errorf("shell/exec-out require an explicit device command")
		}
	case "logcat":
	case "get-state", "get-serialno", "features", "root", "unroot", "remount", "disable-verity", "enable-verity":
		if len(req.Args) != 1 {
			return fmt.Errorf("this ADB subcommand accepts no additional arguments")
		}
	case "reboot":
		if len(req.Args) > 2 {
			return fmt.Errorf("reboot accepts at most one device mode")
		}
	default:
		return fmt.Errorf("unsupported ADB subcommand: use device-local shell, exec-out, logcat, get-state, get-serialno, features, root/unroot, remount, verity or reboot; omit adb and all host/target-selection options")
	}
	if req.TimeoutSeconds < 0 || req.TimeoutSeconds > 60 {
		return fmt.Errorf("timeout_seconds must be 1 to 60, or omitted")
	}
	if req.MaxOutputBytes < 0 || req.MaxOutputBytes > AndroidADBMaxOutputBytes {
		return fmt.Errorf("max_output_bytes must be 1 to 64000, or omitted")
	}
	return nil
}

// ScopedADBArguments builds server-controlled adb arguments scoped via `-s <target>`.
func ScopedADBArguments(target string, args []string) []string {
	result := []string{"-s", target, args[0]}
	if args[0] == "shell" || args[0] == "exec-out" {
		remote := make([]string, 0, len(args)-1)
		for _, arg := range args[1:] {
			remote = append(remote, "'"+strings.ReplaceAll(arg, "'", `'\''`)+"'")
		}
		return append(result, strings.Join(remote, " "))
	}
	return append(result, args[1:]...)
}

type adbOutputBudget struct {
	mu        sync.Mutex
	remaining int
	truncated bool
}

type adbOutputWriter struct {
	budget *adbOutputBudget
	text   strings.Builder
}

func (w *adbOutputWriter) Write(p []byte) (int, error) {
	w.budget.mu.Lock()
	defer w.budget.mu.Unlock()
	keep := len(p)
	if keep > w.budget.remaining {
		keep = w.budget.remaining
		w.budget.truncated = true
	}
	w.text.Write(p[:keep])
	w.budget.remaining -= keep
	return len(p), nil
}

func RunBoundedADBCommand(ctx context.Context, args []string, limit int) (*AndroidADBExecResult, error) {
	return runBoundedDeviceCommand(ctx, "adb", args, limit)
}

func runBoundedDeviceCommand(ctx context.Context, name string, args []string, limit int) (*AndroidADBExecResult, error) {
	if limit <= 0 || limit > AndroidADBMaxOutputBytes {
		limit = AndroidADBDefaultOutputBytes
	}
	budget := &adbOutputBudget{remaining: limit}
	stdout, stderr := &adbOutputWriter{budget: budget}, &adbOutputWriter{budget: budget}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	result := &AndroidADBExecResult{
		Stdout:    strings.ToValidUTF8(stdout.text.String(), "\uFFFD"),
		Stderr:    strings.ToValidUTF8(stderr.text.String(), "\uFFFD"),
		Truncated: budget.truncated,
		ExitCode:  0,
	}
	if err != nil {
		result.ExitCode = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		}
		result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		result.Cancelled = errors.Is(ctx.Err(), context.Canceled)
		if !errors.As(err, &exitErr) && ctx.Err() == nil {
			return result, fmt.Errorf("ADB execution failed: %w", err)
		}
	}
	return result, nil
}
