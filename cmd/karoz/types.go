package main

import (
	"context"

	agentdomain "github.com/karoz/karoz/internal/agent"
	artifactdomain "github.com/karoz/karoz/internal/artifact"
	collaborationdomain "github.com/karoz/karoz/internal/collaboration"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	tooldomain "github.com/karoz/karoz/internal/tool"
	"sync"
	"time"
)

type app struct {
	mu                 sync.Mutex
	artifactOpsMu      sync.Mutex
	handoffOpsMu       sync.Mutex
	handoffReplyMu     sync.Mutex
	schedulerPersistMu sync.Mutex
	settings           Settings
	tasks              map[string][]Task
	agents             map[string][]Agent
	archives           map[string][]AgentArchiveMessage
	memories           map[string][]AgentMemoryEntry
	blackboard         map[string][]AgentBlackboardEntry
	artifacts          map[string][]Artifact
	inbox              map[string][]AgentInboxMessage
	taskHooks          map[string][]TaskRuntimeHook
	agentRoutes        map[string][]AgentRoute
	agentMessages      map[string][]AgentMessage
	agentSessions      map[string]AgentSessionState
	projectAliases     map[string]string
	agentRuns          map[string]AgentRun
	agentRunCancels    map[string]context.CancelFunc
	schedulerQueue     *runtimedomain.SchedulerQueue
	schedulerExecutors map[ScheduledRunKind]ScheduledRunExecutor
	runtimeHooks       map[string]bool
	runtimeWatchers    map[string]map[chan RuntimeEvent]bool
	residentToolsOnce  sync.Once
	residentTools      *tooldomain.Registry[ResidentToolContext]
	modelProvider      runtimedomain.ModelProvider[CLI2APIRequest, ResidentToolContext, AgentStreamCallbacks]
	dynamicTools       tooldomain.DynamicProvider
}

type Settings struct {
	DataDir            string                     `json:"data_dir"`
	ProjectsRoot       string                     `json:"projects_root"`
	ExtraProjectsRoots []string                   `json:"extra_projects_roots"`
	MCPServers         map[string]MCPServerConfig `json:"mcp_servers,omitempty"`
}

type MCPServerConfig struct {
	Type     string            `json:"type,omitempty"`
	Command  string            `json:"command"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	URL      string            `json:"url,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
}

type Project struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Path          string `json:"path"`
	WorkspaceRoot string `json:"workspace_root,omitempty"`
	WorkspaceType string `json:"workspace_type,omitempty"`
	DefaultBranch string `json:"default_branch"`
	AgentName     string `json:"agent_name"`
}

type Agent = agentdomain.Agent

type AgentTemplate struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	DisplayName  string         `json:"display_name"`
	ShortName    string         `json:"short_name"`
	Role         string         `json:"role"`
	SystemPrompt string         `json:"system_prompt"`
	Emoji        string         `json:"emoji"`
	Summary      string         `json:"summary"`
	Config       map[string]any `json:"config"`
	Source       string         `json:"source"`
	Deprecated   bool           `json:"deprecated"`
}

type AgentMessage struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	AgentID   string    `json:"agent_id"`
	SessionID string    `json:"session_id"`
	Seq       int64     `json:"seq"`
	Role      string    `json:"role"`
	Intent    string    `json:"intent"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type AgentMessagesPage struct {
	Messages      []AgentMessage `json:"messages"`
	HasMore       bool           `json:"has_more"`
	NextBeforeSeq int64          `json:"next_before_seq,omitempty"`
}

type AgentArchiveMessage struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	AgentID    string    `json:"agent_id"`
	SessionID  string    `json:"session_id"`
	Seq        int64     `json:"seq"`
	Role       string    `json:"role"`
	Intent     string    `json:"intent"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
	ArchivedAt time.Time `json:"archived_at"`
}

type AgentMemoryEntry struct {
	ID         string         `json:"id"`
	ProjectID  string         `json:"project_id"`
	AgentID    string         `json:"agent_id"`
	SessionID  string         `json:"session_id"`
	Layer      string         `json:"layer"`
	State      string         `json:"state"`
	Priority   int            `json:"priority"`
	Summary    string         `json:"summary"`
	Detail     string         `json:"detail"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	ArchivedAt *time.Time     `json:"archived_at,omitempty"`
}

type AgentBlackboardEntry = collaborationdomain.BlackboardEntry
type RuntimeEvent = runtimedomain.Event
type AgentInboxMessage = collaborationdomain.Handoff

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

type AgentRoute struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	FromAgentID string    `json:"from_agent_id"`
	ToAgentID   string    `json:"to_agent_id"`
	Intent      string    `json:"intent"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type AgentRoutesUpdateRequest struct {
	Routes []AgentRoute `json:"routes"`
}

type AgentSessionState struct {
	SessionID           string    `json:"session_id"`
	ProjectID           string    `json:"project_id"`
	AgentID             string    `json:"agent_id"`
	ShortWindowStartSeq int64     `json:"short_window_start_seq"`
	BoundarySeq         int64     `json:"boundary_seq"`
	LongTermVersion     int64     `json:"long_term_version"`
	ResidentSummary     string    `json:"resident_summary"`
	CoveredSeqStart     int64     `json:"covered_seq_start"`
	CoveredSeqEnd       int64     `json:"covered_seq_end"`
	LastCheckpointAt    time.Time `json:"last_checkpoint_at"`
}

type Task struct {
	ID             string     `json:"id"`
	ProjectID      string     `json:"project_id"`
	Type           string     `json:"type"`
	Status         string     `json:"status"`
	Title          string     `json:"title"`
	Description    string     `json:"description"`
	Goal           string     `json:"goal"`
	ArtifactIDs    []string   `json:"artifact_ids,omitempty"`
	Result         string     `json:"result,omitempty"`
	FailureSummary string     `json:"failure_summary,omitempty"`
	WorktreePath   string     `json:"worktree_path,omitempty"`
	BaseBranch     string     `json:"base_branch,omitempty"`
	TaskBranch     string     `json:"task_branch,omitempty"`
	CommitSHA      string     `json:"commit_sha,omitempty"`
	MergedAt       *time.Time `json:"merged_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type AgentMessageRequest struct {
	Message string `json:"message"`
	Type    string `json:"type"`
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

type AgentTeam struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Agents      []AgentTeamMember `json:"agents"`
	Edges       []AgentTeamEdge   `json:"edges"`
}

type AgentTeamMember struct {
	ID           string   `json:"id"`
	Nickname     string   `json:"nickname"`
	TemplateID   string   `json:"template_id"`
	Role         string   `json:"role"`
	AcceptFrom   []string `json:"accept_from"`
	ReportTo     []string `json:"report_to"`
	StartupOrder int      `json:"startup_order"`
	DependsOn    []string `json:"depends_on"`
}

type AgentTeamEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type AgentUpdateRequest struct {
	Nickname     string  `json:"nickname"`
	SystemPrompt *string `json:"system_prompt"`
}

type CLI2APIRequest struct {
	Provider string `json:"provider"`
	Prompt   string `json:"prompt"`
	Workdir  string `json:"workdir,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

type CLI2APIResponse struct {
	Provider string `json:"provider"`
	Output   string `json:"output"`
	Stub     bool   `json:"stub"`
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
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Goal        string   `json:"goal"`
	ArtifactIDs []string `json:"artifact_ids,omitempty"`
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
