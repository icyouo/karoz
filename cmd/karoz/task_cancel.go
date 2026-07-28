package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// cancelTask coordinates with the short integration lock so cancellation can
// stop executor and verifier processes but can never interrupt a primary merge.
func (a *app) cancelTask(project Project, requested Task) (Task, error) {
	lock := a.projectIntegrationLock(project.ID)
	if !lock.TryLock() {
		return requested, errors.New("task is in protected worktree or merge maintenance; retry cancellation after it leaves the critical section")
	}
	defer lock.Unlock()
	task, ok := a.findTask(project.ID, requested.ID)
	if !ok {
		return Task{}, errors.New("task not found")
	}
	switch strings.ToLower(task.Status) {
	case "cancelled", "canceled":
		return task, nil
	case "cancelling":
		return task, nil
	case "merging":
		return task, errors.New("task is already in the protected merge section")
	case "running", "verifying", "deploying":
		task.Status = "cancelling"
		task.FailureSummary = ""
		task.Result = ""
		task.UpdatedAt = time.Now().UTC()
		a.updateTask(project.ID, task)
		a.saveOrLog("tasks", a.saveTasks())
		a.appendTaskLog(project.ID, task.ID, "cancellation requested")
		if a.cancelTaskRun(project.ID, task.ID) {
			return task, nil
		}
		// There is no owned process to wait for (for example, a recovered task).
		task.Status = "cancelled"
		task.Result = "task cancelled"
		task.UpdatedAt = time.Now().UTC()
		a.updateTask(project.ID, task)
		a.saveOrLog("tasks", a.saveTasks())
		a.appendTaskLog(project.ID, task.ID, "task cancelled without an active process")
		return task, nil
	default:
		return task, fmt.Errorf("task status %q cannot be cancelled", task.Status)
	}
}
