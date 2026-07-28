package main

import (
	"errors"
	"path/filepath"

	monitordomain "github.com/karoz/karoz/internal/monitor"
)

// RegisterProject extends the dormant runtime in place. It never rebuilds the
// supervisor, so existing process handles keep their server-owned contexts.
func (runtime *processRuntimePersistence) RegisterProject(project Project) error {
	return runtime.RegisterProjectPrepared(project, nil)
}

// RegisterProjectPrepared validates identity admission before invoking prepare,
// then keeps the registry lane exclusive until the durable runtime partition is
// committed. A rejected identity therefore cannot mutate application config.
func (runtime *processRuntimePersistence) RegisterProjectPrepared(
	project Project,
	prepare func() error,
) error {
	identities, err := resolveRuntimeProjectIdentities(runtime.store.root, []Project{project})
	if err != nil {
		return err
	}
	identity := identities[0]
	runtime.registryMu.Lock()
	defer runtime.registryMu.Unlock()

	runtime.indexMu.Lock()
	alreadyRegistered, candidate, err := runtime.validateProjectRegistrationLocked(identity)
	if err != nil {
		runtime.indexMu.Unlock()
		return err
	}
	if alreadyRegistered {
		runtime.indexMu.Unlock()
		if prepare != nil {
			return prepare()
		}
		return nil
	}
	runtime.indexMu.Unlock()
	if prepare != nil {
		if err := prepare(); err != nil {
			return err
		}
	}

	runtime.indexMu.Lock()
	alreadyRegistered, candidate, err = runtime.validateProjectRegistrationLocked(identity)
	if err != nil {
		runtime.indexMu.Unlock()
		return err
	}
	if alreadyRegistered {
		runtime.indexMu.Unlock()
		return nil
	}
	entry := candidate.Projects[identity.SafeProjectKey]
	if err := runtime.store.saveJSON(
		filepath.Join("project-runtime", "index.json"), candidate,
	); err != nil {
		runtime.indexMu.Unlock()
		return err
	}
	runtime.index = candidate
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

func (runtime *processRuntimePersistence) validateProjectRegistrationLocked(
	identity monitordomain.RuntimeProjectIdentity,
) (bool, runtimeProjectIndex, error) {
	if existing := runtime.projects[identity.ProjectID]; existing != nil {
		if existing.identity != identity {
			return false, runtimeProjectIndex{}, errors.New("runtime project registration identity mismatch")
		}
		return true, runtime.index, nil
	}

	entry, exists := runtime.index.Projects[identity.SafeProjectKey]
	if exists {
		if entry.Project != identity {
			return false, runtimeProjectIndex{}, errors.New("runtime project sentinel identity mismatch")
		}
		return false, runtimeProjectIndex{}, errors.New("runtime project is disabled and requires bootstrap recovery")
	}
	for _, existing := range runtime.index.Projects {
		if existing.Project.CanonicalProjectPath == identity.CanonicalProjectPath &&
			existing.Project.ProjectID != identity.ProjectID {
			return false, runtimeProjectIndex{}, errors.New("runtime project canonical path is already registered")
		}
	}
	entry = runtimeProjectIndexEntry{
		Project: identity, State: "initializing", Generation: 1,
	}
	candidate := cloneRuntimeProjectIndex(runtime.index)
	candidate.Projects[identity.SafeProjectKey] = entry
	if err := validateRuntimeProjectIndex(candidate); err != nil {
		return false, runtimeProjectIndex{}, err
	}
	return false, candidate, nil
}

func cloneRuntimeProjectIndex(index runtimeProjectIndex) runtimeProjectIndex {
	cloned := runtimeProjectIndex{
		SchemaVersion: index.SchemaVersion,
		Projects:      make(map[string]runtimeProjectIndexEntry, len(index.Projects)),
	}
	for key, entry := range index.Projects {
		cloned.Projects[key] = entry
	}
	return cloned
}

func (runtime *processRuntimePersistence) ProjectError(projectID string) error {
	runtime.healthMu.RLock()
	defer runtime.healthMu.RUnlock()
	return runtime.projectErrs[projectID]
}
