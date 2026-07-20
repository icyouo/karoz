package main

import (
	"context"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	"path/filepath"
	"testing"
	"time"
)

func newSchedulerTestApp(dataDir string) *app {
	return &app{
		settings:           Settings{DataDir: dataDir},
		agentRuns:          map[string]AgentRun{},
		agentRunCancels:    map[string]context.CancelFunc{},
		schedulerQueue:     runtimedomain.NewSchedulerQueue(),
		schedulerExecutors: map[ScheduledRunKind]ScheduledRunExecutor{},
		runtimeHooks:       map[string]bool{},
		runtimeWatchers:    map[string]map[chan RuntimeEvent]bool{},
		tasks:              map[string][]Task{},
		inbox:              map[string][]AgentInboxMessage{},
		blackboard:         map[string][]AgentBlackboardEntry{},
	}
}

func writeScheduledRunSnapshot(t *testing.T, dataDir string, jobs []ScheduledRun) {
	t.Helper()
	if err := writeJSONFileAtomic(filepath.Join(dataDir, "agent-run-queue.json"), scheduledRunSnapshot{Jobs: jobs}, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledRunsRecoverQueueOrderAndInterruptedAttempt(t *testing.T) {
	dataDir := t.TempDir()
	base := time.Now().UTC().Add(-time.Minute)
	queued := ScheduledRun{
		ID: "queued", ProjectID: "p1", AgentID: "designer", Kind: ScheduledRunKind("test"),
		Trigger: RunTriggerHandoff, DedupKey: "dedup/queued", Status: ScheduledRunQueued,
		MaxAttempts: 3, TimeoutMS: 1000, CreatedAt: base, UpdatedAt: base,
	}
	running := ScheduledRun{
		ID: "running", ProjectID: "p1", AgentID: "designer", Kind: ScheduledRunKind("test"),
		Trigger: RunTriggerTaskEvent, DedupKey: "dedup/running", Status: ScheduledRunRunning,
		MaxAttempts: 3, TimeoutMS: 1000, CreatedAt: base.Add(time.Second), UpdatedAt: base.Add(time.Second),
	}
	cancelled := ScheduledRun{
		ID: "cancelled", ProjectID: "p1", AgentID: "designer", Kind: ScheduledRunKind("test"),
		Status: ScheduledRunCancelled, MaxAttempts: 3, CreatedAt: base.Add(2 * time.Second), UpdatedAt: base.Add(2 * time.Second),
	}
	failed := ScheduledRun{
		ID: "failed", ProjectID: "p1", AgentID: "designer", Kind: ScheduledRunKind("test"),
		Status: ScheduledRunFailed, Attempt: 3, MaxAttempts: 3, CreatedAt: base.Add(3 * time.Second), UpdatedAt: base.Add(3 * time.Second),
	}

	writeScheduledRunSnapshot(t, dataDir, []ScheduledRun{queued, running, cancelled, failed})

	after := newSchedulerTestApp(dataDir)
	if err := after.loadScheduledRuns(); err != nil {
		t.Fatal(err)
	}
	queue := after.schedulerQueue.QueueIDs(projectAgentKey("p1", "designer"))
	if len(queue) != 2 || queue[0] != queued.ID || queue[1] != running.ID {
		t.Fatalf("recovered queue = %#v", queue)
	}
	recoveredRunning, _ := after.schedulerQueue.Job(running.ID)
	if recoveredRunning.Status != ScheduledRunQueued || recoveredRunning.Attempt != 1 || recoveredRunning.StartedAt != nil {
		t.Fatalf("recovered running job = %+v", recoveredRunning)
	}
	if _, ok := after.schedulerQueue.Job(cancelled.ID); ok {
		t.Fatal("cancelled job was recovered")
	}
	if recoveredFailed, _ := after.schedulerQueue.Job(failed.ID); recoveredFailed.Status != ScheduledRunFailed {
		t.Fatalf("failed job = %+v", recoveredFailed)
	}
	if !after.schedulerQueue.HasDedup(queued.DedupKey) || !after.schedulerQueue.HasDedup(running.DedupKey) {
		t.Fatal("dedup index was not rebuilt")
	}

	duplicate := queued
	duplicate.ID = "duplicate"
	if _, scheduled := after.scheduleAgentRun(duplicate); scheduled {
		t.Fatal("duplicate recovered job was scheduled")
	}
}

func TestScheduledRunRecoveryStopsAtMaxAttempts(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	job := ScheduledRun{
		ID: "exhausted", ProjectID: "p1", AgentID: "designer", Kind: ScheduledRunKind("test"),
		Status: ScheduledRunRunning, Attempt: 2, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now,
	}
	writeScheduledRunSnapshot(t, dataDir, []ScheduledRun{job})

	after := newSchedulerTestApp(dataDir)
	if err := after.loadScheduledRuns(); err != nil {
		t.Fatal(err)
	}
	recovered, _ := after.schedulerQueue.Job(job.ID)
	if recovered.Status != ScheduledRunFailed || recovered.Attempt != 3 {
		t.Fatalf("recovered exhausted job = %+v", recovered)
	}
	if queued := after.scheduledAgentRunCount("p1", "designer"); queued != 0 {
		t.Fatalf("exhausted job was queued: %d", queued)
	}
}

func TestResumeScheduledRunsExecutesRecoveredJob(t *testing.T) {
	dataDir := t.TempDir()
	kind := ScheduledRunKind("test")
	job, err := newScheduledRun(kind, AgentRunInput{
		RunID: "recover-me", ProjectID: "p1", AgentID: "designer", Trigger: RunTriggerSystem,
	}, "dedup/recover-me", map[string]string{"value": "persisted"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	writeScheduledRunSnapshot(t, dataDir, []ScheduledRun{job})

	after := newSchedulerTestApp(dataDir)
	if err := after.loadScheduledRuns(); err != nil {
		t.Fatal(err)
	}
	executed := make(chan ScheduledRun, 1)
	after.schedulerExecutors[kind] = func(_ context.Context, recovered ScheduledRun) error {
		executed <- recovered
		return nil
	}
	after.resumeScheduledRuns()
	select {
	case recovered := <-executed:
		if recovered.ID != job.ID || recovered.Trigger != RunTriggerSystem {
			t.Fatalf("executed recovered job = %+v", recovered)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recovered job did not execute")
	}
	deadline := time.Now().Add(2 * time.Second)
	for after.agentRunActive("p1", "designer") || after.scheduledAgentRunCount("p1", "designer") > 0 || after.scheduledAgentWorkerActive("p1", "designer") {
		if time.Now().After(deadline) {
			t.Fatal("recovered scheduler did not become idle")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestScheduledHandoffRecoveryReturnsWorkingInboxToDelivered(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	job, err := newScheduledRun(
		ScheduledRunHandoff,
		AgentRunInput{RunID: "handoff-run", ProjectID: "p1", AgentID: "designer", Trigger: RunTriggerHandoff, MessageID: "inbox-1"},
		"handoff/p1/designer/inbox-1",
		HandoffRunPayload{InboxMessageID: "inbox-1"},
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	job.Status = ScheduledRunRunning
	workingAt := now
	inbox := AgentInboxMessage{
		ID: "inbox-1", ProjectID: "p1", SourceAgentID: "product", TargetAgentID: "designer",
		CorrelationID: "corr-1", MessageType: "handoff", Subject: "Design", Body: "Create it",
		Objective: "Create design", ExpectedOutput: "Mockup", Status: HandoffWorking,
		CreatedAt: now, UpdatedAt: now, WorkingAt: &workingAt,
	}
	before := newSchedulerTestApp(dataDir)
	before.inbox[projectAgentKey("p1", "designer")] = []AgentInboxMessage{inbox}
	if err := before.saveInbox(); err != nil {
		t.Fatal(err)
	}
	writeScheduledRunSnapshot(t, dataDir, []ScheduledRun{job})

	after := newSchedulerTestApp(dataDir)
	if err := after.loadInbox(); err != nil {
		t.Fatal(err)
	}
	if err := after.loadScheduledRuns(); err != nil {
		t.Fatal(err)
	}
	recovered, ok := after.inboxMessage("p1", "designer", "inbox-1")
	if !ok || recovered.Status != HandoffDelivered {
		t.Fatalf("recovered handoff = %+v ok=%v", recovered, ok)
	}
	if recoveredJob, _ := after.schedulerQueue.Job(job.ID); recoveredJob.Status != ScheduledRunQueued || recoveredJob.Attempt != 1 {
		t.Fatalf("recovered scheduled run = %+v", recoveredJob)
	}
}
