package main

import plandomain "github.com/karoz/karoz/internal/plan"

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

type AgentGroup = plandomain.AgentGroup
type GroupInboxMessage = plandomain.GroupInboxMessage
type WorkPlan = plandomain.WorkPlan
type PlanStep = plandomain.PlanStep
type PlanTaskAttempt = plandomain.PlanTaskAttempt
type PlanReview = plandomain.PlanReview

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

type PlanHistoryReconciliationRequest struct {
	PlanID          string                          `json:"plan_id"`
	ExpectedVersion int64                           `json:"expected_version,omitempty"`
	Steps           []PlanHistoryReconciliationStep `json:"steps"`
}

type PlanHistoryReconciliationStep struct {
	StepID   string   `json:"step_id"`
	TaskIDs  []string `json:"task_ids"`
	Decision string   `json:"decision"`
	Evidence string   `json:"evidence"`
}
