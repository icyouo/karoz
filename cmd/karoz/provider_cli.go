package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func (a *app) invokeClaude(ctx context.Context, workdir, prompt, mode string) (CLI2APIResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, taskExecutorTimeout())
	defer cancel()
	permissionMode := "plan"
	if strings.EqualFold(mode, "edit") {
		permissionMode = "bypassPermissions"
	}
	result, err := a.runCapturedCommand(ctx, workdir, "claude", "--print", "--permission-mode", permissionMode, "--output-format", "text", "--no-session-persistence", prompt)
	if ctx.Err() != nil {
		return CLI2APIResponse{}, ctx.Err()
	}
	if err != nil {
		return CLI2APIResponse{}, fmt.Errorf("claude failed: %w: %s", err, strings.TrimSpace(result.Output()))
	}
	return CLI2APIResponse{Provider: "claude", Output: strings.TrimSpace(result.Output())}, nil
}

func (a *app) invokeCodex(ctx context.Context, workdir, prompt, mode string) (CLI2APIResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, taskExecutorTimeout())
	defer cancel()
	sandbox := "read-only"
	if strings.EqualFold(mode, "edit") {
		sandbox = "danger-full-access"
	}
	result, err := a.runCapturedCommand(ctx, workdir, "codex", "exec", "--sandbox", sandbox, "-C", workdir, prompt)
	if ctx.Err() != nil {
		return CLI2APIResponse{}, ctx.Err()
	}
	if err != nil {
		return CLI2APIResponse{}, fmt.Errorf("codex failed: %w: %s", err, strings.TrimSpace(result.Output()))
	}
	return CLI2APIResponse{Provider: "codex", Output: strings.TrimSpace(result.Output())}, nil
}

func taskExecutorTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("KAROZ_TASK_TIMEOUT"))
	if raw == "" {
		return 30 * time.Minute
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if minutes, err := strconv.Atoi(raw); err == nil && minutes > 0 {
		return time.Duration(minutes) * time.Minute
	}
	return 30 * time.Minute
}

func (a *app) invokeTaskExecutor(ctx context.Context, req CLI2APIRequest) (CLI2APIResponse, error) {
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" || provider == "auto" {
		if _, err := exec.LookPath("codex"); err == nil {
			provider = "codex"
		} else if _, err := exec.LookPath("claude"); err == nil {
			provider = "claude"
		} else {
			return CLI2APIResponse{}, errors.New("no local task executor found: install codex or claude, or set KAROZ_TASK_PROVIDER")
		}
	}
	workdir := strings.TrimSpace(req.Workdir)
	if workdir == "" {
		return CLI2APIResponse{}, errors.New("task workdir is required")
	}
	switch provider {
	case "codex", "codex-cli":
		return a.invokeCodex(ctx, workdir, req.Prompt, req.Mode)
	case "claude", "claude-code":
		return a.invokeClaude(ctx, workdir, req.Prompt, req.Mode)
	case "codex-direct", "codex-oauth", "codex-api", "cliproxy", "cli2api", "external":
		return CLI2APIResponse{}, fmt.Errorf("%s is not a task coding executor; use codex or claude for task execution", provider)
	default:
		return CLI2APIResponse{}, fmt.Errorf("unsupported task provider %q", provider)
	}
}
