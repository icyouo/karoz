package main

import taskdomain "github.com/karoz/karoz/internal/task"

// appTaskRepository is the JSON-backed repository for the task service.
type appTaskRepository struct{ app *app }

func (a *app) ensureTaskService() *taskdomain.Service {
	if a.taskService == nil {
		a.taskService = taskdomain.NewService(appTaskRepository{app: a})
	}
	return a.taskService
}

func (repository appTaskRepository) List(projectID string) []taskdomain.Task {
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	return append([]taskdomain.Task(nil), repository.app.projectTasksLocked().tasks[projectID]...)
}

func (repository appTaskRepository) Find(projectID, taskID string) (taskdomain.Task, bool) {
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	for _, task := range repository.app.projectTasksLocked().tasks[projectID] {
		if task.ID == taskID {
			return task, true
		}
	}
	return taskdomain.Task{}, false
}

func (repository appTaskRepository) Insert(task taskdomain.Task) {
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	state := repository.app.projectTasksLocked()
	state.tasks[task.ProjectID] = append([]taskdomain.Task{task}, state.tasks[task.ProjectID]...)
}

func (repository appTaskRepository) Update(task taskdomain.Task) bool {
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	state := repository.app.projectTasksLocked()
	list := state.tasks[task.ProjectID]
	for index := range list {
		if list[index].ID == task.ID {
			list[index] = task
			state.tasks[task.ProjectID] = list
			return true
		}
	}
	return false
}

func (repository appTaskRepository) Mutate(projectID, taskID string, mutate func(*taskdomain.Task) bool) (taskdomain.Task, bool) {
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	state := repository.app.projectTasksLocked()
	list := state.tasks[projectID]
	for index := range list {
		if list[index].ID != taskID {
			continue
		}
		if !mutate(&list[index]) {
			return list[index], false
		}
		state.tasks[projectID] = list
		return list[index], true
	}
	return taskdomain.Task{}, false
}
