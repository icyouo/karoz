package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
)

func (a *app) executeResidentRunBackgroundTool(
	ctx context.Context,
	toolCtx ResidentToolContext,
	args map[string]any,
) (string, error) {
	command := toolStringArg(args, "command", 20000)
	if command == "" {
		return toolJSON(map[string]any{
			"error": "validation_error", "message": "command is required",
		}), nil
	}
	if !a.processRuntimeReady() {
		return toolJSON(map[string]any{
			"error":   "runtime_unavailable",
			"message": "background process runtime is unavailable",
		}), nil
	}
	workdir, err := canonicalResidentWorkdir(
		firstNonEmpty(toolCtx.Workdir, toolCtx.Project.Path),
	)
	if err != nil {
		return toolJSON(map[string]any{
			"error": "validation_error", "message": err.Error(),
		}), nil
	}
	projectRoot, err := canonicalResidentWorkdir(toolCtx.Project.Path)
	if err != nil || !pathInside(workdir, projectRoot) {
		return toolJSON(map[string]any{
			"error":   "validation_error",
			"message": "background process workdir must stay inside the current project",
		}), nil
	}
	subject, err := newResidentBashSubjectFromCanonical(
		residentBashOperationBackgroundStart,
		toolCtx.Project.ID,
		toolCtx.Agent.ID,
		workdir,
		command,
		"",
	)
	if err != nil {
		return toolJSON(map[string]any{
			"error": "validation_error", "message": err.Error(),
		}), nil
	}
	lifetime, err := backgroundLifetimeArg(
		args,
		a.processSupervisor.config.MaxLifetime,
	)
	if err != nil {
		return toolJSON(map[string]any{
			"error": "validation_error", "message": err.Error(),
		}), nil
	}
	if normalizeChatTurnType(toolCtx.TurnType) != "dev" &&
		!a.consumeResidentBashApprovalSubject(toolCtx.RunID, subject) {
		return a.requestResidentBashApprovalSubject(
			toolCtx,
			subject,
			command,
		), nil
	}

	a.backgroundOwnerMu.Lock()
	defer a.backgroundOwnerMu.Unlock()
	if _, exists := a.projectAgent(toolCtx.Project, toolCtx.Agent.ID); !exists {
		return toolJSON(map[string]any{
			"error":   "owner_not_found",
			"message": "background process owner is no longer registered",
		}), nil
	}
	if err := a.markScheduledRunEffectsStarted(toolCtx.RunID); err != nil {
		return toolJSON(map[string]any{
			"error": "effect_barrier_failed", "message": err.Error(),
		}), err
	}
	record, err := a.processSupervisor.Start(ctx, processStartRequest{
		ID: randomID(), ProjectID: toolCtx.Project.ID, AgentID: toolCtx.Agent.ID,
		RunID: toolCtx.RunID, Command: command, Workdir: workdir,
		Description: toolStringArg(args, "description", 500),
		Lifetime:    lifetime,
	})
	if err != nil {
		return toolJSON(map[string]any{
			"error": "background_start_failed", "message": err.Error(),
		}), nil
	}
	view := newProcessView(
		record,
		a.processLastLine(record),
		time.Now().UTC(),
	)
	return toolJSON(map[string]any{"process": view}), nil
}

func (a *app) executeResidentListProcessesTool(
	_ context.Context,
	toolCtx ResidentToolContext,
	args map[string]any,
) (string, error) {
	items, err := a.processViews(
		toolCtx.Project.ID,
		toolCtx.Agent.ID,
		clampToolInt(args, "limit", 20, 1, 100),
	)
	if err != nil {
		return toolJSON(map[string]any{
			"error": "process_list_failed", "message": err.Error(),
		}), nil
	}
	return toolJSON(map[string]any{"processes": items}), nil
}

func (a *app) executeResidentReadProcessLogTool(
	_ context.Context,
	toolCtx ResidentToolContext,
	args map[string]any,
) (string, error) {
	processID := toolStringArg(args, "process_id", 128)
	if processID == "" {
		return toolJSON(map[string]any{
			"error": "validation_error", "message": "process_id is required",
		}), nil
	}
	record, err := a.processRecord(toolCtx.Project.ID, processID)
	if err != nil || record.AgentID != toolCtx.Agent.ID {
		return toolJSON(map[string]any{
			"error": "not_found", "message": "process not found",
		}), nil
	}
	tail := true
	offset := 0
	if _, provided := args["offset"]; provided {
		tail = false
		offset = clampToolInt(args, "offset", 0, 0, math.MaxInt)
	}
	if _, provided := args["tail"]; provided {
		tail = toolBoolArg(args, "tail", tail)
	}
	window, err := a.readProcessLog(
		toolCtx.Project.ID,
		processID,
		offset,
		clampToolInt(args, "limit", 50, 1, maxProcessLogWindowLines),
		tail,
	)
	if err != nil {
		code := "process_log_failed"
		message := "process log is unavailable"
		if errors.Is(err, errProcessLogGone) {
			code = "gone"
			message = "process log is gone"
		} else if strings.Contains(
			err.Error(),
			"offset exceeds bounded scan window",
		) {
			code = "validation_error"
			message = "requested log offset exceeds the bounded scan window"
		}
		return toolJSON(map[string]any{
			"error": code, "message": message,
		}), nil
	}
	return toolJSON(map[string]any{"log": window}), nil
}

func (a *app) executeResidentStopProcessTool(
	_ context.Context,
	toolCtx ResidentToolContext,
	args map[string]any,
) (string, error) {
	processID := toolStringArg(args, "process_id", 128)
	if processID == "" {
		return toolJSON(map[string]any{
			"error": "validation_error", "message": "process_id is required",
		}), nil
	}
	record, err := a.processRecord(toolCtx.Project.ID, processID)
	if err != nil || record.AgentID != toolCtx.Agent.ID {
		return toolJSON(map[string]any{
			"error": "not_found", "message": "process not found",
		}), nil
	}
	subject, err := newResidentBashSubjectFromCanonical(
		residentBashOperationBackgroundStop,
		record.ProjectID,
		record.AgentID,
		record.Workdir,
		record.Command,
		record.ID,
	)
	if err != nil {
		return toolJSON(map[string]any{
			"error": "validation_error", "message": err.Error(),
		}), nil
	}
	if normalizeChatTurnType(toolCtx.TurnType) != "dev" &&
		!a.consumeResidentBashApprovalSubject(toolCtx.RunID, subject) {
		return a.requestResidentBashApprovalSubject(
			toolCtx,
			subject,
			record.Command,
		), nil
	}

	a.backgroundOwnerMu.Lock()
	defer a.backgroundOwnerMu.Unlock()
	current, err := a.processRecord(toolCtx.Project.ID, processID)
	if err != nil || current.AgentID != toolCtx.Agent.ID {
		return toolJSON(map[string]any{
			"error": "not_found", "message": "process not found",
		}), nil
	}
	if !current.State.Terminal() {
		if err := a.markScheduledRunEffectsStarted(toolCtx.RunID); err != nil {
			return toolJSON(map[string]any{
				"error": "effect_barrier_failed", "message": err.Error(),
			}), err
		}
	}
	view, err := a.stopProcess(
		toolCtx.Project.ID,
		toolCtx.Agent.ID,
		processID,
	)
	if err != nil {
		return toolJSON(map[string]any{
			"error": "background_stop_failed", "message": err.Error(),
		}), nil
	}
	return toolJSON(map[string]any{"process": view}), nil
}

func backgroundLifetimeArg(
	args map[string]any,
	maximum time.Duration,
) (time.Duration, error) {
	raw, exists := args["lifetime_ms"]
	if !exists {
		return 0, nil
	}
	var milliseconds int64
	switch value := raw.(type) {
	case float64:
		if math.Trunc(value) != value || value > math.MaxInt64 {
			return 0, errors.New("lifetime_ms must be an integer")
		}
		milliseconds = int64(value)
	case int:
		milliseconds = int64(value)
	case int64:
		milliseconds = value
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, errors.New("lifetime_ms must be an integer")
		}
		milliseconds = parsed
	default:
		return 0, errors.New("lifetime_ms must be an integer")
	}
	if milliseconds <= 0 {
		return 0, errors.New("lifetime_ms must be positive")
	}
	if maximum <= 0 || milliseconds > maximum.Milliseconds() {
		return 0, errors.New("lifetime_ms exceeds the configured maximum")
	}
	lifetime := time.Duration(milliseconds) * time.Millisecond
	if lifetime <= 0 {
		return 0, errors.New("lifetime_ms exceeds the configured maximum")
	}
	return lifetime, nil
}
