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
	RunTriggerMonitor    = runtimedomain.TriggerMonitor
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
	if a.agentRunContexts == nil {
		a.agentRunContexts = map[string]context.Context{}
	}
	if a.agentRunWorkers == nil {
		a.agentRunWorkers = map[string]string{}
	}
	if a.agentRunCancelling == nil {
		a.agentRunCancelling = map[string]string{}
	}
	if a.agentRunResultCommitted == nil {
		a.agentRunResultCommitted = map[string]string{}
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
		Origin:    run.Origin,
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
			Origin:    run.Origin,
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
	delete(a.agentRunContexts, key)
	delete(a.agentRunWorkers, key)
	delete(a.agentRunCancelling, key)
	delete(a.agentRunResultCommitted, key)
	delete(a.agentRuns, key)
	a.mu.Unlock()
	a.notifyAgentRunFinished(key)
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
		Origin:    run.Origin,
		CreatedAt: now,
	})
	return run, true
}

func (a *app) claimAgentRunWorker(projectID, agentID, expectedRunID string) bool {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	defer a.mu.Unlock()
	run, ok := a.agentRuns[key]
	if !ok || !run.State.Active() || run.ID != expectedRunID {
		return false
	}
	if a.agentRunWorkers == nil {
		a.agentRunWorkers = map[string]string{}
	}
	// A Run has exactly one executor. In particular, a stale start must not
	// overwrite the cancellation ownership of the worker that already claimed
	// this Run.
	if a.agentRunWorkers[key] != "" {
		return false
	}
	a.agentRunWorkers[key] = expectedRunID
	return true
}

// claimAndBindAgentRunWorkerContext is the direct-Run start boundary. It
// installs the worker claim and its cancellation handle while holding the
// same lock that cancelAgentRun uses. That leaves no interval where a cancel
// can accept a Run whose worker has been claimed but cannot yet be signalled.
func (a *app) claimAndBindAgentRunWorkerContext(parent context.Context, projectID, agentID, expectedRunID string) (context.Context, bool) {
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
	if a.agentRunWorkers == nil {
		a.agentRunWorkers = map[string]string{}
	}
	if a.agentRunWorkers[key] != "" {
		a.mu.Unlock()
		cancel()
		return parent, false
	}
	if a.agentRunCancels == nil {
		a.agentRunCancels = map[string]context.CancelFunc{}
	}
	if a.agentRunContexts == nil {
		a.agentRunContexts = map[string]context.Context{}
	}
	a.agentRunWorkers[key] = expectedRunID
	a.agentRunCancels[key] = cancel
	a.agentRunContexts[key] = ctx
	a.mu.Unlock()
	return ctx, true
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
	a.mu.Lock()
	run, ok := a.agentRuns[key]
	if !ok || !run.State.Active() || strings.TrimSpace(expectedRunID) == "" || run.ID != expectedRunID {
		a.mu.Unlock()
		return parent, false
	}
	// Scheduler Begin pre-installs a Background-owned context atomically with
	// its worker claim. Keep that owner as the explicit-cancel handle, while
	// returning a child that also observes the SchedulerWorker parent. This
	// prevents a scheduler shutdown/deadline from being lost merely because the
	// owner intentionally outlives an HTTP observer.
	if existing := a.agentRunContexts[key]; existing != nil {
		a.mu.Unlock()
		return contextWithParentCancellation(existing, parent), true
	}
	ctx, cancel := context.WithCancel(parent)
	if a.agentRunCancels == nil {
		a.agentRunCancels = map[string]context.CancelFunc{}
	}
	if a.agentRunContexts == nil {
		a.agentRunContexts = map[string]context.Context{}
	}
	a.agentRunCancels[key] = cancel
	a.agentRunContexts[key] = ctx
	a.mu.Unlock()
	return ctx, true
}

// contextWithParentCancellation keeps owner authoritative for explicit Run
// cancellation while making the returned execution context stop when parent
// stops. context has no multi-parent primitive; this bridge deliberately owns
// no application state and is released when either source context ends.
func contextWithParentCancellation(owner, parent context.Context) context.Context {
	if owner == nil {
		return parent
	}
	if parent == nil || parent.Done() == nil {
		return owner
	}
	child, cancel := context.WithCancel(owner)
	if parent.Err() != nil {
		cancel()
		return child
	}
	stopParent := context.AfterFunc(parent, cancel)
	context.AfterFunc(child, func() { stopParent() })
	return child
}

func (a *app) cancelAgentRun(projectID, agentID string) (AgentRun, bool) {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	run, ok := a.agentRuns[key]
	if !ok || !run.State.Active() || a.agentRunResultCommitted[key] == run.ID {
		a.mu.Unlock()
		return AgentRun{}, false
	}
	cancel := a.agentRunCancels[key]
	workerOwnsRun := a.agentRunWorkers[key] == run.ID
	if workerOwnsRun {
		if a.agentRunCancelling == nil {
			a.agentRunCancelling = map[string]string{}
		}
		a.agentRunCancelling[key] = run.ID
	}
	a.mu.Unlock()
	if cancel != nil && workerOwnsRun {
		cancel()
		return run, true
	}
	if cancel != nil {
		cancel()
	}
	if _, finished := a.finishAgentRun(projectID, agentID, run.ID, RunStateCancelled, context.Canceled); finished {
		if ledger := a.runLedger(run.ID); ledger != nil {
			ledger.publish("cancelled", map[string]any{"message": "Agent run cancelled."})
		}
	}
	return run, true
}

// commitAgentRunSuccessWithLedger is the direct-Run success-side
// linearization point. Scheduled Runs use the generic helper below with their
// own result intent but the same cancellation/terminal claim.
func (a *app) commitAgentRunSuccessWithLedger(project Project, agent Agent, runID, body string) bool {
	return a.commitAgentRunResultWithLedger(project, agent, runID, "result", body, true)
}

// commitAgentRunResultWithLedger is the one success-side linearization point:
// result persistence, terminal state, cancellation ownership, and terminal
// delivery all commit as one claim. A cancelling Run cannot persist a result;
// after this claim succeeds cancel sees no active Run and returns conflict.
//
// Direct Runs cancel their owned context as part of finalization. Scheduler
// workers keep that context alive until their Execute callback returns, so a
// scheduled result uses cancelOwnedContext=false and lets SchedulerWorker clean
// up its timeout child normally.
func (a *app) commitAgentRunResultWithLedger(project Project, agent Agent, runID, intent, body string, cancelOwnedContext bool) bool {
	projectID, agentID := project.ID, agent.ID
	key := projectAgentKey(projectID, agentID)
	now := time.Now().UTC()
	a.mu.Lock()
	run, ok := a.agentRuns[key]
	if !ok || !run.State.Active() || run.ID != runID || a.agentRunCancelling[key] == runID {
		a.mu.Unlock()
		return false
	}
	if a.agentRunResultCommitted == nil {
		a.agentRunResultCommitted = map[string]string{}
	}
	a.agentRunResultCommitted[key] = runID
	a.appendAgentMessageLockedWithTranscript(projectID, agentID, "assistant", firstNonEmpty(intent, "result"), body, agentTranscriptAppendMetadata{RunID: runID})
	previous := run.State
	run = runtimedomain.Finish(run, RunStateDone, nil, now)
	cancel := a.agentRunCancels[key]
	delete(a.agentRunCancels, key)
	delete(a.agentRunContexts, key)
	delete(a.agentRunWorkers, key)
	delete(a.agentRunCancelling, key)
	delete(a.agentRunResultCommitted, key)
	delete(a.agentRuns, key)
	a.mu.Unlock()
	a.notifyAgentRunFinished(key)
	a.persistAppendedAgentMessage(projectID, agentID)
	a.revokeResidentBashApprovalsForRun(run.ID)
	if cancelOwnedContext && cancel != nil {
		cancel()
	}
	a.emitRuntimeStateChanged(RuntimeEvent{
		ID: randomID(), ProjectID: projectID, Kind: "agent_run_changed", EntityID: agentID,
		RunID: run.ID, Trigger: string(run.Trigger), From: string(previous), To: string(RunStateDone),
		Reason: string(run.Trigger), CreatedAt: now,
	})
	if ledger := a.runLedger(runID); ledger != nil {
		payload := map[string]any{"message": body}
		if agent.ID != "" {
			payload["agent"] = a.agentWithRuntimeState(project, agent)
		}
		ledger.publish("done", payload)
	}
	return true
}

// waitForAgentRunFinished is the scheduler's availability signal. It checks
// and registers under the same lock as finishAgentRun, so a Run cannot finish
// in the gap between observing it active and subscribing to its wake-up.
func (a *app) waitForAgentRunFinished(ctx context.Context, projectID, agentID string) bool {
	if ctx == nil {
		return false
	}
	key := projectAgentKey(projectID, agentID)
	ch := make(chan struct{})
	a.mu.Lock()
	if run, active := a.agentRuns[key]; !active || !run.State.Active() {
		a.mu.Unlock()
		return true
	}
	if a.agentRunFinishedWatchers == nil {
		a.agentRunFinishedWatchers = map[string]map[chan struct{}]struct{}{}
	}
	if a.agentRunFinishedWatchers[key] == nil {
		a.agentRunFinishedWatchers[key] = map[chan struct{}]struct{}{}
	}
	a.agentRunFinishedWatchers[key][ch] = struct{}{}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if watchers := a.agentRunFinishedWatchers[key]; watchers != nil {
			delete(watchers, ch)
			if len(watchers) == 0 {
				delete(a.agentRunFinishedWatchers, key)
			}
		}
		a.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	}
}

func (a *app) notifyAgentRunFinished(key string) {
	a.mu.Lock()
	watchers := a.agentRunFinishedWatchers[key]
	delete(a.agentRunFinishedWatchers, key)
	a.mu.Unlock()
	for ch := range watchers {
		close(ch)
	}
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
