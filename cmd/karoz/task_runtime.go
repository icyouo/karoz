package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

func taskRunKey(projectID, taskID string) string { return projectID + "/" + taskID }

type taskRun struct {
	token  string
	cancel context.CancelFunc
}

// claimTaskRun atomically changes a runnable task to running and installs the
// cancellation handle before callers can observe the running state.
func (a *app) claimTaskRun(projectID, taskID string) (Task, context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(context.Background())
	key := taskRunKey(projectID, taskID)
	a.taskRunMu.Lock()
	defer a.taskRunMu.Unlock()
	if a.taskRunCancels == nil {
		a.taskRunCancels = map[string]taskRun{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	list := a.tasks[projectID]
	for i := range list {
		if list[i].ID != taskID {
			continue
		}
		if !taskRunnable(list[i].Status) || a.taskRunCancels[key].cancel != nil {
			cancel()
			return Task{}, nil, nil, false
		}
		list[i].Status = "running"
		list[i].FailureSummary = ""
		list[i].Result = ""
		list[i].UpdatedAt = time.Now().UTC()
		a.tasks[projectID] = list
		token := randomID()
		a.taskRunCancels[key] = taskRun{token: token, cancel: cancel}
		finish := func() {
			a.taskRunMu.Lock()
			current := a.taskRunCancels[key]
			if current.token == token {
				delete(a.taskRunCancels, key)
			}
			a.taskRunMu.Unlock()
			cancel()
		}
		return list[i], ctx, finish, true
	}
	cancel()
	return Task{}, nil, nil, false
}

func (a *app) cancelTaskRun(projectID, taskID string) bool {
	a.taskRunMu.Lock()
	run := a.taskRunCancels[taskRunKey(projectID, taskID)]
	a.taskRunMu.Unlock()
	if run.cancel == nil {
		return false
	}
	run.cancel()
	return true
}

func runTaskCommand(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	prepareResidentBashProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(out), ctx.Err()
	}
	return string(out), err
}

func taskWasCancelled(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
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
