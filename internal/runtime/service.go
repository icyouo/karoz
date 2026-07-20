package runtime

import "time"

var validStateTransitions = map[State]map[State]bool{
	StateQueued: {
		StatePreparingContext: true,
	},
	StatePreparingContext: {
		StateInvokingModel: true,
		StateCompleting:    true,
	},
	StateInvokingModel: {
		StateExecutingTool: true,
		StateCompleting:    true,
	},
	StateExecutingTool: {
		StateWaitingModel: true,
	},
	StateWaitingModel: {
		StateInvokingModel: true,
		StateExecutingTool: true,
		StateCompleting:    true,
	},
}

func NewRun(input RunInput, runID string, now time.Time) Run {
	return Run{
		ID: runID, ProjectID: input.ProjectID, AgentID: input.AgentID, Trigger: NormalizeTrigger(input.Trigger),
		TurnType: input.TurnType, SourceID: input.SourceID, MessageID: input.MessageID,
		Provider: input.Provider, Model: input.Model, ThinkingEffort: input.ThinkingEffort, ModelConfigVersion: input.ModelConfigVersion,
		State: StatePreparingContext, StartedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
}

func CanTransition(from, to State) bool {
	if !from.Active() {
		return false
	}
	return from == to || validStateTransitions[from][to]
}

func Transition(run Run, next State, now time.Time) (Run, bool) {
	if !CanTransition(run.State, next) {
		return run, false
	}
	if run.State == next {
		return run, false
	}
	changed := run.State != next
	run.State = next
	run.UpdatedAt = now.UTC()
	return run, changed
}

func Finish(run Run, final State, runErr error, now time.Time) Run {
	if final != StateFailed && final != StateCancelled {
		final = StateDone
	}
	endedAt := now.UTC()
	run.State = final
	run.UpdatedAt = endedAt
	run.EndedAt = &endedAt
	if runErr != nil {
		run.Error = runErr.Error()
	}
	return run
}
