package httpapi

import "net/http"

type Handlers struct {
	Index              http.HandlerFunc
	Settings           http.HandlerFunc
	FolderDialog       http.HandlerFunc
	AgentTemplates     http.HandlerFunc
	AgentTeamTemplates http.HandlerFunc
	Diagnostics        http.HandlerFunc
	CLI2API            http.HandlerFunc
	Projects           http.HandlerFunc
	ProjectScoped      http.HandlerFunc
}

func NewMux(handlers Handlers) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handlers.Index)
	mux.HandleFunc("/api/settings", handlers.Settings)
	mux.HandleFunc("/api/folder-dialog", handlers.FolderDialog)
	mux.HandleFunc("/api/agent-templates", handlers.AgentTemplates)
	mux.HandleFunc("/api/agent-team-templates", handlers.AgentTeamTemplates)
	mux.HandleFunc("/api/diagnostics", handlers.Diagnostics)
	mux.HandleFunc("/api/cli2api", handlers.CLI2API)
	mux.HandleFunc("/api/projects", handlers.Projects)
	mux.HandleFunc("/api/projects/", handlers.ProjectScoped)
	return mux
}
