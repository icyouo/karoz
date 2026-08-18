package process

import (
	"reflect"
	"testing"
	"time"
)

func TestStateMachineAndNormalize(t *testing.T) {
	allowed := [][2]State{
		{StateStarting, StateRunning}, {StateStarting, StateFailed}, {StateStarting, StateInterrupted},
		{StateRunning, StateSucceeded}, {StateRunning, StateFailed}, {StateRunning, StateKilled}, {StateRunning, StateInterrupted},
	}
	for _, transition := range allowed {
		if !CanTransition(transition[0], transition[1]) {
			t.Fatalf("expected transition %s -> %s", transition[0], transition[1])
		}
	}
	illegal := [][2]State{
		{StateStarting, StateSucceeded}, {StateStarting, StateKilled},
		{StateRunning, StateStarting}, {State("unknown"), StateRunning},
	}
	for _, transition := range illegal {
		if CanTransition(transition[0], transition[1]) {
			t.Fatalf("illegal transition accepted: %s -> %s", transition[0], transition[1])
		}
	}
	for _, terminal := range []State{StateSucceeded, StateFailed, StateKilled, StateInterrupted} {
		if CanTransition(terminal, StateRunning) {
			t.Fatalf("terminal state %s transitioned", terminal)
		}
	}
	if !StateSucceeded.Succeeded() || StateKilled.Succeeded() || StateInterrupted.Succeeded() {
		t.Fatal("success predicate must depend on terminal state")
	}

	now := time.Date(2026, 7, 29, 1, 2, 3, 0, time.UTC)
	for _, state := range []State{StateStarting, StateRunning} {
		got := Normalize(Process{State: state, PID: 10, GuardPID: 11, PGID: 12}, now)
		if got.State != StateInterrupted || got.PID != 0 || got.GuardPID != 0 || got.PGID != 0 {
			t.Fatalf("unsafe normalized process: %+v", got)
		}
		if got.EndedAt == nil || !got.EndedAt.Equal(now) {
			t.Fatalf("missing normalized end time: %+v", got)
		}
	}
	terminal := Process{State: StateKilled, PID: 99}
	if got := Normalize(terminal, now); !reflect.DeepEqual(got, terminal) {
		t.Fatalf("terminal record changed: %+v", got)
	}
}
