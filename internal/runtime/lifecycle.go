package runtime

import (
	"strings"
	"sync"
	"time"
)

// RunRepository is the application-owned persistence seam for active runs.
// The runtime package owns lifecycle policy while callers remain free to keep
// the actual representation in memory, a snapshot, or a durable store.
type RunRepository interface {
	Find(projectID, agentID string) (Run, bool)
	Insert(Run) bool
	Mutate(projectID, agentID string, mutate func(*Run) bool) (Run, bool)
	Remove(projectID, agentID, expectedRunID string, mutate func(Run) Run) (Run, bool)
}

// RunLifecycle centralizes the state-machine boundary for an Agent Run. Its
// small lock closes the check-then-create/update gaps between callers while
// the repository lock protects the host application's aggregate map.
type RunLifecycle struct {
	repository RunRepository
	mu         sync.Mutex
	now        func() time.Time
}

func NewRunLifecycle(repository RunRepository) *RunLifecycle {
	return &RunLifecycle{
		repository: repository,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

func (lifecycle *RunLifecycle) clock() time.Time {
	if lifecycle != nil && lifecycle.now != nil {
		return lifecycle.now().UTC()
	}
	return time.Now().UTC()
}

// Begin creates a run unless the project/agent already has an active one.
// runID is supplied by the caller so ID generation stays at the application
// boundary and can be deterministic in tests or scheduled-run replay.
func (lifecycle *RunLifecycle) Begin(input RunInput, runID string) (Run, bool) {
	if lifecycle == nil || lifecycle.repository == nil {
		return Run{}, false
	}
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.AgentID = strings.TrimSpace(input.AgentID)
	runID = strings.TrimSpace(runID)
	if input.ProjectID == "" || input.AgentID == "" || runID == "" {
		return Run{}, false
	}

	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if current, ok := lifecycle.repository.Find(input.ProjectID, input.AgentID); ok && current.State.Active() {
		return current, false
	}
	run := NewRun(input, runID, lifecycle.clock())
	if lifecycle.repository.Insert(run) {
		return run, true
	}
	// A repository may reject an insert when another owner won a race outside
	// this lifecycle instance. Return that active run to preserve the existing
	// busy-agent contract rather than exposing an empty result.
	if current, ok := lifecycle.repository.Find(input.ProjectID, input.AgentID); ok && current.State.Active() {
		return current, false
	}
	return Run{}, false
}

func (lifecycle *RunLifecycle) Find(projectID, agentID string) (Run, bool) {
	if lifecycle == nil || lifecycle.repository == nil {
		return Run{}, false
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	return lifecycle.repository.Find(strings.TrimSpace(projectID), strings.TrimSpace(agentID))
}

func (lifecycle *RunLifecycle) Active(projectID, agentID string) (Run, bool) {
	run, ok := lifecycle.Find(projectID, agentID)
	return run, ok && run.State.Active()
}

// Transition applies a legal state transition for the expected run. The
// fourth return value reports whether the expected active run existed; the
// third reports whether its state actually changed.
func (lifecycle *RunLifecycle) Transition(projectID, agentID, expectedRunID string, next State) (Run, State, bool, bool) {
	if lifecycle == nil || lifecycle.repository == nil || strings.TrimSpace(expectedRunID) == "" {
		return Run{}, "", false, false
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	var previous State
	changed := false
	run, ok := lifecycle.repository.Mutate(strings.TrimSpace(projectID), strings.TrimSpace(agentID), func(current *Run) bool {
		if current == nil || current.ID != expectedRunID || !current.State.Active() || !CanTransition(current.State, next) {
			return false
		}
		previous = current.State
		updated, didChange := Transition(*current, next, lifecycle.clock())
		*current = updated
		changed = didChange
		return true
	})
	return run, previous, changed, ok
}

// Finish atomically transforms and removes the expected run from the active
// registry. Cleanup of context/cancellation handles remains an application
// concern and can happen immediately after this method returns.
func (lifecycle *RunLifecycle) Finish(projectID, agentID, expectedRunID string, final State, runErr error) (Run, State, bool) {
	if lifecycle == nil || lifecycle.repository == nil || strings.TrimSpace(expectedRunID) == "" {
		return Run{}, "", false
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	previous := State("")
	run, ok := lifecycle.repository.Remove(strings.TrimSpace(projectID), strings.TrimSpace(agentID), expectedRunID, func(current Run) Run {
		previous = current.State
		return Finish(current, final, runErr, lifecycle.clock())
	})
	return run, previous, ok
}

// EnqueueInterrupt appends a collaboration input to the active run. It is
// intentionally rejected for a stale or terminal run so an interrupt cannot
// leak into a later run for the same agent.
func (lifecycle *RunLifecycle) EnqueueInterrupt(projectID, agentID string, item Interrupt) (Run, bool) {
	if lifecycle == nil || lifecycle.repository == nil {
		return Run{}, false
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	run, ok := lifecycle.repository.Mutate(strings.TrimSpace(projectID), strings.TrimSpace(agentID), func(current *Run) bool {
		if current == nil || !current.State.Active() {
			return false
		}
		current.Interrupts = append(current.Interrupts, item)
		if !item.CreatedAt.IsZero() {
			current.UpdatedAt = item.CreatedAt.UTC()
		} else {
			current.UpdatedAt = lifecycle.clock()
		}
		return true
	})
	return run, ok
}

// DrainInterrupts consumes the pending inputs only for the expected active
// run. The returned slice is detached from the stored run before the next
// provider poll can append another interrupt.
func (lifecycle *RunLifecycle) DrainInterrupts(projectID, agentID, expectedRunID string) ([]Interrupt, bool) {
	if lifecycle == nil || lifecycle.repository == nil || strings.TrimSpace(expectedRunID) == "" {
		return []Interrupt{}, false
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	items := []Interrupt{}
	_, ok := lifecycle.repository.Mutate(strings.TrimSpace(projectID), strings.TrimSpace(agentID), func(current *Run) bool {
		if current == nil || current.ID != expectedRunID || len(current.Interrupts) == 0 {
			return false
		}
		items = append(items, current.Interrupts...)
		current.Interrupts = nil
		current.UpdatedAt = lifecycle.clock()
		return true
	})
	if !ok {
		return []Interrupt{}, false
	}
	return items, true
}
