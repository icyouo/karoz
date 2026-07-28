package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTaskCancelStopsExecutorProcessGroup(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			a, project, task := newIntegrationFixture(t, "task-cancel-executor-"+provider, "task.txt", "change")
			task.Status = "pending"
			a.updateTask(project.ID, task)
			bin := t.TempDir()
			pidFile := filepath.Join(t.TempDir(), "executor.pid")
			startsFile := filepath.Join(t.TempDir(), "executor.starts")
			writeExecutable(t, filepath.Join(bin, provider), `#!/bin/sh
while [ "$#" -gt 0 ] && [ "$1" != "-C" ]; do shift; done
shift
sleep 30 &
echo started >> "$TASK_TEST_STARTS"
echo $! > "$TASK_TEST_PID_FILE"
wait
`)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("KAROZ_TASK_PROVIDER", provider)
			t.Setenv("TASK_TEST_PID_FILE", pidFile)
			t.Setenv("TASK_TEST_STARTS", startsFile)

			// Install the winning run/handle first, then race a stale start with cancel.
			a.runTaskAsync(project, task, "winner")
			pid := waitForPID(t, pidFile)
			start := make(chan struct{})
			var interleaving sync.WaitGroup
			var updated Task
			var cancelErr error
			interleaving.Add(2)
			go func() {
				defer interleaving.Done()
				<-start
				a.runTaskAsync(project, task, "stale_duplicate")
			}()
			go func() {
				defer interleaving.Done()
				<-start
				updated, cancelErr = a.cancelTask(project, task)
			}()
			close(start)
			interleaving.Wait()
			if starts, err := os.ReadFile(startsFile); err != nil || strings.Count(strings.TrimSpace(string(starts)), "started") != 1 {
				t.Fatalf("duplicate start launched more than one executor: starts=%q err=%v", starts, err)
			}
			if cancelErr != nil || updated.Status != "cancelling" {
				t.Fatalf("concurrent cancel did not reach owned run task=%+v err=%v", updated, cancelErr)
			}
			// Repeated cancellation is intentionally idempotent while the process exits.
			if repeated, err := a.cancelTask(project, task); err != nil || repeated.Status != "cancelling" {
				t.Fatalf("repeat cancel task=%+v err=%v", repeated, err)
			}
			finished := waitForTaskStatus(t, a, project.ID, task.ID, "cancelled")
			if finished.WorktreeState != "clean" {
				t.Fatalf("cancelled clean executor worktree state=%+v", finished)
			}
			assertPIDStopped(t, pid)
		})
	}
}

func TestTaskCancelBeforeIntegrationLockPreventsPrimaryMutation(t *testing.T) {
	a, project, task := newIntegrationFixture(t, "task-cancel-before-integration", "task.txt", "change")
	task.Status = "pending"
	a.updateTask(project.ID, task)
	claimed, ctx, finishRun, ok := a.claimTaskRun(project.ID, task.ID)
	if !ok {
		t.Fatal("could not claim test task run")
	}
	defer finishRun()
	before := capturePrimary(t, project.Path)
	arrived := make(chan struct{})
	release := make(chan struct{})
	a.taskIntegrationPreLockHook = func() {
		close(arrived)
		<-release
	}
	defer func() { a.taskIntegrationPreLockHook = nil }()
	result := make(chan Task, 1)
	go func() { result <- a.integrateTaskWithContext(ctx, project, claimed, false) }()
	<-arrived // paused after the legacy ctx check, before project-lock acquisition
	if _, err := a.cancelTask(project, claimed); err != nil {
		t.Fatalf("cancel before integration: %v", err)
	}
	close(release)
	updated := <-result
	if updated.Status != "cancelled" {
		t.Fatalf("cancel before integration result=%+v", updated)
	}
	assertPrimaryUnchanged(t, project.Path, before)
}

func TestTaskCancelStopsVerifierAndPreservesDirtyWorktree(t *testing.T) {
	a, project, task := newIntegrationFixture(t, "task-cancel-verifier", "task.txt", "change")
	task.Status = "pending"
	a.updateTask(project.ID, task)
	bin := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "verifier.pid")
	writeExecutable(t, filepath.Join(bin, "codex"), `#!/bin/sh
while [ "$#" -gt 0 ] && [ "$1" != "-C" ]; do shift; done
shift
printf change > "$1/changed-by-executor.txt"
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KAROZ_TASK_PROVIDER", "codex")
	t.Setenv("TASK_TEST_PID_FILE", pidFile)
	t.Setenv("KAROZ_VERIFY_COMMAND", `sleep 30 & echo $! > "$TASK_TEST_PID_FILE"; wait`)

	a.runTaskAsync(project, task, "test")
	pid := waitForPID(t, pidFile)
	latest := waitForTaskStatus(t, a, project.ID, task.ID, "verifying")
	if _, err := a.cancelTask(project, latest); err != nil {
		t.Fatalf("cancel verifier: %v", err)
	}
	finished := waitForTaskStatus(t, a, project.ID, task.ID, "cancelled")
	if finished.WorktreeState != "recoverable_dirty" || !strings.Contains(finished.WorktreeDetail, "changed-by-executor.txt") {
		t.Fatalf("dirty verifier worktree was not preserved: %+v", finished)
	}
	if _, err := os.Stat(filepath.Join(finished.WorktreePath, "changed-by-executor.txt")); err != nil {
		t.Fatalf("recoverable worktree was removed: %v", err)
	}
	assertPIDStopped(t, pid)
}

func TestTaskCancelCannotInterruptMergeCriticalSection(t *testing.T) {
	a, project, task := newIntegrationFixture(t, "task-cancel-merge", "task.txt", "change")
	task.Status = "merging"
	a.updateTask(project.ID, task)
	lock := a.projectIntegrationLock(project.ID)
	lock.Lock()
	_, err := a.cancelTask(project, task)
	lock.Unlock()
	if err == nil {
		t.Fatal("cancellation unexpectedly entered merge critical section")
	}
	if latest, _ := a.findTask(project.ID, task.ID); latest.Status != "merging" {
		t.Fatalf("merge task status changed during rejected cancel: %+v", latest)
	}
}

func TestTaskCancelFinishRaceHasOneTerminalState(t *testing.T) {
	a, project, task := newIntegrationFixture(t, "task-cancel-finish-race", "task.txt", "change")
	task.Status = "pending"
	a.updateTask(project.ID, task)
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "codex"), "#!/bin/sh\nsleep 0.03\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KAROZ_TASK_PROVIDER", "codex")
	a.runTaskAsync(project, task, "test")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if current, ok := a.findTask(project.ID, task.ID); ok && current.Status == "running" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	_, _ = a.cancelTask(project, task)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if current, ok := a.findTask(project.ID, task.ID); ok && taskStatusIsTerminal(current.Status) {
			if current.Status != "cancelled" && current.Status != "failed" {
				t.Fatalf("cancel/finish produced unexpected terminal task: %+v", current)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, _ := a.findTask(project.ID, task.ID)
	t.Fatalf("cancel/finish race left nonterminal task: %+v", current)
}

func TestRecoverInterruptedTaskPreservesDirtyWorktree(t *testing.T) {
	a, project, task := newIntegrationFixture(t, "task-recovery", "task.txt", "change")
	project.ID = projectID(project.Path)
	task.ProjectID = project.ID
	a.settings.ProjectsRoot = filepath.Dir(project.Path)
	worktree, err := a.expectedTaskWorktree(project.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, project.Path, "worktree", "add", worktree, task.TaskBranch)
	writeTestFile(t, filepath.Join(worktree, "recover-after-restart.txt"), "preserve")
	task.Status = "running"
	task.WorktreePath = worktree
	a.tasks[project.ID] = []Task{task}
	if err := a.recoverInterruptedTasks(); err != nil {
		t.Fatal(err)
	}
	recovered, _ := a.findTask(project.ID, task.ID)
	if recovered.Status != "failed" || recovered.WorktreeState != "recoverable_dirty" {
		t.Fatalf("interrupted recovery task=%+v", recovered)
	}
	if _, err := os.Stat(filepath.Join(worktree, "recover-after-restart.txt")); err != nil {
		t.Fatalf("startup recovery removed dirty worktree: %v", err)
	}
}

func TestTaskWorktreeCleanupPreservesDirtyAndRemovesOnlyRegisteredPath(t *testing.T) {
	a, project, task := newIntegrationFixture(t, "task-cleanup", "task.txt", "change")
	worktree, err := a.expectedTaskWorktree(project.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, project.Path, "worktree", "add", worktree, task.TaskBranch)
	task.Status = "cancelled"
	task.WorktreePath = worktree
	a.updateTask(project.ID, task)
	writeTestFile(t, filepath.Join(worktree, "recoverable.txt"), "keep me")
	if _, err := a.cleanupTaskWorktree(project, task); err == nil {
		t.Fatal("dirty task worktree cleanup unexpectedly succeeded")
	}
	dirty, _ := a.findTask(project.ID, task.ID)
	if dirty.WorktreeState != "recoverable_dirty" {
		t.Fatalf("dirty worktree state=%+v", dirty)
	}
	if _, err := os.Stat(filepath.Join(worktree, "recoverable.txt")); err != nil {
		t.Fatalf("dirty worktree content was removed: %v", err)
	}
	if err := os.Remove(filepath.Join(worktree, "recoverable.txt")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(a.settings.DataDir, "outside.txt")
	writeTestFile(t, outside, "do not delete")
	cleaned, err := a.cleanupTaskWorktree(project, dirty)
	if err != nil || cleaned.WorktreeState != "removed" {
		t.Fatalf("clean worktree cleanup task=%+v err=%v", cleaned, err)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("registered worktree remains after cleanup: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("cleanup touched unrelated path: %v", err)
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for child PID in %s", path)
	return 0
}

func waitForTaskStatus(t *testing.T, a *app, projectID, taskID, wanted string) Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if task, ok := a.findTask(projectID, taskID); ok && task.Status == wanted {
			return task
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, _ := a.findTask(projectID, taskID)
	t.Fatalf("timed out waiting for task %s status %s; got %+v", taskID, wanted, task)
	return Task{}
}

func assertPIDStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := run("", "kill", "-0", fmt.Sprintf("%d", pid)); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant process %d still running", pid)
}
