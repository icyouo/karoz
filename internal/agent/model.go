package agent

import (
	"strings"
	"time"
)

type Agent struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"project_id"`
	TemplateID    string     `json:"template_id"`
	Name          string     `json:"name"`
	Nickname      string     `json:"nickname"`
	DisplayName   string     `json:"display_name"`
	ShortName     string     `json:"short_name"`
	Role          string     `json:"role"`
	SystemPrompt  string     `json:"system_prompt"`
	Emoji         string     `json:"emoji"`
	Summary       string     `json:"summary"`
	GroupID       string     `json:"group_id,omitempty"`
	GroupName     string     `json:"group_name,omitempty"`
	GroupRole     string     `json:"group_role,omitempty"`
	GroupOrder    int        `json:"group_order,omitempty"`
	Type          string     `json:"type"`
	Runtime       string     `json:"runtime"`
	SessionID     string     `json:"session_id"`
	State         string     `json:"state"`
	StatusMessage string     `json:"status_message"`
	MessageCount  int        `json:"message_count"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
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
