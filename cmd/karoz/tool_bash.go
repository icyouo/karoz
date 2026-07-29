package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	residentBashSubjectVersion = 1

	residentBashOperationForeground      = "foreground"
	residentBashOperationBackgroundStart = "background_start"
	residentBashOperationBackgroundStop  = "background_stop"

	residentBashApprovalTTL     = 10 * time.Minute
	residentBashApprovePrefix   = "resident_bash_approve:"
	residentBashDenyPrefix      = "resident_bash_deny:"
	residentBashApprovalPending = "pending"
	residentBashApprovalGranted = "approved"
)

// residentBashSubject is the versioned canonical approval identity for every
// resident command operation. Equality is structured-field equality; labels
// shown to users are never parsed back into authorization.
type residentBashSubject struct {
	Version          int    `json:"version"`
	Operation        string `json:"operation"`
	ProjectID        string `json:"project_id"`
	AgentID          string `json:"agent_id"`
	CanonicalWorkdir string `json:"canonical_workdir"`
	ProcessID        string `json:"process_id"`
	CommandSHA256    string `json:"command_sha256"`
}

func newResidentBashSubject(
	operation, projectID, agentID, workdir, command, processID string,
) (residentBashSubject, error) {
	canonicalWorkdir, err := canonicalResidentWorkdir(workdir)
	if err != nil {
		return residentBashSubject{}, err
	}
	return newResidentBashSubjectFromCanonical(
		operation,
		projectID,
		agentID,
		canonicalWorkdir,
		command,
		processID,
	)
}

func newResidentBashSubjectFromCanonical(
	operation, projectID, agentID, canonicalWorkdir, command, processID string,
) (residentBashSubject, error) {
	canonicalWorkdir = strings.TrimSpace(canonicalWorkdir)
	if !filepath.IsAbs(canonicalWorkdir) ||
		filepath.Clean(canonicalWorkdir) != canonicalWorkdir {
		return residentBashSubject{}, errors.New(
			"resident command canonical workdir is invalid",
		)
	}
	sum := sha256.Sum256([]byte(command))
	subject := residentBashSubject{
		Version: residentBashSubjectVersion, Operation: strings.TrimSpace(operation),
		ProjectID: strings.TrimSpace(projectID), AgentID: strings.TrimSpace(agentID),
		CanonicalWorkdir: canonicalWorkdir, ProcessID: strings.TrimSpace(processID),
		CommandSHA256: hex.EncodeToString(sum[:]),
	}
	if err := subject.validate(); err != nil {
		return residentBashSubject{}, err
	}
	return subject, nil
}

func (subject residentBashSubject) validate() error {
	if subject.Version != residentBashSubjectVersion {
		return errors.New("resident command approval subject version is unsupported")
	}
	switch subject.Operation {
	case residentBashOperationForeground, residentBashOperationBackgroundStart:
		if subject.ProcessID != "" {
			return errors.New("resident command start subject must not include a process id")
		}
	case residentBashOperationBackgroundStop:
		if subject.ProcessID == "" {
			return errors.New("resident command stop subject requires a process id")
		}
	default:
		return errors.New("resident command approval operation is invalid")
	}
	if subject.ProjectID == "" || subject.AgentID == "" ||
		subject.CanonicalWorkdir == "" || len(subject.CommandSHA256) != sha256.Size*2 {
		return errors.New("resident command approval subject is incomplete")
	}
	if _, err := hex.DecodeString(subject.CommandSHA256); err != nil ||
		subject.CommandSHA256 != strings.ToLower(subject.CommandSHA256) {
		return errors.New("resident command approval digest is invalid")
	}
	return nil
}

func (subject residentBashSubject) canonicalJSON() ([]byte, error) {
	if err := subject.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(subject)
}

func canonicalResidentWorkdir(workdir string) (string, error) {
	workdir = strings.TrimSpace(workdir)
	if workdir == "" {
		return "", errors.New("resident command workdir is required")
	}
	absolute, err := filepath.Abs(workdir)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("canonicalize resident command workdir: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("resident command workdir must be a directory")
	}
	return filepath.Clean(canonical), nil
}

func (a *app) executeResidentBashTool(ctx context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
	command := toolStringArg(args, "command", 20000)
	if command == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "command is required"}), nil
	}

	subject, err := newResidentBashSubject(
		residentBashOperationForeground,
		toolCtx.Project.ID,
		toolCtx.Agent.ID,
		firstNonEmpty(toolCtx.Workdir, toolCtx.Project.Path),
		command,
		"",
	)
	if err != nil {
		return toolJSON(map[string]any{"error": "validation_error", "message": err.Error()}), nil
	}
	if normalizeChatTurnType(toolCtx.TurnType) != "dev" &&
		!a.consumeResidentBashApprovalSubject(toolCtx.RunID, subject) {
		return a.requestResidentBashApprovalSubject(toolCtx, subject, command), nil
	}
	if err := a.markScheduledRunEffectsStarted(toolCtx.RunID); err != nil {
		return toolJSON(map[string]any{"error": "effect_barrier_failed", "message": err.Error()}), err
	}

	requestedTimeout := clampToolInt(args, "timeout_ms", 60000, 1, 300000)
	// The provider loop gives tools only the remaining tool-phase context. Do
	// not advertise or attempt a Bash timeout that outlives that context.
	requestedTimeout = clampResidentBashTimeout(ctx, requestedTimeout)
	result := runResidentBashTool(
		ctx,
		subject.CanonicalWorkdir,
		command,
		requestedTimeout,
		clampToolInt(args, "max_output", 20000, 1, 200000),
	)
	if err := ctx.Err(); err != nil {
		return toolJSON(result), err
	}
	return toolJSON(result), nil
}

func clampResidentBashTimeout(ctx context.Context, requestedMS int) int {
	if requestedMS < 1 {
		requestedMS = 1
	}
	if ctx == nil {
		return requestedMS
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return requestedMS
	}
	remaining := time.Until(deadline).Milliseconds()
	if remaining < 1 {
		return 1
	}
	if remaining < int64(requestedMS) {
		return int(remaining)
	}
	return requestedMS
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
	subject, err := newResidentBashSubject(
		residentBashOperationForeground,
		toolCtx.Project.ID,
		toolCtx.Agent.ID,
		firstNonEmpty(toolCtx.Workdir, toolCtx.Project.Path),
		command,
		"",
	)
	if err != nil {
		return toolJSON(map[string]any{"error": "validation_error", "message": err.Error()})
	}
	return a.requestResidentBashApprovalSubject(toolCtx, subject, command)
}

func (a *app) requestResidentBashApprovalSubject(
	toolCtx ResidentToolContext,
	subject residentBashSubject,
	command string,
) string {
	now := time.Now().UTC()
	approval := ResidentBashApproval{}
	a.mu.Lock()
	if a.residentBashApprovals == nil {
		a.residentBashApprovals = map[string]ResidentBashApproval{}
	}
	ownerCreatedAt, ownerAvailable := a.residentApprovalOwnerLocked(
		toolCtx.Project.ID,
		toolCtx.Agent,
	)
	if !ownerAvailable {
		a.mu.Unlock()
		return toolJSON(map[string]any{
			"error":   "owner_not_found",
			"message": "resident command owner is no longer registered",
		})
	}
	for id, candidate := range a.residentBashApprovals {
		if !candidate.ExpiresAt.After(now) {
			delete(a.residentBashApprovals, id)
			continue
		}
		if candidate.Subject == subject &&
			candidate.OwnerCreatedAt.Equal(ownerCreatedAt) &&
			candidate.State == residentBashApprovalPending {
			approval = candidate
			break
		}
	}
	if approval.ID == "" {
		approval = ResidentBashApproval{
			ID: randomID(), Subject: subject, State: residentBashApprovalPending,
			OwnerCreatedAt: ownerCreatedAt,
			CreatedAt:      now,
			ExpiresAt:      now.Add(residentBashApprovalTTL),
		}
		a.residentBashApprovals[approval.ID] = approval
	}
	a.mu.Unlock()

	agentName := firstNonEmpty(toolCtx.Agent.Nickname, toolCtx.Agent.DisplayName, toolCtx.Agent.Name, toolCtx.Agent.ID, "resident agent")
	action, choiceLabel := "run this command", "Run command"
	switch subject.Operation {
	case residentBashOperationBackgroundStart:
		action, choiceLabel = "start this background command", "Start background process"
	case residentBashOperationBackgroundStop:
		action, choiceLabel = "stop background process "+subject.ProcessID, "Stop background process"
	}
	return toolJSON(map[string]any{
		"kind":          "choice_request",
		"status":        "pending_user_choice",
		"approval_type": "resident_bash",
		"approval_id":   approval.ID,
		"operation":     subject.Operation,
		"question":      fmt.Sprintf("Allow %s to %s in %s?\n\n$ %s", agentName, action, firstNonEmpty(toolCtx.Project.Name, toolCtx.Project.ID, "the project"), command),
		"mode":          "yes_no",
		"choices": []map[string]string{
			{"id": residentBashApprovePrefix + approval.ID, "label": choiceLabel, "description": "Allow this exact operation once."},
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
	if approval.Subject.ProjectID != projectID ||
		approval.Subject.AgentID != agentID {
		return true, errors.New("bash approval belongs to a different project or agent")
	}
	if !a.residentApprovalStillOwnedLocked(approval) {
		delete(a.residentBashApprovals, id)
		return true, errors.New("bash approval owner is no longer registered")
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

func (a *app) consumeResidentBashApprovalSubject(
	runID string,
	subject residentBashSubject,
) bool {
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
		if approval.RunID == runID && approval.Subject == subject &&
			approval.State == residentBashApprovalGranted &&
			a.residentApprovalStillOwnedLocked(approval) {
			delete(a.residentBashApprovals, id)
			return true
		}
	}
	return false
}

// consumeResidentBashApproval keeps the historical foreground test/helper
// boundary while production callers use the exact structured subject above.
func (a *app) consumeResidentBashApproval(
	projectID, agentID, runID, command string,
) bool {
	sum := sha256.Sum256([]byte(command))
	digest := hex.EncodeToString(sum[:])
	a.mu.Lock()
	workdir := ""
	for _, approval := range a.residentBashApprovals {
		if approval.Subject.Operation == residentBashOperationForeground &&
			approval.Subject.ProjectID == projectID &&
			approval.Subject.AgentID == agentID &&
			approval.Subject.CommandSHA256 == digest {
			workdir = approval.Subject.CanonicalWorkdir
			break
		}
	}
	a.mu.Unlock()
	if workdir == "" {
		return false
	}
	subject, err := newResidentBashSubject(
		residentBashOperationForeground,
		projectID,
		agentID,
		workdir,
		command,
		"",
	)
	return err == nil && a.consumeResidentBashApprovalSubject(runID, subject)
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

func (a *app) revokeResidentBashApprovalsForOwnerLocked(
	projectID, agentID string,
) {
	for id, approval := range a.residentBashApprovals {
		if approval.Subject.ProjectID == projectID &&
			approval.Subject.AgentID == agentID {
			delete(a.residentBashApprovals, id)
		}
	}
}

func (a *app) residentApprovalOwnerLocked(
	projectID string,
	requested Agent,
) (time.Time, bool) {
	key := projectAgentKey(projectID, requested.ID)
	if a.backgroundOwnerDeleting[key] {
		return time.Time{}, false
	}
	agents, tracked := a.agents[projectID]
	if !tracked {
		return requested.CreatedAt, true
	}
	for _, current := range agents {
		if current.ID != requested.ID {
			continue
		}
		if !requested.CreatedAt.IsZero() &&
			!current.CreatedAt.IsZero() &&
			!requested.CreatedAt.Equal(current.CreatedAt) {
			return time.Time{}, false
		}
		return current.CreatedAt, true
	}
	return time.Time{}, false
}

func (a *app) residentApprovalStillOwnedLocked(
	approval ResidentBashApproval,
) bool {
	key := projectAgentKey(
		approval.Subject.ProjectID,
		approval.Subject.AgentID,
	)
	if a.backgroundOwnerDeleting[key] {
		return false
	}
	agents, tracked := a.agents[approval.Subject.ProjectID]
	if !tracked {
		return true
	}
	for _, current := range agents {
		if current.ID == approval.Subject.AgentID {
			return current.CreatedAt.Equal(approval.OwnerCreatedAt)
		}
	}
	return false
}
