package task

import "time"

type Task struct {
	ID                 string     `json:"id"`
	ProjectID          string     `json:"project_id"`
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	Title              string     `json:"title"`
	Description        string     `json:"description"`
	Goal               string     `json:"goal"`
	ArtifactIDs        []string   `json:"artifact_ids,omitempty"`
	OwnerAgentID       string     `json:"owner_agent_id,omitempty"`
	PlanID             string     `json:"plan_id,omitempty"`
	PlanStepID         string     `json:"plan_step_id,omitempty"`
	Attempt            int        `json:"attempt,omitempty"`
	ParentTaskID       string     `json:"parent_task_id,omitempty"`
	Result             string     `json:"result,omitempty"`
	FailureSummary     string     `json:"failure_summary,omitempty"`
	WorktreePath       string     `json:"worktree_path,omitempty"`
	BaseBranch         string     `json:"base_branch,omitempty"`
	BaseCommit         string     `json:"base_commit,omitempty"`
	TaskBranch         string     `json:"task_branch,omitempty"`
	CommitSHA          string     `json:"commit_sha,omitempty"`
	MergedAt           *time.Time `json:"merged_at,omitempty"`
	MergeBlockedReason string     `json:"merge_blocked_reason,omitempty"`
	MergeBlockedDetail string     `json:"merge_blocked_detail,omitempty"`
	MergeAttempts      int        `json:"merge_attempts,omitempty"`
	WorktreeState      string     `json:"worktree_state,omitempty"`
	WorktreeDetail     string     `json:"worktree_detail,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type TaskRuntimeHook struct {
	ID              string         `json:"id"`
	TaskID          string         `json:"task_id"`
	ProjectID       string         `json:"project_id"`
	AgentID         string         `json:"agent_id"`
	SessionID       string         `json:"session_id"`
	HookType        string         `json:"hook_type"`
	Status          string         `json:"status"`
	RequestPayload  map[string]any `json:"request_payload,omitempty"`
	ResponsePayload map[string]any `json:"response_payload,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	DeliveredAt     *time.Time     `json:"delivered_at,omitempty"`
}
