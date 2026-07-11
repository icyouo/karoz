package collaboration

import (
	"strings"
	"time"
)

type HandoffStatus string

const (
	HandoffQueued    HandoffStatus = "queued"
	HandoffDelivered HandoffStatus = "delivered"
	HandoffClaimed   HandoffStatus = "claimed"
	HandoffWorking   HandoffStatus = "working"
	HandoffReplied   HandoffStatus = "replied"
	HandoffDeclined  HandoffStatus = "declined"
	HandoffFailed    HandoffStatus = "failed"
	HandoffClosed    HandoffStatus = "closed"
)

func NormalizeHandoffStatus(status string) HandoffStatus {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "pending":
		return HandoffDelivered
	case string(HandoffQueued), string(HandoffDelivered), string(HandoffClaimed), string(HandoffWorking), string(HandoffReplied), string(HandoffDeclined), string(HandoffFailed), string(HandoffClosed):
		return HandoffStatus(strings.ToLower(strings.TrimSpace(status)))
	case "acked", "reported", "consumed":
		return HandoffClosed
	default:
		return HandoffDelivered
	}
}

func HandoffOpen(status string) bool {
	switch NormalizeHandoffStatus(status) {
	case HandoffQueued, HandoffDelivered, HandoffClaimed, HandoffWorking:
		return true
	default:
		return false
	}
}

func ValidHandoffTransition(from, to string) bool {
	fromStatus := NormalizeHandoffStatus(from)
	toStatus := NormalizeHandoffStatus(to)
	if fromStatus == toStatus {
		return true
	}
	switch fromStatus {
	case HandoffQueued:
		return toStatus == HandoffDelivered || toStatus == HandoffFailed || toStatus == HandoffClosed
	case HandoffDelivered:
		return toStatus == HandoffClaimed || toStatus == HandoffWorking || toStatus == HandoffReplied || toStatus == HandoffDeclined || toStatus == HandoffFailed || toStatus == HandoffClosed
	case HandoffClaimed:
		return toStatus == HandoffDelivered || toStatus == HandoffWorking || toStatus == HandoffReplied || toStatus == HandoffDeclined || toStatus == HandoffFailed || toStatus == HandoffClosed
	case HandoffWorking:
		return toStatus == HandoffDelivered || toStatus == HandoffReplied || toStatus == HandoffDeclined || toStatus == HandoffFailed || toStatus == HandoffClosed
	case HandoffReplied, HandoffDeclined:
		return toStatus == HandoffClosed
	case HandoffFailed:
		return toStatus == HandoffDelivered || toStatus == HandoffClosed
	default:
		return false
	}
}

type Handoff struct {
	ID              string     `json:"id"`
	ProjectID       string     `json:"project_id"`
	SourceAgentID   string     `json:"source_agent_id"`
	TargetAgentID   string     `json:"target_agent_id"`
	CorrelationID   string     `json:"correlation_id"`
	ParentRunID     string     `json:"parent_run_id,omitempty"`
	MessageType     string     `json:"message_type,omitempty"`
	Intent          string     `json:"intent"`
	Subject         string     `json:"subject"`
	Body            string     `json:"body"`
	Objective       string     `json:"objective"`
	ExpectedOutput  string     `json:"expected_output"`
	ArtifactIDs     []string   `json:"artifact_ids,omitempty"`
	ThreadKey       string     `json:"thread_key"`
	ReplyToID       string     `json:"reply_to_id,omitempty"`
	ResultOwnerID   string     `json:"result_owner_agent_id,omitempty"`
	UpstreamID      string     `json:"upstream_handoff_id,omitempty"`
	Priority        int        `json:"priority"`
	Status          string     `json:"status"`
	Result          string     `json:"result,omitempty"`
	ResultMessageID string     `json:"result_message_id,omitempty"`
	FailureReason   string     `json:"failure_reason,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	DeliveredAt     *time.Time `json:"delivered_at,omitempty"`
	ClaimedAt       *time.Time `json:"claimed_at,omitempty"`
	WorkingAt       *time.Time `json:"working_at,omitempty"`
	ConsumedAt      *time.Time `json:"consumed_at,omitempty"`
	ReportedAt      *time.Time `json:"reported_at,omitempty"`
	RepliedAt       *time.Time `json:"replied_at,omitempty"`
	AckedAt         *time.Time `json:"acked_at,omitempty"`
	DeclinedAt      *time.Time `json:"declined_at,omitempty"`
	FailedAt        *time.Time `json:"failed_at,omitempty"`
	ClosedAt        *time.Time `json:"closed_at,omitempty"`
}

type BlackboardEntry struct {
	ID                   string     `json:"id"`
	ProjectID            string     `json:"project_id"`
	AgentID              string     `json:"agent_id"`
	AgentName            string     `json:"agent_name"`
	ActivityKind         string     `json:"activity_kind"`
	Summary              string     `json:"summary"`
	Detail               string     `json:"detail,omitempty"`
	SourceType           string     `json:"source_type,omitempty"`
	SourceID             string     `json:"source_id,omitempty"`
	EventKind            string     `json:"event_kind,omitempty"`
	Derived              bool       `json:"derived,omitempty"`
	CorrelationID        string     `json:"correlation_id,omitempty"`
	RunID                string     `json:"run_id,omitempty"`
	SourceInboxMessageID string     `json:"source_inbox_message_id,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	Status               string     `json:"status,omitempty"`
	RequiresAction       bool       `json:"requires_action,omitempty"`
	OwnerAgentID         string     `json:"owner_agent_id,omitempty"`
	TargetAgentID        string     `json:"target_agent_id,omitempty"`
	TargetRole           string     `json:"target_role,omitempty"`
	ExpiresAt            *time.Time `json:"expires_at,omitempty"`
	ClaimedAt            *time.Time `json:"claimed_at,omitempty"`
	HandledAt            *time.Time `json:"handled_at,omitempty"`
	HandledBy            string     `json:"handled_by,omitempty"`
	HandlingResult       string     `json:"handling_result,omitempty"`
	RoutedToAgentID      string     `json:"routed_to_agent_id,omitempty"`
	CreatedTaskID        string     `json:"created_task_id,omitempty"`
}
