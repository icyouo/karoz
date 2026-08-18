package main

import "sync"

// projectRegistry owns durable project metadata that is not derivable from a
// Git checkout, currently user-assigned display aliases.
type projectRegistry struct {
	aliases                          map[string]string
	registrationMu                   sync.Mutex
	createAfterRegistrationHook      func()
	settingsUpdateBeforeRegistryHook func()
	importSettingsSave               func() error
}

func newProjectRegistry() *projectRegistry {
	return &projectRegistry{aliases: map[string]string{}}
}

func (a *app) projectRegistryLocked() *projectRegistry {
	if a.projectRegistry == nil {
		a.projectRegistry = newProjectRegistry()
	}
	return a.projectRegistry
}
