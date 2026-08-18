package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestDockerWorkspaceSettingsAreLocked(t *testing.T) {
	t.Setenv("KAROZ_WORKSPACE_SETTINGS_LOCKED", "1")
	a, _ := newHandlerTestApp(t)

	get := serveHTTPRequest(a, http.MethodGet, "/api/settings", "")
	if get.Code != http.StatusOK {
		t.Fatalf("GET /api/settings status = %d, want 200; body=%s", get.Code, get.Body.String())
	}
	var settings SettingsResponse
	if err := json.Unmarshal(get.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode settings response: %v", err)
	}
	if !settings.WorkspaceSettingsLocked {
		t.Fatal("workspace_settings_locked = false, want true")
	}

	put := serveHTTPRequest(a, http.MethodPut, "/api/settings", `{"projects_root":"`+a.settings.ProjectsRoot+`","extra_projects_roots":[]}`)
	if put.Code != http.StatusForbidden {
		t.Fatalf("PUT /api/settings status = %d, want 403; body=%s", put.Code, put.Body.String())
	}
}
