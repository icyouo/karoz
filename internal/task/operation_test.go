package task

import (
	"testing"
	"time"
)

func TestTaskExposesSharedOperationSnapshot(t *testing.T) {
	zero := int64(0)
	startedAt := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	op := (Task{ID: "task-1", ProjectID: "p", OwnerAgentID: "a", Type: "feature", Status: "running", MaxRuntimeMS: &zero, StartedAt: &startedAt}).Operation(time.Hour)
	if op.ID != "task-1" || op.OwnerID != "p/a" || op.Class != "task/feature" || !op.Deadline.Unlimited || op.Terminal() || op.StartedAt == nil || !op.StartedAt.Equal(startedAt) {
		t.Fatalf("operation snapshot = %+v", op)
	}
}
