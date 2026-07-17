package runtime

import (
	"encoding/json"
	"time"
)

type Trigger string

const (
	TriggerUserDirect Trigger = "user_direct"
	TriggerHandoff    Trigger = "handoff"
	TriggerTaskEvent  Trigger = "task_event"
	TriggerSystem     Trigger = "system"
)

func NormalizeTrigger(trigger Trigger) Trigger {
	switch trigger {
	case TriggerUserDirect, TriggerHandoff, TriggerTaskEvent, TriggerSystem:
		return trigger
	default:
		return TriggerSystem
	}
}

type State string

const (
	StateQueued           State = "queued"
	StatePreparingContext State = "preparing_context"
	StateInvokingModel    State = "invoking_model"
	StateExecutingTool    State = "executing_tool"
	StateWaitingModel     State = "waiting_model"
	StateCompleting       State = "completing"
	StateDone             State = "done"
	StateFailed           State = "failed"
	StateCancelled        State = "cancelled"
)

func (state State) Active() bool {
	switch state {
	case StateDone, StateFailed, StateCancelled:
		return false
	default:
		return true
	}
}

type RunInput struct {
	RunID     string  `json:"run_id,omitempty"`
	ProjectID string  `json:"project_id"`
	AgentID   string  `json:"agent_id"`
	Trigger   Trigger `json:"trigger"`
	TurnType  string  `json:"turn_type,omitempty"`
	SourceID  string  `json:"source_id,omitempty"`
	MessageID string  `json:"message_id,omitempty"`
}

type Interrupt struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	AgentID   string    `json:"agent_id"`
	MessageID string    `json:"message_id"`
	Body      string    `json:"body"`
	TurnType  string    `json:"turn_type"`
	CreatedAt time.Time `json:"created_at"`
}

type Run struct {
	ID         string      `json:"id"`
	ProjectID  string      `json:"project_id"`
	AgentID    string      `json:"agent_id"`
	Trigger    Trigger     `json:"trigger"`
	TurnType   string      `json:"turn_type,omitempty"`
	SourceID   string      `json:"source_id,omitempty"`
	MessageID  string      `json:"message_id,omitempty"`
	State      State       `json:"state"`
	Error      string      `json:"error,omitempty"`
	StartedAt  time.Time   `json:"started_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
	EndedAt    *time.Time  `json:"ended_at,omitempty"`
	Interrupts []Interrupt `json:"interrupts,omitempty"`
}

type Event struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Kind      string    `json:"kind"`
	EntityID  string    `json:"entity_id"`
	RunID     string    `json:"run_id,omitempty"`
	Trigger   string    `json:"trigger,omitempty"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type ScheduledKind string

const (
	ScheduledHandoff       ScheduledKind = "handoff"
	ScheduledTaskEvent     ScheduledKind = "task_event"
	ScheduledIdleReconcile ScheduledKind = "idle_reconcile"
)

type ScheduledStatus string

const (
	ScheduledQueued    ScheduledStatus = "queued"
	ScheduledRunning   ScheduledStatus = "running"
	ScheduledFailed    ScheduledStatus = "failed"
	ScheduledCancelled ScheduledStatus = "cancelled"
)

type ScheduledRun struct {
	ID               string          `json:"id"`
	ProjectID        string          `json:"project_id"`
	AgentID          string          `json:"agent_id"`
	Kind             ScheduledKind   `json:"kind"`
	Trigger          Trigger         `json:"trigger"`
	TurnType         string          `json:"turn_type,omitempty"`
	SourceID         string          `json:"source_id,omitempty"`
	MessageID        string          `json:"message_id,omitempty"`
	DedupKey         string          `json:"dedup_key,omitempty"`
	Payload          json.RawMessage `json:"payload,omitempty"`
	Status           ScheduledStatus `json:"status"`
	Attempt          int             `json:"attempt"`
	MaxAttempts      int             `json:"max_attempts"`
	TimeoutMS        int64           `json:"timeout_ms"`
	Error            string          `json:"error,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	StartedAt        *time.Time      `json:"started_at,omitempty"`
	LastFailedAt     *time.Time      `json:"last_failed_at,omitempty"`
	EffectsStarted   bool            `json:"effects_started,omitempty"`
	EffectsStartedAt *time.Time      `json:"effects_started_at,omitempty"`
}

func (job ScheduledRun) Timeout(defaultTimeout time.Duration) time.Duration {
	if job.TimeoutMS <= 0 {
		return defaultTimeout
	}
	return time.Duration(job.TimeoutMS) * time.Millisecond
}

func (job ScheduledRun) RunInput() RunInput {
	return RunInput{
		RunID: job.ID, ProjectID: job.ProjectID, AgentID: job.AgentID, Trigger: job.Trigger,
		TurnType: job.TurnType, SourceID: job.SourceID, MessageID: job.MessageID,
	}
}
