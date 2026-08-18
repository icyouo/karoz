package main

import runtimedomain "github.com/karoz/karoz/internal/runtime"

// appAgentRunRepository adapts the app's active-run store to the runtime
// lifecycle port. Lifecycle policy lives in internal/runtime; application
// side effects remain attached to the app aggregate.
type appAgentRunRepository struct {
	app *app
}

func (repository appAgentRunRepository) Find(projectID, agentID string) (runtimedomain.Run, bool) {
	if repository.app == nil {
		return runtimedomain.Run{}, false
	}
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	run, ok := repository.app.agentRuntimeLocked().runs[projectAgentKey(projectID, agentID)]
	return run, ok
}

func (repository appAgentRunRepository) Insert(run runtimedomain.Run) bool {
	if repository.app == nil {
		return false
	}
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	runtime := repository.app.agentRuntimeLocked()
	key := projectAgentKey(run.ProjectID, run.AgentID)
	if current, ok := runtime.runs[key]; ok && current.State.Active() {
		return false
	}
	// A terminal predecessor may have left control handles behind while its
	// application-side cleanup is completing. A new Run must start with a clean
	// control slot and must never inherit the predecessor's cancel hook.
	repository.clearControlStateLocked(key)
	runtime.runs[key] = run
	return true
}

func (repository appAgentRunRepository) Mutate(projectID, agentID string, mutate func(*runtimedomain.Run) bool) (runtimedomain.Run, bool) {
	if repository.app == nil || mutate == nil {
		return runtimedomain.Run{}, false
	}
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	runtime := repository.app.agentRuntimeLocked()
	key := projectAgentKey(projectID, agentID)
	current, ok := runtime.runs[key]
	if !ok || !mutate(&current) {
		return current, false
	}
	runtime.runs[key] = current
	return current, true
}

func (repository appAgentRunRepository) Remove(projectID, agentID, expectedRunID string, mutate func(runtimedomain.Run) runtimedomain.Run) (runtimedomain.Run, bool) {
	if repository.app == nil || mutate == nil {
		return runtimedomain.Run{}, false
	}
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	runtime := repository.app.agentRuntimeLocked()
	key := projectAgentKey(projectID, agentID)
	current, ok := runtime.runs[key]
	if !ok || current.ID != expectedRunID {
		return runtimedomain.Run{}, false
	}
	finished := mutate(current)
	delete(runtime.runs, key)
	return finished, true
}

func (repository appAgentRunRepository) MutateControl(projectID, agentID, expectedRunID string, mutate func(*runtimedomain.Run, *runtimedomain.RunControlState) bool) (runtimedomain.Run, runtimedomain.RunControlState, bool) {
	if repository.app == nil || mutate == nil {
		return runtimedomain.Run{}, runtimedomain.RunControlState{}, false
	}
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	runtime := repository.app.agentRuntimeLocked()
	key := projectAgentKey(projectID, agentID)
	run, ok := runtime.runs[key]
	if !ok || run.ID != expectedRunID {
		return runtimedomain.Run{}, runtimedomain.RunControlState{}, false
	}
	state := repository.controlStateLocked(key, expectedRunID)
	if !mutate(&run, &state) {
		return run, state, false
	}
	runtime.runs[key] = run
	repository.writeControlStateLocked(key, expectedRunID, state)
	return run, state, true
}

func (repository appAgentRunRepository) MutateActiveControl(projectID, agentID string, mutate func(*runtimedomain.Run, *runtimedomain.RunControlState) bool) (runtimedomain.Run, runtimedomain.RunControlState, bool) {
	if repository.app == nil || mutate == nil {
		return runtimedomain.Run{}, runtimedomain.RunControlState{}, false
	}
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	runtime := repository.app.agentRuntimeLocked()
	key := projectAgentKey(projectID, agentID)
	run, ok := runtime.runs[key]
	if !ok {
		return runtimedomain.Run{}, runtimedomain.RunControlState{}, false
	}
	state := repository.controlStateLocked(key, run.ID)
	if !mutate(&run, &state) {
		return run, state, false
	}
	runtime.runs[key] = run
	repository.writeControlStateLocked(key, run.ID, state)
	return run, state, true
}

func (repository appAgentRunRepository) CommitResult(projectID, agentID, expectedRunID string, mutate func(*runtimedomain.Run, *runtimedomain.RunControlState) (runtimedomain.Run, bool)) (runtimedomain.Run, runtimedomain.RunControlState, bool) {
	if repository.app == nil || mutate == nil {
		return runtimedomain.Run{}, runtimedomain.RunControlState{}, false
	}
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	runtime := repository.app.agentRuntimeLocked()
	key := projectAgentKey(projectID, agentID)
	run, ok := runtime.runs[key]
	if !ok || run.ID != expectedRunID {
		return runtimedomain.Run{}, runtimedomain.RunControlState{}, false
	}
	state := repository.controlStateLocked(key, expectedRunID)
	finished, accepted := mutate(&run, &state)
	if !accepted {
		return run, state, false
	}
	delete(runtime.runs, key)
	repository.clearControlStateLocked(key)
	return finished, state, true
}

func (repository appAgentRunRepository) FinishRun(projectID, agentID, expectedRunID string, mutate func(*runtimedomain.Run, *runtimedomain.RunControlState) (runtimedomain.Run, bool)) (runtimedomain.Run, runtimedomain.RunControlState, bool) {
	if repository.app == nil || mutate == nil {
		return runtimedomain.Run{}, runtimedomain.RunControlState{}, false
	}
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	runtime := repository.app.agentRuntimeLocked()
	key := projectAgentKey(projectID, agentID)
	run, ok := runtime.runs[key]
	if !ok || run.ID != expectedRunID {
		return runtimedomain.Run{}, runtimedomain.RunControlState{}, false
	}
	state := repository.controlStateLocked(key, expectedRunID)
	finished, accepted := mutate(&run, &state)
	if !accepted {
		return run, state, false
	}
	delete(runtime.runs, key)
	repository.clearControlStateLocked(key)
	return finished, state, true
}

func (repository appAgentRunRepository) controlStateLocked(key, expectedRunID string) runtimedomain.RunControlState {
	runtime := repository.app.agentRuntimeLocked()
	return runtimedomain.RunControlState{
		WorkerRunID:   runtime.workers[key],
		Context:       runtime.contexts[key],
		Cancel:        runtime.cancels[key],
		Cancelling:    runtime.cancelling[key] == expectedRunID,
		ResultClaimed: runtime.resultCommitted[key] == expectedRunID,
	}
}

func (repository appAgentRunRepository) clearControlStateLocked(key string) {
	runtime := repository.app.agentRuntimeLocked()
	delete(runtime.cancels, key)
	delete(runtime.contexts, key)
	delete(runtime.workers, key)
	delete(runtime.cancelling, key)
	delete(runtime.resultCommitted, key)
}

func (repository appAgentRunRepository) writeControlStateLocked(key, expectedRunID string, state runtimedomain.RunControlState) {
	runtime := repository.app.agentRuntimeLocked()
	if state.WorkerRunID != "" {
		runtime.workers[key] = state.WorkerRunID
	} else {
		delete(runtime.workers, key)
	}
	if state.Context != nil {
		runtime.contexts[key] = state.Context
	} else {
		delete(runtime.contexts, key)
	}
	if state.Cancel != nil {
		runtime.cancels[key] = state.Cancel
	} else {
		delete(runtime.cancels, key)
	}
	if state.Cancelling {
		runtime.cancelling[key] = expectedRunID
	} else {
		delete(runtime.cancelling, key)
	}
	if state.ResultClaimed {
		runtime.resultCommitted[key] = expectedRunID
	} else {
		delete(runtime.resultCommitted, key)
	}
}

func (a *app) agentRunLifecycleService() *runtimedomain.RunLifecycle {
	a.mu.Lock()
	defer a.mu.Unlock()
	runtime := a.agentRuntimeLocked()
	if runtime.lifecycle == nil {
		runtime.lifecycle = runtimedomain.NewRunLifecycle(appAgentRunRepository{app: a})
	}
	return runtime.lifecycle
}

func (a *app) agentRunControlService() *runtimedomain.RunControl {
	a.mu.Lock()
	defer a.mu.Unlock()
	runtime := a.agentRuntimeLocked()
	if runtime.control == nil {
		runtime.control = runtimedomain.NewRunControl(appAgentRunRepository{app: a})
	}
	return runtime.control
}
