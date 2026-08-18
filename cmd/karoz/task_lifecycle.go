package main

import (
	"errors"
	executiondomain "github.com/karoz/karoz/internal/execution"
	"math"
	"os"
	"strings"
	"time"

	taskdomain "github.com/karoz/karoz/internal/task"
)

const defaultTaskMaxRuntime = time.Hour

func normalizeTaskMaxRuntimeMS(value *int64) (*int64, error) {
	if value == nil {
		milliseconds := defaultTaskMaxRuntime.Milliseconds()
		return &milliseconds, nil
	}
	if *value < 0 {
		return nil, errors.New("max_runtime_ms must be zero (unlimited) or a positive integer")
	}
	if *value > math.MaxInt64/int64(time.Millisecond) {
		return nil, errors.New("max_runtime_ms is too large")
	}
	milliseconds := *value
	return &milliseconds, nil
}

func normalizeTaskSandboxMode(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "host":
		return "host", nil
	case "required":
		return "required", nil
	default:
		return "", errors.New("sandbox_mode must be host or required")
	}
}

func taskSandboxPolicy(mode string) executiondomain.SandboxPolicy {
	if mode == "required" {
		return executiondomain.SandboxPolicy{Filesystem: true, Network: true, Processes: true}
	}
	return executiondomain.SandboxPolicy{}
}

func (a *app) taskSandboxEnforcer() executiondomain.SandboxEnforcer {
	if a != nil && a.sandboxEnforcer != nil {
		return a.sandboxEnforcer
	}
	return executiondomain.UnsupportedSandboxEnforcer{}
}

func (a *app) createTask(project Project, req TaskCreateRequest) (Task, error) {
	maxRuntimeMS, err := normalizeTaskMaxRuntimeMS(req.MaxRuntimeMS)
	if err != nil {
		return Task{}, err
	}
	sandboxMode, err := normalizeTaskSandboxMode(req.SandboxMode)
	if err != nil {
		return Task{}, err
	}
	now := time.Now().UTC()
	typ := normalizeTaskType(req.Type)
	task := Task{
		ID:           taskID(),
		ProjectID:    project.ID,
		Type:         typ,
		Status:       "pending",
		Title:        firstNonEmpty(strings.TrimSpace(req.Title), defaultTaskTitle(typ)),
		Description:  strings.TrimSpace(req.Description),
		Goal:         strings.TrimSpace(req.Goal),
		MaxRuntimeMS: maxRuntimeMS,
		SandboxMode:  sandboxMode,
		ArtifactIDs:  append([]string{}, req.ArtifactIDs...),
		OwnerAgentID: strings.TrimSpace(req.OwnerAgentID),
		PlanID:       strings.TrimSpace(req.PlanID),
		PlanStepID:   strings.TrimSpace(req.PlanStepID),
		Attempt:      req.Attempt,
		ParentTaskID: strings.TrimSpace(req.ParentTaskID),
		BaseBranch:   project.DefaultBranch,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	a.insertTask(task)
	a.saveOrLog("tasks", a.saveTasks())
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
	return task, nil
}

func (a *app) runTask(project Project, task Task) Task {
	claimed, ctx, finishRun, ok := a.claimTaskRun(project.ID, task.ID)
	if !ok {
		if latest, found := a.findTask(project.ID, task.ID); found {
			a.appendTaskLog(project.ID, task.ID, "run skipped: task status is "+latest.Status)
			return latest
		}
		return task
	}
	task = claimed
	defer finishRun()
	a.saveOrLog("tasks", a.saveTasks())
	a.appendTaskLog(project.ID, task.ID, "task started")
	if err := a.taskSandboxEnforcer().Enforce(taskSandboxPolicy(task.SandboxMode)); err != nil {
		task.Status = "failed"
		task.FailureSummary = "task sandbox unavailable: " + err.Error()
		task.UpdatedAt = time.Now().UTC()
		a.appendTaskLog(project.ID, task.ID, task.FailureSummary)
		a.updateTask(project.ID, task)
		a.saveOrLog("tasks", a.saveTasks())
		a.notifyTaskRuntimeHooks(project, task)
		return task
	}

	switch task.Type {
	case "deploy", "deployment":
		task = a.runDeploymentTask(ctx, project, task)
	default:
		task = a.runDevelopmentTask(ctx, project, task)
	}
	task.UpdatedAt = time.Now().UTC()
	a.updateTask(project.ID, task)
	a.saveOrLog("tasks", a.saveTasks())
	if taskStatusIsTerminal(task.Status) {
		a.notifyTaskRuntimeHooks(project, task)
	}
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
	return taskdomain.IsRunnable(status)
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
	claimed, ctx, finishRun, ok := a.claimTaskRun(project.ID, task.ID)
	if !ok {
		if latest, found := a.findTask(project.ID, task.ID); found {
			a.appendTaskLog(project.ID, task.ID, "async run skipped: task status is "+latest.Status+" source="+source)
			return latest
		}
		return task
	}
	task = claimed
	a.saveOrLog("tasks", a.saveTasks())
	a.appendTaskLog(project.ID, task.ID, "task queued for execution source="+source)
	go func(started Task) {
		defer finishRun()
		a.appendTaskLog(project.ID, started.ID, "task started")
		switch started.Type {
		case "deploy", "deployment":
			started = a.runDeploymentTask(ctx, project, started)
		default:
			started = a.runDevelopmentTask(ctx, project, started)
		}
		started.UpdatedAt = time.Now().UTC()
		a.updateTask(project.ID, started)
		a.saveOrLog("tasks", a.saveTasks())
		if taskStatusIsTerminal(started.Status) {
			a.notifyTaskRuntimeHooks(project, started)
		}
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
