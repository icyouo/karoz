package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduledRunTimeoutCancelsBlockingExecutor(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	kind := ScheduledRunKind("timeout-blocking-executor")
	job, err := newScheduledRun(kind, AgentRunInput{
		RunID: "scheduled-timeout", ProjectID: "project", AgentID: "agent", Trigger: RunTriggerSystem,
	}, "scheduled/timeout", map[string]string{"test": "value"}, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	job.MaxAttempts = 1
	started := make(chan struct{})
	stopped := make(chan error, 1)
	a.agentRuntimeLocked().schedulerExecutors[kind] = func(ctx context.Context, _ ScheduledRun) error {
		close(started)
		<-ctx.Done()
		stopped <- ctx.Err()
		return ctx.Err()
	}
	if _, scheduled := a.scheduleAgentRun(job); !scheduled {
		t.Fatal("scheduled run was not accepted")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not start the blocking executor")
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocking executor context error=%v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled executor outlived its TimeoutMS deadline")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stored, found := a.ensureSchedulerQueue().Job(job.ID)
		if found && stored.Status == ScheduledRunFailed && !a.scheduledAgentWorkerActive(job.ProjectID, job.AgentID) {
			ledger := a.runLedger(job.ID)
			if ledger == nil {
				t.Fatal("timed-out scheduled Run has no ledger")
			}
			waitForLedgerTerminal(t, ledger)
			if !ledgerHasTerminalType(ledger, "error") || ledgerHasTerminalType(ledger, "done") {
				t.Fatal("timed-out scheduled Run did not publish exactly an error terminal event")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	stored, found := a.ensureSchedulerQueue().Job(job.ID)
	t.Fatalf("timed-out scheduled job=%+v found=%v", stored, found)
}

func TestScheduledBoundContextObservesOwnerAndWorkerParent(t *testing.T) {
	for _, test := range []struct {
		name   string
		cancel func(a *app, parent context.CancelFunc, projectID, agentID string) bool
	}{
		{
			name: "scheduler parent",
			cancel: func(_ *app, parent context.CancelFunc, _, _ string) bool {
				parent()
				return true
			},
		},
		{
			name: "explicit run cancel",
			cancel: func(a *app, _ context.CancelFunc, projectID, agentID string) bool {
				_, accepted := a.cancelAgentRun(projectID, agentID)
				return accepted
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
			projectID, agentID := "project", "agent"
			run, started := a.beginAgentRun(AgentRunInput{RunID: "bound-" + test.name, ProjectID: projectID, AgentID: agentID, Trigger: RunTriggerSystem})
			if !started {
				t.Fatal("could not begin scheduled Run")
			}
			if _, claimed := a.claimAndBindAgentRunWorkerContext(context.Background(), projectID, agentID, run.ID); !claimed {
				t.Fatal("could not claim scheduled Run")
			}
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			bound, ok := a.bindAgentRunContext(parent, projectID, agentID, run.ID)
			if !ok {
				t.Fatal("could not bind scheduled Run context")
			}
			if !test.cancel(a, cancelParent, projectID, agentID) {
				t.Fatal("cancellation was not accepted")
			}
			select {
			case <-bound.Done():
			case <-time.After(time.Second):
				t.Fatal("bound scheduled context did not observe its cancellation parent")
			}
		})
	}
}

func TestScheduledCancelBeforeBindDoesNotExecuteOrRetry(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	kind := ScheduledRunKind("cancel-before-bind")
	job, err := newScheduledRun(kind, AgentRunInput{
		RunID: "scheduled-cancel-before-bind", ProjectID: "project", AgentID: "agent", Trigger: RunTriggerSystem,
	}, "scheduled/cancel-before-bind", map[string]string{"test": "value"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enteredBind := make(chan struct{})
	releaseBind := make(chan struct{})
	a.agentRuntimeLocked().scheduledRunBeforeBindHook = func() {
		close(enteredBind)
		<-releaseBind
	}
	defer func() { a.agentRuntimeLocked().scheduledRunBeforeBindHook = nil }()
	var executions atomic.Int32
	a.agentRuntimeLocked().schedulerExecutors[kind] = func(context.Context, ScheduledRun) error {
		executions.Add(1)
		return nil
	}
	if _, scheduled := a.scheduleAgentRun(job); !scheduled {
		t.Fatal("scheduled run was not accepted")
	}
	select {
	case <-enteredBind:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not reach Bind barrier")
	}
	if _, accepted := a.cancelAgentRun(job.ProjectID, job.AgentID); !accepted {
		t.Fatal("cancel was not accepted before Bind")
	}
	close(releaseBind)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stored, found := a.ensureSchedulerQueue().Job(job.ID)
		if found && stored.Status == ScheduledRunCancelled && !a.scheduledAgentWorkerActive(job.ProjectID, job.AgentID) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	stored, found := a.ensureSchedulerQueue().Job(job.ID)
	if !found || stored.Status != ScheduledRunCancelled {
		t.Fatalf("cancel-before-Bind job=%+v found=%v", stored, found)
	}
	if executions.Load() != 0 {
		t.Fatalf("provider/executor ran %d times after accepted cancellation", executions.Load())
	}
	if stored.Attempt != 0 {
		t.Fatalf("accepted cancellation retried or consumed an attempt: %+v", stored)
	}
	ledger := a.runLedger(job.ID)
	if ledger == nil {
		t.Fatal("missing scheduled run ledger")
	}
	waitForLedgerTerminal(t, ledger)
	if !ledgerHasTerminalType(ledger, "cancelled") || ledgerHasTerminalType(ledger, "done") {
		t.Fatal("cancel-before-Bind did not produce exactly a cancelled terminal ledger")
	}
}

func TestScheduledHandoffTaskPlanCancelAfterProviderBeforeResultCommit(t *testing.T) {
	for _, test := range []struct {
		name   string
		intent string
	}{
		{name: "handoff", intent: "result"},
		{name: "task_event", intent: "task_result"},
		{name: "plan_event", intent: "plan_result"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
			project := Project{ID: "project", Name: "project", Path: t.TempDir(), DefaultBranch: "main"}
			agent := Agent{ID: "agent", ProjectID: project.ID, Name: "Agent", Role: "implementation"}
			a.agentDirectoryLocked().agents[project.ID] = []Agent{agent}
			run, started := a.beginAgentRun(AgentRunInput{
				RunID: "scheduled-" + test.name, ProjectID: project.ID, AgentID: agent.ID,
				Trigger: RunTriggerSystem, TurnType: "ask",
			})
			if !started {
				t.Fatal("could not begin scheduled Run")
			}
			if _, claimed := a.claimAndBindAgentRunWorkerContext(context.Background(), project.ID, agent.ID, run.ID); !claimed {
				t.Fatal("could not atomically claim scheduled Run")
			}
			ledger := a.createRunLedger(run.ID)
			ledger.publish("meta", map[string]any{"run_id": run.ID})
			atCommit := make(chan struct{})
			releaseCommit := make(chan struct{})
			a.agentRuntimeLocked().scheduledRunBeforeResultCommitHook = func() {
				close(atCommit)
				<-releaseCommit
			}
			result := make(chan error, 1)
			go func() {
				result <- a.commitScheduledRunResult(project, agent, run.ID, test.intent, "provider result")
			}()
			select {
			case <-atCommit:
			case <-time.After(2 * time.Second):
				t.Fatal("scheduled result did not reach post-provider barrier")
			}
			if _, accepted := a.cancelAgentRun(project.ID, agent.ID); !accepted {
				t.Fatal("cancel did not win before scheduled result commit")
			}
			close(releaseCommit)
			if err := <-result; err != context.Canceled {
				t.Fatalf("scheduled result commit error=%v, want context.Canceled", err)
			}
			a.agentRuntimeLocked().scheduledRunBeforeResultCommitHook = nil
			a.finishAgentRunWithLedger(project, agent, run.ID, RunStateCancelled, context.Canceled, "Agent run cancelled.")
			waitForLedgerTerminal(t, ledger)
			for _, message := range a.agentMessagesForDisplay(project.ID, agent.ID) {
				if message.Role == "assistant" && message.Intent == test.intent {
					t.Fatalf("cancelled scheduled %s persisted a success result: %+v", test.name, message)
				}
			}
			if !ledgerHasTerminalType(ledger, "cancelled") || ledgerHasTerminalType(ledger, "done") {
				t.Fatalf("scheduled %s terminal events do not reflect cancellation", test.name)
			}
		})
	}
}
