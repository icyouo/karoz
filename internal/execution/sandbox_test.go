package execution

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestHostRunnerFailsClosedForRequestedSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	_, err := (HostRunner{}).Run(context.Background(), CommandRequest{
		Name: "sh", Args: []string{"-c", "exit 99"}, Sandbox: SandboxPolicy{Filesystem: true},
	})
	var unavailable SandboxUnavailableError
	if !errors.As(err, &unavailable) || !unavailable.Policy.Filesystem {
		t.Fatalf("sandbox request did not fail closed: %v", err)
	}
}
