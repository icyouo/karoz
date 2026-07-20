package agent

import (
	"strings"
	"time"
)

type Agent struct {
	ID                 string     `json:"id"`
	ProjectID          string     `json:"project_id"`
	TemplateID         string     `json:"template_id"`
	Name               string     `json:"name"`
	Nickname           string     `json:"nickname"`
	DisplayName        string     `json:"display_name"`
	ShortName          string     `json:"short_name"`
	Role               string     `json:"role"`
	SystemPrompt       string     `json:"system_prompt"`
	Emoji              string     `json:"emoji"`
	Summary            string     `json:"summary"`
	GroupID            string     `json:"group_id,omitempty"`
	GroupName          string     `json:"group_name,omitempty"`
	GroupRole          string     `json:"group_role,omitempty"`
	GroupOrder         int        `json:"group_order,omitempty"`
	Type               string     `json:"type"`
	Runtime            string     `json:"runtime"`
	ChatMode           string     `json:"chat_mode,omitempty"`
	Provider           string     `json:"provider,omitempty"`
	Model              string     `json:"model,omitempty"`
	ThinkingEffort     string     `json:"thinking_effort,omitempty"`
	ModelConfigVersion int64      `json:"model_config_version,omitempty"`
	SessionID          string     `json:"session_id"`
	State              string     `json:"state"`
	StatusMessage      string     `json:"status_message"`
	MessageCount       int        `json:"message_count"`
	LastSeenAt         *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type Capabilities struct {
	CanDirectChat         bool
	CanCreateTasks        bool
	CanDelegate           bool
	CanCreateArtifacts    bool
	CanDesignArtifacts    bool
	CanManageAgents       bool
	CanManageRoutes       bool
	CanInspectProjectWide bool
	CanReconcileBacklog   bool
}

func IsKaroz(agent Agent) bool {
	return agent.ID == "karoz"
}

func roleHaystack(agent Agent) string {
	return strings.ToLower(strings.Join([]string{agent.Name, agent.DisplayName, agent.ShortName, agent.Nickname, agent.Role}, " "))
}

func IsDesign(agent Agent) bool {
	haystack := roleHaystack(agent)
	return strings.Contains(haystack, "design") || strings.Contains(haystack, "ux")
}

func IsReviewer(agent Agent) bool {
	haystack := roleHaystack(agent)
	return strings.Contains(haystack, "review") || strings.Contains(haystack, "critic") || strings.Contains(haystack, "qa")
}

func IsBuilder(agent Agent) bool {
	haystack := roleHaystack(agent)
	return strings.Contains(haystack, "implementation") || strings.Contains(haystack, "build") || strings.Contains(haystack, "frontend") || strings.Contains(haystack, "backend")
}

func CapabilitiesFor(agent Agent) Capabilities {
	capabilities := Capabilities{
		CanDirectChat: true, CanCreateTasks: true, CanDelegate: true, CanCreateArtifacts: true,
		CanDesignArtifacts: IsDesign(agent),
	}
	if IsKaroz(agent) {
		capabilities.CanManageAgents = true
		capabilities.CanManageRoutes = true
		capabilities.CanInspectProjectWide = true
		capabilities.CanReconcileBacklog = true
	}
	return capabilities
}

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

type AgentTeam struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	Description         string            `json:"description"`
	CoordinatorMemberID string            `json:"coordinator_member_id"`
	Agents              []AgentTeamMember `json:"agents"`
	Edges               []AgentTeamEdge   `json:"edges"`
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
