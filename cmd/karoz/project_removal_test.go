package main

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

func TestRemoveProjectFromStudioHidesItButKeepsCheckout(t *testing.T) {
	a, project := newHandlerTestApp(t)

	response := serveHTTPRequest(a, http.MethodDelete, "/api/projects/"+project.ID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("DELETE project status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(project.Path); err != nil {
		t.Fatalf("project checkout was removed: %v", err)
	}

	projectsResponse := serveHTTPRequest(a, http.MethodGet, "/api/projects", "")
	if projectsResponse.Code != http.StatusOK {
		t.Fatalf("GET projects status=%d body=%s", projectsResponse.Code, projectsResponse.Body.String())
	}
	var projects []Project
	if err := json.Unmarshal(projectsResponse.Body.Bytes(), &projects); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	for _, item := range projects {
		if item.ID == project.ID {
			t.Fatalf("removed project still listed: %+v", projects)
		}
	}
	if len(a.settings.ExcludedProjectIDs) != 1 || a.settings.ExcludedProjectIDs[0] != project.ID {
		t.Fatalf("excluded projects = %+v", a.settings.ExcludedProjectIDs)
	}
}
