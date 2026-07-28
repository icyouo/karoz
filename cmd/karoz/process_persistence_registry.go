package main

import (
	"errors"
	"path/filepath"
)

// RegisterProject extends the dormant runtime in place. It never rebuilds the
// supervisor, so existing process handles keep their server-owned contexts.
func (runtime *processRuntimePersistence) RegisterProject(project Project) error {
	identities, err := resolveRuntimeProjectIdentities(runtime.store.root, []Project{project})
	if err != nil {
		return err
	}
	identity := identities[0]
	runtime.registryMu.Lock()
	defer runtime.registryMu.Unlock()

	if existing := runtime.projects[identity.ProjectID]; existing != nil {
		if existing.identity != identity {
			return errors.New("runtime project registration identity mismatch")
		}
		return nil
	}

	runtime.indexMu.Lock()
	entry, exists := runtime.index.Projects[identity.SafeProjectKey]
	if exists {
		runtime.indexMu.Unlock()
		if entry.Project != identity {
			return errors.New("runtime project sentinel identity mismatch")
		}
		return errors.New("runtime project is disabled and requires bootstrap recovery")
	}
	entry = runtimeProjectIndexEntry{
		Project: identity, State: "initializing", Generation: 1,
	}
	runtime.index.Projects[identity.SafeProjectKey] = entry
	if err := runtime.store.saveJSON(
		filepath.Join("project-runtime", "index.json"), runtime.index,
	); err != nil {
		delete(runtime.index.Projects, identity.SafeProjectKey)
		runtime.indexMu.Unlock()
		return err
	}
	runtime.indexMu.Unlock()
	if err := runtime.persistenceFail(processPersistAfterIndexInitializing); err != nil {
		return err
	}

	runtime.authorityMu.Lock()
	if _, exists := runtime.authority.Projects[identity.SafeProjectKey]; exists {
		runtime.authorityMu.Unlock()
		return errors.New("untracked process authority partition exists for new project")
	}
	runtime.authority.Projects[identity.SafeProjectKey] = processAuthorityProject{
		Project: identity, Records: map[string]durableProcessRecord{},
		Tombstones: map[string]processTombstone{},
	}
	runtime.authority.Generation++
	if err := runtime.saveAuthorityLocked(); err != nil {
		delete(runtime.authority.Projects, identity.SafeProjectKey)
		runtime.authorityMu.Unlock()
		return err
	}
	runtime.authorityMu.Unlock()
	if err := runtime.persistenceFail(processPersistAfterAuthorityInitialize); err != nil {
		return err
	}

	projectRuntime, err := runtime.loadOrInitializeProject(identity, true, false)
	if err != nil {
		runtime.disableProject(identity, err)
		return err
	}

	runtime.indexMu.Lock()
	entry = runtime.index.Projects[identity.SafeProjectKey]
	if entry.Project != identity || entry.State != "initializing" {
		runtime.indexMu.Unlock()
		return errors.New("runtime project sentinel changed during registration")
	}
	entry.State = "ready"
	entry.Generation++
	runtime.index.Projects[identity.SafeProjectKey] = entry
	if err := runtime.store.saveJSON(
		filepath.Join("project-runtime", "index.json"), runtime.index,
	); err != nil {
		runtime.indexMu.Unlock()
		return err
	}
	runtime.indexMu.Unlock()
	if err := runtime.persistenceFail(processPersistAfterIndexReady); err != nil {
		return err
	}

	runtime.projects[identity.ProjectID] = projectRuntime
	runtime.healthMu.Lock()
	delete(runtime.projectErrs, identity.ProjectID)
	delete(runtime.disabledKeys, identity.SafeProjectKey)
	runtime.healthMu.Unlock()
	return nil
}

func (runtime *processRuntimePersistence) ProjectError(projectID string) error {
	runtime.healthMu.RLock()
	defer runtime.healthMu.RUnlock()
	return runtime.projectErrs[projectID]
}
