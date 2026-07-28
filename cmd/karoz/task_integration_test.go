package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTaskIntegrationCleanBaseCreatesMergeCommit(t *testing.T) {
	a, project, task := newIntegrationFixture(t, "task-clean", "task.txt", "task change")
	updated := a.integrateTask(project, task, true)
	if updated.Status != "done" || updated.MergedAt == nil {
		t.Fatalf("merge result = %+v", updated)
	}
	parents := strings.Fields(gitTest(t, project.Path, "show", "-s", "--format=%P", "HEAD"))
	if len(parents) != 2 {
		t.Fatalf("expected one merge commit, parents=%v", parents)
	}
	if got := gitTest(t, project.Path, "show", "HEAD:task.txt"); strings.TrimSpace(got) != "task change" {
		t.Fatalf("merged file = %q", got)
	}
	if updated.FailureSummary != "" {
		t.Fatalf("successful merge retained failure summary: %q", updated.FailureSummary)
	}
}

func TestTaskIntegrationMergesRecordedCommitWhenBranchMoves(t *testing.T) {
	a, project, task := newIntegrationFixture(t, "task-immutable-commit", "task.txt", "recorded commit")
	gitTest(t, project.Path, "branch", "-f", task.TaskBranch, task.BaseCommit)
	updated := a.integrateTask(project, task, true)
	if updated.Status != "done" {
		t.Fatalf("merge result = %+v", updated)
	}
	if got := strings.TrimSpace(gitTest(t, project.Path, "show", "HEAD:task.txt")); got != "recorded commit" {
		t.Fatalf("mutable branch was merged instead of recorded commit: %q", got)
	}
	if current := strings.TrimSpace(gitTest(t, project.Path, "rev-parse", task.TaskBranch)); current != task.BaseCommit {
		t.Fatalf("test setup did not move branch: got=%s base=%s", current, task.BaseCommit)
	}
}

func TestTaskIntegrationBlocksPrimaryCheckoutChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, repo string)
	}{
		{"dirty tracked", func(t *testing.T, repo string) { writeTestFile(t, filepath.Join(repo, "base.txt"), "dirty") }},
		{"staged only", func(t *testing.T, repo string) {
			writeTestFile(t, filepath.Join(repo, "base.txt"), "staged")
			gitTest(t, repo, "add", "base.txt")
		}},
		{"untracked only", func(t *testing.T, repo string) { writeTestFile(t, filepath.Join(repo, "untracked.txt"), "untracked") }},
		{"deleted tracked", func(t *testing.T, repo string) {
			if err := os.Remove(filepath.Join(repo, "base.txt")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, project, task := newIntegrationFixture(t, "task-"+strings.ReplaceAll(tc.name, " ", "-"), "task.txt", "task change")
			tc.mutate(t, project.Path)
			before := capturePrimary(t, project.Path)
			updated := a.integrateTask(project, task, true)
			if updated.Status != "waiting_merge" || updated.MergeBlockedReason != "workspace_dirty" {
				t.Fatalf("blocked result = %+v", updated)
			}
			if updated.FailureSummary != "" {
				t.Fatalf("waiting_merge must not be a failure: %+v", updated)
			}
			assertPrimaryUnchanged(t, project.Path, before)
		})
	}
}

func TestTaskIntegrationWrongBranchAndRewrittenBaseBlockWithoutCheckout(t *testing.T) {
	t.Run("wrong branch", func(t *testing.T) {
		a, project, task := newIntegrationFixture(t, "task-wrong-branch", "task.txt", "task change")
		gitTest(t, project.Path, "checkout", "-b", "other")
		before := capturePrimary(t, project.Path)
		updated := a.integrateTask(project, task, true)
		if updated.Status != "waiting_merge" || updated.MergeBlockedReason != "branch_mismatch" {
			t.Fatalf("blocked result = %+v", updated)
		}
		assertPrimaryUnchanged(t, project.Path, before)
	})

	t.Run("rewritten base", func(t *testing.T) {
		a, project, task := newIntegrationFixture(t, "task-rewritten", "task.txt", "task change")
		gitTest(t, project.Path, "checkout", "--orphan", "rewritten")
		gitTest(t, project.Path, "rm", "-rf", ".")
		writeTestFile(t, filepath.Join(project.Path, "replacement.txt"), "replacement")
		gitTest(t, project.Path, "add", "replacement.txt")
		gitTest(t, project.Path, "commit", "-m", "replacement base")
		gitTest(t, project.Path, "branch", "-M", "main")
		before := capturePrimary(t, project.Path)
		updated := a.integrateTask(project, task, true)
		if updated.Status != "waiting_merge" || updated.MergeBlockedReason != "base_rewritten" {
			t.Fatalf("blocked result = %+v", updated)
		}
		assertPrimaryUnchanged(t, project.Path, before)
	})
}

func TestTaskIntegrationAdvancedBaseMergesAndConflictRestores(t *testing.T) {
	t.Run("advanced base", func(t *testing.T) {
		a, project, task := newIntegrationFixture(t, "task-advanced", "task.txt", "task change")
		writeTestFile(t, filepath.Join(project.Path, "base-advance.txt"), "advance")
		gitTest(t, project.Path, "add", "base-advance.txt")
		gitTest(t, project.Path, "commit", "-m", "advance base")
		updated := a.integrateTask(project, task, true)
		if updated.Status != "done" {
			t.Fatalf("advanced-base result = %+v", updated)
		}
	})

	t.Run("merge conflict abort restores exact snapshot", func(t *testing.T) {
		a, project, task := newIntegrationFixture(t, "task-conflict", "shared.txt", "task version")
		writeTestFile(t, filepath.Join(project.Path, "shared.txt"), "base version")
		gitTest(t, project.Path, "add", "shared.txt")
		gitTest(t, project.Path, "commit", "-m", "primary conflicting change")
		before := capturePrimary(t, project.Path)
		updated := a.integrateTask(project, task, true)
		if updated.Status != "waiting_merge" || updated.MergeBlockedReason != "merge_conflict" {
			t.Fatalf("conflict result = %+v", updated)
		}
		assertPrimaryUnchanged(t, project.Path, before)
		if got := gitTest(t, project.Path, "diff", "--name-only", "--diff-filter=U"); strings.TrimSpace(got) != "" {
			t.Fatalf("unmerged files remain: %s", got)
		}
	})
}

func TestTaskIntegrationSerializesAndMergeRetryIsIdempotent(t *testing.T) {
	a, project, first := newIntegrationFixture(t, "task-one", "one.txt", "one")
	second := createTaskBranch(t, project, "task-two", "two.txt", "two", first.BaseCommit)
	a.tasks[project.ID] = append(a.tasks[project.ID], second)

	var wg sync.WaitGroup
	for _, task := range []Task{first, second} {
		wg.Add(1)
		go func(task Task) {
			defer wg.Done()
			a.integrateTask(project, task, true)
		}(task)
	}
	wg.Wait()
	if got := strings.TrimSpace(gitTest(t, project.Path, "status", "--porcelain=v1", "--untracked-files=all")); got != "" {
		t.Fatalf("primary checkout dirty after concurrent integration: %s", got)
	}
	if merges := strings.TrimSpace(gitTest(t, project.Path, "rev-list", "--merges", "--count", "main")); merges != "2" {
		t.Fatalf("merge count after serialized integrations = %s", merges)
	}

	// Recreate a waiting task and issue two API retries. The second request waits
	// for the project lock, re-reads done state, and cannot add another merge.
	third := createTaskBranch(t, project, "task-three", "three.txt", "three", gitTest(t, project.Path, "rev-parse", "HEAD"))
	third.Status = "waiting_merge"
	a.updateTask(project.ID, third)
	var responses [2]Task
	wg = sync.WaitGroup{}
	for i := range responses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			a.handleTasks(recorder, httptest.NewRequest(http.MethodPost, "/", nil), project, []string{third.ID, "merge"})
			if recorder.Code != http.StatusOK {
				t.Errorf("merge retry status=%d body=%s", recorder.Code, recorder.Body.String())
				return
			}
			if err := json.NewDecoder(recorder.Body).Decode(&responses[i]); err != nil {
				t.Errorf("decode retry response: %v", err)
			}
		}(i)
	}
	wg.Wait()
	for _, response := range responses {
		if response.Status != "done" {
			t.Fatalf("idempotent retry response = %+v", response)
		}
	}
	if merges := strings.TrimSpace(gitTest(t, project.Path, "rev-list", "--merges", "--count", "main")); merges != "3" {
		t.Fatalf("duplicate retry created merge count %s", merges)
	}
}

func TestRetryTaskMergePublishesTerminalSideEffectsExactlyOnce(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "1")
	a, project, task := newIntegrationFixture(t, "task-retry-effects", "retry.txt", "retry effects")
	task.PlanID = "plan-1"
	task.PlanStepID = "step-1"
	a.updateTask(project.ID, task)
	a.taskHooks[project.ID+"/"+task.ID] = []TaskRuntimeHook{{
		ID: "hook-1", TaskID: task.ID, ProjectID: project.ID, AgentID: "agent-1",
		HookType: "resident_task_completion", Status: "pending",
	}}
	a.plans[project.ID] = []WorkPlan{{
		ID: "plan-1", ProjectID: project.ID, OwnerAgentID: "owner-1", Status: PlanActive,
		Steps: []PlanStep{{
			ID: "step-1", Status: PlanStepRunning,
			TaskAttempts: []PlanTaskAttempt{{TaskID: task.ID, Status: "running"}},
		}},
	}}

	blocker := ScheduledRun{
		ID: "plan-worker-blocker", ProjectID: project.ID, AgentID: "owner-1",
		Kind: ScheduledRunPlanEvent, Status: ScheduledRunQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if enqueued := a.ensureSchedulerQueue().Enqueue(blocker); !enqueued.Accepted || !enqueued.StartWorker {
		t.Fatalf("failed to reserve plan scheduler worker: %+v", enqueued)
	}
	runtimeEvents := make(chan RuntimeEvent, 64)
	a.addRuntimeWatcher(project.ID, runtimeEvents)
	defer a.removeRuntimeWatcher(project.ID, runtimeEvents)

	updated, err := a.retryTaskMerge(project, task)
	if err != nil {
		t.Fatalf("retry merge failed: %v", err)
	}
	if updated.Status != "done" || updated.MergedAt == nil {
		t.Fatalf("retry merge result = %+v", updated)
	}
	duplicate, err := a.retryTaskMerge(project, updated)
	if err != nil || duplicate.Status != "done" {
		t.Fatalf("duplicate retry result = %+v err=%v", duplicate, err)
	}

	hooks := a.taskHooks[project.ID+"/"+task.ID]
	if len(hooks) != 1 || hooks[0].Status != "delivered" || hooks[0].DeliveredAt == nil {
		t.Fatalf("completion hooks = %+v", hooks)
	}
	var hookMessages int
	for _, message := range a.agentMessagesFor(project.ID, "agent-1") {
		if message.Intent == "task_hook" {
			hookMessages++
		}
	}
	if hookMessages != 1 {
		t.Fatalf("task hook delivery messages = %d, want 1", hookMessages)
	}
	plan, ok := a.planByID(project.ID, "plan-1")
	if !ok {
		t.Fatal("plan disappeared after retry merge")
	}
	step := plan.Steps[0]
	if step.Status != PlanStepAwaitingDecision {
		t.Fatalf("plan step status = %s, want %s", step.Status, PlanStepAwaitingDecision)
	}
	if len(step.TaskAttempts) != 1 || step.TaskAttempts[0].Status != "done" {
		t.Fatalf("plan task attempts = %+v", step.TaskAttempts)
	}

	var planJobs []ScheduledRun
	for _, job := range a.ensureSchedulerQueue().Jobs() {
		if job.Kind == ScheduledRunPlanEvent && job.SourceID == task.PlanID {
			planJobs = append(planJobs, job)
		}
	}
	if len(planJobs) != 1 {
		t.Fatalf("task-terminal plan jobs = %+v all_jobs=%+v", planJobs, a.ensureSchedulerQueue().Jobs())
	}
	scheduled := planJobs[0]
	if scheduled.Status != ScheduledRunQueued && scheduled.Status != ScheduledRunRunning {
		t.Fatalf("task-terminal plan job status = %s", scheduled.Status)
	}
	if scheduled.AgentID != "owner-1" {
		t.Fatalf("task-terminal plan job agent = %s", scheduled.AgentID)
	}
	var payload PlanEventRunPayload
	if err := json.Unmarshal(scheduled.Payload, &payload); err != nil {
		t.Fatalf("decode plan event payload: %v", err)
	}
	if payload.Event != "task_terminal" || payload.TaskID != task.ID || payload.StepID != task.PlanStepID {
		t.Fatalf("plan event payload = %+v", payload)
	}
	for _, job := range a.ensureSchedulerQueue().Jobs() {
		if job.Kind == ScheduledRunPlanEvent && job.SourceID == task.PlanID && job.ID != scheduled.ID {
			t.Fatalf("duplicate retry scheduled another plan event: %+v", job)
		}
	}

	taskChanged := 0
	timer := time.NewTimer(150 * time.Millisecond)
	defer timer.Stop()
collectEvents:
	for {
		select {
		case event := <-runtimeEvents:
			if event.Kind == "task_changed" && event.EntityID == task.ID {
				taskChanged++
				if event.From != "waiting_merge" || event.To != "done" || event.Reason != "task_merge_retried" {
					t.Fatalf("retry task event = %+v", event)
				}
			}
		case <-timer.C:
			break collectEvents
		}
	}
	if taskChanged != 1 {
		t.Fatalf("retry task_changed events = %d, want 1", taskChanged)
	}
}

func TestRetryTaskMergeDoesNotRepeatTerminalPlanMutation(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	project := Project{ID: "p1", Name: "project"}
	task := Task{ID: "t1", ProjectID: project.ID, Status: "done", PlanID: "plan-1", PlanStepID: "step-1"}
	a.plans[project.ID] = []WorkPlan{{
		ID: "plan-1", ProjectID: project.ID, Status: PlanActive,
		Steps: []PlanStep{{
			ID: "step-1", Status: PlanStepAwaitingDecision, Version: 2,
			TaskAttempts: []PlanTaskAttempt{{TaskID: task.ID, Status: "done"}},
		}},
	}}
	before, _ := a.planByID(project.ID, task.PlanID)
	a.notifyTaskRuntimeHooks(project, task)
	after, _ := a.planByID(project.ID, task.PlanID)
	if after.Version != before.Version || after.Steps[0].Version != before.Steps[0].Version {
		t.Fatalf("repeated terminal notification mutated plan\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestTaskIntegrationBusyWaitsWithoutTouchingPrimary(t *testing.T) {
	a, project, task := newIntegrationFixture(t, "task-busy", "task.txt", "task change")
	lock := a.projectIntegrationLock(project.ID)
	lock.Lock()
	before := capturePrimary(t, project.Path)
	updated := a.integrateTask(project, task, false)
	lock.Unlock()
	if updated.Status != "waiting_merge" || updated.MergeBlockedReason != "integration_busy" {
		t.Fatalf("busy integration result = %+v", updated)
	}
	assertPrimaryUnchanged(t, project.Path, before)
}

func TestTaskIntegrationPostMergeVerificationFailureWaitsAndRecovers(t *testing.T) {
	a, project, task := newIntegrationFixture(t, "task-post-merge-hook", "task.txt", "task change")
	task.PlanID = "plan-1"
	task.PlanStepID = "step-1"
	a.updateTask(project.ID, task)
	a.taskHooks[project.ID+"/"+task.ID] = []TaskRuntimeHook{{ID: "hook-1", TaskID: task.ID, ProjectID: project.ID, AgentID: "agent-1", HookType: "resident_task_completion", Status: "pending"}}
	a.plans[project.ID] = []WorkPlan{{ID: "plan-1", ProjectID: project.ID, Status: PlanActive, Steps: []PlanStep{{ID: "step-1", Status: PlanStepRunning, TaskAttempts: []PlanTaskAttempt{{TaskID: task.ID, Status: "running"}}}}}}

	hookPath := filepath.Join(project.Path, ".git", "hooks", "post-merge")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nprintf dirty > post-merge-dirty\n"), 0755); err != nil {
		t.Fatal(err)
	}
	updated := a.integrateTask(project, task, true)
	if updated.Status != "waiting_merge" || updated.MergeBlockedReason != "integration_failed" {
		t.Fatalf("post-merge verification result = %+v", updated)
	}
	if updated.FailureSummary != "" || updated.Result != "" || updated.MergedAt != nil {
		t.Fatalf("post-merge verification falsely reported terminal state: %+v", updated)
	}
	if merged, err := gitIsAncestor(project.Path, task.CommitSHA, strings.TrimSpace(gitTest(t, project.Path, "rev-parse", "HEAD"))); err != nil || !merged {
		t.Fatalf("recorded task commit was not retained after post-merge verification failure: merged=%t err=%v", merged, err)
	}
	if status := gitTest(t, project.Path, "status", "--porcelain=v1", "--untracked-files=all"); !strings.Contains(status, "post-merge-dirty") {
		t.Fatalf("post-merge hook did not dirty primary checkout: %q", status)
	}
	if evidence, err := os.ReadFile(filepath.Join(project.Path, "post-merge-dirty")); err != nil || string(evidence) != "dirty" {
		t.Fatalf("post-merge recovery evidence was not preserved: content=%q err=%v", evidence, err)
	}
	if merges := strings.TrimSpace(gitTest(t, project.Path, "rev-list", "--merges", "--count", "main")); merges != "1" {
		t.Fatalf("merge count after failed verification = %s", merges)
	}

	a.notifyTaskRuntimeHooks(project, updated)
	if got := a.taskHooks[project.ID+"/"+task.ID][0].Status; got != "pending" {
		t.Fatalf("waiting_merge delivered hook status=%s", got)
	}
	plan, _ := a.planByID(project.ID, "plan-1")
	if got := plan.Steps[0].Status; got != PlanStepRunning {
		t.Fatalf("waiting_merge advanced plan step to %s", got)
	}

	if err := os.Remove(filepath.Join(project.Path, "post-merge-dirty")); err != nil {
		t.Fatal(err)
	}
	retried, err := a.retryTaskMerge(project, updated)
	if err != nil {
		t.Fatalf("clean retry failed: %v", err)
	}
	if retried.Status != "done" || retried.MergedAt == nil || retried.MergeAttempts != 1 {
		t.Fatalf("clean retry result = %+v", retried)
	}
	if merges := strings.TrimSpace(gitTest(t, project.Path, "rev-list", "--merges", "--count", "main")); merges != "1" {
		t.Fatalf("clean retry created duplicate merge count=%s", merges)
	}
}

func TestWaitingMergeDoesNotDeliverHooksOrAdvancePlan(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	project := Project{ID: "p1", Name: "project"}
	now := time.Now().UTC()
	task := Task{ID: "t1", ProjectID: project.ID, Status: "waiting_merge", PlanID: "plan-1", PlanStepID: "step-1", UpdatedAt: now}
	a.tasks[project.ID] = []Task{task}
	a.taskHooks[project.ID+"/"+task.ID] = []TaskRuntimeHook{{ID: "hook-1", TaskID: task.ID, ProjectID: project.ID, AgentID: "agent-1", HookType: "resident_task_completion", Status: "pending"}}
	a.plans[project.ID] = []WorkPlan{{ID: "plan-1", ProjectID: project.ID, Status: PlanActive, Steps: []PlanStep{{ID: "step-1", Status: PlanStepRunning, TaskAttempts: []PlanTaskAttempt{{TaskID: task.ID, Status: "running"}}}}}}
	a.notifyTaskRuntimeHooks(project, task)
	if got := a.taskHooks[project.ID+"/"+task.ID][0].Status; got != "pending" {
		t.Fatalf("waiting merge delivered hook status=%s", got)
	}
	plan, _ := a.planByID(project.ID, "plan-1")
	if got := plan.Steps[0].Status; got != PlanStepRunning {
		t.Fatalf("waiting merge advanced plan step to %s", got)
	}
}

func TestPrepareDevelopmentTaskRefusesRepositoryWithoutHEAD(t *testing.T) {
	repo := t.TempDir()
	gitTest(t, repo, "init", "-b", "main")
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	project := Project{ID: "empty", Path: repo, DefaultBranch: "main"}
	_, err := a.prepareDevelopmentTask(context.Background(), project, Task{ID: "task-empty", ProjectID: project.ID})
	if err == nil || !strings.Contains(err.Error(), "no initial commit") {
		t.Fatalf("empty repository error = %v", err)
	}
	if count := strings.TrimSpace(gitTest(t, repo, "rev-list", "--all", "--count")); count != "0" {
		t.Fatalf("Karoz unexpectedly created a base commit, count=%s", count)
	}
}

func newIntegrationFixture(t *testing.T, taskID, filename, contents string) (*app, Project, Task) {
	t.Helper()
	repo := t.TempDir()
	gitTest(t, repo, "init", "-b", "main")
	gitTest(t, repo, "config", "user.name", "Karoz Test")
	gitTest(t, repo, "config", "user.email", "karoz-test@example.invalid")
	writeTestFile(t, filepath.Join(repo, "base.txt"), "base")
	gitTest(t, repo, "add", "base.txt")
	gitTest(t, repo, "commit", "-m", "base")
	base := strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD"))
	project := projectFromPath(repo, repo, "main")
	project.Name = "test"
	task := createTaskBranch(t, project, taskID, filename, contents, base)
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: repo})
	a.tasks[project.ID] = []Task{task}
	return a, project, task
}

func createTaskBranch(t *testing.T, project Project, taskID, filename, contents, base string) Task {
	t.Helper()
	branch := "karoz/task-" + taskID
	gitTest(t, project.Path, "checkout", "-b", branch, strings.TrimSpace(base))
	writeTestFile(t, filepath.Join(project.Path, filename), contents)
	gitTest(t, project.Path, "add", filename)
	gitTest(t, project.Path, "commit", "-m", "task change")
	sha := strings.TrimSpace(gitTest(t, project.Path, "rev-parse", "HEAD"))
	gitTest(t, project.Path, "checkout", "main")
	return Task{ID: taskID, ProjectID: project.ID, Type: "feature", Status: "waiting_merge", Title: taskID, BaseBranch: "main", BaseCommit: strings.TrimSpace(base), TaskBranch: branch, CommitSHA: sha, UpdatedAt: time.Now().UTC()}
}

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := run(dir, "git", args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func capturePrimary(t *testing.T, repo string) primarySnapshot {
	t.Helper()
	return primarySnapshot{
		head:   strings.TrimSpace(gitTest(t, repo, "rev-parse", "HEAD")),
		branch: strings.TrimSpace(gitTest(t, repo, "branch", "--show-current")),
		status: strings.TrimSpace(gitTest(t, repo, "status", "--porcelain=v1", "--untracked-files=all")),
	}
}

func assertPrimaryUnchanged(t *testing.T, repo string, before primarySnapshot) {
	t.Helper()
	after := capturePrimary(t, repo)
	if after != before {
		t.Fatalf("primary checkout changed\nbefore=%+v\nafter=%+v", before, after)
	}
}
