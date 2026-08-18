package main

import (
	"context"
	"errors"
	"strings"
	"time"
)

// claimTaskRun atomically changes a runnable task to running and installs the
// cancellation handle before callers can observe the running state.
func (a *app) claimTaskRun(projectID, taskID string) (Task, context.Context, func(), bool) {
	return a.ensureTaskService().Claim(projectID, taskID)
}

func (a *app) cancelTaskRun(projectID, taskID string) bool {
	return a.ensureTaskService().Cancel(projectID, taskID)
}

func (a *app) runTaskCommand(ctx context.Context, dir, name string, args ...string) (string, error) {
	result, err := a.runCapturedCommand(ctx, dir, name, args...)
	return result.Output(), err
}

func (a *app) gitOutput(dir string, args ...string) string {
	out, err := a.runTaskCommand(context.Background(), dir, "git", args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func taskWasCancelled(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

func taskTimedOut(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.DeadlineExceeded)
}

func (a *app) cancelledTask(project Project, task Task, phase string) Task {
	task.Status = "cancelled"
	task.FailureSummary = ""
	task.Result = "task cancelled"
	task.UpdatedAt = time.Now().UTC()
	if phase = strings.TrimSpace(phase); phase != "" {
		a.appendTaskLog(project.ID, task.ID, "task cancelled during "+phase)
	} else {
		a.appendTaskLog(project.ID, task.ID, "task cancelled")
	}
	task = a.refreshTaskWorktreeState(project, task)
	return task
}

func (a *app) timedOutTask(project Project, task Task, phase string) Task {
	task.Status = "failed"
	task.FailureSummary = "task exceeded its maximum runtime"
	task.Result = ""
	task.UpdatedAt = time.Now().UTC()
	if phase = strings.TrimSpace(phase); phase != "" {
		a.appendTaskLog(project.ID, task.ID, "maximum runtime exceeded during "+phase)
	} else {
		a.appendTaskLog(project.ID, task.ID, "maximum runtime exceeded")
	}
	task = a.refreshTaskWorktreeState(project, task)
	return task
}
