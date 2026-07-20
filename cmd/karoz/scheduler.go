package main

import (
	"context"
	"encoding/json"
	"errors"
	persistenceadapter "github.com/karoz/karoz/internal/persistence"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	"log"
	"strings"
	"time"
)

const (
	ScheduledRunHandoff       = runtimedomain.ScheduledHandoff
	ScheduledRunTaskEvent     = runtimedomain.ScheduledTaskEvent
	ScheduledRunPlanEvent     = runtimedomain.ScheduledPlanEvent
	ScheduledRunIdleReconcile = runtimedomain.ScheduledIdleReconcile
)

const (
	ScheduledRunQueued    = runtimedomain.ScheduledQueued
	ScheduledRunRunning   = runtimedomain.ScheduledRunning
	ScheduledRunFailed    = runtimedomain.ScheduledFailed
	ScheduledRunCancelled = runtimedomain.ScheduledCancelled
)

type ScheduledRunKind = runtimedomain.ScheduledKind
type ScheduledRunStatus = runtimedomain.ScheduledStatus
type ScheduledRun = runtimedomain.ScheduledRun

type HandoffRunPayload struct {
	InboxMessageID string `json:"inbox_message_id"`
}

type TaskEventRunPayload struct {
	TaskID string `json:"task_id"`
	HookID string `json:"hook_id"`
}

type PlanEventRunPayload struct {
	PlanID      string `json:"plan_id"`
	PlanVersion int64  `json:"plan_version,omitempty"`
	StepID      string `json:"step_id,omitempty"`
	Event       string `json:"event"`
	TaskID      string `json:"task_id,omitempty"`
}

type IdleReconcileRunPayload struct {
	Reason string `json:"reason"`
}

type ScheduledRunExecutor func(context.Context, ScheduledRun) error

type scheduledRunSnapshot struct {
	Jobs []ScheduledRun `json:"jobs"`
}

func newScheduledRun(kind ScheduledRunKind, input AgentRunInput, dedupKey string, payload any, timeout time.Duration) (ScheduledRun, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return ScheduledRun{}, err
	}
	now := time.Now().UTC()
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	return ScheduledRun{
		ID:          firstNonEmpty(input.RunID, randomID()),
		ProjectID:   strings.TrimSpace(input.ProjectID),
		AgentID:     strings.TrimSpace(input.AgentID),
		Kind:        kind,
		Trigger:     normalizeRunTrigger(input.Trigger),
		TurnType:    normalizeChatTurnType(input.TurnType),
		SourceID:    strings.TrimSpace(input.SourceID),
		MessageID:   strings.TrimSpace(input.MessageID),
		DedupKey:    strings.TrimSpace(dedupKey),
		Payload:     raw,
		Status:      ScheduledRunQueued,
		MaxAttempts: 3,
		TimeoutMS:   timeout.Milliseconds(),
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

func (a *app) scheduleAgentRun(job ScheduledRun) (string, bool) {
	job.ProjectID = strings.TrimSpace(job.ProjectID)
	job.AgentID = strings.TrimSpace(job.AgentID)
	job.DedupKey = strings.TrimSpace(job.DedupKey)
	if job.ProjectID == "" || job.AgentID == "" || strings.TrimSpace(string(job.Kind)) == "" {
		return "", false
	}
	if strings.TrimSpace(job.ID) == "" {
		job.ID = randomID()
	}
	job.Trigger = normalizeRunTrigger(job.Trigger)
	job.TurnType = normalizeChatTurnType(job.TurnType)
	if job.MaxAttempts <= 0 {
		job.MaxAttempts = 3
	}
	if job.TimeoutMS <= 0 {
		job.TimeoutMS = (3 * time.Minute).Milliseconds()
	}
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	job.Status = ScheduledRunQueued
	key := projectAgentKey(job.ProjectID, job.AgentID)
	queue := a.ensureSchedulerQueue()
	enqueued := queue.Enqueue(job)
	if !enqueued.Accepted {
		return job.ID, false
	}
	if err := a.saveScheduledRuns(); err != nil {
		log.Printf("save scheduled runs: %v", err)
		if queue.RollbackEnqueue(job.ID, enqueued.StartWorker) {
			go a.runScheduledAgentQueue(key)
		}
		return job.ID, false
	}
	a.emitScheduledRunQueued(job)
	if enqueued.StartWorker {
		go a.runScheduledAgentQueue(key)
	}
	return job.ID, true
}

func (a *app) runScheduledAgentQueue(key string) {
	a.newSchedulerWorker().Run(context.Background(), key)
}

func (a *app) newSchedulerWorker() *runtimedomain.SchedulerWorker {
	return runtimedomain.NewSchedulerWorker(a.ensureSchedulerQueue(), runtimedomain.SchedulerWorkerHooks{
		Begin: func(job ScheduledRun) bool {
			_, started := a.beginAgentRun(job.RunInput())
			return started
		},
		Bind: func(ctx context.Context, job ScheduledRun) (context.Context, bool) {
			return a.bindAgentRunContext(ctx, job.ProjectID, job.AgentID, job.ID)
		},
		Execute: a.executeScheduledRun,
		Finish: func(job ScheduledRun, runErr error) {
			if runErr != nil {
				a.finishAgentRun(job.ProjectID, job.AgentID, job.ID, RunStateFailed, runErr)
				return
			}
			a.transitionAgentRun(job.ProjectID, job.AgentID, job.ID, RunStateCompleting)
			a.finishAgentRun(job.ProjectID, job.AgentID, job.ID, RunStateDone, nil)
		},
		Claimed: func(ScheduledRun) {
			if err := a.saveScheduledRuns(); err != nil {
				log.Printf("save claimed scheduled run: %v", err)
			}
		},
		Completed: a.handleScheduledRunCompletion,
		RunFailed: func(job ScheduledRun, runErr error) {
			if !errors.Is(runErr, context.Canceled) {
				log.Printf("scheduled agent run failed project=%s agent=%s kind=%s run=%s: %v", job.ProjectID, job.AgentID, job.Kind, job.ID, runErr)
			}
		},
	})
}

func (a *app) handleScheduledRunCompletion(completed runtimedomain.CompletionResult) {
	if err := a.saveScheduledRuns(); err != nil {
		log.Printf("save completed scheduled run: %v", err)
	}
	if completed.Requeue {
		if completed.Job.Kind == ScheduledRunHandoff {
			a.retryHandoff(completed.Job.ProjectID, completed.Job.AgentID, firstNonEmpty(completed.Job.MessageID, handoffMessageID(completed.Job)))
		}
		a.emitScheduledRunQueued(completed.Job)
	}
	if completed.NotifyFailure {
		a.notifyFailedHandoff(completed.Job)
	}
}

func handoffMessageID(job ScheduledRun) string {
	var payload HandoffRunPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return ""
	}
	return payload.InboxMessageID
}

func (a *app) executeScheduledRun(ctx context.Context, job ScheduledRun) error {
	a.mu.Lock()
	override := a.schedulerExecutors[job.Kind]
	a.mu.Unlock()
	if override != nil {
		return override(ctx, job)
	}
	switch job.Kind {
	case ScheduledRunHandoff:
		return a.executeHandoffScheduledRun(ctx, job)
	case ScheduledRunTaskEvent:
		return a.executeTaskEventScheduledRun(ctx, job)
	case ScheduledRunPlanEvent:
		return a.executePlanEventScheduledRun(ctx, job)
	case ScheduledRunIdleReconcile:
		return a.executeIdleReconcileScheduledRun(ctx, job)
	default:
		return errors.New("unknown scheduled run kind: " + string(job.Kind))
	}
}

func (a *app) emitScheduledRunQueued(job ScheduledRun) {
	a.emitRuntimeStateChanged(RuntimeEvent{
		ID:        randomID(),
		ProjectID: job.ProjectID,
		Kind:      "agent_run_queued",
		EntityID:  job.AgentID,
		RunID:     job.ID,
		Trigger:   string(job.Trigger),
		From:      "idle",
		To:        string(RunStateQueued),
		Reason:    string(job.Trigger),
		CreatedAt: time.Now().UTC(),
	})
}

func (a *app) ensureSchedulerQueue() *runtimedomain.SchedulerQueue {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.schedulerQueue == nil {
		a.schedulerQueue = runtimedomain.NewSchedulerQueue()
	}
	return a.schedulerQueue
}

func (a *app) ensureSchedulerExecutorsLocked() {
	if a.schedulerExecutors == nil {
		a.schedulerExecutors = map[ScheduledRunKind]ScheduledRunExecutor{}
	}
}

func (a *app) scheduledAgentRunCount(projectID, agentID string) int {
	key := projectAgentKey(projectID, agentID)
	return a.ensureSchedulerQueue().PendingCount(key)
}

func (a *app) scheduledAgentWorkerActive(projectID, agentID string) bool {
	key := projectAgentKey(projectID, agentID)
	return a.ensureSchedulerQueue().WorkerActive(key)
}

func (a *app) saveScheduledRuns() error {
	a.schedulerPersistMu.Lock()
	defer a.schedulerPersistMu.Unlock()
	jobs := a.ensureSchedulerQueue().Jobs()
	return persistenceadapter.NewJSONStore(a.settings.DataDir).Save("agent-run-queue.json", scheduledRunSnapshot{Jobs: jobs}, 0644)
}

func (a *app) markScheduledRunEffectsStarted(runID string) error {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	_, found, changed := a.ensureSchedulerQueue().MarkEffectsStarted(runID, time.Now().UTC())
	if !found || !changed {
		return nil
	}
	return a.saveScheduledRuns()
}

func (a *app) loadScheduledRuns() error {
	var snapshot scheduledRunSnapshot
	found, err := persistenceadapter.NewJSONStore(a.settings.DataDir).Load("agent-run-queue.json", &snapshot)
	if err != nil || !found {
		return err
	}
	recovery := a.ensureSchedulerQueue().Recover(snapshot.Jobs, time.Now().UTC())
	for _, job := range recovery.TerminalFailures {
		if job.Kind == ScheduledRunHandoff {
			a.notifyFailedHandoff(job)
		}
	}
	for _, job := range recovery.Jobs {
		if job.Kind != ScheduledRunHandoff {
			continue
		}
		messageID := firstNonEmpty(job.MessageID, handoffMessageID(job))
		if msg, ok := a.inboxMessage(job.ProjectID, job.AgentID, messageID); ok && (msg.Status == HandoffClaimed || msg.Status == HandoffWorking || msg.Status == HandoffFailed) {
			a.retryHandoff(job.ProjectID, job.AgentID, messageID)
		}
	}
	if recovery.Normalized {
		return a.saveScheduledRuns()
	}
	return nil
}

func (a *app) resumeScheduledRuns() {
	keys := a.ensureSchedulerQueue().ResumeKeys()
	for _, key := range keys {
		go a.runScheduledAgentQueue(key)
	}
}
