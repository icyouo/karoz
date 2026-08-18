package main

import (
	"strings"
	"time"
)

func (a *app) tasksForProject(projectID string) []Task {
	return a.ensureTaskService().List(projectID)
}

func (a *app) findTask(projectID, taskID string) (Task, bool) {
	return a.ensureTaskService().Find(projectID, taskID)
}

func (a *app) updateTask(projectID string, task Task) {
	if !a.ensureTaskService().Update(task) {
		a.ensureTaskService().Insert(task)
	}
}

func (a *app) insertTask(task Task) {
	a.ensureTaskService().Insert(task)
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
	state := a.projectTasksLocked()
	for projectID, list := range state.tasks {
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
		state.tasks[projectID] = list
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
