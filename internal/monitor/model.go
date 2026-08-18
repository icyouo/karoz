package monitor

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	MaxEventKinds      = 16
	MaxSourceGaps      = 16
	MaxPendingFires    = 32
	MaxEventPayload    = 16 * 1024
	MaxProbeDetail     = 1024
	DefaultCooldownMS  = int64(60_000)
	MinimumCooldownMS  = int64(5_000)
	RateWindow         = 5 * time.Minute
	MaxFiresRateWindow = 6
)

type TriggerKind string

const (
	TriggerRuntimeEvent  TriggerKind = "runtime_event"
	TriggerProcessExit   TriggerKind = "process_exit"
	TriggerProcessOutput TriggerKind = "process_output"
	TriggerScriptProbe   TriggerKind = "script_probe"
)

type ActionKind string

const (
	ActionNotifyAgent ActionKind = "notify_agent"
	ActionBlackboard  ActionKind = "blackboard"
)

type Action struct {
	Revision int        `json:"revision"`
	Kind     ActionKind `json:"kind"`
	AgentID  string     `json:"agent_id,omitempty"`
	TurnType string     `json:"turn_type,omitempty"`
	Topic    string     `json:"topic,omitempty"`
	Template string     `json:"template,omitempty"`
}

type State string

const (
	StateActive    State = "active"
	StateDisabled  State = "disabled"
	StateExhausted State = "exhausted"
	StateExpired   State = "expired"
	StateError     State = "error"
)

type Trigger struct {
	Kind     TriggerKind `json:"kind"`
	Revision int         `json:"revision"`

	EventKinds           []string `json:"event_kinds,omitempty"`
	EntityID             string   `json:"entity_id,omitempty"`
	FromState            string   `json:"from_state,omitempty"`
	ToState              string   `json:"to_state,omitempty"`
	IncludeMonitorEvents bool     `json:"include_monitor_events,omitempty"`

	ProcessID   string `json:"process_id,omitempty"`
	FailureOnly bool   `json:"failure_only,omitempty"`
	Pattern     string `json:"pattern,omitempty"`

	ProbeLanguage     string `json:"probe_language,omitempty"`
	ProbePath         string `json:"probe_path,omitempty"`
	ProbeSHA256       string `json:"probe_sha256,omitempty"`
	IntervalMS        int64  `json:"interval_ms,omitempty"`
	TimeoutMS         int64  `json:"timeout_ms,omitempty"`
	ApprovalReceiptID string `json:"approval_receipt_id,omitempty"`
}

type Monitor struct {
	ID        string  `json:"id"`
	ProjectID string  `json:"project_id"`
	AgentID   string  `json:"agent_id"`
	Name      string  `json:"name"`
	Revision  int     `json:"revision"`
	Trigger   Trigger `json:"trigger"`
	Action    Action  `json:"action"`
	State     State   `json:"state"`

	CooldownMS  int64 `json:"cooldown_ms"`
	MaxTriggers int   `json:"max_triggers,omitempty"`

	TriggerCount           int                        `json:"trigger_count"`
	Sequence               int                        `json:"sequence"`
	LastMatch              string                     `json:"last_match,omitempty"`
	LastError              string                     `json:"last_error,omitempty"`
	ErrorCode              string                     `json:"error_code,omitempty"`
	SourceGaps             map[string]SourceGapStatus `json:"source_gaps,omitempty"`
	LastCheckedAt          *time.Time                 `json:"last_checked_at,omitempty"`
	ConsecutiveProbeErrors int                        `json:"consecutive_probe_errors"`
	LastFiredAt            *time.Time                 `json:"last_fired_at,omitempty"`
	RecentFires            []time.Time                `json:"recent_fires,omitempty"`
	PendingFires           []PendingFire              `json:"pending_fires,omitempty"`

	NextCheckAt  *time.Time `json:"next_check_at,omitempty"`
	ProbeRunning bool       `json:"-"`

	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type ProbeApprovalReceipt struct {
	ID                string     `json:"id"`
	ProjectID         string     `json:"project_id"`
	AgentID           string     `json:"agent_id"`
	MonitorID         string     `json:"monitor_id"`
	TriggerRevision   int        `json:"trigger_revision"`
	ApprovalFlow      string     `json:"approval_flow"`
	ApprovalRunID     string     `json:"approval_run_id,omitempty"`
	ChoiceRequestID   string     `json:"choice_request_id,omitempty"`
	OperatorSessionID string     `json:"operator_session_id,omitempty"`
	ChallengeID       string     `json:"challenge_id,omitempty"`
	CanonicalWorkdir  string     `json:"canonical_workdir"`
	Language          string     `json:"language"`
	SnapshotPath      string     `json:"snapshot_path"`
	SnapshotDevice    uint64     `json:"snapshot_device"`
	SnapshotInode     uint64     `json:"snapshot_inode"`
	NormalizedSource  []byte     `json:"normalized_source"`
	SourceSHA256      string     `json:"source_sha256"`
	IntervalMS        int64      `json:"interval_ms"`
	TimeoutMS         int64      `json:"timeout_ms"`
	ApprovedBy        string     `json:"approved_by"`
	ApprovedAt        time.Time  `json:"approved_at"`
	ExpiresAt         time.Time  `json:"expires_at"`
	ClaimedMutationID string     `json:"claimed_mutation_id,omitempty"`
	ClaimedAt         *time.Time `json:"claimed_at,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
}

type Origin struct {
	Kind      string `json:"kind"`
	MonitorID string `json:"monitor_id,omitempty"`
	FireID    string `json:"fire_id,omitempty"`
}

func (origin Origin) Validate() error {
	switch origin.Kind {
	case "monitor":
		if strings.TrimSpace(origin.MonitorID) == "" || strings.TrimSpace(origin.FireID) == "" {
			return errors.New("monitor origin requires monitor_id and fire_id")
		}
	case "user", "runtime":
		if origin.MonitorID != "" || origin.FireID != "" {
			return errors.New("user/runtime origin forbids monitor_id and fire_id")
		}
	default:
		return errors.New("invalid origin kind")
	}
	return nil
}

func (event Event) Validate() error {
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.ProjectID) == "" ||
		strings.TrimSpace(event.AuthorityID) == "" ||
		strings.TrimSpace(event.Kind) == "" || strings.TrimSpace(event.EntityID) == "" ||
		event.AuthorityGeneration == 0 {
		return errors.New("event identity is incomplete")
	}
	if err := event.Origin.Validate(); err != nil {
		return err
	}
	if len(event.Payload) > MaxEventPayload || len(event.Payload) > 0 && !json.Valid(event.Payload) {
		return errors.New("event payload is invalid")
	}
	return nil
}

type Event struct {
	ID                  string          `json:"id"`
	ProjectID           string          `json:"project_id"`
	AuthorityID         string          `json:"authority_id"`
	AuthorityGeneration uint64          `json:"authority_generation"`
	Kind                string          `json:"kind"`
	EntityID            string          `json:"entity_id"`
	Origin              Origin          `json:"origin"`
	At                  time.Time       `json:"at"`
	Payload             json.RawMessage `json:"payload,omitempty"`
}

type FrozenAction struct {
	MonitorRevision  int             `json:"monitor_revision"`
	ActionRevision   int             `json:"action_revision"`
	Kind             string          `json:"kind"`
	AgentID          string          `json:"agent_id,omitempty"`
	TurnType         string          `json:"turn_type,omitempty"`
	Topic            string          `json:"topic,omitempty"`
	RenderedBriefing string          `json:"rendered_briefing,omitempty"`
	ActionPayload    json.RawMessage `json:"action_payload,omitempty"`
}

type PendingFire struct {
	ID            string          `json:"id"`
	MonitorID     string          `json:"monitor_id"`
	EventID       string          `json:"event_id"`
	Sequence      int             `json:"sequence"`
	DedupKey      string          `json:"dedup_key"`
	EventBriefing json.RawMessage `json:"event_briefing,omitempty"`
	Action        FrozenAction    `json:"action"`
	CreatedAt     time.Time       `json:"created_at"`
	Attempts      int             `json:"attempts"`
	Status        string          `json:"status"`
	ClaimToken    string          `json:"claim_token,omitempty"`
}

type Admission string

const (
	AdmissionAdmitted       Admission = "admitted"
	AdmissionAlreadyPresent Admission = "already_present"
	AdmissionFailed         Admission = "failed"
)
