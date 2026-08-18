package runtime

import (
	"context"
	"testing"
	"time"
)

type controlMemoryRepository struct {
	run   Run
	state RunControlState
}

func (repository *controlMemoryRepository) MutateControl(_ string, _ string, expectedRunID string, mutate func(*Run, *RunControlState) bool) (Run, RunControlState, bool) {
	if repository.run.ID != expectedRunID || mutate == nil || !mutate(&repository.run, &repository.state) {
		return repository.run, repository.state, false
	}
	return repository.run, repository.state, true
}

func (repository *controlMemoryRepository) MutateActiveControl(_ string, _ string, mutate func(*Run, *RunControlState) bool) (Run, RunControlState, bool) {
	if mutate == nil || !mutate(&repository.run, &repository.state) {
		return repository.run, repository.state, false
	}
	return repository.run, repository.state, true
}

func (repository *controlMemoryRepository) CommitResult(_ string, _ string, expectedRunID string, mutate func(*Run, *RunControlState) (Run, bool)) (Run, RunControlState, bool) {
	if repository.run.ID != expectedRunID || mutate == nil {
		return repository.run, repository.state, false
	}
	finished, ok := mutate(&repository.run, &repository.state)
	if !ok {
		return repository.run, repository.state, false
	}
	repository.run = finished
	return repository.run, repository.state, true
}

func (repository *controlMemoryRepository) FinishRun(_ string, _ string, expectedRunID string, mutate func(*Run, *RunControlState) (Run, bool)) (Run, RunControlState, bool) {
	if repository.run.ID != expectedRunID || mutate == nil {
		return repository.run, repository.state, false
	}
	finished, ok := mutate(&repository.run, &repository.state)
	if !ok {
		return repository.run, repository.state, false
	}
	repository.run = finished
	return repository.run, repository.state, true
}

func TestRunControlClaimsWorkerAndBindsCancellationAtomically(t *testing.T) {
	repository := &controlMemoryRepository{run: Run{ID: "run-1", State: StateInvokingModel}}
	control := NewRunControl(repository)
	ctx, claimed := control.ClaimAndBindWorkerContext(context.Background(), "p1", "a1", "run-1")
	if !claimed || repository.state.WorkerRunID != "run-1" || repository.state.Context != ctx || repository.state.Cancel == nil {
		t.Fatalf("claim state = %+v, claimed=%v", repository.state, claimed)
	}
	if control.ClaimWorker("p1", "a1", "run-1") {
		t.Fatal("second worker claim unexpectedly succeeded")
	}
	if _, claimed := control.ClaimAndBindWorkerContext(context.Background(), "p1", "a1", "stale"); claimed {
		t.Fatal("stale worker claim unexpectedly succeeded")
	}
	repository.state.Cancel()
	if ctx.Err() != context.Canceled {
		t.Fatalf("worker context error = %v", ctx.Err())
	}
}

func TestRunControlBindsSchedulerParentWithoutReplacingOwner(t *testing.T) {
	repository := &controlMemoryRepository{run: Run{ID: "run-1", State: StatePreparingContext}}
	control := NewRunControl(repository)
	owner, claimed := control.ClaimAndBindWorkerContext(context.Background(), "p1", "a1", "run-1")
	if !claimed {
		t.Fatal("worker claim failed")
	}
	parent, cancelParent := context.WithCancel(context.Background())
	bound, ok := control.BindContext(parent, "p1", "a1", "run-1")
	if !ok || bound == owner {
		t.Fatal("scheduler binding did not return a child context")
	}
	cancelParent()
	select {
	case <-bound.Done():
		if bound.Err() != context.Canceled {
			t.Fatalf("bound context error = %v", bound.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("bound context did not observe scheduler parent cancellation")
	}
	if owner.Err() != nil {
		t.Fatalf("parent cancellation replaced owner context: %v", owner.Err())
	}
	repository.state.Cancel()
}

func TestRunControlCancellationWinsOverResultCommit(t *testing.T) {
	repository := &controlMemoryRepository{run: Run{ID: "run-1", State: StateInvokingModel}, state: RunControlState{WorkerRunID: "run-1"}}
	control := NewRunControl(repository)
	request, accepted := control.RequestCancel("p1", "a1")
	if !accepted || request.Run.ID != "run-1" || !request.WorkerOwned || !repository.state.Cancelling {
		t.Fatalf("cancel request = %+v, state=%+v, accepted=%v", request, repository.state, accepted)
	}
	if _, _, committed := control.CommitResult("p1", "a1", "run-1", func(run Run) Run {
		run.State = StateDone
		return run
	}); committed {
		t.Fatal("result commit won after cancellation claim")
	}
}

func TestRunControlCommitsResultOnce(t *testing.T) {
	repository := &controlMemoryRepository{run: Run{ID: "run-1", State: StateInvokingModel}}
	control := NewRunControl(repository)
	finished, state, committed := control.CommitResult("p1", "a1", "run-1", func(run Run) Run {
		run.State = StateDone
		return run
	})
	if !committed || finished.State != StateDone || !state.ResultClaimed {
		t.Fatalf("commit = %+v, state=%+v, committed=%v", finished, state, committed)
	}
	if _, _, committed := control.CommitResult("p1", "a1", "run-1", func(run Run) Run { return run }); committed {
		t.Fatal("duplicate result commit unexpectedly succeeded")
	}
}

func TestRunControlFinishesAndNormalizesTerminalState(t *testing.T) {
	repository := &controlMemoryRepository{run: Run{ID: "run-1", State: StateInvokingModel}}
	control := NewRunControl(repository)
	now := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	finished, state, previous, ok := control.FinishRun("p1", "a1", "run-1", StateCompleting, nil, now)
	if !ok || previous != StateInvokingModel || finished.State != StateDone || finished.EndedAt == nil || !finished.EndedAt.Equal(now) || state.Context != nil {
		t.Fatalf("finish = %+v, state=%+v, previous=%s, ok=%v", finished, state, previous, ok)
	}
}
