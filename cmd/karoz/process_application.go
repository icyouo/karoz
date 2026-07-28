package main

import (
	"context"
	"errors"
	"io"
	"time"

	processdomain "github.com/karoz/karoz/internal/process"
)

func (a *app) bootstrapProcessRuntime() error {
	projects, err := a.scanProjects()
	if err != nil {
		return err
	}
	runtime, err := newProcessRuntimePersistence(
		a.settings.DataDir,
		projects,
		a.processPersistenceFail,
	)
	if err != nil {
		return err
	}
	supervisor, err := newProcessSupervisor(
		a.supervisorCtx,
		runtime,
		runtime,
		func(record processdomain.Process) (io.WriteCloser, error) {
			return runtime.OpenLog(record)
		},
		processSupervisorConfig{PrepareRecord: runtime.PrepareRecord},
	)
	if err != nil {
		return err
	}
	a.processRuntime = runtime
	a.processSupervisor = supervisor
	return nil
}

func (a *app) shutdownProcessRuntime(ctx context.Context) error {
	if a.processSupervisor == nil {
		a.supervisorCancel()
		return nil
	}
	err := a.processSupervisor.Shutdown(ctx)
	a.supervisorCancel()
	return err
}

func (a *app) processRuntimeReady() bool {
	return a.processRuntime != nil && a.processSupervisor != nil
}

func (a *app) processRecord(projectID, processID string) (processdomain.Process, error) {
	if a.processRuntime == nil {
		return processdomain.Process{}, errors.New("process runtime is unavailable")
	}
	for _, record := range a.processRuntime.List(projectID) {
		if record.ID == processID {
			return record, nil
		}
	}
	return processdomain.Process{}, errors.New("process not found")
}

func defaultProcessRetentionPolicy() processdomain.RetentionPolicy {
	return processdomain.RetentionPolicy{
		MaxRecords: 200, MaxAge: 7 * 24 * time.Hour, MaxTotalBytes: 256 << 20,
	}
}
