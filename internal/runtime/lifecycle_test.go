package runtime

import (
	"testing"
	"time"
)

type lifecycleMemoryRepository struct {
	runs map[string]Run
}

func (repository *lifecycleMemoryRepository) key(projectID, agentID string) string {
	return projectID + "/" + agentID
}

func (repository *lifecycleMemoryRepository) Find(projectID, agentID string) (Run, bool) {
	run, ok := repository.runs[repository.key(projectID, agentID)]
	return run, ok
}

func (repository *lifecycleMemoryRepository) Insert(run Run) bool {
	key := repository.key(run.ProjectID, run.AgentID)
	if current, ok := repository.runs[key]; ok && current.State.Active() {
		return false
	}
	repository.runs[key] = run
	return true
}

func (repository *lifecycleMemoryRepository) Mutate(projectID, agentID string, mutate func(*Run) bool) (Run, bool) {
	key := repository.key(projectID, agentID)
	current, ok := repository.runs[key]
	if !ok || mutate == nil || !mutate(&current) {
		return current, false
	}
	repository.runs[key] = current
	return current, true
}

func (repository *lifecycleMemoryRepository) Remove(projectID, agentID, expectedRunID string, mutate func(Run) Run) (Run, bool) {
	key := repository.key(projectID, agentID)
	current, ok := repository.runs[key]
	if !ok || current.ID != expectedRunID || mutate == nil {
		return Run{}, false
	}
	finished := mutate(current)
	delete(repository.runs, key)
	return finished, true
}

func TestRunLifecycleOwnsBeginTransitionAndFinishPolicy(t *testing.T) {
	repository := &lifecycleMemoryRepository{runs: map[string]Run{}}
	lifecycle := NewRunLifecycle(repository)
	clock := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	lifecycle.now = func() time.Time { return clock }

	run, started := lifecycle.Begin(RunInput{ProjectID: " p1 ", AgentID: " designer ", Trigger: TriggerHandoff}, "run-1")
	if !started || run.State != StatePreparingContext || !run.StartedAt.Equal(clock) {
		t.Fatalf("begin = %+v, started=%v", run, started)
	}
	if duplicate, started := lifecycle.Begin(RunInput{ProjectID: "p1", AgentID: "designer"}, "run-2"); started || duplicate.ID != "run-1" {
		t.Fatalf("duplicate begin = %+v, started=%v", duplicate, started)
	}

	clock = clock.Add(time.Second)
	updated, previous, changed, ok := lifecycle.Transition("p1", "designer", "run-1", StateInvokingModel)
	if !ok || !changed || previous != StatePreparingContext || updated.State != StateInvokingModel || !updated.UpdatedAt.Equal(clock) {
		t.Fatalf("transition = %+v, previous=%s, changed=%v, ok=%v", updated, previous, changed, ok)
	}
	if _, _, _, ok := lifecycle.Transition("p1", "designer", "stale", StateCompleting); ok {
		t.Fatal("stale transition unexpectedly succeeded")
	}

	clock = clock.Add(time.Second)
	finished, previous, ok := lifecycle.Finish("p1", "designer", "run-1", StateFailed, errLifecycleTest{})
	if !ok || previous != StateInvokingModel || finished.State != StateFailed || finished.EndedAt == nil || !finished.EndedAt.Equal(clock) {
		t.Fatalf("finish = %+v, previous=%s, ok=%v", finished, previous, ok)
	}
	if _, active := lifecycle.Active("p1", "designer"); active {
		t.Fatal("finished run remained active")
	}
}

func TestRunLifecycleRejectsStaleInterruptsAndDrainsOnce(t *testing.T) {
	repository := &lifecycleMemoryRepository{runs: map[string]Run{}}
	lifecycle := NewRunLifecycle(repository)
	run, started := lifecycle.Begin(RunInput{ProjectID: "p1", AgentID: "designer"}, "run-1")
	if !started {
		t.Fatal("begin failed")
	}
	item := Interrupt{ID: "interrupt-1", ProjectID: "p1", AgentID: "designer", Body: "continue", CreatedAt: time.Now().UTC()}
	if _, queued := lifecycle.EnqueueInterrupt("p1", "designer", item); !queued {
		t.Fatal("interrupt enqueue failed")
	}
	items, drained := lifecycle.DrainInterrupts("p1", "designer", run.ID)
	if !drained || len(items) != 1 || items[0].ID != item.ID {
		t.Fatalf("drain = %+v, drained=%v", items, drained)
	}
	if items, drained := lifecycle.DrainInterrupts("p1", "designer", run.ID); drained || len(items) != 0 {
		t.Fatalf("second drain = %+v, drained=%v", items, drained)
	}
	if _, queued := lifecycle.EnqueueInterrupt("p1", "designer", Interrupt{ID: "stale"}); !queued {
		t.Fatal("active run rejected a second interrupt")
	}
}

type errLifecycleTest struct{}

func (errLifecycleTest) Error() string { return "lifecycle failure" }
