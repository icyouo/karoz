package execution

import (
	"fmt"
	"strings"
)

// SandboxPolicy describes containment requested by an operation. Worktree
// placement is not containment: any non-empty requirement must be confirmed
// by an Enforcer before the host process is started.
type SandboxPolicy struct {
	Filesystem bool
	Network    bool
	Processes  bool
}

func (policy SandboxPolicy) Required() bool {
	return policy.Filesystem || policy.Network || policy.Processes
}

func (policy SandboxPolicy) String() string {
	parts := make([]string, 0, 3)
	if policy.Filesystem {
		parts = append(parts, "filesystem")
	}
	if policy.Network {
		parts = append(parts, "network")
	}
	if policy.Processes {
		parts = append(parts, "processes")
	}
	return strings.Join(parts, ",")
}

// SandboxUnavailableError proves a requested boundary was not silently
// dropped. Callers can surface the requested capabilities in their audit/UI.
type SandboxUnavailableError struct {
	Policy SandboxPolicy
	Reason string
}

func (err SandboxUnavailableError) Error() string {
	reason := strings.TrimSpace(err.Reason)
	if reason == "" {
		reason = "no enforcing adapter is configured"
	}
	return fmt.Sprintf("sandbox unavailable for %s: %s", err.Policy.String(), reason)
}

// SandboxEnforcer returns nil only after it has enforced every requested
// restriction for the command about to start.
type SandboxEnforcer interface{ Enforce(SandboxPolicy) error }

type UnsupportedSandboxEnforcer struct{}

func (UnsupportedSandboxEnforcer) Enforce(policy SandboxPolicy) error {
	if !policy.Required() {
		return nil
	}
	return SandboxUnavailableError{Policy: policy}
}
