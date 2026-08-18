package runtime

import (
	"context"
	"strings"
	"time"
)

// RunControlState is the non-durable execution ownership attached to a Run.
// The application adapter decides where these handles live; the runtime
// service owns the rules for claiming and binding them.
type RunControlState struct {
	WorkerRunID   string
	Context       context.Context
	Cancel        context.CancelFunc
	Cancelling    bool
	ResultClaimed bool
}

// ControlRepository performs one atomic read/mutate of a Run and its control
// state. Implementations must hold their aggregate lock for the whole call.
type ControlRepository interface {
	MutateControl(projectID, agentID, expectedRunID string, mutate func(*Run, *RunControlState) bool) (Run, RunControlState, bool)
	MutateActiveControl(projectID, agentID string, mutate func(*Run, *RunControlState) bool) (Run, RunControlState, bool)
	CommitResult(projectID, agentID, expectedRunID string, mutate func(*Run, *RunControlState) (Run, bool)) (Run, RunControlState, bool)
	FinishRun(projectID, agentID, expectedRunID string, mutate func(*Run, *RunControlState) (Run, bool)) (Run, RunControlState, bool)
}

// RunControl contains worker ownership and context-binding policy without
// taking ownership of application persistence or provider side effects.
type RunControl struct {
	repository ControlRepository
}

func NewRunControl(repository ControlRepository) *RunControl {
	return &RunControl{repository: repository}
}

type CancelRequest struct {
	Run         Run
	Cancel      context.CancelFunc
	WorkerOwned bool
}

// RequestCancel claims cancellation before any terminal side effect. The
// cancelling marker blocks a concurrent result commit; a worker-owned run
// remains in the registry until its worker observes the cancelled context.
func (control *RunControl) RequestCancel(projectID, agentID string) (CancelRequest, bool) {
	if control == nil || control.repository == nil {
		return CancelRequest{}, false
	}
	request := CancelRequest{}
	run, state, ok := control.repository.MutateActiveControl(projectID, agentID, func(current *Run, currentState *RunControlState) bool {
		if current == nil || !current.State.Active() || currentState == nil || currentState.ResultClaimed {
			return false
		}
		request.Run = *current
		request.Cancel = currentState.Cancel
		request.WorkerOwned = currentState.WorkerRunID == current.ID
		currentState.Cancelling = true
		return true
	})
	if !ok {
		return CancelRequest{}, false
	}
	request.Run = run
	request.Cancel = state.Cancel
	return request, true
}

// CommitResult is the success-side linearization point. The finalize callback
// runs while the repository still owns the active Run lock, which lets the
// application append its durable assistant message and remove the Run in one
// atomic operation.
func (control *RunControl) CommitResult(projectID, agentID, expectedRunID string, finalize func(Run) Run) (Run, RunControlState, bool) {
	if control == nil || control.repository == nil || strings.TrimSpace(expectedRunID) == "" || finalize == nil {
		return Run{}, RunControlState{}, false
	}
	return control.repository.CommitResult(projectID, agentID, expectedRunID, func(current *Run, state *RunControlState) (Run, bool) {
		if current == nil || current.ID != expectedRunID || !current.State.Active() || state == nil || state.Cancelling || state.ResultClaimed {
			return Run{}, false
		}
		state.ResultClaimed = true
		return finalize(*current), true
	})
}

// FinishRun is the terminal cleanup boundary for failed/cancelled runs. The
// adapter removes the active Run and every ephemeral control handle while it
// still owns the same lock used by cancellation and result commits.
func (control *RunControl) FinishRun(projectID, agentID, expectedRunID string, final State, runErr error, now time.Time) (Run, RunControlState, State, bool) {
	if control == nil || control.repository == nil || strings.TrimSpace(expectedRunID) == "" {
		return Run{}, RunControlState{}, "", false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	previous := State("")
	run, state, ok := control.repository.FinishRun(projectID, agentID, expectedRunID, func(current *Run, currentState *RunControlState) (Run, bool) {
		if current == nil || current.ID != expectedRunID || currentState == nil {
			return Run{}, false
		}
		previous = current.State
		return Finish(*current, final, runErr, now), true
	})
	return run, state, previous, ok
}

func (control *RunControl) ClaimWorker(projectID, agentID, expectedRunID string) bool {
	if control == nil || control.repository == nil || strings.TrimSpace(expectedRunID) == "" {
		return false
	}
	_, _, ok := control.repository.MutateControl(projectID, agentID, expectedRunID, func(run *Run, state *RunControlState) bool {
		if run == nil || run.ID != expectedRunID || !run.State.Active() || state == nil || state.WorkerRunID != "" {
			return false
		}
		state.WorkerRunID = expectedRunID
		return true
	})
	return ok
}

// ClaimAndBindWorkerContext is the direct-run start boundary. Claim and
// cancellation ownership are installed in one repository mutation, so an
// explicit cancel cannot observe a worker that has no signal yet.
func (control *RunControl) ClaimAndBindWorkerContext(parent context.Context, projectID, agentID, expectedRunID string) (context.Context, bool) {
	if parent == nil {
		parent = context.Background()
	}
	if control == nil || control.repository == nil || strings.TrimSpace(expectedRunID) == "" {
		return parent, false
	}
	ctx, cancel := context.WithCancel(parent)
	_, _, ok := control.repository.MutateControl(projectID, agentID, expectedRunID, func(run *Run, state *RunControlState) bool {
		if run == nil || run.ID != expectedRunID || !run.State.Active() || state == nil || state.WorkerRunID != "" {
			return false
		}
		state.WorkerRunID = expectedRunID
		state.Context = ctx
		state.Cancel = cancel
		return true
	})
	if !ok {
		cancel()
		return parent, false
	}
	return ctx, true
}

// BindContext keeps an existing scheduler-owned context authoritative for
// explicit cancellation while returning a child that also observes parent.
func (control *RunControl) BindContext(parent context.Context, projectID, agentID, expectedRunID string) (context.Context, bool) {
	if parent == nil {
		parent = context.Background()
	}
	if control == nil || control.repository == nil || strings.TrimSpace(expectedRunID) == "" {
		return parent, false
	}
	ctx, cancel := context.WithCancel(parent)
	var existing context.Context
	_, _, ok := control.repository.MutateControl(projectID, agentID, expectedRunID, func(run *Run, state *RunControlState) bool {
		if run == nil || run.ID != expectedRunID || !run.State.Active() || state == nil {
			return false
		}
		if state.Context != nil {
			existing = state.Context
			return true
		}
		state.Context = ctx
		state.Cancel = cancel
		return true
	})
	if !ok {
		cancel()
		return parent, false
	}
	if existing != nil {
		cancel()
		return ContextWithParentCancellation(existing, parent), true
	}
	return ctx, true
}

// ContextWithParentCancellation bridges two contexts without changing the
// owner cancellation handle stored by the application.
func ContextWithParentCancellation(owner, parent context.Context) context.Context {
	if owner == nil {
		return parent
	}
	if parent == nil || parent.Done() == nil {
		return owner
	}
	child, cancel := context.WithCancel(owner)
	if parent.Err() != nil {
		cancel()
		return child
	}
	stopParent := context.AfterFunc(parent, cancel)
	context.AfterFunc(child, func() { stopParent() })
	return child
}
