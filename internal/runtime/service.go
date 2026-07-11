package runtime

import "time"

func NewRun(input RunInput, runID string, now time.Time) Run {
	return Run{
		ID: runID, ProjectID: input.ProjectID, AgentID: input.AgentID, Trigger: NormalizeTrigger(input.Trigger),
		TurnType: input.TurnType, SourceID: input.SourceID, MessageID: input.MessageID,
		State: StatePreparingContext, StartedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
}

func Transition(run Run, next State, now time.Time) (Run, bool) {
	if !run.State.Active() {
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
