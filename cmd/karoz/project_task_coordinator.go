package main

import "sync"

// projectTaskCoordinator owns the durable Task projection while
// internal/task.Service owns claim/cancel runtime handles and lifecycle rules.
// Call agentRuntimeLocked-style accessors with app.mu held.
type projectTaskCoordinator struct {
	tasks                  map[string][]Task
	hooks                  map[string][]TaskRuntimeHook
	integrationLocksMu     sync.Mutex
	integrationLocks       map[string]*sync.Mutex
	integrationPreLockHook func()
}

func newProjectTaskCoordinator() *projectTaskCoordinator {
	return &projectTaskCoordinator{tasks: map[string][]Task{}, hooks: map[string][]TaskRuntimeHook{}, integrationLocks: map[string]*sync.Mutex{}}
}

func (a *app) projectTasksLocked() *projectTaskCoordinator {
	if a.projectTasks == nil {
		a.projectTasks = newProjectTaskCoordinator()
	}
	return a.projectTasks
}
