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

func TestFailClosedEnforcerRejectsRequiredPolicyWhenRuntimeIsDown(t *testing.T) {
	enforcer := FailClosedEnforcer{Check: func(context.Context) error {
		return errors.New("sandboxd unavailable")
	}}
	err := enforcer.Enforce(SandboxPolicy{Filesystem: true, Network: true, Processes: true})
	var unavailable SandboxUnavailableError
	if !errors.As(err, &unavailable) || !unavailable.Policy.Filesystem || !unavailable.Policy.Network || !unavailable.Policy.Processes {
		t.Fatalf("unavailable sandbox was not rejected: %v", err)
	}
	if err := (FailClosedEnforcer{Check: func(context.Context) error { return nil }}).Enforce(SandboxPolicy{Filesystem: true}); err != nil {
		t.Fatalf("healthy sandbox was rejected: %v", err)
	}
	if err := (FailClosedEnforcer{}).Enforce(SandboxPolicy{}); err != nil {
		t.Fatalf("empty policy was rejected: %v", err)
	}
}
