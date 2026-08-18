package process

import (
	"testing"
	"time"
)

func TestProcessExposesSharedOperationSnapshot(t *testing.T) {
	op := (Process{ID: "p-1", ProjectID: "project", AgentID: "agent", State: StateInterrupted, LifetimeMS: 0}).Operation(time.Hour)
	if op.ID != "p-1" || op.OwnerID != "project/agent" || op.Class != "background_process" || !op.Deadline.Unlimited || !op.Terminal() {
		t.Fatalf("operation snapshot = %+v", op)
	}
}
