package runtime

import (
	"sort"
	"sync"
	"time"
)

type SchedulerQueue struct {
	mu     sync.Mutex
	queues map[string][]string
	jobs   map[string]ScheduledRun
	active map[string]bool
	dedup  map[string]bool
}

func NewSchedulerQueue() *SchedulerQueue {
	return &SchedulerQueue{
		queues: map[string][]string{}, jobs: map[string]ScheduledRun{}, active: map[string]bool{}, dedup: map[string]bool{},
	}
}

func AgentKey(projectID, agentID string) string { return projectID + "/" + agentID }

type EnqueueResult struct {
	Accepted    bool
	StartWorker bool
}

func (queue *SchedulerQueue) Enqueue(job ScheduledRun) EnqueueResult {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if job.DedupKey != "" && queue.dedup[job.DedupKey] {
		return EnqueueResult{}
	}
	if _, exists := queue.jobs[job.ID]; exists {
		return EnqueueResult{}
	}
	key := AgentKey(job.ProjectID, job.AgentID)
	if job.DedupKey != "" {
		queue.dedup[job.DedupKey] = true
	}
	queue.jobs[job.ID] = job
	queue.queues[key] = append(queue.queues[key], job.ID)
	start := !queue.active[key]
	if start {
		queue.active[key] = true
	}
	return EnqueueResult{Accepted: true, StartWorker: start}
}

func (queue *SchedulerQueue) RollbackEnqueue(jobID string, startedWorker bool) bool {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	job, ok := queue.jobs[jobID]
	if !ok {
		return false
	}
	key := AgentKey(job.ProjectID, job.AgentID)
	delete(queue.jobs, jobID)
	if job.DedupKey != "" {
		delete(queue.dedup, job.DedupKey)
	}
	ids := queue.queues[key]
	for i, id := range ids {
		if id == jobID {
			ids = append(ids[:i], ids[i+1:]...)
			break
		}
	}
	if len(ids) == 0 {
		delete(queue.queues, key)
	} else {
		queue.queues[key] = ids
	}
	if startedWorker {
		if len(ids) > 0 {
			queue.active[key] = true
			return true
		}
		delete(queue.active, key)
	}
	return false
}

func (queue *SchedulerQueue) Claim(key string, now time.Time) (ScheduledRun, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	ids := queue.queues[key]
	for len(ids) > 0 {
		id := ids[0]
		ids = ids[1:]
		job, ok := queue.jobs[id]
		if !ok || job.Status != ScheduledQueued {
			continue
		}
		if len(ids) == 0 {
			delete(queue.queues, key)
		} else {
			queue.queues[key] = ids
		}
		now = now.UTC()
		job.Status = ScheduledRunning
		job.StartedAt = &now
		job.UpdatedAt = now
		queue.jobs[id] = job
		return job, true
	}
	delete(queue.queues, key)
	delete(queue.active, key)
	return ScheduledRun{}, false
}

type CompletionOutcome string

const (
	CompletionSucceeded CompletionOutcome = "succeeded"
	CompletionCancelled CompletionOutcome = "cancelled"
	CompletionFailed    CompletionOutcome = "failed"
)

type CompletionResult struct {
	Job           ScheduledRun
	Found         bool
	Requeue       bool
	NotifyFailure bool
}

func (queue *SchedulerQueue) Complete(jobID string, outcome CompletionOutcome, message string, now time.Time) CompletionResult {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	job, ok := queue.jobs[jobID]
	if !ok {
		return CompletionResult{}
	}
	now = now.UTC()
	result := CompletionResult{Job: job, Found: true}
	switch outcome {
	case CompletionSucceeded:
		delete(queue.jobs, jobID)
		if job.DedupKey != "" {
			delete(queue.dedup, job.DedupKey)
		}
	case CompletionCancelled:
		job.Status = ScheduledCancelled
		job.Error = message
		job.UpdatedAt = now
		queue.jobs[jobID] = job
		if job.DedupKey != "" {
			delete(queue.dedup, job.DedupKey)
		}
		result.Job = job
	default:
		job.Attempt++
		job.Error = message
		job.UpdatedAt = now
		job.LastFailedAt = &now
		job.StartedAt = nil
		if job.Attempt < job.MaxAttempts && !job.EffectsStarted {
			job.Status = ScheduledQueued
			key := AgentKey(job.ProjectID, job.AgentID)
			queue.queues[key] = append(queue.queues[key], job.ID)
			result.Requeue = true
		} else {
			job.Status = ScheduledFailed
			result.NotifyFailure = job.Kind == ScheduledHandoff
			if job.DedupKey != "" {
				delete(queue.dedup, job.DedupKey)
			}
		}
		queue.jobs[jobID] = job
		result.Job = job
	}
	return result
}

type RecoveryResult struct {
	Normalized       bool
	Jobs             []ScheduledRun
	TerminalFailures []ScheduledRun
}

func (queue *SchedulerQueue) Recover(jobs []ScheduledRun, now time.Time) RecoveryResult {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.queues = map[string][]string{}
	queue.jobs = map[string]ScheduledRun{}
	queue.active = map[string]bool{}
	queue.dedup = map[string]bool{}
	sort.SliceStable(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
	})
	result := RecoveryResult{}
	for _, job := range jobs {
		if job.ID == "" || job.ProjectID == "" || job.AgentID == "" {
			result.Normalized = true
			continue
		}
		if job.MaxAttempts <= 0 {
			job.MaxAttempts = 3
			result.Normalized = true
		}
		switch job.Status {
		case ScheduledCancelled:
			result.Normalized = true
			continue
		case ScheduledRunning:
			if job.EffectsStarted {
				job.Attempt++
				job.StartedAt = nil
				job.Error = "automatic retry suppressed after interrupted side effects"
				job.UpdatedAt = now.UTC()
				job.Status = ScheduledFailed
				queue.jobs[job.ID] = job
				result.TerminalFailures = append(result.TerminalFailures, job)
				result.Normalized = true
				continue
			}
			job.Attempt++
			job.StartedAt = nil
			job.Error = "requeued after interrupted process"
			job.UpdatedAt = now.UTC()
			job.Status = ScheduledQueued
			result.Normalized = true
		case ScheduledFailed:
			queue.jobs[job.ID] = job
			continue
		default:
			job.Status = ScheduledQueued
		}
		if job.Attempt >= job.MaxAttempts {
			job.Status = ScheduledFailed
			queue.jobs[job.ID] = job
			result.TerminalFailures = append(result.TerminalFailures, job)
			result.Normalized = true
			continue
		}
		queue.jobs[job.ID] = job
		if job.DedupKey != "" {
			queue.dedup[job.DedupKey] = true
		}
		key := AgentKey(job.ProjectID, job.AgentID)
		queue.queues[key] = append(queue.queues[key], job.ID)
		result.Jobs = append(result.Jobs, job)
	}
	return result
}

func (queue *SchedulerQueue) MarkEffectsStarted(jobID string, now time.Time) (ScheduledRun, bool, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	job, ok := queue.jobs[jobID]
	if !ok || job.Status != ScheduledRunning {
		return ScheduledRun{}, false, false
	}
	if job.EffectsStarted {
		return job, true, false
	}
	now = now.UTC()
	job.EffectsStarted = true
	job.EffectsStartedAt = &now
	job.UpdatedAt = now
	queue.jobs[jobID] = job
	return job, true, true
}

func (queue *SchedulerQueue) ResumeKeys() []string {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	var keys []string
	for key, ids := range queue.queues {
		if len(ids) == 0 || queue.active[key] {
			continue
		}
		queue.active[key] = true
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (queue *SchedulerQueue) CancelAgent(projectID, agentID, reason string, now time.Time) []ScheduledRun {
	key := AgentKey(projectID, agentID)
	queue.mu.Lock()
	defer queue.mu.Unlock()
	var cancelled []ScheduledRun
	for _, id := range queue.queues[key] {
		job, ok := queue.jobs[id]
		if !ok {
			continue
		}
		job.Status = ScheduledCancelled
		job.Error = reason
		job.UpdatedAt = now.UTC()
		queue.jobs[id] = job
		if job.DedupKey != "" {
			delete(queue.dedup, job.DedupKey)
		}
		cancelled = append(cancelled, job)
	}
	delete(queue.queues, key)
	return cancelled
}

func (queue *SchedulerQueue) Jobs() []ScheduledRun {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	jobs := make([]ScheduledRun, 0, len(queue.jobs))
	for _, job := range queue.jobs {
		jobs = append(jobs, job)
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
	})
	return jobs
}

func (queue *SchedulerQueue) Job(id string) (ScheduledRun, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	job, ok := queue.jobs[id]
	return job, ok
}

func (queue *SchedulerQueue) QueueIDs(key string) []string {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return append([]string{}, queue.queues[key]...)
}

func (queue *SchedulerQueue) PendingCount(key string) int {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return len(queue.queues[key])
}

func (queue *SchedulerQueue) WorkerActive(key string) bool {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return queue.active[key]
}

func (queue *SchedulerQueue) HasDedup(key string) bool {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return queue.dedup[key]
}
