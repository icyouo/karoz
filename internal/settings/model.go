package settings

import (
	"os"
	"path/filepath"
	"strings"
)

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

func (s Settings) WorkspaceRoots() []string {
	roots := []string{filepath.Clean(expandHome(s.ProjectsRoot))}
	roots = append(roots, normalizeWorkspaceRoots(s.ExtraProjectsRoots, s.ProjectsRoot)...)
	return roots
}

func expandHome(path string) string {
	if path == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}

func normalizeWorkspaceRoots(roots []string, mainRoot string) []string {
	mainRoot = filepath.Clean(expandHome(strings.TrimSpace(mainRoot)))
	seen := map[string]bool{mainRoot: true}
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		clean := filepath.Clean(expandHome(root))
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out
}
