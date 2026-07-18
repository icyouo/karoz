package main

import "time"

const (
	PlanDraft            = "draft"
	PlanAwaitingApproval = "awaiting_approval"
	PlanActive           = "active"
	PlanPaused           = "paused"
	PlanBlocked          = "blocked"
	PlanCompleted        = "completed"
	PlanCancelled        = "cancelled"

	PlanStepPending          = "pending"
	PlanStepReady            = "ready"
	PlanStepDispatching      = "dispatching"
	PlanStepRunning          = "running"
	PlanStepAwaitingDecision = "awaiting_decision"
	PlanStepReviewing        = "reviewing"
	PlanStepChangesRequested = "changes_requested"
	PlanStepBlocked          = "blocked"
	PlanStepCompleted        = "completed"
	PlanStepSkipped          = "skipped"
	PlanStepCancelled        = "cancelled"
)

type AgentGroup struct {
	ID                 string    `json:"id"`
	ProjectID          string    `json:"project_id"`
	Name               string    `json:"name"`
	TemplateID         string    `json:"template_id,omitempty"`
	CoordinatorAgentID string    `json:"coordinator_agent_id"`
	MemberAgentIDs     []string  `json:"member_agent_ids"`
	Version            int64     `json:"version"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type GroupInboxMessage struct {
	ID                  string    `json:"id"`
	ProjectID           string    `json:"project_id"`
	GroupID             string    `json:"group_id"`
	SourceAgentID       string    `json:"source_agent_id"`
	SourceGroupID       string    `json:"source_group_id,omitempty"`
	CoordinatorAgentID  string    `json:"coordinator_agent_id"`
	PreferredMemberID   string    `json:"preferred_member_id,omitempty"`
	Intent              string    `json:"intent"`
	Subject             string    `json:"subject"`
	Body                string    `json:"body"`
	Objective           string    `json:"objective"`
	ExpectedOutput      string    `json:"expected_output"`
	Status              string    `json:"status"`
	AgentInboxMessageID string    `json:"agent_inbox_message_id,omitempty"`
	ParentPlanID        string    `json:"parent_plan_id,omitempty"`
	ParentStepID        string    `json:"parent_step_id,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type WorkPlan struct {
	ID                  string     `json:"id"`
	ProjectID           string     `json:"project_id"`
	Title               string     `json:"title"`
	Goal                string     `json:"goal"`
	Status              string     `json:"status"`
	Revision            int        `json:"revision"`
	RequestedViaAgentID string     `json:"requested_via_agent_id,omitempty"`
	AuthoredByAgentID   string     `json:"authored_by_agent_id"`
	OwnerType           string     `json:"owner_type"`
	OwnerGroupID        string     `json:"owner_group_id,omitempty"`
	OwnerAgentID        string     `json:"owner_agent_id"`
	ApprovedBy          string     `json:"approved_by,omitempty"`
	ApprovedAt          *time.Time `json:"approved_at,omitempty"`
	MaxConcurrency      int        `json:"max_concurrency"`
	Version             int64      `json:"version"`
	Steps               []PlanStep `json:"steps"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type PlanStep struct {
	ID                 string            `json:"id"`
	Title              string            `json:"title"`
	Description        string            `json:"description"`
	Status             string            `json:"status"`
	Dependencies       []string          `json:"dependencies,omitempty"`
	AssignedAgentID    string            `json:"assigned_agent_id,omitempty"`
	AssignedGroupID    string            `json:"assigned_group_id,omitempty"`
	AcceptanceCriteria []string          `json:"acceptance_criteria,omitempty"`
	TaskAttempts       []PlanTaskAttempt `json:"task_attempts,omitempty"`
	Reviews            []PlanReview      `json:"reviews,omitempty"`
	ChildPlanID        string            `json:"child_plan_id,omitempty"`
	Result             string            `json:"result,omitempty"`
	Blocker            string            `json:"blocker,omitempty"`
	Version            int64             `json:"version"`
}

type PlanTaskAttempt struct {
	Attempt   int       `json:"attempt"`
	TaskID    string    `json:"task_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type PlanReview struct {
	ID              string    `json:"id"`
	Mode            string    `json:"mode"`
	ReviewerAgentID string    `json:"reviewer_agent_id,omitempty"`
	HandoffID       string    `json:"handoff_id,omitempty"`
	TaskID          string    `json:"task_id,omitempty"`
	Status          string    `json:"status"`
	Decision        string    `json:"decision,omitempty"`
	Findings        string    `json:"findings,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type WorkPlanDraftRequest struct {
	Title          string     `json:"title"`
	Goal           string     `json:"goal"`
	MaxConcurrency int        `json:"max_concurrency,omitempty"`
	Steps          []PlanStep `json:"steps"`
}

type PlanActionRequest struct {
	Action          string   `json:"action"`
	StepID          string   `json:"step_id,omitempty"`
	AgentID         string   `json:"agent_id,omitempty"`
	GroupID         string   `json:"group_id,omitempty"`
	ReviewerAgentID string   `json:"reviewer_agent_id,omitempty"`
	TaskType        string   `json:"task_type,omitempty"`
	Result          string   `json:"result,omitempty"`
	Reason          string   `json:"reason,omitempty"`
	Title           string   `json:"title,omitempty"`
	Description     string   `json:"description,omitempty"`
	Goal            string   `json:"goal,omitempty"`
	ExpectedVersion int64    `json:"expected_version,omitempty"`
	Dependencies    []string `json:"dependencies,omitempty"`
}
