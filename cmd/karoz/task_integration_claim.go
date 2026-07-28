package main

import (
	"context"
	"time"
)

// Called with the per-project integration lock held. This is the single
// handoff from cancelable execution to protected primary-checkout mutation.
func (a *app) claimTaskIntegration(ctx context.Context, projectID, taskID string) (Task, bool) {
	a.taskRunMu.Lock()
	defer a.taskRunMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	list := a.tasks[projectID]
	for i := range list {
		if list[i].ID != taskID {
			continue
		}
		task := list[i]
		if taskWasCancelled(ctx) || task.Status == "cancelling" || task.Status == "cancelled" || task.Status == "canceled" {
			return task, true
		}
		if task.Status == "done" {
			return task, false
		}
		task.Status = "merging"
		task.MergeBlockedReason = ""
		task.MergeBlockedDetail = ""
		task.UpdatedAt = time.Now().UTC()
		list[i] = task
		a.tasks[projectID] = list
		return task, false
	}
	return Task{ID: taskID, ProjectID: projectID, Status: "cancelled", UpdatedAt: time.Now().UTC()}, true
}
