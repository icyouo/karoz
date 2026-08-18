package task

import (
	"context"
	"testing"
	"time"
)

type memoryRepository struct {
	tasks map[string][]Task
}

func (repository *memoryRepository) List(projectID string) []Task {
	return append([]Task(nil), repository.tasks[projectID]...)
}

func (repository *memoryRepository) Find(projectID, taskID string) (Task, bool) {
	for _, task := range repository.tasks[projectID] {
		if task.ID == taskID {
			return task, true
		}
	}
	return Task{}, false
}

func (repository *memoryRepository) Insert(task Task) {
	repository.tasks[task.ProjectID] = append([]Task{task}, repository.tasks[task.ProjectID]...)
}

func (repository *memoryRepository) Update(task Task) bool {
	for index := range repository.tasks[task.ProjectID] {
		if repository.tasks[task.ProjectID][index].ID == task.ID {
			repository.tasks[task.ProjectID][index] = task
			return true
		}
	}
	return false
}

func (repository *memoryRepository) Mutate(projectID, taskID string, mutate func(*Task) bool) (Task, bool) {
	for index := range repository.tasks[projectID] {
		if repository.tasks[projectID][index].ID != taskID {
			continue
		}
		current := &repository.tasks[projectID][index]
		if !mutate(current) {
			return *current, false
		}
		return *current, true
	}
	return Task{}, false
}

func TestServiceDelegatesTaskStateToRepository(t *testing.T) {
	repository := &memoryRepository{tasks: map[string][]Task{}}
	service := NewService(repository)
	service.Insert(Task{ID: "task-1", ProjectID: "project-1", Status: "pending"})
	if tasks := service.List("project-1"); len(tasks) != 1 || tasks[0].ID != "task-1" {
		t.Fatalf("list = %+v", tasks)
	}
	task, ok := service.Find("project-1", "task-1")
	if !ok || task.Status != "pending" {
		t.Fatalf("find = %+v, ok=%v", task, ok)
	}
	task.Status = "running"
	if !service.Update(task) {
		t.Fatal("update reported missing task")
	}
	if updated, _ := service.Find("project-1", "task-1"); updated.Status != "running" {
		t.Fatalf("updated task = %+v", updated)
	}
	if service.Update(Task{ID: "missing", ProjectID: "project-1"}) {
		t.Fatal("missing update unexpectedly succeeded")
	}
}

func TestServiceClaimCancelAndFinishOwnRuntimeHandles(t *testing.T) {
	finite := int64(15)
	repository := &memoryRepository{tasks: map[string][]Task{
		"project-1": {{ID: "finite", ProjectID: "project-1", Status: "pending", MaxRuntimeMS: &finite}},
	}}
	service := NewService(repository)
	claimed, ctx, finish, ok := service.Claim("project-1", "finite")
	if !ok || claimed.Status != "running" || claimed.StartedAt == nil {
		t.Fatalf("claim = %+v, ok=%v", claimed, ok)
	}
	persisted, found := service.Find("project-1", "finite")
	if !found || persisted.StartedAt == nil || !persisted.StartedAt.Equal(*claimed.StartedAt) {
		t.Fatalf("claim did not persist operation start: %+v", persisted)
	}
	if _, _, _, duplicate := service.Claim("project-1", "finite"); duplicate {
		t.Fatal("duplicate claim unexpectedly succeeded")
	}
	select {
	case <-ctx.Done():
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("finite runtime error = %v", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("finite runtime did not expire")
	}
	finish()

	unlimited := int64(0)
	repository.Insert(Task{ID: "unlimited", ProjectID: "project-1", Status: "pending", MaxRuntimeMS: &unlimited})
	_, unlimitedCtx, unlimitedFinish, ok := service.Claim("project-1", "unlimited")
	if !ok {
		t.Fatal("unlimited claim failed")
	}
	if !service.Cancel("project-1", "unlimited") {
		t.Fatal("cancel did not find active unlimited run")
	}
	if unlimitedCtx.Err() != context.Canceled {
		t.Fatalf("unlimited context error = %v", unlimitedCtx.Err())
	}
	unlimitedFinish()
	if service.Cancel("project-1", "unlimited") {
		t.Fatal("finished run remained cancellable")
	}
}
