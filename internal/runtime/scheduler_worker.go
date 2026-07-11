package runtime

import (
	"context"
	"errors"
	"time"
)

type SchedulerWorkerHooks struct {
	Begin     func(ScheduledRun) bool
	Bind      func(context.Context, ScheduledRun) (context.Context, bool)
	Execute   func(context.Context, ScheduledRun) error
	Finish    func(ScheduledRun, error)
	Claimed   func(ScheduledRun)
	Completed func(CompletionResult)
	RunFailed func(ScheduledRun, error)
}

type SchedulerWorker struct {
	queue          *SchedulerQueue
	hooks          SchedulerWorkerHooks
	now            func() time.Time
	retryInterval  time.Duration
	defaultTimeout time.Duration
}

func NewSchedulerWorker(queue *SchedulerQueue, hooks SchedulerWorkerHooks) *SchedulerWorker {
	return &SchedulerWorker{
		queue: queue, hooks: hooks, now: time.Now,
		retryInterval: 25 * time.Millisecond, defaultTimeout: 3 * time.Minute,
	}
}

func (worker *SchedulerWorker) Run(ctx context.Context, key string) {
	if worker == nil || worker.queue == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		job, ok := worker.queue.Claim(key, worker.now().UTC())
		if !ok {
			return
		}
		if worker.hooks.Claimed != nil {
			worker.hooks.Claimed(job)
		}
		if !worker.waitUntilStarted(ctx, job) {
			worker.complete(job, ctx.Err())
			return
		}

		runCtx, cancel := context.WithTimeout(ctx, job.Timeout(worker.defaultTimeout))
		boundCtx := runCtx
		bound := true
		if worker.hooks.Bind != nil {
			boundCtx, bound = worker.hooks.Bind(runCtx, job)
		}
		if !bound {
			cancel()
			worker.complete(job, errors.New("active run disappeared before scheduler context binding"))
			continue
		}
		var runErr error
		if worker.hooks.Execute == nil {
			runErr = errors.New("scheduled run executor is not configured")
		} else {
			runErr = worker.hooks.Execute(boundCtx, job)
		}
		if runErr == nil && boundCtx.Err() != nil {
			runErr = boundCtx.Err()
		}
		cancel()
		if worker.hooks.Finish != nil {
			worker.hooks.Finish(job, runErr)
		}
		if runErr != nil && worker.hooks.RunFailed != nil {
			worker.hooks.RunFailed(job, runErr)
		}
		worker.complete(job, runErr)
	}
}

func (worker *SchedulerWorker) waitUntilStarted(ctx context.Context, job ScheduledRun) bool {
	if worker.hooks.Begin == nil || worker.hooks.Begin(job) {
		return true
	}
	ticker := time.NewTicker(worker.retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if worker.hooks.Begin(job) {
				return true
			}
		}
	}
}

func (worker *SchedulerWorker) complete(job ScheduledRun, runErr error) {
	outcome := CompletionSucceeded
	message := ""
	if errors.Is(runErr, context.Canceled) {
		outcome = CompletionCancelled
		message = runErr.Error()
	} else if runErr != nil {
		outcome = CompletionFailed
		message = runErr.Error()
	}
	completed := worker.queue.Complete(job.ID, outcome, message, worker.now().UTC())
	if completed.Found && worker.hooks.Completed != nil {
		worker.hooks.Completed(completed)
	}
}
