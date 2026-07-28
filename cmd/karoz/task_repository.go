package main

import (
	"strings"
	"time"
)

func (a *app) tasksForProject(projectID string) []Task {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]Task{}, a.tasks[projectID]...)
	return out
}

func (a *app) findTask(projectID, taskID string) (Task, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, task := range a.tasks[projectID] {
		if task.ID == taskID {
			return task, true
		}
	}
	return Task{}, false
}

func (a *app) updateTask(projectID string, task Task) {
	a.mu.Lock()
	defer a.mu.Unlock()
	list := a.tasks[projectID]
	for i := range list {
		if list[i].ID == task.ID {
			list[i] = task
			a.tasks[projectID] = list
			return
		}
	}
	a.tasks[projectID] = append([]Task{task}, list...)
}

func (a *app) recoverInterruptedTasks() error {
	now := time.Now().UTC()
	var interrupted []Task
	projectsByID := map[string]Project{}
	if projects, err := a.scanProjects(); err == nil {
		for _, project := range projects {
			projectsByID[project.ID] = project
		}
	}
	a.mu.Lock()
	for projectID, list := range a.tasks {
		for i := range list {
			if !taskStatusIsLive(list[i].Status) {
				continue
			}
			list[i].Status = "failed"
			list[i].FailureSummary = "task interrupted because the Karoz server stopped before the executor completed"
			if project, ok := projectsByID[projectID]; ok {
				list[i], _ = a.inspectTaskWorktree(project, list[i])
			}
			list[i].UpdatedAt = now
			interrupted = append(interrupted, list[i])
		}
		a.tasks[projectID] = list
	}
	a.mu.Unlock()
	if len(interrupted) == 0 {
		return nil
	}
	if err := a.saveTasks(); err != nil {
		return err
	}
	for _, task := range interrupted {
		a.appendTaskLog(task.ProjectID, task.ID, task.FailureSummary)
	}
	return nil
}

func taskStatusIsLive(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "verifying", "deploying", "cancelling", "merging":
		return true
	default:
		return false
	}
}
