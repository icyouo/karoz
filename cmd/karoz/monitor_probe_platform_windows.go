//go:build windows

package main

import (
	"context"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
)

const scriptProbeSupported = false

func readMonitorProbeSnapshot(
	*secureRuntimeStore,
	string,
) (monitorProbeSnapshot, error) {
	return monitorProbeSnapshot{}, errScriptProbeUnsupported
}

func executeMonitorProbe(
	context.Context,
	string,
	string,
	[]byte,
	time.Duration,
) (monitordomain.ProbeExecution, string, error) {
	return monitordomain.ProbeExecution{}, "", errScriptProbeUnsupported
}
