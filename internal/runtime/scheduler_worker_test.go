package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestSchedulerWorkerOrchestratesSerialExecutionAndRetry(t *testing.T) {
	queue := NewSchedulerQueue()
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	jobs := []ScheduledRun{
		{ID: "first", ProjectID: "p1", AgentID: "designer", Status: ScheduledQueued, MaxAttempts: 2, CreatedAt: now},
		{ID: "second", ProjectID: "p1", AgentID: "designer", Status: ScheduledQueued, MaxAttempts: 2, CreatedAt: now.Add(time.Second)},
	}
	queue.Enqueue(jobs[0])
	queue.Enqueue(jobs[1])
	var executed []string
	var completed []string
	worker := NewSchedulerWorker(queue, SchedulerWorkerHooks{
		Begin: func(ScheduledRun) bool { return true },
		Bind:  func(ctx context.Context, _ ScheduledRun) (context.Context, bool) { return ctx, true },
		Execute: func(_ context.Context, job ScheduledRun) error {
			executed = append(executed, job.ID)
			if job.ID == "first" && len(executed) == 1 {
				return errors.New("temporary")
			}
			return nil
		},
		Completed: func(result CompletionResult) { completed = append(completed, result.Job.ID) },
	})
	worker.now = func() time.Time { now = now.Add(time.Second); return now }
	worker.Run(context.Background(), AgentKey("p1", "designer"))
	if !reflect.DeepEqual(executed, []string{"first", "second", "first"}) {
		t.Fatalf("execution order = %+v", executed)
	}
	if !reflect.DeepEqual(completed, executed) {
		t.Fatalf("completion order = %+v", completed)
	}
	if len(queue.Jobs()) != 0 || queue.WorkerActive(AgentKey("p1", "designer")) {
		t.Fatalf("queue not drained: %+v", queue.Jobs())
	}
}

func TestSchedulerWorkerCompletesWhenBindingFails(t *testing.T) {
	queue := NewSchedulerQueue()
	job := ScheduledRun{ID: "job", ProjectID: "p1", AgentID: "designer", Status: ScheduledQueued, MaxAttempts: 1, CreatedAt: time.Now()}
	queue.Enqueue(job)
	worker := NewSchedulerWorker(queue, SchedulerWorkerHooks{
		Begin: func(ScheduledRun) bool { return true },
		Bind:  func(context.Context, ScheduledRun) (context.Context, bool) { return nil, false },
	})
	worker.Run(context.Background(), AgentKey("p1", "designer"))
	failed, ok := queue.Job(job.ID)
	if !ok || failed.Status != ScheduledFailed || failed.Error != "active run disappeared before scheduler context binding" {
		t.Fatalf("failed job = %+v, found=%v", failed, ok)
	}
}
