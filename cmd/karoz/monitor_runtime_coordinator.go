package main

import (
	"context"
	"sync"

	monitordomain "github.com/karoz/karoz/internal/monitor"
)

// monitorRuntimeCoordinator owns the durable monitor registry together with
// the process-local script-probe handles that operate on it. Persistence stays
// in the monitor adapter; app only wires the coordinator into HTTP and runtime
// event entry points.
type monitorRuntimeCoordinator struct {
	monitorCtx               context.Context
	monitorCancel            context.CancelFunc
	monitorProbeWG           sync.WaitGroup
	monitorProbeStopping     bool
	monitorProbeCancels      map[string]context.CancelFunc
	monitorProbeProjectSlots map[string]chan struct{}
	monitorProbeReservations map[string]monitorProbeReservation
	monitorProbeChallenges   map[string]monitorProbeChallenge
	monitorProbeReceipts     map[string]monitordomain.ProbeApprovalReceipt
	monitorProbeSessions     map[string]monitorProbeOperatorSession
	monitors                 map[string][]Monitor
}

func newMonitorRuntimeCoordinator(ctx context.Context, cancel context.CancelFunc) *monitorRuntimeCoordinator {
	return &monitorRuntimeCoordinator{
		monitorCtx:               ctx,
		monitorCancel:            cancel,
		monitorProbeCancels:      map[string]context.CancelFunc{},
		monitorProbeProjectSlots: map[string]chan struct{}{},
		monitorProbeReservations: map[string]monitorProbeReservation{},
		monitorProbeChallenges:   map[string]monitorProbeChallenge{},
		monitorProbeReceipts:     map[string]monitordomain.ProbeApprovalReceipt{},
		monitorProbeSessions:     map[string]monitorProbeOperatorSession{},
		monitors:                 map[string][]Monitor{},
	}
}
