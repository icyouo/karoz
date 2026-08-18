package task

import (
	"context"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	"strings"
	"sync"
	"time"
)

const DefaultMaxRuntime = time.Hour

// Repository is the durable task state port. The application owns the
// concrete persistence today; this interface lets task lifecycle code stop
// reaching into the app's aggregate map directly.
type Repository interface {
	List(projectID string) []Task
	Find(projectID, taskID string) (Task, bool)
	Insert(Task)
	Update(Task) bool
	Mutate(projectID, taskID string, mutate func(*Task) bool) (Task, bool)
}

type Service struct {
	repository Repository
	runtimeMu  sync.Mutex
	runs       map[string]taskRun
	nextToken  uint64
	now        func() time.Time
	defaultMax time.Duration
}

type taskRun struct {
	token  uint64
	cancel context.CancelFunc
}

func NewService(repository Repository) *Service {
	return &Service{
		repository: repository,
		runs:       make(map[string]taskRun),
		now:        func() time.Time { return time.Now().UTC() },
		defaultMax: DefaultMaxRuntime,
	}
}

func taskRunKey(projectID, taskID string) string { return projectID + "/" + taskID }

func (service *Service) List(projectID string) []Task {
	if service == nil || service.repository == nil {
		return nil
	}
	return service.repository.List(projectID)
}

func (service *Service) Find(projectID, taskID string) (Task, bool) {
	if service == nil || service.repository == nil {
		return Task{}, false
	}
	return service.repository.Find(projectID, taskID)
}

func (service *Service) Insert(task Task) {
	if service == nil || service.repository == nil {
		return
	}
	service.repository.Insert(task)
}

func (service *Service) Update(task Task) bool {
	if service == nil || service.repository == nil {
		return false
	}
	return service.repository.Update(task)
}

func IsRunnable(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "pending", "failed", "deploy_failed":
		return true
	default:
		return false
	}
}

// Claim atomically reserves a runnable task and installs its cancellation
// handle before any caller can observe the running state.
func (service *Service) Claim(projectID, taskID string) (Task, context.Context, func(), bool) {
	if service == nil || service.repository == nil {
		return Task{}, nil, nil, false
	}
	service.runtimeMu.Lock()
	defer service.runtimeMu.Unlock()
	if service.runs == nil {
		service.runs = make(map[string]taskRun)
	}
	key := taskRunKey(projectID, taskID)
	if _, active := service.runs[key]; active {
		return Task{}, nil, nil, false
	}
	var ctx context.Context
	var cancel context.CancelFunc
	claimed, ok := service.repository.Mutate(projectID, taskID, func(task *Task) bool {
		if !IsRunnable(task.Status) {
			return false
		}
		maximum := service.defaultMax
		if maximum <= 0 {
			maximum = DefaultMaxRuntime
		}
		ctx, cancel = runtimedomain.DeadlineFromMilliseconds(task.MaxRuntimeMS, maximum).Bind(context.Background())
		task.Status = "running"
		task.FailureSummary = ""
		task.Result = ""
		now := time.Now
		if service.now != nil {
			now = service.now
		}
		startedAt := now().UTC()
		task.StartedAt = &startedAt
		task.UpdatedAt = startedAt
		return true
	})
	if !ok || cancel == nil {
		if cancel != nil {
			cancel()
		}
		return Task{}, nil, nil, false
	}
	service.nextToken++
	service.runs[key] = taskRun{token: service.nextToken, cancel: cancel}
	token := service.nextToken
	finish := func() {
		service.runtimeMu.Lock()
		current, active := service.runs[key]
		if active && current.token == token {
			delete(service.runs, key)
		}
		service.runtimeMu.Unlock()
		cancel()
	}
	return claimed, ctx, finish, true
}

func (service *Service) Cancel(projectID, taskID string) bool {
	if service == nil {
		return false
	}
	service.runtimeMu.Lock()
	run, active := service.runs[taskRunKey(projectID, taskID)]
	service.runtimeMu.Unlock()
	if !active || run.cancel == nil {
		return false
	}
	run.cancel()
	return true
}

// WithRuntimeLock coordinates protected task transitions (for example, the
// handoff into a merge critical section) with Claim and Cancel.
func (service *Service) WithRuntimeLock(fn func()) {
	if service == nil || fn == nil {
		return
	}
	service.runtimeMu.Lock()
	defer service.runtimeMu.Unlock()
	fn()
}
