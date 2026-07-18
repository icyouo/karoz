package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	residentBashApprovalTTL     = 10 * time.Minute
	residentBashApprovePrefix   = "resident_bash_approve:"
	residentBashDenyPrefix      = "resident_bash_deny:"
	residentBashApprovalPending = "pending"
	residentBashApprovalGranted = "approved"
)

func (a *app) executeResidentBashTool(ctx context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
	command := toolStringArg(args, "command", 20000)
	if command == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "command is required"}), nil
	}

	if normalizeChatTurnType(toolCtx.TurnType) != "dev" && !a.consumeResidentBashApproval(toolCtx.Project.ID, toolCtx.Agent.ID, toolCtx.RunID, command) {
		return a.requestResidentBashApproval(toolCtx, command), nil
	}
	if err := a.markScheduledRunEffectsStarted(toolCtx.RunID); err != nil {
		return toolJSON(map[string]any{"error": "effect_barrier_failed", "message": err.Error()}), err
	}

	result := runResidentBashTool(ctx, toolCtx.Workdir, command, clampToolInt(args, "timeout_ms", 60000, 1, 300000), clampToolInt(args, "max_output", 20000, 1, 200000))
	if err := ctx.Err(); err != nil {
		return toolJSON(result), err
	}
	return toolJSON(result), nil
}

func runResidentBashTool(parent context.Context, workdir, command string, timeoutMS, maxOutput int) BashToolResult {
	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = workdir
	prepareResidentBashProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	output := newBoundedCommandOutput(maxOutput)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	_ = stopResidentBashProcessTree(cmd)
	text, truncated := output.Result()
	result := BashToolResult{
		OK:         err == nil,
		Workspace:  workdir,
		Command:    command,
		DurationMS: time.Since(startedAt).Milliseconds(),
		Truncated:  truncated,
	}
	if cmd.ProcessState != nil {
		result.Code = cmd.ProcessState.ExitCode()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.OK = false
		result.Code = -1
		result.Error = "command timed out"
	} else if err != nil {
		result.Error = err.Error()
	}
	if result.OK {
		result.Stdout = text
	} else {
		result.Stderr = text
	}
	return result
}

type boundedCommandOutput struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func newBoundedCommandOutput(limit int) *boundedCommandOutput {
	if limit < 1 {
		limit = 1
	}
	return &boundedCommandOutput{data: make([]byte, 0, limit), limit: limit}
}

func (w *boundedCommandOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := len(p)
	remaining := w.limit - len(w.data)
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		w.data = append(w.data, p...)
	}
	if written > remaining {
		w.truncated = true
	}
	return written, nil
}

func (w *boundedCommandOutput) Result() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.ToValidUTF8(string(w.data), "�"), w.truncated
}

func (a *app) requestResidentBashApproval(toolCtx ResidentToolContext, command string) string {
	now := time.Now().UTC()
	approval := ResidentBashApproval{}
	a.mu.Lock()
	if a.residentBashApprovals == nil {
		a.residentBashApprovals = map[string]ResidentBashApproval{}
	}
	for id, candidate := range a.residentBashApprovals {
		if !candidate.ExpiresAt.After(now) {
			delete(a.residentBashApprovals, id)
			continue
		}
		if candidate.ProjectID == toolCtx.Project.ID && candidate.AgentID == toolCtx.Agent.ID && candidate.Command == command && candidate.State == residentBashApprovalPending {
			approval = candidate
			break
		}
	}
	if approval.ID == "" {
		approval = ResidentBashApproval{
			ID:        randomID(),
			ProjectID: toolCtx.Project.ID,
			AgentID:   toolCtx.Agent.ID,
			Command:   command,
			State:     residentBashApprovalPending,
			CreatedAt: now,
			ExpiresAt: now.Add(residentBashApprovalTTL),
		}
		a.residentBashApprovals[approval.ID] = approval
	}
	a.mu.Unlock()

	agentName := firstNonEmpty(toolCtx.Agent.Nickname, toolCtx.Agent.DisplayName, toolCtx.Agent.Name, toolCtx.Agent.ID, "resident agent")
	return toolJSON(map[string]any{
		"kind":          "choice_request",
		"status":        "pending_user_choice",
		"approval_type": "resident_bash",
		"approval_id":   approval.ID,
		"question":      fmt.Sprintf("Allow %s to run this command in %s?\n\n$ %s", agentName, firstNonEmpty(toolCtx.Project.Name, toolCtx.Project.ID, "the project"), command),
		"mode":          "yes_no",
		"choices": []map[string]string{
			{"id": residentBashApprovePrefix + approval.ID, "label": "Run command", "description": "Allow this exact command once."},
			{"id": residentBashDenyPrefix + approval.ID, "label": "Cancel", "description": "Do not run the command."},
		},
	})
}

func isResidentBashChoice(choiceID string) bool {
	choiceID = strings.TrimSpace(choiceID)
	return strings.HasPrefix(choiceID, residentBashApprovePrefix) || strings.HasPrefix(choiceID, residentBashDenyPrefix)
}

func (a *app) resolveResidentBashChoice(projectID, agentID, runID, choiceID string) (bool, error) {
	choiceID = strings.TrimSpace(choiceID)
	approved := strings.HasPrefix(choiceID, residentBashApprovePrefix)
	denied := strings.HasPrefix(choiceID, residentBashDenyPrefix)
	if !approved && !denied {
		return false, nil
	}
	id := strings.TrimPrefix(choiceID, residentBashApprovePrefix)
	if denied {
		id = strings.TrimPrefix(choiceID, residentBashDenyPrefix)
	}

	now := time.Now().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	approval, ok := a.residentBashApprovals[id]
	if !ok || !approval.ExpiresAt.After(now) {
		delete(a.residentBashApprovals, id)
		return true, errors.New("bash approval is missing or expired")
	}
	if approval.ProjectID != projectID || approval.AgentID != agentID {
		return true, errors.New("bash approval belongs to a different project or agent")
	}
	if approval.State != residentBashApprovalPending {
		return true, errors.New("bash approval has already been resolved")
	}
	if denied {
		delete(a.residentBashApprovals, id)
		return true, nil
	}
	if strings.TrimSpace(runID) == "" {
		return true, errors.New("bash approval requires an active run")
	}
	approval.State = residentBashApprovalGranted
	approval.RunID = runID
	a.residentBashApprovals[id] = approval
	return true, nil
}

func (a *app) consumeResidentBashApproval(projectID, agentID, runID, command string) bool {
	if strings.TrimSpace(runID) == "" {
		return false
	}
	now := time.Now().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, approval := range a.residentBashApprovals {
		if !approval.ExpiresAt.After(now) {
			delete(a.residentBashApprovals, id)
			continue
		}
		if approval.ProjectID == projectID && approval.AgentID == agentID && approval.RunID == runID && approval.Command == command && approval.State == residentBashApprovalGranted {
			delete(a.residentBashApprovals, id)
			return true
		}
	}
	return false
}

func (a *app) revokeResidentBashApprovalsForRun(runID string) {
	if strings.TrimSpace(runID) == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, approval := range a.residentBashApprovals {
		if approval.RunID == runID {
			delete(a.residentBashApprovals, id)
		}
	}
}
