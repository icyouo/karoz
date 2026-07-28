package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// runDevelopmentTask has three deliberately separate phases: record the base and
// create the isolated worktree, execute there, then integrate under the project's
// short-lived integration lock. It must never change the user's primary checkout.
func (a *app) runDevelopmentTask(ctx context.Context, project Project, task Task) Task {
	var err error
	task, err = a.prepareDevelopmentTask(ctx, project, task)
	if err != nil {
		if taskWasCancelled(ctx) {
			return a.cancelledTask(project, task, "worktree preparation")
		}
		return a.failDevelopmentTask(project, task, "prepare task worktree failed", err)
	}
	worktree := task.WorktreePath

	provider := getenv("KAROZ_TASK_PROVIDER", "auto")
	prompt := a.buildDevelopmentPrompt(project, task)
	a.appendTaskLog(project.ID, task.ID, "invoking task executor provider="+provider)
	a.appendTaskLog(project.ID, task.ID, "task executor workdir "+worktree)
	cli, err := invokeTaskExecutor(ctx, CLI2APIRequest{
		Provider: provider,
		Prompt:   prompt,
		Workdir:  worktree,
		Mode:     "edit",
	})
	if err != nil {
		if taskWasCancelled(ctx) {
			return a.cancelledTask(project, task, "task executor")
		}
		return a.failDevelopmentTask(project, task, "task executor failed", err)
	}
	if strings.TrimSpace(cli.Output) != "" {
		a.appendTaskLog(project.ID, task.ID, cli.Output)
	}

	diffStat := gitOutput(worktree, "status", "--short")
	if strings.TrimSpace(diffStat) == "" {
		task.Status = "failed"
		task.FailureSummary = "task executor completed without repository changes"
		a.appendTaskLog(project.ID, task.ID, task.FailureSummary)
		return task
	}
	a.appendTaskLog(project.ID, task.ID, "changes detected")
	a.appendTaskLog(project.ID, task.ID, diffStat)

	if verify := strings.TrimSpace(os.Getenv("KAROZ_VERIFY_COMMAND")); verify != "" {
		task.Status = "verifying"
		task.UpdatedAt = time.Now().UTC()
		a.updateTask(project.ID, task)
		a.saveOrLog("tasks", a.saveTasks())
		a.appendTaskLog(project.ID, task.ID, "running verification: "+verify)
		out, verifyErr := runTaskCommand(ctx, worktree, "sh", "-lc", verify)
		a.appendTaskLog(project.ID, task.ID, out)
		if verifyErr != nil {
			if taskWasCancelled(ctx) {
				return a.cancelledTask(project, task, "verification")
			}
			return a.failDevelopmentTask(project, task, "verification failed", verifyErr)
		}
	}

	if taskWasCancelled(ctx) {
		return a.cancelledTask(project, task, "before commit")
	}
	if out, addErr := runTaskCommand(ctx, worktree, "git", "add", "-A"); addErr != nil {
		if taskWasCancelled(ctx) {
			return a.cancelledTask(project, task, "git add")
		}
		a.appendTaskLog(project.ID, task.ID, out)
		return a.failDevelopmentTask(project, task, "git add failed", addErr)
	}
	commitMessage := "karoz: " + task.Title
	if out, commitErr := runTaskCommand(ctx, worktree, "git", "commit", "-m", commitMessage); commitErr != nil {
		if taskWasCancelled(ctx) {
			return a.cancelledTask(project, task, "git commit")
		}
		a.appendTaskLog(project.ID, task.ID, out)
		return a.failDevelopmentTask(project, task, "git commit failed", commitErr)
	}
	task.CommitSHA = gitOutput(worktree, "rev-parse", "HEAD")
	if task.CommitSHA == "" {
		return a.failDevelopmentTask(project, task, "resolve task commit failed", errors.New("git rev-parse HEAD returned no commit"))
	}
	task.UpdatedAt = time.Now().UTC()
	a.updateTask(project.ID, task)
	a.saveOrLog("tasks", a.saveTasks())
	a.appendTaskLog(project.ID, task.ID, "committed "+task.CommitSHA)

	if taskWasCancelled(ctx) {
		return a.cancelledTask(project, task, "before integration")
	}
	return a.integrateTaskWithContext(ctx, project, task, false)
}

func (a *app) failDevelopmentTask(project Project, task Task, prefix string, err error) Task {
	task.Status = "failed"
	task.FailureSummary = prefix + ": " + err.Error()
	task = a.refreshTaskWorktreeState(project, task)
	a.appendTaskLog(project.ID, task.ID, task.FailureSummary)
	return task
}

// prepareDevelopmentTask persists the exact primary branch and commit before the
// worktree is made. A repository without HEAD is user-owned state: Karoz refuses
// to create a surprise initialization commit.
func (a *app) prepareDevelopmentTask(ctx context.Context, project Project, task Task) (Task, error) {
	lock := a.projectIntegrationLock(project.ID)
	lock.Lock()
	defer lock.Unlock()

	baseBranch, baseCommit, err := resolveProjectBase(ctx, project)
	if err != nil {
		return task, err
	}
	worktree, err := filepath.Abs(filepath.Join(a.settings.DataDir, "worktrees", project.ID, task.ID))
	if err != nil {
		return task, fmt.Errorf("resolve worktree path: %w", err)
	}
	branch := "karoz/task-" + task.ID[:minTaskIDPrefix(task.ID)]
	task.BaseBranch = baseBranch
	task.BaseCommit = baseCommit
	task.TaskBranch = branch
	task.WorktreePath = worktree
	task.MergeBlockedReason = ""
	task.MergeBlockedDetail = ""
	task.UpdatedAt = time.Now().UTC()
	a.updateTask(project.ID, task)
	a.saveOrLog("tasks", a.saveTasks())

	if err := os.MkdirAll(filepath.Dir(worktree), 0755); err != nil {
		return task, fmt.Errorf("create worktree parent: %w", err)
	}
	if _, statErr := os.Stat(worktree); statErr == nil {
		task, clean := a.inspectTaskWorktree(project, task)
		task.UpdatedAt = time.Now().UTC()
		a.updateTask(project.ID, task)
		a.saveOrLog("tasks", a.saveTasks())
		if !clean {
			return task, errors.New("existing task worktree has recoverable changes; clean it or use the cleanup action before rerunning")
		}
		return task, errors.New("existing clean task worktree must be removed through the cleanup action before rerunning")
	}
	a.appendTaskLog(project.ID, task.ID, "creating worktree "+worktree)
	a.appendTaskLog(project.ID, task.ID, "base branch "+baseBranch+" at "+baseCommit)
	if out, addErr := runTaskCommand(ctx, project.Path, "git", "worktree", "add", "-B", branch, worktree, baseCommit); addErr != nil {
		a.appendTaskLog(project.ID, task.ID, out)
		return task, fmt.Errorf("create worktree: %w", addErr)
	}
	a.appendTaskLog(project.ID, task.ID, "worktree ready")
	return task, nil
}

func minTaskIDPrefix(id string) int {
	if len(id) < 10 {
		return len(id)
	}
	return 10
}

func resolveProjectBase(ctx context.Context, project Project) (string, string, error) {
	head, err := runTaskCommand(ctx, project.Path, "git", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", "", errors.New("repository has no initial commit; initialize it yourself before creating a Karoz development task")
	}
	branch, err := runTaskCommand(ctx, project.Path, "git", "branch", "--show-current")
	if err != nil {
		return "", "", fmt.Errorf("read primary branch: %w", err)
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", "", errors.New("primary repository is detached; switch it to a branch before creating a Karoz development task")
	}
	return branch, strings.TrimSpace(head), nil
}

func (a *app) projectIntegrationLock(projectID string) *sync.Mutex {
	a.taskIntegrationLocksMu.Lock()
	defer a.taskIntegrationLocksMu.Unlock()
	if a.taskIntegrationLocks == nil {
		a.taskIntegrationLocks = map[string]*sync.Mutex{}
	}
	lock := a.taskIntegrationLocks[projectID]
	if lock == nil {
		lock = &sync.Mutex{}
		a.taskIntegrationLocks[projectID] = lock
	}
	return lock
}

// retryTaskMerge is intentionally synchronous. Duplicate callers share the same
// project lock, then re-read the task: the second caller sees the completed merge
// and cannot create another merge commit.
func (a *app) retryTaskMerge(project Project, task Task) (Task, error) {
	lock := a.projectIntegrationLock(project.ID)
	lock.Lock()
	defer lock.Unlock()
	latest, ok := a.findTask(project.ID, task.ID)
	if !ok {
		return Task{}, errors.New("task not found")
	}
	if latest.Status == "done" {
		return latest, nil
	}
	if latest.Status != "waiting_merge" {
		return latest, fmt.Errorf("task status %q cannot be merged; only waiting_merge tasks may be retried", latest.Status)
	}
	return a.integrateTaskLocked(context.Background(), project, latest), nil
}

func (a *app) integrateTask(project Project, task Task, waitForLock bool) Task {
	return a.integrateTaskWithContext(context.Background(), project, task, waitForLock)
}

func (a *app) integrateTaskWithContext(ctx context.Context, project Project, task Task, waitForLock bool) Task {
	// Test seam: production leaves this nil. It makes the cancellation handoff
	// boundary independently verifiable without weakening TryLock semantics.
	if a.taskIntegrationPreLockHook != nil {
		a.taskIntegrationPreLockHook()
	}
	lock := a.projectIntegrationLock(project.ID)
	if waitForLock {
		lock.Lock()
		defer lock.Unlock()
		return a.integrateTaskLocked(ctx, project, task)
	}
	if !lock.TryLock() {
		return a.blockTaskMerge(project, task, "integration_busy", "another task is currently maintaining this project's integration checkout")
	}
	defer lock.Unlock()
	return a.integrateTaskLocked(ctx, project, task)
}

func (a *app) integrateTaskLocked(ctx context.Context, project Project, task Task) Task {
	var cancelled bool
	task, cancelled = a.claimTaskIntegration(ctx, project.ID, task.ID)
	if cancelled {
		return a.cancelledTask(project, task, "before integration")
	}
	if task.Status == "done" {
		return task
	}
	a.saveOrLog("tasks", a.saveTasks())
	if task.CommitSHA == "" || task.BaseBranch == "" || task.BaseCommit == "" || task.TaskBranch == "" {
		return a.blockTaskMerge(project, task, "integration_failed", "task is missing recorded branch or commit metadata")
	}

	snapshot, reason, detail := inspectPrimaryForMerge(project, task)
	if reason != "" {
		return a.blockTaskMerge(project, task, reason, detail)
	}
	if alreadyMerged, _ := gitIsAncestor(project.Path, task.CommitSHA, snapshot.head); alreadyMerged {
		return a.finishTaskMerge(project, task, "task commit was already present on "+task.BaseBranch)
	}

	task.MergeAttempts++
	task.UpdatedAt = time.Now().UTC()
	a.updateTask(project.ID, task)
	a.saveOrLog("tasks", a.saveTasks())
	a.appendTaskLog(project.ID, task.ID, "merging recorded task commit "+task.CommitSHA+" into primary branch "+task.BaseBranch)
	out, err := run(project.Path, "git", "merge", "--no-ff", task.CommitSHA, "-m", "karoz: merge "+task.Title)
	if err == nil {
		if verification := verifyMergedPrimary(project, task); verification != "" {
			return a.blockTaskMerge(project, task, "integration_failed", "post-merge verification failed: "+verification)
		}
		return a.finishTaskMerge(project, task, normalizeTaskType(task.Type)+" task committed and merged into "+task.BaseBranch)
	}

	conflicts := gitOutput(project.Path, "diff", "--name-only", "--diff-filter=U")
	abortOut, abortErr := run(project.Path, "git", "merge", "--abort")
	if abortErr != nil {
		return a.blockTaskMerge(project, task, "integration_failed", "merge failed and git merge --abort failed: "+strings.TrimSpace(abortOut))
	}
	if restoreReason := verifyPrimarySnapshot(project, snapshot); restoreReason != "" {
		return a.blockTaskMerge(project, task, "integration_failed", "merge abort did not restore primary checkout exactly: "+restoreReason)
	}
	if strings.TrimSpace(conflicts) != "" {
		a.appendTaskLog(project.ID, task.ID, "merge conflict aborted; primary checkout restored\n"+strings.TrimSpace(out))
		return a.blockTaskMerge(project, task, "merge_conflict", strings.TrimSpace(conflicts))
	}
	a.appendTaskLog(project.ID, task.ID, "merge failed and primary checkout was restored\n"+strings.TrimSpace(out))
	return a.blockTaskMerge(project, task, "integration_failed", strings.TrimSpace(out))
}

type primarySnapshot struct {
	head   string
	branch string
	status string
}

func inspectPrimaryForMerge(project Project, task Task) (primarySnapshot, string, string) {
	branch, err := run(project.Path, "git", "branch", "--show-current")
	if err != nil {
		return primarySnapshot{}, "integration_failed", "cannot read primary branch: " + err.Error()
	}
	branch = strings.TrimSpace(branch)
	if branch != task.BaseBranch {
		return primarySnapshot{}, "branch_mismatch", fmt.Sprintf("primary checkout is on %q; task was recorded on %q", branch, task.BaseBranch)
	}
	status, err := run(project.Path, "git", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return primarySnapshot{}, "integration_failed", "cannot inspect primary checkout: " + err.Error()
	}
	if strings.TrimSpace(status) != "" {
		return primarySnapshot{}, "workspace_dirty", strings.TrimSpace(status)
	}
	head, err := run(project.Path, "git", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return primarySnapshot{}, "integration_failed", "cannot resolve primary HEAD: " + err.Error()
	}
	head = strings.TrimSpace(head)
	if ok, err := gitIsAncestor(project.Path, task.BaseCommit, head); err != nil || !ok {
		return primarySnapshot{}, "base_rewritten", "current base no longer contains recorded base commit " + task.BaseCommit
	}
	if ok, err := gitIsAncestor(project.Path, task.BaseCommit, task.CommitSHA); err != nil || !ok {
		return primarySnapshot{}, "base_rewritten", "task commit does not descend from recorded base commit " + task.BaseCommit
	}
	return primarySnapshot{head: head, branch: branch, status: strings.TrimSpace(status)}, "", ""
}

func verifyPrimarySnapshot(project Project, snapshot primarySnapshot) string {
	head := gitOutput(project.Path, "rev-parse", "--verify", "HEAD^{commit}")
	if head != snapshot.head {
		return "HEAD changed from " + snapshot.head + " to " + head
	}
	branch := gitOutput(project.Path, "branch", "--show-current")
	if branch != snapshot.branch {
		return "branch changed from " + snapshot.branch + " to " + branch
	}
	status := gitOutput(project.Path, "status", "--porcelain=v1", "--untracked-files=all")
	if status != snapshot.status {
		return "working tree/index status changed"
	}
	return ""
}

func verifyMergedPrimary(project Project, task Task) string {
	branch := gitOutput(project.Path, "branch", "--show-current")
	if branch != task.BaseBranch {
		return "branch is " + branch + ", expected " + task.BaseBranch
	}
	head := gitOutput(project.Path, "rev-parse", "--verify", "HEAD^{commit}")
	if head == "" {
		return "cannot resolve primary HEAD"
	}
	if ok, err := gitIsAncestor(project.Path, task.CommitSHA, head); err != nil || !ok {
		return "recorded task commit is not an ancestor of primary HEAD"
	}
	if status := gitOutput(project.Path, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
		return "primary checkout is not clean: " + status
	}
	return ""
}

func gitIsAncestor(dir, older, newer string) (bool, error) {
	_, err := run(dir, "git", "merge-base", "--is-ancestor", older, newer)
	if err == nil {
		return true, nil
	}
	if _, verifyErr := run(dir, "git", "rev-parse", "--verify", older+"^{commit}"); verifyErr != nil {
		return false, verifyErr
	}
	if _, verifyErr := run(dir, "git", "rev-parse", "--verify", newer+"^{commit}"); verifyErr != nil {
		return false, verifyErr
	}
	return false, nil
}

func (a *app) blockTaskMerge(project Project, task Task, reason, detail string) Task {
	task.Status = "waiting_merge"
	task.MergeBlockedReason = reason
	task.MergeBlockedDetail = limitString(strings.TrimSpace(detail), 2000)
	task.FailureSummary = ""
	task.Result = ""
	task.UpdatedAt = time.Now().UTC()
	a.updateTask(project.ID, task)
	a.saveOrLog("tasks", a.saveTasks())
	logLine := "merge blocked: " + reason
	if task.MergeBlockedDetail != "" {
		logLine += ": " + task.MergeBlockedDetail
	}
	a.appendTaskLog(project.ID, task.ID, logLine)
	return task
}

func (a *app) finishTaskMerge(project Project, task Task, result string) Task {
	now := time.Now().UTC()
	task.Status = "done"
	task.MergedAt = &now
	task.Result = result
	task.FailureSummary = ""
	task.MergeBlockedReason = ""
	task.MergeBlockedDetail = ""
	task.UpdatedAt = now
	a.updateTask(project.ID, task)
	a.saveOrLog("tasks", a.saveTasks())
	a.appendTaskLog(project.ID, task.ID, task.Result)
	if task.WorktreePath != "" {
		if cleaned, err := a.cleanupTaskWorktreeLocked(project, task); err == nil {
			task = cleaned
		} else {
			task = a.refreshTaskWorktreeState(project, task)
			a.appendTaskLog(project.ID, task.ID, "retaining task worktree: "+err.Error())
		}
	}
	return task
}

func (a *app) buildDevelopmentPrompt(project Project, task Task) string {
	goal := firstNonEmpty(task.Goal, task.Description, task.Title)
	taskType := normalizeTaskType(task.Type)
	artifactContext := ""
	if artifacts, err := a.validateArtifactRefs(project.ID, task.ArtifactIDs); err == nil && len(artifacts) > 0 {
		var lines []string
		for _, artifact := range artifacts {
			full, pathErr := a.safeWorkspacePath(project.ID, artifact.AgentID, artifact.Path)
			if pathErr != nil {
				continue
			}
			lines = append(lines, fmt.Sprintf("- artifact_id=%s kind=%s revision=%d status=%s path=%s", artifact.ID, artifact.Kind, artifact.Revision, artifact.Status, full))
		}
		if len(lines) > 0 {
			artifactContext = "\nReferenced Artifacts (treat approved design artifacts as implementation contracts):\n" + strings.Join(lines, "\n") + "\n"
		}
	}
	return strings.TrimSpace(fmt.Sprintf(`You are Karoz running inside a task worktree.

Project: %s
Repository path: %s
Task type: %s
Task title: %s
%s

Implement the requested change directly in this repository worktree.
Keep the change focused, preserve existing project conventions, and do not commit or merge yourself.
When finished, summarize the files changed and any verification you ran.

User request:
%s`, project.Name, project.Path, taskType, task.Title, artifactContext, goal))
}

func (a *app) runDeploymentTask(ctx context.Context, project Project, task Task) Task {
	a.appendTaskLog(project.ID, task.ID, "running local deployment placeholder")
	out, err := runTaskCommand(ctx, project.Path, "sh", "-lc", "if [ -f package.json ]; then npm run build; else echo 'No deploy command configured for this project.'; fi")
	a.appendTaskLog(project.ID, task.ID, out)
	if err != nil {
		if taskWasCancelled(ctx) {
			return a.cancelledTask(project, task, "deployment")
		}
		task.Status = "deploy_failed"
		task.FailureSummary = err.Error()
		return task
	}
	task.Status = "done"
	task.Result = "deployment command completed"
	return task
}
