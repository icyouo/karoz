package runtime

import (
	"testing"
	"time"
)

func TestScheduledRunExposesSharedOperationSnapshot(t *testing.T) {
	zero := int64(0)
	job := ScheduledRun{ID: "op-1", ProjectID: "p", AgentID: "a", Kind: ScheduledTaskEvent, Status: ScheduledCancelled, TimeoutMS: zero}
	op := job.Operation(time.Minute)
	if op.ID != job.ID || op.OwnerID != "p/a" || op.Class != "scheduled/task_event" || !op.Terminal() || !op.Deadline.Unlimited {
		t.Fatalf("operation snapshot = %+v", op)
	}
}

func TestAgentRunExposesSharedOperationSnapshot(t *testing.T) {
	startedAt := time.Date(2026, 8, 15, 1, 2, 3, 0, time.UTC)
	run := Run{
		ID: "run-1", ProjectID: "project", AgentID: "agent", TurnType: "dev",
		State: StateInvokingModel, StartedAt: startedAt, UpdatedAt: startedAt,
	}
	op := run.Operation(45 * time.Second)
	if op.ID != run.ID || op.OwnerID != "project/agent" || op.Class != "agent_run/dev" || op.State != string(StateInvokingModel) || op.Deadline.Unlimited || op.Deadline.Duration != 45*time.Second || op.StartedAt == nil || !op.StartedAt.Equal(startedAt) || op.Terminal() {
		t.Fatalf("operation snapshot = %+v", op)
	}
}
