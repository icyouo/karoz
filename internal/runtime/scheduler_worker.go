package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type SchedulerWorkerHooks struct {
	Begin              func(ScheduledRun) bool
	WaitForRunFinished func(context.Context, ScheduledRun) bool
	Bind               func(context.Context, ScheduledRun) (context.Context, bool)
	Execute            func(context.Context, ScheduledRun) error
	Finish             func(ScheduledRun, error)
	Claimed            func(ScheduledRun)
	Completed          func(CompletionResult)
	RunFailed          func(ScheduledRun, error)
}

type SchedulerWorker struct {
	queue            *SchedulerQueue
	hooks            SchedulerWorkerHooks
	now              func() time.Time
	defaultTimeout   time.Duration
	defaultStartWait time.Duration
}

func NewSchedulerWorker(queue *SchedulerQueue, hooks SchedulerWorkerHooks) *SchedulerWorker {
	return &SchedulerWorker{
		queue: queue, hooks: hooks, now: time.Now,
		defaultTimeout: 3 * time.Minute,
		// Waiting for a busy agent is queue admission, not resident execution.
		// Keep it bounded for legacy and manually constructed jobs, but never
		// charge it to the selected turn's provider/tool/final-response budget.
		defaultStartWait: 3 * time.Minute,
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
		claimedAt := worker.now().UTC()
		if job.StartedAt != nil {
			claimedAt = job.StartedAt.UTC()
		}
		startDeadline := claimedAt.Add(job.StartWait(worker.defaultStartWait))
		startCtx, cancelStart := context.WithTimeout(ctx, startDeadline.Sub(worker.now().UTC()))
		startErr := worker.waitUntilStarted(startCtx, job)
		cancelStart()
		if startErr != nil {
			if worker.hooks.RunFailed != nil && !errors.Is(startErr, context.Canceled) {
				worker.hooks.RunFailed(job, startErr)
			}
			worker.complete(job, startErr)
			if ctx.Err() != nil {
				return
			}
			continue
		}

		// Bind the Run-owned cancellation context before adding the scheduler
		// timeout. A scheduler Begin may have installed its explicit cancel
		// handle atomically; deriving the deadline in the opposite order would
		// replace that ownership and re-open a cancel-before-Bind race.
		boundCtx := ctx
		bound := true
		if worker.hooks.Bind != nil {
			boundCtx, bound = worker.hooks.Bind(ctx, job)
		}
		if !bound {
			worker.complete(job, errors.New("active run disappeared before scheduler context binding"))
			continue
		}
		// The resident execution window begins only after the agent becomes
		// available and its Run-owned context is bound. A busy-agent wait has a
		// separate deadline above, so it cannot eat into provider/tool work or
		// the final-response reserve.
		runCtx, cancel := context.WithTimeout(boundCtx, job.Timeout(worker.defaultTimeout))
		if err := runCtx.Err(); err != nil {
			cancel()
			if worker.hooks.Finish != nil {
				worker.hooks.Finish(job, err)
			}
			if worker.hooks.RunFailed != nil {
				worker.hooks.RunFailed(job, err)
			}
			worker.complete(job, err)
			continue
		}
		var runErr error
		if worker.hooks.Execute == nil {
			runErr = errors.New("scheduled run executor is not configured")
		} else {
			// Execute must receive the execution-deadline child, not only the
			// Run-owned cancellation context. The latter preserves explicit
			// cancellation, while runCtx enforces the selected turn total.
			runErr = worker.hooks.Execute(runCtx, job)
		}
		if runErr == nil && runCtx.Err() != nil {
			runErr = runCtx.Err()
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

func (worker *SchedulerWorker) waitUntilStarted(ctx context.Context, job ScheduledRun) error {
	for {
		if err := ctx.Err(); err != nil {
			return schedulerStartWaitError(err)
		}
		if worker.hooks.Begin == nil || worker.hooks.Begin(job) {
			return nil
		}
		if worker.hooks.WaitForRunFinished == nil {
			<-ctx.Done()
			return schedulerStartWaitError(ctx.Err())
		}
		if !worker.hooks.WaitForRunFinished(ctx, job) {
			return schedulerStartWaitError(ctx.Err())
		}
	}
}

func schedulerStartWaitError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("scheduled Run start deadline exceeded while waiting for the active agent: %w", err)
	}
	return err
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
