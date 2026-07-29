package main

import (
	"context"

	agentdomain "github.com/karoz/karoz/internal/agent"
	artifactdomain "github.com/karoz/karoz/internal/artifact"
	collaborationdomain "github.com/karoz/karoz/internal/collaboration"
	monitordomain "github.com/karoz/karoz/internal/monitor"
	processdomain "github.com/karoz/karoz/internal/process"
	projectdomain "github.com/karoz/karoz/internal/project"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	settingsdomain "github.com/karoz/karoz/internal/settings"
	taskdomain "github.com/karoz/karoz/internal/task"
	tooldomain "github.com/karoz/karoz/internal/tool"
	"sync"
	"time"
)

type app struct {
	mu                                 sync.Mutex
	supervisorCtx                      context.Context
	supervisorCancel                   context.CancelFunc
	processRuntime                     *processRuntimePersistence
	processSupervisor                  *processSupervisor
	processPersistenceFail             func(processPersistenceFailpoint) error
	processTerminalDrainMu             sync.Mutex
	processTerminalWake                chan struct{}
	processTerminalWorkerOnce          sync.Once
	processOutputMonitorOnce           sync.Once
	processOutputMonitorCh             chan processOutputObservation
	processOutputBaselines             map[string]uint64
	processOutputCursors               map[string]uint64
	processOutputGapMu                 sync.Mutex
	processOutputGapDrainMu            sync.Mutex
	processOutputPendingGaps           map[string]processOutputGapDelta
	processOutputGapWake               chan struct{}
	processOutputGapWorkerOnce         sync.Once
	monitorCtx                         context.Context
	monitorCancel                      context.CancelFunc
	monitorProbeWG                     sync.WaitGroup
	monitorProbeStopping               bool
	monitorProbeCancels                map[string]context.CancelFunc
	monitorProbeProjectSlots           map[string]chan struct{}
	monitorProbeReservations           map[string]monitorProbeReservation
	monitorProbeChallenges             map[string]monitorProbeChallenge
	monitorProbeReceipts               map[string]monitordomain.ProbeApprovalReceipt
	monitorProbeSessions               map[string]monitorProbeOperatorSession
	backgroundOwnerMu                  sync.Mutex
	backgroundOwnerDeleting            map[string]bool
	projectRegistrationMu              sync.Mutex
	projectCreateAfterRegistrationHook func()
	settingsUpdateBeforeRegistryHook   func()
	projectImportSettingsSave          func() error
	taskRunMu                          sync.Mutex
	taskRunCancels                     map[string]taskRun
	taskIntegrationLocksMu             sync.Mutex
	taskIntegrationLocks               map[string]*sync.Mutex
	taskIntegrationPreLockHook         func()
	artifactOpsMu                      sync.Mutex
	handoffOpsMu                       sync.Mutex
	handoffReplyMu                     sync.Mutex
	schedulerPersistMu                 sync.Mutex
	scheduledRunsSaveOverride          func(scheduledRunSnapshot) error
	settings                           Settings
	tasks                              map[string][]Task
	agents                             map[string][]Agent
	archives                           map[string][]AgentArchiveMessage
	memories                           map[string][]AgentMemoryEntry
	blackboard                         map[string][]AgentBlackboardEntry
	monitors                           map[string][]Monitor
	artifacts                          map[string][]Artifact
	groups                             map[string][]AgentGroup
	groupInbox                         map[string][]GroupInboxMessage
	plans                              map[string][]WorkPlan
	inbox                              map[string][]AgentInboxMessage
	taskHooks                          map[string][]TaskRuntimeHook
	agentRoutes                        map[string][]AgentRoute
	agentMessages                      map[string][]AgentMessage
	agentTranscripts                   map[string][]AgentTranscriptItem
	agentSessions                      map[string]AgentSessionState
	projectAliases                     map[string]string
	agentRuns                          map[string]AgentRun
	agentRunCancels                    map[string]context.CancelFunc
	agentRunContexts                   map[string]context.Context
	agentRunWorkers                    map[string]string
	agentRunCancelling                 map[string]string
	agentRunResultCommitted            map[string]string
	agentRunLedgers                    map[string]*agentRunLedger
	agentRunFinishedWatchers           map[string]map[chan struct{}]struct{}
	agentRunAfterProviderHook          func()
	agentRunAfterSuccessHook           func()
	scheduledRunBeforeBindHook         func()
	scheduledRunBeforeResultCommitHook func()
	residentBashApprovals              map[string]ResidentBashApproval
	schedulerQueue                     *runtimedomain.SchedulerQueue
	schedulerExecutors                 map[ScheduledRunKind]ScheduledRunExecutor
	runtimeHooks                       map[string]bool
	runtimeWatchers                    map[string]map[chan RuntimeEvent]bool
	residentToolsOnce                  sync.Once
	residentTools                      *tooldomain.Registry[ResidentToolContext]
	modelProvider                      runtimedomain.ModelProvider[CLI2APIRequest, ResidentToolContext, AgentStreamCallbacks]
	dynamicTools                       tooldomain.DynamicProvider
}

type Settings = settingsdomain.Settings
type MCPServerConfig = settingsdomain.MCPServerConfig

type Project = projectdomain.Project

type Agent = agentdomain.Agent

type AgentTemplate = agentdomain.AgentTemplate

type AgentMessage = agentdomain.AgentMessage
type AgentTranscriptItem = agentdomain.AgentTranscriptItem
type AgentContextMessage = agentdomain.AgentContextMessage
type AgentMessagesPage = agentdomain.AgentMessagesPage
type AgentArchiveMessage = agentdomain.AgentArchiveMessage
type AgentMemoryEntry = agentdomain.AgentMemoryEntry

type AgentBlackboardEntry = collaborationdomain.BlackboardEntry
type Monitor = monitordomain.Monitor

type monitorProbeReservation struct {
	ID              string    `json:"id"`
	ProjectID       string    `json:"project_id"`
	AgentID         string    `json:"agent_id"`
	OwnerCreatedAt  time.Time `json:"owner_created_at"`
	MonitorID       string    `json:"monitor_id"`
	TriggerRevision int       `json:"trigger_revision"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type monitorProbeChallenge struct {
	ID                string    `json:"id"`
	ReservationID     string    `json:"reservation_id"`
	OperatorSessionID string    `json:"operator_session_id"`
	ApprovalRunID     string    `json:"approval_run_id,omitempty"`
	ChoiceRequestID   string    `json:"choice_request_id,omitempty"`
	ProjectID         string    `json:"project_id"`
	AgentID           string    `json:"agent_id"`
	MonitorID         string    `json:"monitor_id"`
	TriggerRevision   int       `json:"trigger_revision"`
	CanonicalWorkdir  string    `json:"canonical_workdir"`
	Language          string    `json:"language"`
	NormalizedSource  []byte    `json:"normalized_source"`
	SourceSHA256      string    `json:"source_sha256"`
	IntervalMS        int64     `json:"interval_ms"`
	TimeoutMS         int64     `json:"timeout_ms"`
	ExpiresAt         time.Time `json:"expires_at"`
	State             string    `json:"state"`
	StagingReceiptID  string    `json:"staging_receipt_id,omitempty"`
	StagingPath       string    `json:"staging_path,omitempty"`
	ConsumedReceiptID string    `json:"consumed_receipt_id,omitempty"`
}

type monitorProbeOperatorSession struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type processOutputGapDelta struct {
	ProjectID string
	ProcessID string
	Recent    []processdomain.SeqRange
	LostLines uint64
	GapCount  uint64
	OldestSeq uint64
	NewestSeq uint64
}
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
	Provider       string                `json:"provider"`
	Model          string                `json:"model,omitempty"`
	ThinkingEffort string                `json:"thinking_effort,omitempty"`
	Prompt         string                `json:"prompt"`
	Workdir        string                `json:"workdir,omitempty"`
	Mode           string                `json:"mode,omitempty"`
	Transcript     []AgentTranscriptItem `json:"-"`
}

type CLI2APIResponse struct {
	Provider string `json:"provider"`
	Output   string `json:"output"`
	Stub     bool   `json:"stub"`
}

type ResidentModelDescriptor struct {
	Provider      string   `json:"provider"`
	ID            string   `json:"id"`
	DisplayName   string   `json:"display_name"`
	EffortLevels  []string `json:"effort_levels"`
	ContextWindow int64    `json:"context_window"`
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
	ID             string
	RunID          string
	Subject        residentBashSubject
	State          string
	OwnerCreatedAt time.Time
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

type AgentStreamCallbacks struct {
	OnDelta           func(string)
	OnToolStart       func(codexToolCall)
	OnToolResult      func(codexToolCall, string, bool)
	OnBudgetExhausted func(map[string]any)
	OnInterrupt       func([]AgentInterrupt)
	PollInterrupts    func() []AgentInterrupt
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
