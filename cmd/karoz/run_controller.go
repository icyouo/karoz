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
	input.RunID = strings.TrimSpace(input.RunID)
	if input.RunID == "" {
		input.RunID = randomID()
	}
	a.mu.Lock()
	a.agentRuntimeLocked()
	for _, candidate := range a.agentDirectoryLocked().agents[input.ProjectID] {
		if candidate.ID != input.AgentID {
			continue
		}
		candidate = normalizeAgentModelConfig(candidate)
		input.Provider, input.Model = candidate.Provider, candidate.Model
		input.ThinkingEffort, input.ModelConfigVersion = candidate.ThinkingEffort, candidate.ModelConfigVersion
		break
	}
	a.mu.Unlock()
	run, created := a.agentRunLifecycleService().Begin(input, input.RunID)
	if !created {
		return run, false
	}
	a.appendAgentRunStateEvent(run, "idle", string(input.Trigger))
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
		CreatedAt: run.StartedAt,
	})
	return run, true
}

func (a *app) transitionAgentRun(projectID, agentID, expectedRunID string, next RunState) (AgentRun, bool) {
	run, previous, changed, ok := a.agentRunLifecycleService().Transition(projectID, agentID, strings.TrimSpace(expectedRunID), next)
	if !ok {
		return AgentRun{}, false
	}
	if changed {
		a.appendAgentRunStateEvent(run, string(previous), string(run.Trigger))
		a.emitRuntimeStateChanged(RuntimeEvent{
			ID:        randomID(),
			ProjectID: projectID,
			Kind:      "agent_run_changed",
			EntityID:  agentID,
			RunID:     run.ID,
			Trigger:   string(run.Trigger),
			From:      string(previous),
			To:        string(run.State),
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
	run, controlState, previous, ok := a.agentRunControlService().FinishRun(projectID, agentID, strings.TrimSpace(expectedRunID), final, runErr, time.Now().UTC())
	if !ok {
		return AgentRun{}, false
	}
	cancel := controlState.Cancel
	a.notifyAgentRunFinished(key)
	a.revokeResidentBashApprovalsForRun(run.ID)
	if cancel != nil {
		cancel()
	}
	a.appendAgentRunStateEvent(run, string(previous), string(run.Trigger))
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
		CreatedAt: run.UpdatedAt,
	})
	return run, true
}

func (a *app) claimAgentRunWorker(projectID, agentID, expectedRunID string) bool {
	return a.agentRunControlService().ClaimWorker(projectID, agentID, strings.TrimSpace(expectedRunID))
}

// claimAndBindAgentRunWorkerContext is the direct-Run start boundary. It
// installs the worker claim and its cancellation handle while holding the
// same lock that cancelAgentRun uses. That leaves no interval where a cancel
// can accept a Run whose worker has been claimed but cannot yet be signalled.
func (a *app) claimAndBindAgentRunWorkerContext(parent context.Context, projectID, agentID, expectedRunID string) (context.Context, bool) {
	return a.agentRunControlService().ClaimAndBindWorkerContext(parent, projectID, agentID, strings.TrimSpace(expectedRunID))
}

func (a *app) activeAgentRun(projectID, agentID string) (AgentRun, bool) {
	return a.agentRunLifecycleService().Active(projectID, agentID)
}

func (a *app) agentRunActive(projectID, agentID string) bool {
	_, ok := a.activeAgentRun(projectID, agentID)
	return ok
}

func (a *app) bindAgentRunContext(parent context.Context, projectID, agentID, expectedRunID string) (context.Context, bool) {
	return a.agentRunControlService().BindContext(parent, projectID, agentID, strings.TrimSpace(expectedRunID))
}

func (a *app) cancelAgentRun(projectID, agentID string) (AgentRun, bool) {
	request, ok := a.agentRunControlService().RequestCancel(projectID, agentID)
	if !ok {
		return AgentRun{}, false
	}
	if request.Cancel != nil && request.WorkerOwned {
		request.Cancel()
		return request.Run, true
	}
	if request.Cancel != nil {
		request.Cancel()
	}
	if _, finished := a.finishAgentRun(projectID, agentID, request.Run.ID, RunStateCancelled, context.Canceled); finished {
		if ledger := a.runLedger(request.Run.ID); ledger != nil {
			ledger.publish("cancelled", map[string]any{"message": "Agent run cancelled."})
		}
	}
	return request.Run, true
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
	previous := RunState("")
	run, controlState, ok := a.agentRunControlService().CommitResult(projectID, agentID, runID, func(current AgentRun) AgentRun {
		previous = current.State
		a.appendAgentMessageLockedWithTranscript(projectID, agentID, "assistant", firstNonEmpty(intent, "result"), body, agentTranscriptAppendMetadata{RunID: runID})
		finished := runtimedomain.Finish(current, RunStateDone, nil, now)
		session := a.ensureAgentSessionLocked(projectID, agentID)
		a.appendAgentSessionEventLocked(newAgentRunStateSessionEvent(session, finished, string(previous), string(finished.Trigger)))
		return finished
	})
	if !ok {
		return false
	}
	cancel := controlState.Cancel
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
	runtime := a.agentRuntimeLocked()
	if run, active := runtime.runs[key]; !active || !run.State.Active() {
		a.mu.Unlock()
		return true
	}
	if runtime.finishedWatchers[key] == nil {
		runtime.finishedWatchers[key] = map[chan struct{}]struct{}{}
	}
	runtime.finishedWatchers[key][ch] = struct{}{}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		runtime := a.agentRuntimeLocked()
		if watchers := runtime.finishedWatchers[key]; watchers != nil {
			delete(watchers, ch)
			if len(watchers) == 0 {
				delete(runtime.finishedWatchers, key)
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
	runtime := a.agentRuntimeLocked()
	watchers := runtime.finishedWatchers[key]
	delete(runtime.finishedWatchers, key)
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
	if _, ok := a.agentRunLifecycleService().EnqueueInterrupt(projectID, agentID, item); !ok {
		return AgentInterrupt{}, false
	}
	return item, true
}

func (a *app) drainAgentInterrupts(projectID, agentID, expectedRunID string) []AgentInterrupt {
	items, _ := a.agentRunLifecycleService().DrainInterrupts(projectID, agentID, strings.TrimSpace(expectedRunID))
	return items
}
