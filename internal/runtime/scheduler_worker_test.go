package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
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
	if !reflect.DeepEqual(executed, []string{"first", "first", "second"}) {
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

func TestSchedulerWorkerWakesOnRunFinishedSignalWithoutPolling(t *testing.T) {
	queue := NewSchedulerQueue()
	job := ScheduledRun{ID: "wake", ProjectID: "p1", AgentID: "designer", Status: ScheduledQueued, MaxAttempts: 1, TimeoutMS: 1000, CreatedAt: time.Now()}
	queue.Enqueue(job)
	wake := make(chan struct{})
	waiting := make(chan struct{})
	executed := make(chan struct{})
	var beginCalls atomic.Int32
	worker := NewSchedulerWorker(queue, SchedulerWorkerHooks{
		Begin: func(ScheduledRun) bool {
			return beginCalls.Add(1) > 1
		},
		WaitForRunFinished: func(ctx context.Context, _ ScheduledRun) bool {
			close(waiting)
			select {
			case <-ctx.Done():
				return false
			case <-wake:
				return true
			}
		},
		Bind: func(ctx context.Context, _ ScheduledRun) (context.Context, bool) { return ctx, true },
		Execute: func(context.Context, ScheduledRun) error {
			close(executed)
			return nil
		},
	})
	go worker.Run(context.Background(), AgentKey(job.ProjectID, job.AgentID))
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("worker did not subscribe to the Run-finished signal")
	}
	time.Sleep(60 * time.Millisecond)
	if calls := beginCalls.Load(); calls != 1 {
		t.Fatalf("Begin was polled while waiting: %d calls", calls)
	}
	close(wake)
	select {
	case <-executed:
	case <-time.After(time.Second):
		t.Fatal("worker did not wake and execute after the signal")
	}
	if calls := beginCalls.Load(); calls != 2 {
		t.Fatalf("Begin calls after wake = %d, want exactly 2", calls)
	}
}

func TestSchedulerWorkerStartDeadlineIncludesBusyWait(t *testing.T) {
	queue := NewSchedulerQueue()
	job := ScheduledRun{ID: "deadline", ProjectID: "p1", AgentID: "designer", Status: ScheduledQueued, MaxAttempts: 1, TimeoutMS: 2_000, StartWaitMS: 45, CreatedAt: time.Now()}
	queue.Enqueue(job)
	worker := NewSchedulerWorker(queue, SchedulerWorkerHooks{
		Begin: func(ScheduledRun) bool { return false },
		WaitForRunFinished: func(ctx context.Context, _ ScheduledRun) bool {
			<-ctx.Done()
			return false
		},
	})
	started := time.Now()
	worker.Run(context.Background(), AgentKey(job.ProjectID, job.AgentID))
	if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
		t.Fatalf("busy wait ignored claimed-job deadline: %s", elapsed)
	}
	stored, ok := queue.Job(job.ID)
	if !ok || stored.Status != ScheduledFailed || !strings.Contains(stored.Error, "start deadline exceeded") {
		t.Fatalf("expired busy job = %+v found=%v", stored, ok)
	}
}

func TestSchedulerWorkerExecutionDeadlineStartsAfterBusyWait(t *testing.T) {
	queue := NewSchedulerQueue()
	job := ScheduledRun{
		ID: "separate-deadlines", ProjectID: "p1", AgentID: "designer", Status: ScheduledQueued, MaxAttempts: 1,
		TimeoutMS: 300, StartWaitMS: 1_000, CreatedAt: time.Now(),
	}
	queue.Enqueue(job)
	enteredWait := make(chan struct{})
	waiting := make(chan struct{})
	executed := make(chan time.Duration, 1)
	worker := NewSchedulerWorker(queue, SchedulerWorkerHooks{
		Begin: func(ScheduledRun) bool {
			select {
			case <-waiting:
				return true
			default:
				return false
			}
		},
		WaitForRunFinished: func(ctx context.Context, _ ScheduledRun) bool {
			select {
			case <-enteredWait:
			default:
				close(enteredWait)
			}
			select {
			case <-waiting:
				return true
			case <-ctx.Done():
				return false
			}
		},
		Bind: func(ctx context.Context, _ ScheduledRun) (context.Context, bool) { return ctx, true },
		Execute: func(ctx context.Context, _ ScheduledRun) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				return errors.New("scheduled execution has no deadline")
			}
			executed <- time.Until(deadline)
			return nil
		},
	})
	go worker.Run(context.Background(), AgentKey(job.ProjectID, job.AgentID))

	// Make the busy wait long enough that the old shared 300ms deadline would
	// leave roughly half the execution window. The new execution deadline is
	// created only after Begin succeeds and therefore remains nearly whole.
	select {
	case <-enteredWait:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not enter busy wait")
	}
	time.Sleep(150 * time.Millisecond)
	close(waiting)
	select {
	case remaining := <-executed:
		if remaining < 240*time.Millisecond {
			t.Fatalf("busy wait consumed execution deadline: remaining=%s", remaining)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not execute after busy wait")
	}
}
