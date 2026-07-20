package main

import (
	"context"

	agentdomain "github.com/karoz/karoz/internal/agent"
	artifactdomain "github.com/karoz/karoz/internal/artifact"
	collaborationdomain "github.com/karoz/karoz/internal/collaboration"
	projectdomain "github.com/karoz/karoz/internal/project"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	settingsdomain "github.com/karoz/karoz/internal/settings"
	taskdomain "github.com/karoz/karoz/internal/task"
	tooldomain "github.com/karoz/karoz/internal/tool"
	"sync"
	"time"
)

type app struct {
	mu                    sync.Mutex
	artifactOpsMu         sync.Mutex
	handoffOpsMu          sync.Mutex
	handoffReplyMu        sync.Mutex
	schedulerPersistMu    sync.Mutex
	settings              Settings
	tasks                 map[string][]Task
	agents                map[string][]Agent
	archives              map[string][]AgentArchiveMessage
	memories              map[string][]AgentMemoryEntry
	blackboard            map[string][]AgentBlackboardEntry
	artifacts             map[string][]Artifact
	groups                map[string][]AgentGroup
	groupInbox            map[string][]GroupInboxMessage
	plans                 map[string][]WorkPlan
	inbox                 map[string][]AgentInboxMessage
	taskHooks             map[string][]TaskRuntimeHook
	agentRoutes           map[string][]AgentRoute
	agentMessages         map[string][]AgentMessage
	agentSessions         map[string]AgentSessionState
	projectAliases        map[string]string
	agentRuns             map[string]AgentRun
	agentRunCancels       map[string]context.CancelFunc
	residentBashApprovals map[string]ResidentBashApproval
	schedulerQueue        *runtimedomain.SchedulerQueue
	schedulerExecutors    map[ScheduledRunKind]ScheduledRunExecutor
	runtimeHooks          map[string]bool
	runtimeWatchers       map[string]map[chan RuntimeEvent]bool
	residentToolsOnce     sync.Once
	residentTools         *tooldomain.Registry[ResidentToolContext]
	modelProvider         runtimedomain.ModelProvider[CLI2APIRequest, ResidentToolContext, AgentStreamCallbacks]
	dynamicTools          tooldomain.DynamicProvider
}

type Settings = settingsdomain.Settings
type MCPServerConfig = settingsdomain.MCPServerConfig

type Project = projectdomain.Project

type Agent = agentdomain.Agent

type AgentTemplate = agentdomain.AgentTemplate

type AgentMessage = agentdomain.AgentMessage
type AgentMessagesPage = agentdomain.AgentMessagesPage
type AgentArchiveMessage = agentdomain.AgentArchiveMessage
type AgentMemoryEntry = agentdomain.AgentMemoryEntry

type AgentBlackboardEntry = collaborationdomain.BlackboardEntry
type RuntimeEvent = runtimedomain.Event
type AgentInboxMessage = collaborationdomain.Handoff

type TaskRuntimeHook = taskdomain.TaskRuntimeHook

type AgentRoute = agentdomain.AgentRoute

type AgentRoutesUpdateRequest struct {
	Routes []AgentRoute `json:"routes"`
}

type AgentSessionState = agentdomain.AgentSessionState

type Task = taskdomain.Task

type AgentMessageRequest struct {
	Message  string `json:"message"`
	Type     string `json:"type"`
	ChoiceID string `json:"choice_id,omitempty"`
}

type AgentAttachment struct {
	ID           string    `json:"id"`
	Filename     string    `json:"filename"`
	MimeType     string    `json:"mime_type"`
	SizeBytes    int64     `json:"size_bytes"`
	Path         string    `json:"path"`
	OriginalName string    `json:"original_name,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type WorkspaceFile struct {
	Path      string    `json:"path"`
	Filename  string    `json:"filename"`
	MimeType  string    `json:"mime_type"`
	SizeBytes int64     `json:"size_bytes"`
	UpdatedAt time.Time `json:"updated_at"`
}

type WorkspaceFilePreview struct {
	Path     string `json:"path"`
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

type Artifact = artifactdomain.Artifact
type ArtifactRevision = artifactdomain.Revision

type ArtifactStatusUpdateRequest struct {
	Status       string `json:"status"`
	Note         string `json:"note,omitempty"`
	ActorAgentID string `json:"actor_agent_id,omitempty"`
}

type AgentCreateRequest struct {
	TemplateID string `json:"template_id"`
	Nickname   string `json:"nickname"`
	GroupID    string `json:"group_id,omitempty"`
	GroupName  string `json:"group_name,omitempty"`
	GroupRole  string `json:"group_role,omitempty"`
	GroupOrder int    `json:"group_order,omitempty"`
}

type AgentTeamCreateRequest struct {
	TemplateID string `json:"template_id"`
	Instance   string `json:"instance"`
}

type AgentTeamCreateResponse struct {
	GroupID string       `json:"group_id"`
	Team    AgentTeam    `json:"team"`
	Agents  []Agent      `json:"agents"`
	Routes  []AgentRoute `json:"routes"`
	Created int          `json:"created"`
	Reused  int          `json:"reused"`
}

type AgentTeam = agentdomain.AgentTeam
type AgentTeamMember = agentdomain.AgentTeamMember
type AgentTeamEdge = agentdomain.AgentTeamEdge

type AgentUpdateRequest struct {
	Nickname                   string  `json:"nickname"`
	SystemPrompt               *string `json:"system_prompt"`
	ChatMode                   *string `json:"chat_mode"`
	Provider                   *string `json:"provider"`
	Model                      *string `json:"model"`
	ThinkingEffort             *string `json:"thinking_effort"`
	ExpectedModelConfigVersion *int64  `json:"expected_model_config_version"`
}

type CLI2APIRequest struct {
	Provider       string `json:"provider"`
	Model          string `json:"model,omitempty"`
	ThinkingEffort string `json:"thinking_effort,omitempty"`
	Prompt         string `json:"prompt"`
	Workdir        string `json:"workdir,omitempty"`
	Mode           string `json:"mode,omitempty"`
}

type CLI2APIResponse struct {
	Provider string `json:"provider"`
	Output   string `json:"output"`
	Stub     bool   `json:"stub"`
}

type ResidentModelDescriptor struct {
	Provider     string   `json:"provider"`
	ID           string   `json:"id"`
	DisplayName  string   `json:"display_name"`
	EffortLevels []string `json:"effort_levels"`
}

type ResidentProviderDescriptor struct {
	ID          string                    `json:"id"`
	DisplayName string                    `json:"display_name"`
	Transport   string                    `json:"transport"`
	Available   bool                      `json:"available"`
	Reason      string                    `json:"reason,omitempty"`
	Models      []ResidentModelDescriptor `json:"models"`
}

type ResidentToolContext struct {
	Project         Project
	Agent           Agent
	Workdir         string
	RunID           string
	TurnType        string
	EnforceRunScope bool
	EnforcePolicy   bool
}

type BashToolResult struct {
	OK         bool   `json:"ok"`
	Workspace  string `json:"workspace"`
	Command    string `json:"command"`
	Code       int    `json:"code"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type ResidentBashApproval struct {
	ID        string
	ProjectID string
	AgentID   string
	RunID     string
	Command   string
	State     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type AgentStreamCallbacks struct {
	OnDelta        func(string)
	OnToolStart    func(codexToolCall)
	OnToolResult   func(codexToolCall, string, bool)
	OnInterrupt    func([]AgentInterrupt)
	PollInterrupts func() []AgentInterrupt
}

type AgentInterrupt = runtimedomain.Interrupt

type codexToolCall = tooldomain.Call

type TaskCreateRequest struct {
	Type         string   `json:"type"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Goal         string   `json:"goal"`
	ArtifactIDs  []string `json:"artifact_ids,omitempty"`
	OwnerAgentID string   `json:"owner_agent_id,omitempty"`
	PlanID       string   `json:"plan_id,omitempty"`
	PlanStepID   string   `json:"plan_step_id,omitempty"`
	Attempt      int      `json:"attempt,omitempty"`
	ParentTaskID string   `json:"parent_task_id,omitempty"`
}

type TaskLogResponse struct {
	Content string `json:"content"`
}

type SettingsUpdateRequest struct {
	ProjectsRoot       string                      `json:"projects_root"`
	ExtraProjectsRoots []string                    `json:"extra_projects_roots"`
	MCPServers         *map[string]MCPServerConfig `json:"mcp_servers,omitempty"`
}

type ProjectCreateRequest struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
	Mode string `json:"mode,omitempty"`
}

type Diagnostics struct {
	CodexCLI       ToolStatus `json:"codex_cli"`
	ClaudeCLI      ToolStatus `json:"claude_cli"`
	ProjectsRootOK bool       `json:"projects_root_ok"`
}

type ToolStatus struct {
	Available bool   `json:"available"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
}
