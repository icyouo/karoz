package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a *app) runDevelopmentTask(project Project, task Task) Task {
	baseRef, err := a.ensureProjectBaseRef(project, task.ID)
	if err != nil {
		task.Status = "failed"
		task.FailureSummary = "prepare base ref failed: " + err.Error()
		a.appendTaskLog(project.ID, task.ID, task.FailureSummary)
		return task
	}
	branch := "karoz/task-" + task.ID[:10]
	worktree, err := filepath.Abs(filepath.Join(a.settings.DataDir, "worktrees", project.ID, task.ID))
	if err != nil {
		task.Status = "failed"
		task.FailureSummary = "resolve worktree path failed: " + err.Error()
		a.appendTaskLog(project.ID, task.ID, task.FailureSummary)
		return task
	}
	task.TaskBranch = branch
	task.WorktreePath = worktree
	_ = os.MkdirAll(filepath.Dir(worktree), 0755)
	if _, err := os.Stat(worktree); err == nil {
		a.appendTaskLog(project.ID, task.ID, "removing existing task worktree "+worktree)
		_, _ = run(project.Path, "git", "worktree", "remove", "--force", worktree)
		_ = os.RemoveAll(worktree)
	}
	a.appendTaskLog(project.ID, task.ID, "creating worktree "+worktree)
	a.appendTaskLog(project.ID, task.ID, "base ref "+baseRef)
	if out, err := run(project.Path, "git", "worktree", "add", "-B", branch, worktree, baseRef); err != nil {
		task.Status = "failed"
		task.FailureSummary = "create worktree failed: " + err.Error()
		a.appendTaskLog(project.ID, task.ID, out)
		a.appendTaskLog(project.ID, task.ID, task.FailureSummary)
		return task
	}
	a.appendTaskLog(project.ID, task.ID, "worktree ready")

	provider := getenv("KAROZ_TASK_PROVIDER", "auto")
	prompt := a.buildDevelopmentPrompt(project, task)
	a.appendTaskLog(project.ID, task.ID, "invoking task executor provider="+provider)
	a.appendTaskLog(project.ID, task.ID, "task executor workdir "+worktree)
	cli, err := invokeTaskExecutor(context.Background(), CLI2APIRequest{
		Provider: provider,
		Prompt:   prompt,
		Workdir:  worktree,
		Mode:     "edit",
	})
	if err != nil {
		task.Status = "failed"
		task.FailureSummary = "task executor failed: " + err.Error()
		a.appendTaskLog(project.ID, task.ID, task.FailureSummary)
		return task
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
		a.updateTask(project.ID, task)
		a.appendTaskLog(project.ID, task.ID, "running verification: "+verify)
		out, err := run(worktree, "sh", "-lc", verify)
		a.appendTaskLog(project.ID, task.ID, out)
		if err != nil {
			task.Status = "failed"
			task.FailureSummary = "verification failed: " + err.Error()
			a.appendTaskLog(project.ID, task.ID, task.FailureSummary)
			return task
		}
	}

	if out, err := run(worktree, "git", "add", "-A"); err != nil {
		task.Status = "failed"
		task.FailureSummary = "git add failed: " + err.Error()
		a.appendTaskLog(project.ID, task.ID, out)
		a.appendTaskLog(project.ID, task.ID, task.FailureSummary)
		return task
	}
	commitMessage := "karoz: " + task.Title
	if out, err := run(worktree, "git", "commit", "-m", commitMessage); err != nil {
		task.Status = "failed"
		task.FailureSummary = "git commit failed: " + err.Error()
		a.appendTaskLog(project.ID, task.ID, out)
		a.appendTaskLog(project.ID, task.ID, task.FailureSummary)
		return task
	}
	task.CommitSHA = gitOutput(worktree, "rev-parse", "HEAD")
	a.appendTaskLog(project.ID, task.ID, "committed "+task.CommitSHA)

	if dirty := gitOutput(project.Path, "status", "--porcelain"); strings.TrimSpace(dirty) != "" {
		a.appendTaskLog(project.ID, task.ID, "main worktree has uncommitted changes; attempting merge and letting git detect conflicts")
		a.appendTaskLog(project.ID, task.ID, dirty)
	}
	if out, err := run(project.Path, "git", "checkout", baseRef); err != nil {
		task.Status = "failed"
		task.FailureSummary = "checkout base branch failed: " + err.Error()
		a.appendTaskLog(project.ID, task.ID, out)
		a.appendTaskLog(project.ID, task.ID, task.FailureSummary)
		return task
	}
	if out, err := run(project.Path, "git", "merge", "--no-ff", branch, "-m", "karoz: merge "+task.Title); err != nil {
		task.Status = "failed"
		task.FailureSummary = "merge task branch failed: " + err.Error()
		a.appendTaskLog(project.ID, task.ID, out)
		a.appendTaskLog(project.ID, task.ID, task.FailureSummary)
		return task
	}
	mergedAt := time.Now().UTC()
	task.MergedAt = &mergedAt
	task.Status = "done"
	task.Result = normalizeTaskType(task.Type) + " task committed and merged into " + baseRef
	a.appendTaskLog(project.ID, task.ID, task.Result)
	return task
}

func (a *app) ensureProjectBaseRef(project Project, taskID string) (string, error) {
	if _, err := run(project.Path, "git", "rev-parse", "--verify", "HEAD^{commit}"); err == nil {
		return resolveProjectBaseRef(project), nil
	}
	a.appendTaskLog(project.ID, taskID, "project has no base commit; creating local workspace snapshot before task worktree")
	if out, err := run(project.Path, "git", "add", "-A"); err != nil {
		a.appendTaskLog(project.ID, taskID, out)
		return "", fmt.Errorf("git add base snapshot failed: %w", err)
	}
	if out, err := run(project.Path, "git", "-c", "user.name=Karoz", "-c", "user.email=karoz@local", "commit", "-m", "karoz: initialize workspace snapshot"); err != nil {
		a.appendTaskLog(project.ID, taskID, out)
		return "", fmt.Errorf("git commit base snapshot failed: %w", err)
	}
	baseRef := resolveProjectBaseRef(project)
	a.appendTaskLog(project.ID, taskID, "created base snapshot on "+baseRef)
	return baseRef, nil
}

func resolveProjectBaseRef(project Project) string {
	if branch := gitOutput(project.Path, "branch", "--show-current"); branch != "" {
		return branch
	}
	if ref := strings.TrimSpace(project.DefaultBranch); ref != "" {
		return ref
	}
	return "HEAD"
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

func (a *app) runDeploymentTask(project Project, task Task) Task {
	a.appendTaskLog(project.ID, task.ID, "running local deployment placeholder")
	out, err := run(project.Path, "sh", "-lc", "if [ -f package.json ]; then npm run build; else echo 'No deploy command configured for this project.'; fi")
	a.appendTaskLog(project.ID, task.ID, out)
	if err != nil {
		task.Status = "deploy_failed"
		task.FailureSummary = err.Error()
		return task
	}
	task.Status = "done"
	task.Result = "deployment command completed"
	return task
}
