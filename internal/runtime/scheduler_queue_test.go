package runtime

import (
	"testing"
	"time"
)

func TestSchedulerQueueSerialDedupAndRetry(t *testing.T) {
	queue := NewSchedulerQueue()
	now := time.Date(2026, 7, 11, 3, 0, 0, 0, time.UTC)
	first := ScheduledRun{ID: "first", ProjectID: "p1", AgentID: "designer", Kind: ScheduledHandoff, Status: ScheduledQueued, DedupKey: "handoff/1", MaxAttempts: 2, CreatedAt: now}
	second := ScheduledRun{ID: "second", ProjectID: "p1", AgentID: "designer", Kind: ScheduledTaskEvent, Status: ScheduledQueued, DedupKey: "task/2", MaxAttempts: 2, CreatedAt: now.Add(time.Second)}
	if result := queue.Enqueue(first); !result.Accepted || !result.StartWorker {
		t.Fatalf("first enqueue = %+v", result)
	}
	if result := queue.Enqueue(second); !result.Accepted || result.StartWorker {
		t.Fatalf("second enqueue = %+v", result)
	}
	duplicate := first
	duplicate.ID = "duplicate"
	if queue.Enqueue(duplicate).Accepted {
		t.Fatal("duplicate dedup key was accepted")
	}
	key := AgentKey("p1", "designer")
	claimed, ok := queue.Claim(key, now.Add(2*time.Second))
	if !ok || claimed.ID != first.ID || claimed.Status != ScheduledRunning {
		t.Fatalf("first claim = %+v ok=%v", claimed, ok)
	}
	failed := queue.Complete(first.ID, CompletionFailed, "temporary", now.Add(3*time.Second))
	if !failed.Requeue || failed.Job.Attempt != 1 || failed.Job.Status != ScheduledQueued {
		t.Fatalf("failed completion = %+v", failed)
	}
	claimed, _ = queue.Claim(key, now.Add(4*time.Second))
	if claimed.ID != second.ID {
		t.Fatalf("retry broke FIFO order, claim=%+v", claimed)
	}
	queue.Complete(second.ID, CompletionSucceeded, "", now.Add(5*time.Second))
	claimed, _ = queue.Claim(key, now.Add(6*time.Second))
	if claimed.ID != first.ID {
		t.Fatalf("requeued job = %+v", claimed)
	}
	exhausted := queue.Complete(first.ID, CompletionFailed, "permanent", now.Add(7*time.Second))
	if exhausted.Requeue || !exhausted.NotifyFailure || exhausted.Job.Status != ScheduledFailed || queue.HasDedup(first.DedupKey) {
		t.Fatalf("exhausted completion = %+v", exhausted)
	}
}

func TestSchedulerQueueRecoveryAndCancellation(t *testing.T) {
	queue := NewSchedulerQueue()
	now := time.Date(2026, 7, 11, 4, 0, 0, 0, time.UTC)
	jobs := []ScheduledRun{
		{ID: "running", ProjectID: "p1", AgentID: "designer", Status: ScheduledRunning, Attempt: 0, MaxAttempts: 3, DedupKey: "running", CreatedAt: now.Add(time.Second)},
		{ID: "queued", ProjectID: "p1", AgentID: "designer", Status: ScheduledQueued, MaxAttempts: 3, DedupKey: "queued", CreatedAt: now},
		{ID: "cancelled", ProjectID: "p1", AgentID: "designer", Status: ScheduledCancelled, MaxAttempts: 3, CreatedAt: now.Add(2 * time.Second)},
	}
	recovery := queue.Recover(jobs, now.Add(3*time.Second))
	if !recovery.Normalized || len(recovery.Jobs) != 2 {
		t.Fatalf("recovery = %+v", recovery)
	}
	ids := queue.QueueIDs(AgentKey("p1", "designer"))
	if len(ids) != 2 || ids[0] != "queued" || ids[1] != "running" {
		t.Fatalf("recovered queue = %+v", ids)
	}
	running, _ := queue.Job("running")
	if running.Attempt != 1 || running.Status != ScheduledQueued || running.StartedAt != nil {
		t.Fatalf("recovered running job = %+v", running)
	}
	cancelled := queue.CancelAgent("p1", "designer", "agent deleted", now.Add(4*time.Second))
	if len(cancelled) != 2 || queue.PendingCount(AgentKey("p1", "designer")) != 0 {
		t.Fatalf("cancelled jobs = %+v", cancelled)
	}
}

func TestSchedulerQueueSuppressesRetryAfterEffectsStart(t *testing.T) {
	queue := NewSchedulerQueue()
	now := time.Date(2026, 7, 18, 4, 0, 0, 0, time.UTC)
	job := ScheduledRun{ID: "effectful", ProjectID: "p1", AgentID: "designer", Kind: ScheduledHandoff, Status: ScheduledQueued, MaxAttempts: 3, CreatedAt: now}
	queue.Enqueue(job)
	queue.Claim(AgentKey(job.ProjectID, job.AgentID), now.Add(time.Second))
	marked, found, changed := queue.MarkEffectsStarted(job.ID, now.Add(2*time.Second))
	if !found || !changed || !marked.EffectsStarted || marked.EffectsStartedAt == nil {
		t.Fatalf("effects marker = %+v found=%v changed=%v", marked, found, changed)
	}
	completed := queue.Complete(job.ID, CompletionFailed, "provider failed", now.Add(3*time.Second))
	if completed.Requeue || !completed.NotifyFailure || completed.Job.Status != ScheduledFailed {
		t.Fatalf("effectful completion = %+v", completed)
	}
	if queue.PendingCount(AgentKey(job.ProjectID, job.AgentID)) != 0 {
		t.Fatal("effectful job was requeued")
	}

	recovered := NewSchedulerQueue().Recover([]ScheduledRun{{
		ID: "interrupted", ProjectID: "p1", AgentID: "designer", Kind: ScheduledHandoff,
		Status: ScheduledRunning, MaxAttempts: 3, EffectsStarted: true, CreatedAt: now,
	}}, now.Add(4*time.Second))
	if len(recovered.Jobs) != 0 || len(recovered.TerminalFailures) != 1 || recovered.TerminalFailures[0].Status != ScheduledFailed {
		t.Fatalf("effectful recovery = %+v", recovered)
	}
}
