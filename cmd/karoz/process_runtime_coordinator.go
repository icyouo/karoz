package main

// processRuntimeCoordinator owns the local handles to durable background
// process operations: their persistence adapter, live supervisor, and the
// scoped persistence fault seam. `app` composes this coordinator but does not
// define process-operation state itself.
type processRuntimeCoordinator struct {
	processRuntime         *processRuntimePersistence
	processSupervisor      *processSupervisor
	processPersistenceFail func(processPersistenceFailpoint) error
}

func newProcessRuntimeCoordinator() *processRuntimeCoordinator {
	return &processRuntimeCoordinator{}
}
