package main

import (
	"os"
	"strings"
	"time"
)

func (a *app) createTask(project Project, req TaskCreateRequest) Task {
	now := time.Now().UTC()
	typ := normalizeTaskType(req.Type)
	task := Task{
		ID:          taskID(),
		ProjectID:   project.ID,
		Type:        typ,
		Status:      "pending",
		Title:       firstNonEmpty(strings.TrimSpace(req.Title), defaultTaskTitle(typ)),
		Description: strings.TrimSpace(req.Description),
		Goal:        strings.TrimSpace(req.Goal),
		ArtifactIDs: append([]string{}, req.ArtifactIDs...),
		BaseBranch:  project.DefaultBranch,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	a.mu.Lock()
	a.tasks[project.ID] = append([]Task{task}, a.tasks[project.ID]...)
	a.mu.Unlock()
	_ = a.saveTasks()
	a.appendTaskLog(project.ID, task.ID, "task created: "+task.Title)
	a.emitRuntimeStateChanged(RuntimeEvent{
		ID:        randomID(),
		ProjectID: project.ID,
		Kind:      "task_changed",
		EntityID:  task.ID,
		To:        task.Status,
		Reason:    "task_created",
		CreatedAt: time.Now().UTC(),
	})
	return task
}

func (a *app) runTask(project Project, task Task) Task {
	if !taskRunnable(task.Status) {
		a.appendTaskLog(project.ID, task.ID, "run skipped: task status is "+task.Status)
		return task
	}
	task.Status = "running"
	task.FailureSummary = ""
	task.Result = ""
	task.UpdatedAt = time.Now().UTC()
	a.updateTask(project.ID, task)
	a.appendTaskLog(project.ID, task.ID, "task started")

	switch task.Type {
	case "deploy", "deployment":
		task = a.runDeploymentTask(project, task)
	default:
		task = a.runDevelopmentTask(project, task)
	}
	task.UpdatedAt = time.Now().UTC()
	a.updateTask(project.ID, task)
	_ = a.saveTasks()
	a.notifyTaskRuntimeHooks(project, task)
	a.emitRuntimeStateChanged(RuntimeEvent{
		ID:        randomID(),
		ProjectID: project.ID,
		Kind:      "task_changed",
		EntityID:  task.ID,
		From:      "running",
		To:        task.Status,
		Reason:    "task_finished",
		CreatedAt: time.Now().UTC(),
	})
	return task
}

func taskRunnable(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "pending", "failed", "deploy_failed":
		return true
	default:
		return false
	}
}

func (a *app) startTaskAsync(project Project, task Task, source string) {
	if strings.EqualFold(os.Getenv("KAROZ_TASK_AUTO_RUN"), "0") || strings.EqualFold(os.Getenv("KAROZ_TASK_AUTO_RUN"), "false") {
		a.appendTaskLog(project.ID, task.ID, "auto run disabled source="+source)
		return
	}
	a.appendTaskLog(project.ID, task.ID, "task queued for execution source="+source)
	go func() {
		latest, ok := a.findTask(project.ID, task.ID)
		if !ok {
			return
		}
		if !taskRunnable(latest.Status) {
			a.appendTaskLog(project.ID, latest.ID, "async run skipped: task status is "+latest.Status)
			return
		}
		a.runTask(project, latest)
	}()
}

func (a *app) runTaskAsync(project Project, task Task, source string) Task {
	if !taskRunnable(task.Status) {
		a.appendTaskLog(project.ID, task.ID, "async run skipped: task status is "+task.Status+" source="+source)
		return task
	}
	task.Status = "running"
	task.FailureSummary = ""
	task.Result = ""
	task.UpdatedAt = time.Now().UTC()
	a.updateTask(project.ID, task)
	_ = a.saveTasks()
	a.appendTaskLog(project.ID, task.ID, "task queued for execution source="+source)
	go func(started Task) {
		a.appendTaskLog(project.ID, started.ID, "task started")
		switch started.Type {
		case "deploy", "deployment":
			started = a.runDeploymentTask(project, started)
		default:
			started = a.runDevelopmentTask(project, started)
		}
		started.UpdatedAt = time.Now().UTC()
		a.updateTask(project.ID, started)
		_ = a.saveTasks()
		a.notifyTaskRuntimeHooks(project, started)
		a.emitRuntimeStateChanged(RuntimeEvent{
			ID:        randomID(),
			ProjectID: project.ID,
			Kind:      "task_changed",
			EntityID:  started.ID,
			From:      "running",
			To:        started.Status,
			Reason:    "task_finished",
			CreatedAt: time.Now().UTC(),
		})
	}(task)
	return task
}
