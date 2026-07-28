package main

import (
	"context"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScheduledRunWakesWhenTheActiveRunFinishes(t *testing.T) {
	a, project := newHandlerTestApp(t)
	active, started := a.beginAgentRun(AgentRunInput{RunID: "active-run", ProjectID: project.ID, AgentID: "worker-a", Trigger: RunTriggerUserDirect})
	if !started {
		t.Fatal("active Run did not start")
	}
	const kind ScheduledRunKind = "wake-test"
	executed := make(chan struct{}, 1)
	a.schedulerExecutors[kind] = func(context.Context, ScheduledRun) error {
		executed <- struct{}{}
		return nil
	}
	job, err := newScheduledRun(kind, AgentRunInput{RunID: "scheduled-wake", ProjectID: project.ID, AgentID: "worker-a", Trigger: RunTriggerSystem}, "wake-test", map[string]string{"reason": "test"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, accepted := a.scheduleAgentRun(job); !accepted {
		t.Fatal("scheduled Run was not accepted")
	}

	watcherDeadline := time.Now().Add(time.Second)
	key := projectAgentKey(project.ID, "worker-a")
	for {
		a.mu.Lock()
		waiting := len(a.agentRunFinishedWatchers[key]) > 0
		a.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(watcherDeadline) {
			t.Fatal("scheduled worker did not subscribe to active Run completion")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, finished := a.finishAgentRun(project.ID, "worker-a", active.ID, RunStateDone, nil); !finished {
		t.Fatal("active Run did not finish")
	}
	select {
	case <-executed:
	case <-time.After(time.Second):
		t.Fatal("scheduled Run did not wake after the active Run finished")
	}
	quiescentDeadline := time.Now().Add(time.Second)
	for a.agentRunActive(project.ID, "worker-a") || a.scheduledAgentRunCount(project.ID, "worker-a") > 0 || a.scheduledAgentWorkerActive(project.ID, "worker-a") {
		if time.Now().After(quiescentDeadline) {
			t.Fatal("scheduled worker did not become quiescent after wake")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestScheduledPlanAndDevBusyWaitRetainExecutionBudgetAndFinalReserve(t *testing.T) {
	for _, test := range []struct {
		name         string
		turnType     string
		total        time.Duration
		tool         time.Duration
		finalReserve time.Duration
		busyWait     time.Duration
	}{
		{name: "plan", turnType: "plan", total: 600 * time.Millisecond, tool: 50 * time.Millisecond, finalReserve: 300 * time.Millisecond, busyWait: 400 * time.Millisecond},
		{name: "dev", turnType: "dev", total: 700 * time.Millisecond, tool: 60 * time.Millisecond, finalReserve: 350 * time.Millisecond, busyWait: 470 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			prefix := "KAROZ_RESIDENT_" + strings.ToUpper(test.turnType) + "_"
			t.Setenv(prefix+"TOTAL_TIMEOUT", test.total.String())
			t.Setenv(prefix+"TOOL_TIMEOUT", test.tool.String())
			t.Setenv(prefix+"FINAL_RESERVE", test.finalReserve.String())
			budget := residentTurnBudgetFor(test.turnType)

			a, project := newHandlerTestApp(t)
			active, started := a.beginAgentRun(AgentRunInput{RunID: "busy-" + test.name, ProjectID: project.ID, AgentID: "worker-a", Trigger: RunTriggerUserDirect})
			if !started {
				t.Fatal("could not create busy Run")
			}
			kind := ScheduledRunKind("budget-after-wait-" + test.name)
			type observation struct {
				executionRemaining time.Duration
				finalRemaining     time.Duration
				err                error
			}
			observed := make(chan observation, 1)
			a.schedulerExecutors[kind] = func(ctx context.Context, _ ScheduledRun) error {
				deadline, ok := ctx.Deadline()
				if !ok {
					err := context.DeadlineExceeded
					observed <- observation{err: err}
					return err
				}
				wire := &budgetTestWire{finalRemaining: make(chan time.Duration, 1)}
				err := invokeResidentToolLoop(ctx, wire, nil, AgentStreamCallbacks{}, budget, func(toolCtx context.Context, _ codexToolCall) (string, error) {
					<-toolCtx.Done()
					return "", toolCtx.Err()
				})
				result := observation{executionRemaining: time.Until(deadline), err: err}
				select {
				case result.finalRemaining = <-wire.finalRemaining:
				default:
				}
				observed <- result
				return err
			}

			job, err := newScheduledRun(kind, AgentRunInput{
				RunID: "scheduled-" + test.name, ProjectID: project.ID, AgentID: "worker-a", Trigger: RunTriggerSystem, TurnType: test.turnType,
			}, "budget-after-wait/"+test.name, map[string]string{"test": test.name}, 0)
			if err != nil {
				t.Fatal(err)
			}
			job.MaxAttempts = 1
			if got := time.Duration(job.TimeoutMS) * time.Millisecond; got != budget.TotalDuration {
				t.Fatalf("scheduled %s execution timeout = %s, want selected budget %s", test.turnType, got, budget.TotalDuration)
			}
			if got := time.Duration(job.StartWaitMS) * time.Millisecond; got != defaultScheduledRunStartWait {
				t.Fatalf("scheduled %s start wait = %s, want %s", test.turnType, got, defaultScheduledRunStartWait)
			}
			if _, scheduled := a.scheduleAgentRun(job); !scheduled {
				t.Fatal("scheduled Run was not accepted")
			}

			key := projectAgentKey(project.ID, "worker-a")
			waitDeadline := time.Now().Add(2 * time.Second)
			for {
				a.mu.Lock()
				waiting := len(a.agentRunFinishedWatchers[key]) > 0
				a.mu.Unlock()
				if waiting {
					break
				}
				if time.Now().After(waitDeadline) {
					t.Fatal("scheduler did not subscribe to the busy Run")
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(test.busyWait)
			if _, finished := a.finishAgentRun(project.ID, "worker-a", active.ID, RunStateDone, nil); !finished {
				t.Fatal("could not finish busy Run")
			}

			select {
			case result := <-observed:
				if result.err != nil {
					t.Fatalf("scheduled %s execution failed after busy wait: %v", test.turnType, result.err)
				}
				if wantMinimum := budget.TotalDuration - 100*time.Millisecond; result.executionRemaining < wantMinimum {
					t.Fatalf("busy wait consumed scheduled %s execution budget: remaining=%s want at least %s", test.turnType, result.executionRemaining, wantMinimum)
				}
				if wantMinimum := budget.FinalResponseReserve - 50*time.Millisecond; result.finalRemaining < wantMinimum {
					t.Fatalf("busy wait consumed scheduled %s final reserve: remaining=%s want at least %s", test.turnType, result.finalRemaining, wantMinimum)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("scheduled %s Run did not execute after busy agent released", test.turnType)
			}
			// The observation is sent from Execute before SchedulerWorker finishes
			// lifecycle persistence and runtime notifications. Keep this test's
			// temporary app alive until that worker has fully drained so cleanup
			// cannot race its durable writes.
			quiescentDeadline := time.Now().Add(2 * time.Second)
			for a.agentRunActive(project.ID, "worker-a") || a.scheduledAgentRunCount(project.ID, "worker-a") > 0 || a.scheduledAgentWorkerActive(project.ID, "worker-a") {
				if time.Now().After(quiescentDeadline) {
					t.Fatalf("scheduled %s worker did not drain after execution", test.turnType)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

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

func TestLoadScheduledRunsUpgradesLegacyDefaultToSelectedExecutionBudget(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()
	jobs := []ScheduledRun{
		{ID: "legacy-plan", ProjectID: "p1", AgentID: "designer", Kind: ScheduledRunKind("test"), Trigger: RunTriggerPlanEvent, TurnType: "plan", Status: ScheduledRunQueued, MaxAttempts: 1, TimeoutMS: (3 * time.Minute).Milliseconds(), CreatedAt: now, UpdatedAt: now},
		{ID: "legacy-dev", ProjectID: "p1", AgentID: "builder", Kind: ScheduledRunKind("test"), Trigger: RunTriggerHandoff, TurnType: "dev", Status: ScheduledRunQueued, MaxAttempts: 1, TimeoutMS: (3 * time.Minute).Milliseconds(), CreatedAt: now.Add(time.Millisecond), UpdatedAt: now.Add(time.Millisecond)},
	}
	writeScheduledRunSnapshot(t, dataDir, jobs)

	after := newSchedulerTestApp(dataDir)
	if err := after.loadScheduledRuns(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id       string
		turnType string
	}{
		{id: "legacy-plan", turnType: "plan"},
		{id: "legacy-dev", turnType: "dev"},
	} {
		stored, found := after.schedulerQueue.Job(test.id)
		if !found {
			t.Fatalf("missing normalized legacy job %s", test.id)
		}
		if got, want := time.Duration(stored.TimeoutMS)*time.Millisecond, residentTurnBudgetFor(test.turnType).TotalDuration; got != want {
			t.Fatalf("legacy %s execution timeout=%s, want selected budget %s", test.turnType, got, want)
		}
		if got := time.Duration(stored.StartWaitMS) * time.Millisecond; got != defaultScheduledRunStartWait {
			t.Fatalf("legacy %s start wait=%s, want %s", test.turnType, got, defaultScheduledRunStartWait)
		}
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
