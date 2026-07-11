package artifact

import (
	"path/filepath"
	"strings"
	"time"
)

type Status string

const (
	Draft      Status = "draft"
	Reviewing  Status = "reviewing"
	Approved   Status = "approved"
	Superseded Status = "superseded"
)

var kinds = map[string]bool{
	"requirements": true, "design_brief": true, "user_flow": true, "wireframe": true,
	"mockup_html": true, "mockup_svg": true, "mockup_image": true, "design_system": true,
	"technical_plan": true, "implementation_handoff": true, "review_report": true,
}

func ValidKind(kind string) bool {
	return kinds[strings.ToLower(strings.TrimSpace(kind))]
}

func InferKind(path string) string {
	lower := strings.ToLower(filepath.ToSlash(path))
	switch {
	case strings.Contains(lower, "requirement"), strings.Contains(lower, "prd"):
		return "requirements"
	case strings.Contains(lower, "design-brief"), strings.Contains(lower, "design_brief"):
		return "design_brief"
	case strings.Contains(lower, "user-flow"), strings.Contains(lower, "user_flow"):
		return "user_flow"
	case strings.Contains(lower, "wireframe"):
		return "wireframe"
	case strings.Contains(lower, "design-system"), strings.Contains(lower, "design_system"):
		return "design_system"
	case strings.Contains(lower, "implementation-handoff"), strings.Contains(lower, "implementation_handoff"):
		return "implementation_handoff"
	case strings.Contains(lower, "review"):
		return "review_report"
	}
	switch strings.ToLower(filepath.Ext(lower)) {
	case ".html", ".htm":
		return "mockup_html"
	case ".svg":
		return "mockup_svg"
	case ".png", ".jpg", ".jpeg", ".webp":
		return "mockup_image"
	default:
		return "technical_plan"
	}
}

func Previewable(mimeType string) bool {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	return strings.HasPrefix(mimeType, "text/") || strings.HasPrefix(mimeType, "image/") || strings.Contains(mimeType, "json")
}

func ValidStatusTransition(from, to string) bool {
	if from == to {
		return true
	}
	switch Status(from) {
	case Draft:
		return Status(to) == Reviewing || Status(to) == Superseded
	case Reviewing:
		return Status(to) == Draft || Status(to) == Approved || Status(to) == Superseded
	case Approved:
		return Status(to) == Superseded
	default:
		return false
	}
}

func RequiresApprovalForBuild(kind string) bool {
	switch kind {
	case "design_brief", "user_flow", "wireframe", "mockup_html", "mockup_svg", "mockup_image", "design_system", "implementation_handoff":
		return true
	default:
		return false
	}
}

type Revision struct {
	Revision         int       `json:"revision"`
	Status           string    `json:"status"`
	Description      string    `json:"description,omitempty"`
	ContentSHA256    string    `json:"content_sha256"`
	SizeBytes        int64     `json:"size_bytes"`
	CreatedByAgentID string    `json:"created_by_agent_id"`
	CreatedByRunID   string    `json:"created_by_run_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

type Artifact struct {
	ID             string     `json:"id"`
	ProjectID      string     `json:"project_id"`
	AgentID        string     `json:"agent_id"`
	Kind           string     `json:"kind"`
	Title          string     `json:"title"`
	Path           string     `json:"path"`
	MimeType       string     `json:"mime_type"`
	Revision       int        `json:"revision"`
	Status         string     `json:"status"`
	Description    string     `json:"description,omitempty"`
	Previewable    bool       `json:"previewable"`
	CreatedByRunID string     `json:"created_by_run_id,omitempty"`
	ApprovedBy     string     `json:"approved_by,omitempty"`
	ApprovedAt     *time.Time `json:"approved_at,omitempty"`
	ReviewNote     string     `json:"review_note,omitempty"`
	SupersedesID   string     `json:"supersedes_id,omitempty"`
	Revisions      []Revision `json:"revisions,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}
