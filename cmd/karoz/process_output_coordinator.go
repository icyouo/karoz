package main

import "sync"

// processOutputCoordinator owns process-local observation, cursor, and gap
// recovery state. Durable process records and monitor diagnostics remain in
// their respective stores; this object is deliberately rebuilt on restart.
type processOutputCoordinator struct {
	monitorOnce   sync.Once
	monitorCh     chan processOutputObservation
	baselines     map[string]uint64
	cursors       map[string]uint64
	gapMu         sync.Mutex
	gapDrainMu    sync.Mutex
	pendingGaps   map[string]processOutputGapDelta
	gapWake       chan struct{}
	gapWorkerOnce sync.Once
}

func newProcessOutputCoordinator() *processOutputCoordinator {
	return &processOutputCoordinator{
		monitorCh:   make(chan processOutputObservation, 256),
		baselines:   map[string]uint64{},
		cursors:     map[string]uint64{},
		pendingGaps: map[string]processOutputGapDelta{},
		gapWake:     make(chan struct{}, 1),
	}
}
