package main

import (
	"context"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	"strings"
	"time"
)

const (
	RunTriggerUserDirect = runtimedomain.TriggerUserDirect
	RunTriggerHandoff    = runtimedomain.TriggerHandoff
	RunTriggerTaskEvent  = runtimedomain.TriggerTaskEvent
	RunTriggerPlanEvent  = runtimedomain.TriggerPlanEvent
	RunTriggerSystem     = runtimedomain.TriggerSystem
)

const (
	RunStateQueued           = runtimedomain.StateQueued
	RunStatePreparingContext = runtimedomain.StatePreparingContext
	RunStateInvokingModel    = runtimedomain.StateInvokingModel
	RunStateExecutingTool    = runtimedomain.StateExecutingTool
	RunStateWaitingModel     = runtimedomain.StateWaitingModel
	RunStateCompleting       = runtimedomain.StateCompleting
	RunStateDone             = runtimedomain.StateDone
	RunStateFailed           = runtimedomain.StateFailed
	RunStateCancelled        = runtimedomain.StateCancelled
)

type RunTrigger = runtimedomain.Trigger
type RunState = runtimedomain.State
type AgentRunInput = runtimedomain.RunInput
type AgentRun = runtimedomain.Run

func normalizeRunTrigger(trigger RunTrigger) RunTrigger {
	return runtimedomain.NormalizeTrigger(trigger)
}

func (a *app) beginAgentRun(input AgentRunInput) (AgentRun, bool) {
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.AgentID = strings.TrimSpace(input.AgentID)
	if input.ProjectID == "" || input.AgentID == "" {
		return AgentRun{}, false
	}
	input.Trigger = normalizeRunTrigger(input.Trigger)
	input.TurnType = normalizeChatTurnType(input.TurnType)
	input.SourceID = strings.TrimSpace(input.SourceID)
	input.MessageID = strings.TrimSpace(input.MessageID)
	key := projectAgentKey(input.ProjectID, input.AgentID)
	now := time.Now().UTC()
	a.mu.Lock()
	if a.agentRuns == nil {
		a.agentRuns = map[string]AgentRun{}
	}
	if a.agentRunCancels == nil {
		a.agentRunCancels = map[string]context.CancelFunc{}
	}
	if current, ok := a.agentRuns[key]; ok && current.State.Active() {
		a.mu.Unlock()
		return current, false
	}
	for _, candidate := range a.agents[input.ProjectID] {
		if candidate.ID != input.AgentID {
			continue
		}
		candidate = normalizeAgentModelConfig(candidate)
		input.Provider, input.Model = candidate.Provider, candidate.Model
		input.ThinkingEffort, input.ModelConfigVersion = candidate.ThinkingEffort, candidate.ModelConfigVersion
		break
	}
	runID := strings.TrimSpace(input.RunID)
	if runID == "" {
		runID = randomID()
	}
	run := runtimedomain.NewRun(input, runID, now)
	a.agentRuns[key] = run
	a.mu.Unlock()
	a.emitRuntimeStateChanged(RuntimeEvent{
		ID:        randomID(),
		ProjectID: input.ProjectID,
		Kind:      "agent_run_changed",
		EntityID:  input.AgentID,
		RunID:     run.ID,
		Trigger:   string(run.Trigger),
		From:      "idle",
		To:        string(run.State),
		Reason:    string(input.Trigger),
		CreatedAt: now,
	})
	return run, true
}

func (a *app) transitionAgentRun(projectID, agentID, expectedRunID string, next RunState) (AgentRun, bool) {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	run, ok := a.agentRuns[key]
	if !ok || !run.State.Active() || strings.TrimSpace(expectedRunID) == "" || run.ID != expectedRunID {
		a.mu.Unlock()
		return AgentRun{}, false
	}
	previous := run.State
	if !runtimedomain.CanTransition(previous, next) {
		a.mu.Unlock()
		return AgentRun{}, false
	}
	run, changed := runtimedomain.Transition(run, next, time.Now().UTC())
	a.agentRuns[key] = run
	a.mu.Unlock()
	if changed {
		a.emitRuntimeStateChanged(RuntimeEvent{
			ID:        randomID(),
			ProjectID: projectID,
			Kind:      "agent_run_changed",
			EntityID:  agentID,
			RunID:     run.ID,
			Trigger:   string(run.Trigger),
			From:      string(previous),
			To:        string(next),
			Reason:    string(run.Trigger),
			CreatedAt: run.UpdatedAt,
		})
	}
	return run, true
}

func (a *app) finishAgentRun(projectID, agentID, expectedRunID string, final RunState, runErr error) (AgentRun, bool) {
	if final != RunStateFailed && final != RunStateCancelled {
		final = RunStateDone
	}
	key := projectAgentKey(projectID, agentID)
	now := time.Now().UTC()
	a.mu.Lock()
	run, ok := a.agentRuns[key]
	if !ok || strings.TrimSpace(expectedRunID) == "" || run.ID != expectedRunID {
		a.mu.Unlock()
		return AgentRun{}, false
	}
	previous := run.State
	run = runtimedomain.Finish(run, final, runErr, now)
	cancel := a.agentRunCancels[key]
	delete(a.agentRunCancels, key)
	delete(a.agentRuns, key)
	a.mu.Unlock()
	a.revokeResidentBashApprovalsForRun(run.ID)
	if cancel != nil {
		cancel()
	}
	a.emitRuntimeStateChanged(RuntimeEvent{
		ID:        randomID(),
		ProjectID: projectID,
		Kind:      "agent_run_changed",
		EntityID:  agentID,
		RunID:     run.ID,
		Trigger:   string(run.Trigger),
		From:      string(previous),
		To:        string(final),
		Reason:    string(run.Trigger),
		CreatedAt: now,
	})
	return run, true
}

func (a *app) activeAgentRun(projectID, agentID string) (AgentRun, bool) {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	defer a.mu.Unlock()
	run, ok := a.agentRuns[key]
	return run, ok && run.State.Active()
}

func (a *app) agentRunActive(projectID, agentID string) bool {
	_, ok := a.activeAgentRun(projectID, agentID)
	return ok
}

func (a *app) bindAgentRunContext(parent context.Context, projectID, agentID, expectedRunID string) (context.Context, bool) {
	if parent == nil {
		parent = context.Background()
	}
	key := projectAgentKey(projectID, agentID)
	ctx, cancel := context.WithCancel(parent)
	a.mu.Lock()
	run, ok := a.agentRuns[key]
	if !ok || !run.State.Active() || strings.TrimSpace(expectedRunID) == "" || run.ID != expectedRunID {
		a.mu.Unlock()
		cancel()
		return parent, false
	}
	if a.agentRunCancels == nil {
		a.agentRunCancels = map[string]context.CancelFunc{}
	}
	previous := a.agentRunCancels[key]
	a.agentRunCancels[key] = cancel
	a.mu.Unlock()
	if previous != nil {
		previous()
	}
	return ctx, true
}

func (a *app) cancelAgentRun(projectID, agentID string) (AgentRun, bool) {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	run, ok := a.agentRuns[key]
	cancel := a.agentRunCancels[key]
	a.mu.Unlock()
	if !ok || !run.State.Active() {
		return AgentRun{}, false
	}
	if cancel != nil {
		cancel()
	}
	return a.finishAgentRun(projectID, agentID, run.ID, RunStateCancelled, context.Canceled)
}

func (a *app) enqueueAgentInterrupt(projectID, agentID string, msg AgentMessage, turnType string) (AgentInterrupt, bool) {
	item := AgentInterrupt{
		ID:        randomID(),
		ProjectID: projectID,
		AgentID:   agentID,
		MessageID: msg.ID,
		Body:      strings.TrimSpace(msg.Body),
		TurnType:  normalizeChatTurnType(turnType),
		CreatedAt: time.Now().UTC(),
	}
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	run, ok := a.agentRuns[key]
	if !ok || !run.State.Active() {
		a.mu.Unlock()
		return AgentInterrupt{}, false
	}
	run.Interrupts = append(run.Interrupts, item)
	run.UpdatedAt = item.CreatedAt
	a.agentRuns[key] = run
	a.mu.Unlock()
	return item, true
}

func (a *app) drainAgentInterrupts(projectID, agentID, expectedRunID string) []AgentInterrupt {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	run, ok := a.agentRuns[key]
	if !ok || run.ID != expectedRunID || len(run.Interrupts) == 0 {
		a.mu.Unlock()
		return []AgentInterrupt{}
	}
	items := append([]AgentInterrupt{}, run.Interrupts...)
	run.Interrupts = nil
	run.UpdatedAt = time.Now().UTC()
	a.agentRuns[key] = run
	a.mu.Unlock()
	return items
}
