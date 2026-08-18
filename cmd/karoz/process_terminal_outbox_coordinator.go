package main

import "sync"

// processTerminalOutboxCoordinator owns only process-local outbox wake and
// drain serialization. The terminal events themselves remain durable records
// in processRuntimePersistence until delivery is acknowledged.
type processTerminalOutboxCoordinator struct {
	drainMu    sync.Mutex
	wake       chan struct{}
	workerOnce sync.Once
}

func newProcessTerminalOutboxCoordinator() *processTerminalOutboxCoordinator {
	return &processTerminalOutboxCoordinator{wake: make(chan struct{}, 1)}
}
