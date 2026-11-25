package device

import (
	"context"
	"strings"
	"testing"
)

func TestValidateAndroidADBExecRequest(t *testing.T) {
	validCases := []AndroidADBExecRequest{
		{Args: []string{"shell", "pm", "list", "packages"}},
		{Args: []string{"exec-out", "screencap", "-p"}},
		{Args: []string{"logcat", "-d", "-t", "50"}, TimeoutSeconds: 10, MaxOutputBytes: 4096},
		{Args: []string{"get-state"}},
		{Args: []string{"reboot", "bootloader"}},
	}
	for _, tc := range validCases {
		if err := ValidateAndroidADBExecRequest(tc); err != nil {
			t.Fatalf("expected valid request %+v, got error: %v", tc, err)
		}
	}

	invalidCases := []struct {
		name string
		req  AndroidADBExecRequest
	}{
		{name: "empty args", req: AndroidADBExecRequest{}},
		{name: "host pull rejected", req: AndroidADBExecRequest{Args: []string{"pull", "/sdcard/a"}}},
		{name: "host install rejected", req: AndroidADBExecRequest{Args: []string{"install", "app.apk"}}},
		{name: "host forward rejected", req: AndroidADBExecRequest{Args: []string{"forward", "tcp:1", "tcp:2"}}},
		{name: "adb prefix rejected", req: AndroidADBExecRequest{Args: []string{"adb", "devices"}}},
		{name: "flag injection rejected", req: AndroidADBExecRequest{Args: []string{"-s", "emulator-5554", "shell", "id"}}},
		{name: "shell without command", req: AndroidADBExecRequest{Args: []string{"shell"}}},
		{name: "get-state extra arg", req: AndroidADBExecRequest{Args: []string{"get-state", "extra"}}},
		{name: "nul byte", req: AndroidADBExecRequest{Args: []string{"shell", "echo\x00hi"}}},
		{name: "timeout too large", req: AndroidADBExecRequest{Args: []string{"logcat"}, TimeoutSeconds: 61}},
		{name: "max bytes too large", req: AndroidADBExecRequest{Args: []string{"logcat"}, MaxOutputBytes: AndroidADBMaxOutputBytes + 1}},
	}
	for _, tc := range invalidCases {
		if err := ValidateAndroidADBExecRequest(tc.req); err == nil {
			t.Fatalf("%s: expected error, got nil", tc.name)
		}
	}
}

func TestScopedADBArguments(t *testing.T) {
	got := ScopedADBArguments("emulator-5554", []string{"shell", "echo", "hello world", "it's"})
	if len(got) != 4 || got[0] != "-s" || got[1] != "emulator-5554" || got[2] != "shell" {
		t.Fatalf("unexpected scoped shell prefix: %v", got)
	}
	wantRemote := `'echo' 'hello world' 'it'\''s'`
	if got[3] != wantRemote {
		t.Fatalf("remote command = %q, want %q", got[3], wantRemote)
	}

	logcat := ScopedADBArguments("10.0.0.2:5555", []string{"logcat", "-d", "-t", "10"})
	if strings.Join(logcat, " ") != "-s 10.0.0.2:5555 logcat -d -t 10" {
		t.Fatalf("unexpected non-shell scoped args: %v", logcat)
	}
}

func TestRunBoundedDeviceCommandTruncatesAndCapturesExitCode(t *testing.T) {
	ctx := context.Background()
	res, err := runBoundedDeviceCommand(ctx, "sh", []string{"-c", "printf '1234567890abcdef'"}, 8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Truncated || res.Stdout != "12345678" || res.ExitCode != 0 {
		t.Fatalf("unexpected bounded result: %+v", res)
	}

	failRes, err := runBoundedDeviceCommand(ctx, "sh", []string{"-c", "echo err >&2; exit 7"}, 256)
	if err != nil {
		t.Fatalf("expected non-zero exit without Go transport error, got %v", err)
	}
	if failRes.ExitCode != 7 || !strings.Contains(failRes.Stderr, "err") {
		t.Fatalf("unexpected failure result: %+v", failRes)
	}
}
