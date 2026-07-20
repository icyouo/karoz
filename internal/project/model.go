package project

type Project struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Path          string `json:"path"`
	WorkspaceRoot string `json:"workspace_root,omitempty"`
	WorkspaceType string `json:"workspace_type,omitempty"`
	DefaultBranch string `json:"default_branch"`
	AgentName     string `json:"agent_name"`
}
