package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a *app) expectedTaskWorktree(projectID, taskID string) (string, error) {
	return filepath.Abs(filepath.Join(a.settings.DataDir, "worktrees", projectID, taskID))
}

func (a *app) registeredTaskWorktree(project Project, task Task) (string, error) {
	if strings.TrimSpace(task.WorktreePath) == "" {
		return "", errors.New("task has no registered worktree")
	}
	expected, err := a.expectedTaskWorktree(project.ID, task.ID)
	if err != nil {
		return "", err
	}
	actual, err := filepath.Abs(task.WorktreePath)
	if err != nil {
		return "", err
	}
	if actual != expected {
		return "", errors.New("registered worktree does not match the task-owned path")
	}
	return actual, nil
}

func (a *app) inspectTaskWorktree(project Project, task Task) (Task, bool) {
	path, err := a.registeredTaskWorktree(project, task)
	if err != nil {
		task.WorktreeState = "unavailable"
		task.WorktreeDetail = err.Error()
		return task, false
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		task.WorktreeState = "removed"
		task.WorktreeDetail = ""
		return task, true
	} else if err != nil {
		task.WorktreeState = "unavailable"
		task.WorktreeDetail = err.Error()
		return task, false
	}
	status, err := a.runTaskCommand(context.Background(), path, "git", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		task.WorktreeState = "unavailable"
		task.WorktreeDetail = err.Error()
		return task, false
	}
	if strings.TrimSpace(status) != "" {
		task.WorktreeState = "recoverable_dirty"
		task.WorktreeDetail = strings.TrimSpace(status)
		return task, false
	}
	task.WorktreeState = "clean"
	task.WorktreeDetail = ""
	return task, true
}

// cleanupTaskWorktree only removes the exact task-owned path after the working
// tree is clean. It never uses force, so dirty work remains recoverable.
func (a *app) cleanupTaskWorktree(project Project, task Task) (Task, error) {
	lock := a.projectIntegrationLock(project.ID)
	lock.Lock()
	defer lock.Unlock()
	return a.cleanupTaskWorktreeLocked(project, task)
}

func (a *app) cleanupTaskWorktreeLocked(project Project, task Task) (Task, error) {
	if task.Status != "done" && task.Status != "waiting_merge" && task.Status != "failed" && task.Status != "cancelled" && task.Status != "canceled" {
		return task, fmt.Errorf("task status %q cannot clean up a worktree", task.Status)
	}
	task, clean := a.inspectTaskWorktree(project, task)
	if !clean {
		task.UpdatedAt = time.Now().UTC()
		a.updateTask(project.ID, task)
		a.saveOrLog("tasks", a.saveTasks())
		return task, errors.New("task worktree has recoverable changes or is unavailable")
	}
	path, err := a.registeredTaskWorktree(project, task)
	if err != nil {
		return task, err
	}
	if task.Status == "waiting_merge" {
		branchHead := a.gitOutput(project.Path, "rev-parse", "--verify", task.TaskBranch+"^{commit}")
		if task.CommitSHA == "" || branchHead == "" {
			return task, errors.New("waiting merge task commit is not reachable from its task branch")
		}
		if reachable, reachErr := a.gitIsAncestor(project.Path, task.CommitSHA, branchHead); reachErr != nil || !reachable {
			return task, errors.New("waiting merge task commit is not reachable from its task branch")
		}
	}
	if task.WorktreeState == "removed" {
		return task, nil
	}
	if out, err := a.runTaskCommand(context.Background(), project.Path, "git", "worktree", "remove", path); err != nil {
		task.WorktreeState = "clean"
		task.WorktreeDetail = strings.TrimSpace(out)
		task.UpdatedAt = time.Now().UTC()
		a.updateTask(project.ID, task)
		a.saveOrLog("tasks", a.saveTasks())
		return task, fmt.Errorf("remove task worktree: %w", err)
	}
	if err := os.RemoveAll(path); err != nil {
		return task, fmt.Errorf("remove registered worktree directory: %w", err)
	}
	task.WorktreeState = "removed"
	task.WorktreeDetail = ""
	task.UpdatedAt = time.Now().UTC()
	a.updateTask(project.ID, task)
	a.saveOrLog("tasks", a.saveTasks())
	a.appendTaskLog(project.ID, task.ID, "removed clean task worktree")
	return task, nil
}

func (a *app) refreshTaskWorktreeState(project Project, task Task) Task {
	task, _ = a.inspectTaskWorktree(project, task)
	task.UpdatedAt = time.Now().UTC()
	a.updateTask(project.ID, task)
	a.saveOrLog("tasks", a.saveTasks())
	return task
}
