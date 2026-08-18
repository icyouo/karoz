package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"

	processdomain "github.com/karoz/karoz/internal/process"
)

func (a *app) bootstrapProcessRuntime() error {
	releaseConfig, err := processReleaseConfigFromEnv()
	if err != nil {
		return err
	}
	projects, err := a.scanProjects()
	if err != nil {
		return err
	}
	runtime, err := newProcessRuntimePersistenceWithRetention(
		a.settings.DataDir,
		projects,
		a.processRuntimeCoordinator.processPersistenceFail,
		releaseConfig.Retention,
	)
	if err != nil {
		return err
	}
	supervisorConfig := releaseConfig.Supervisor
	supervisorConfig.PrepareRecord = runtime.PrepareRecord
	supervisorConfig.TerminalPersisted = func(processdomain.Process) {
		a.wakeProcessTerminalOutbox()
	}
	supervisorConfig.OutputLine = a.enqueueProcessOutputObservation
	supervisor, err := newProcessSupervisor(
		a.supervisorCtx,
		runtime,
		runtime,
		func(record processdomain.Process) (io.WriteCloser, error) {
			return runtime.OpenLog(record)
		},
		supervisorConfig,
	)
	if err != nil {
		return err
	}
	a.processRuntimeCoordinator.processRuntime = runtime
	a.processRuntimeCoordinator.processSupervisor = supervisor
	a.armProcessOutputMonitor()
	a.startProcessTerminalOutbox()
	return nil
}

func (a *app) shutdownProcessRuntime(ctx context.Context) error {
	a.shutdownMonitorProbes()
	if a.processRuntimeCoordinator.processSupervisor == nil {
		if a.processRuntimeCoordinator.processRuntime != nil {
			if err := a.drainProcessOutputGaps(); err != nil {
				return err
			}
		}
		a.supervisorCancel()
		return nil
	}
	if err := a.processRuntimeCoordinator.processSupervisor.Shutdown(ctx); err != nil {
		return err
	}
	if err := a.drainProcessOutputGaps(); err != nil {
		return err
	}
	a.drainProcessTerminalOutbox()
	a.supervisorCancel()
	return nil
}

func (a *app) processRuntimeReady() bool {
	return a.processRuntimeCoordinator.processRuntime != nil &&
		a.processRuntimeCoordinator.processSupervisor != nil
}

func (a *app) processRecord(projectID, processID string) (processdomain.Process, error) {
	if err := a.requireProcessProject(projectID); err != nil {
		return processdomain.Process{}, err
	}
	for _, record := range a.processRuntime.List(projectID) {
		if record.ID == processID {
			if a.processSupervisor != nil {
				if live, ok := a.processSupervisor.LiveSnapshot(processID); ok &&
					live.ProjectID == projectID {
					return preserveProcessOutputCoverage(live, record), nil
				}
			}
			return record, nil
		}
	}
	return processdomain.Process{}, errProcessNotFound
}

func defaultProcessRetentionPolicy() processdomain.RetentionPolicy {
	return processdomain.RetentionPolicy{
		MaxRecords:    defaultProcessTerminalRecords,
		MaxAge:        defaultProcessTerminalAge,
		MaxTotalBytes: defaultProcessLogTotalBytes,
	}
}

func (a *app) registerProcessRuntimeProject(project Project) error {
	if a.processRuntime == nil {
		return nil
	}
	return a.processRuntime.RegisterProject(project)
}

func (a *app) registerProcessRuntimeProjectPrepared(
	project Project,
	intent runtimeProjectImportIntent,
	prepare func() (bool, error),
) error {
	if a.processRuntime == nil {
		_, err := prepare()
		return err
	}
	return a.processRuntime.RegisterProjectPrepared(project, &intent, prepare)
}

func (a *app) processRuntimePersistenceFail(point processPersistenceFailpoint) error {
	if a.processRuntime == nil {
		return nil
	}
	return a.processRuntime.persistenceFail(point)
}

func (a *app) advanceProcessRuntimeProjectImport(
	projectID, expected, next string,
) error {
	if a.processRuntime == nil {
		return nil
	}
	return a.processRuntime.advanceProjectImport(projectID, expected, next)
}

func (a *app) reconcileProjectImportIntents() error {
	if a.processRuntime == nil {
		return nil
	}
	for projectID, intent := range a.processRuntime.pendingProjectImports() {
		settingsDigest, err := configFileSHA256(
			filepath.Join(a.settings.DataDir, "settings.json"),
		)
		if err != nil {
			return err
		}
		if settingsDigest != intent.SettingsAfterSHA256 {
			return errors.New("project import settings digest mismatch")
		}
		aliasesPath := filepath.Join(a.settings.DataDir, "project-aliases.json")
		aliasesDigest, err := configFileSHA256(aliasesPath)
		if err != nil {
			return err
		}
		switch aliasesDigest {
		case intent.AliasesAfterSHA256:
		case intent.AliasesBeforeSHA256:
			a.mu.Lock()
			a.projectRegistryLocked().aliases[projectID] = intent.DesiredAlias
			a.mu.Unlock()
			if err := a.saveProjectAliases(); err != nil {
				return err
			}
			aliasesDigest, err = configFileSHA256(aliasesPath)
			if err != nil {
				return err
			}
			if aliasesDigest != intent.AliasesAfterSHA256 {
				return errors.New("project import alias completion digest mismatch")
			}
		default:
			return errors.New("project import aliases digest mismatch")
		}
		if err := a.processRuntime.completeProjectImport(projectID); err != nil {
			return err
		}
	}
	return nil
}
