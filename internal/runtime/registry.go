package runtime

import (
	"context"
	"strings"
	"sync"
)

func OwnerKey(projectID, agentID string) string {
	return strings.TrimSpace(projectID) + "/" + strings.TrimSpace(agentID)
}

// Registry is the in-memory owner for active Runs and their control handles.
// Durable projections (chat_runs, task rows) stay in the calling application.
type Registry struct {
	mu         sync.Mutex
	idle       *sync.Cond
	runs       map[string]Run
	control    map[string]RunControlState
	byRunID    map[string]string
	owners     map[string]*ownerLane
	lifecycle  *RunLifecycle
	controlSvc *RunControl
}

type ownerLane struct {
	mu   sync.Mutex
	refs int
}

// A persistence callback holds only its owner lane, never the registry map lock.
func (registry *Registry) lockOwner(projectID, agentID string) func() {
	key := OwnerKey(projectID, agentID)
	registry.mu.Lock()
	lane := registry.owners[key]
	if lane == nil {
		lane = &ownerLane{}
		if registry.owners == nil {
			registry.owners = map[string]*ownerLane{}
		}
		registry.owners[key] = lane
	}
	lane.refs++
	registry.mu.Unlock()
	lane.mu.Lock()
	return func() {
		lane.mu.Unlock()
		registry.mu.Lock()
		lane.refs--
		if lane.refs == 0 {
			delete(registry.owners, key)
		}
		registry.mu.Unlock()
	}
}

func NewRegistry() *Registry {
	registry := &Registry{
		runs:    map[string]Run{},
		control: map[string]RunControlState{},
		byRunID: map[string]string{},
		owners:  map[string]*ownerLane{},
	}
	registry.idle = sync.NewCond(&registry.mu)
	registry.lifecycle = NewRunLifecycle(registry)
	registry.controlSvc = NewRunControl(registry)
	return registry
}

func (registry *Registry) Lifecycle() *RunLifecycle {
	if registry == nil {
		return nil
	}
	return registry.lifecycle
}

func (registry *Registry) Control() *RunControl {
	if registry == nil {
		return nil
	}
	return registry.controlSvc
}

func (registry *Registry) Lookup(runID string) (projectID, agentID string, run Run, ok bool) {
	if registry == nil {
		return "", "", Run{}, false
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return "", "", Run{}, false
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	key, found := registry.byRunID[runID]
	if !found {
		return "", "", Run{}, false
	}
	current, found := registry.runs[key]
	if !found || current.ID != runID {
		return "", "", Run{}, false
	}
	return current.ProjectID, current.AgentID, current, true
}

func (registry *Registry) Find(projectID, agentID string) (Run, bool) {
	if registry == nil {
		return Run{}, false
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	run, ok := registry.runs[OwnerKey(projectID, agentID)]
	return run, ok
}

func (registry *Registry) Insert(run Run) bool {
	if registry == nil {
		return false
	}
	unlockOwner := registry.lockOwner(run.ProjectID, run.AgentID)
	defer unlockOwner()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	key := OwnerKey(run.ProjectID, run.AgentID)
	if current, ok := registry.runs[key]; ok && current.State.Active() {
		return false
	}
	registry.clearControlLocked(key)
	registry.runs[key] = run
	registry.byRunID[run.ID] = key
	return true
}

func (registry *Registry) Mutate(projectID, agentID string, mutate func(*Run) bool) (Run, bool) {
	if registry == nil || mutate == nil {
		return Run{}, false
	}
	unlockOwner := registry.lockOwner(projectID, agentID)
	defer unlockOwner()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	key := OwnerKey(projectID, agentID)
	current, ok := registry.runs[key]
	if !ok || !mutate(&current) {
		return current, false
	}
	registry.runs[key] = current
	registry.byRunID[current.ID] = key
	return current, true
}

func (registry *Registry) Remove(projectID, agentID, expectedRunID string, mutate func(Run) Run) (Run, bool) {
	if registry == nil || mutate == nil {
		return Run{}, false
	}
	unlockOwner := registry.lockOwner(projectID, agentID)
	defer unlockOwner()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	key := OwnerKey(projectID, agentID)
	current, ok := registry.runs[key]
	if !ok || current.ID != expectedRunID {
		return Run{}, false
	}
	finished := mutate(current)
	delete(registry.runs, key)
	delete(registry.byRunID, expectedRunID)
	registry.broadcastIdleLocked()
	return finished, true
}

func (registry *Registry) MutateControl(projectID, agentID, expectedRunID string, mutate func(*Run, *RunControlState) bool) (Run, RunControlState, bool) {
	if registry == nil || mutate == nil {
		return Run{}, RunControlState{}, false
	}
	unlockOwner := registry.lockOwner(projectID, agentID)
	defer unlockOwner()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	key := OwnerKey(projectID, agentID)
	run, ok := registry.runs[key]
	if !ok || run.ID != expectedRunID {
		return run, RunControlState{}, false
	}
	state := registry.control[key]
	if !mutate(&run, &state) {
		return run, state, false
	}
	registry.runs[key] = run
	registry.control[key] = state
	registry.byRunID[run.ID] = key
	return run, state, true
}

func (registry *Registry) MutateActiveControl(projectID, agentID string, mutate func(*Run, *RunControlState) bool) (Run, RunControlState, bool) {
	if registry == nil || mutate == nil {
		return Run{}, RunControlState{}, false
	}
	unlockOwner := registry.lockOwner(projectID, agentID)
	defer unlockOwner()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	key := OwnerKey(projectID, agentID)
	run, ok := registry.runs[key]
	if !ok {
		return run, RunControlState{}, false
	}
	state := registry.control[key]
	if !mutate(&run, &state) {
		return run, state, false
	}
	registry.runs[key] = run
	registry.control[key] = state
	registry.byRunID[run.ID] = key
	return run, state, true
}

func (registry *Registry) CommitResult(projectID, agentID, expectedRunID string, mutate func(*Run, *RunControlState) (Run, bool)) (Run, RunControlState, bool) {
	if registry == nil || mutate == nil {
		return Run{}, RunControlState{}, false
	}
	unlockOwner := registry.lockOwner(projectID, agentID)
	defer unlockOwner()
	registry.mu.Lock()
	key := OwnerKey(projectID, agentID)
	run, ok := registry.runs[key]
	if !ok || run.ID != expectedRunID {
		registry.mu.Unlock()
		return Run{}, RunControlState{}, false
	}
	state := registry.control[key]
	registry.mu.Unlock()
	finished, accepted := mutate(&run, &state)
	if !accepted {
		return run, state, false
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	delete(registry.runs, key)
	delete(registry.byRunID, expectedRunID)
	registry.clearControlLocked(key)
	registry.broadcastIdleLocked()
	return finished, state, true
}

func (registry *Registry) FinishRun(projectID, agentID, expectedRunID string, mutate func(*Run, *RunControlState) (Run, bool)) (Run, RunControlState, bool) {
	if registry == nil || mutate == nil {
		return Run{}, RunControlState{}, false
	}
	unlockOwner := registry.lockOwner(projectID, agentID)
	defer unlockOwner()
	registry.mu.Lock()
	key := OwnerKey(projectID, agentID)
	run, ok := registry.runs[key]
	if !ok || run.ID != expectedRunID {
		registry.mu.Unlock()
		return Run{}, RunControlState{}, false
	}
	state := registry.control[key]
	registry.mu.Unlock()
	finished, accepted := mutate(&run, &state)
	if !accepted {
		return run, state, false
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	delete(registry.runs, key)
	delete(registry.byRunID, expectedRunID)
	registry.clearControlLocked(key)
	registry.broadcastIdleLocked()
	return finished, state, true
}

func (registry *Registry) WaitInactive(ctx context.Context, projectID, agentID string) bool {
	if registry == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := OwnerKey(projectID, agentID)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	stop := context.AfterFunc(ctx, func() {
		registry.mu.Lock()
		registry.broadcastIdleLocked()
		registry.mu.Unlock()
	})
	defer stop()
	for {
		if ctx.Err() != nil {
			return false
		}
		run, ok := registry.runs[key]
		if !ok || !run.State.Active() {
			return true
		}
		registry.idle.Wait()
	}
}

func (registry *Registry) broadcastIdleLocked() {
	if registry != nil && registry.idle != nil {
		registry.idle.Broadcast()
	}
}

func (registry *Registry) clearControlLocked(key string) {
	delete(registry.control, key)
}

var _ RunRepository = (*Registry)(nil)
var _ ControlRepository = (*Registry)(nil)
