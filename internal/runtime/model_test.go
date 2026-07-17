package runtime

import (
	"testing"
	"time"
)

func TestRunAndScheduledContracts(t *testing.T) {
	if NormalizeTrigger(Trigger("unknown")) != TriggerSystem {
		t.Fatal("unknown trigger did not normalize to system")
	}
	if StateDone.Active() || StateFailed.Active() || StateCancelled.Active() || !StateExecutingTool.Active() {
		t.Fatal("run terminal/active state contract changed")
	}
	job := ScheduledRun{ID: "run-1", ProjectID: "p1", AgentID: "designer", Trigger: TriggerHandoff, TimeoutMS: 1500}
	input := job.RunInput()
	if input.RunID != job.ID || input.Trigger != TriggerHandoff || job.Timeout(time.Minute) != 1500*time.Millisecond {
		t.Fatalf("scheduled run contract = input=%+v timeout=%s", input, job.Timeout(time.Minute))
	}
}

func TestRunServiceTransitions(t *testing.T) {
	now := time.Date(2026, 7, 11, 2, 0, 0, 0, time.UTC)
	run := NewRun(RunInput{ProjectID: "p1", AgentID: "designer", Trigger: TriggerHandoff}, "run-1", now)
	if run.State != StatePreparingContext || run.ID != "run-1" {
		t.Fatalf("new run = %+v", run)
	}
	if changedRun, changed := Transition(run, StateExecutingTool, now.Add(time.Second)); changed || changedRun.State != StatePreparingContext {
		t.Fatalf("illegal transition was accepted: %+v changed=%v", changedRun, changed)
	}
	run, changed := Transition(run, StateInvokingModel, now.Add(time.Second))
	if !changed || run.State != StateInvokingModel {
		t.Fatalf("invoking run = %+v changed=%v", run, changed)
	}
	run, changed = Transition(run, StateExecutingTool, now.Add(2*time.Second))
	if !changed || run.State != StateExecutingTool {
		t.Fatalf("transitioned run = %+v changed=%v", run, changed)
	}
	run = Finish(run, StateCompleting, nil, now.Add(3*time.Second))
	if run.State != StateDone || run.EndedAt == nil || run.State.Active() {
		t.Fatalf("finished run = %+v", run)
	}
	if _, changed := Transition(run, StateInvokingModel, now.Add(4*time.Second)); changed {
		t.Fatal("terminal run transitioned")
	}
}
