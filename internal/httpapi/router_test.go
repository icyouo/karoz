package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewMuxRoutesProjectAndScopedRequests(t *testing.T) {
	handler := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(name)) }
	}
	mux := NewMux(Handlers{
		Index: handler("index"), Settings: handler("settings"), FolderDialog: handler("folder"),
		AgentTemplates: handler("agents"), AgentTeamTemplates: handler("teams"), Diagnostics: handler("diagnostics"),
		CLI2API: handler("cli"), RuntimeProviders: handler("providers"), Projects: handler("projects"), ProjectScoped: handler("scoped"),
	})
	for path, want := range map[string]string{"/api/projects": "projects", "/api/projects/p1/agents": "scoped", "/api/settings": "settings"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Body.String() != want {
			t.Fatalf("%s response = %q", path, response.Body.String())
		}
	}
}
