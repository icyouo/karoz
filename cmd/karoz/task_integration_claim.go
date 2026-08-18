package main

import (
	"context"
	"time"
)

// Called with the per-project integration lock held. This is the single
// handoff from cancelable execution to protected primary-checkout mutation.
func (a *app) claimTaskIntegration(ctx context.Context, projectID, taskID string) (Task, bool) {
	var result Task
	var cancelled bool
	a.ensureTaskService().WithRuntimeLock(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		state := a.projectTasksLocked()
		list := state.tasks[projectID]
		for i := range list {
			if list[i].ID != taskID {
				continue
			}
			task := list[i]
			if ctx.Err() != nil || task.Status == "cancelling" || task.Status == "cancelled" || task.Status == "canceled" {
				result, cancelled = task, true
				return
			}
			if task.Status == "done" {
				result = task
				return
			}
			task.Status = "merging"
			task.MergeBlockedReason = ""
			task.MergeBlockedDetail = ""
			task.UpdatedAt = time.Now().UTC()
			list[i] = task
			state.tasks[projectID] = list
			result = task
			return
		}
		result = Task{ID: taskID, ProjectID: projectID, Status: "cancelled", UpdatedAt: time.Now().UTC()}
		cancelled = true
	})
	return result, cancelled
}
