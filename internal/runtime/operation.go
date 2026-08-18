package runtime

import "time"

// Operation is the common durable-lifecycle view exposed by Task, Process,
// ScheduledRun, and Agent Run without flattening their domain-specific
// records.
type Operation struct {
	ID        string
	OwnerID   string
	Class     string
	State     string
	Deadline  Deadline
	StartedAt *time.Time
	UpdatedAt time.Time
}

func (operation Operation) Terminal() bool {
	switch operation.State {
	case "done", "failed", "cancelled", "succeeded", "killed", "interrupted":
		return true
	default:
		return false
	}
}

// Operation exposes a direct Agent Run through the same lifecycle vocabulary
// used by durable task, process, and scheduled execution. The caller supplies
// the configured provider-turn default because direct Runs deliberately retain
// their provider-specific budget outside the generic Run record.
func (run Run) Operation(defaultTimeout time.Duration) Operation {
	startedAt := run.StartedAt
	return Operation{
		ID: run.ID, OwnerID: run.ProjectID + "/" + run.AgentID,
		Class: "agent_run/" + run.TurnType, State: string(run.State),
		Deadline:  Deadline{Duration: defaultTimeout},
		StartedAt: &startedAt, UpdatedAt: run.UpdatedAt,
	}
}

func (job ScheduledRun) Operation(defaultTimeout time.Duration) Operation {
	timeoutMS := job.TimeoutMS
	return Operation{
		ID: job.ID, OwnerID: job.ProjectID + "/" + job.AgentID,
		Class: "scheduled/" + string(job.Kind), State: string(job.Status),
		Deadline:  DeadlineFromMilliseconds(&timeoutMS, defaultTimeout),
		StartedAt: job.StartedAt, UpdatedAt: job.UpdatedAt,
	}
}
