package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeModelProvider struct{}

func (fakeModelProvider) Stream(_ context.Context, _ CLI2APIRequest, _ ResidentToolContext, callbacks AgentStreamCallbacks) error {
	callbacks.OnDelta("provider output")
	return nil
}

type fakeDynamicTools struct{}

func (fakeDynamicTools) Specs(context.Context, string) []map[string]any {
	return []map[string]any{{"type": "function", "name": "mcp__fake__echo"}}
}

func (fakeDynamicTools) Call(context.Context, string, string, string) (string, error) {
	return `{"source":"fake-mcp"}`, nil
}

func TestNewAppInitializesRuntimeStateAndHTTPComposition(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	if a.schedulerQueue == nil || a.tasks == nil || a.agents == nil || a.artifacts == nil || a.inbox == nil || a.agentRuns == nil || a.runtimeWatchers == nil {
		t.Fatalf("application state was not fully initialized: %+v", a)
	}
	response := httptest.NewRecorder()
	a.httpHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/agent-templates", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("agent templates status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestResidentRuntimeUsesProviderAndDynamicToolPorts(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	a.modelProvider = fakeModelProvider{}
	a.dynamicTools = fakeDynamicTools{}
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir(), DefaultBranch: "main"}
	agent := Agent{ID: "designer", ProjectID: project.ID, Name: "Designer", Role: "design"}
	a.agents[project.ID] = []Agent{agent}

	output, err := a.runResidentAgentTurn(context.Background(), project, agent, "hello", "ask", nil)
	if err != nil || output != "provider output" {
		t.Fatalf("provider output = %q err=%v", output, err)
	}
	specs := toolSpecNames(a.residentToolSpecsForContext(context.Background(), project.Path, agent))
	if !specs["mcp__fake__echo"] {
		t.Fatalf("dynamic specs = %+v", specs)
	}
	result, err := a.executeResidentTool(context.Background(), ResidentToolContext{Project: project, Agent: agent, Workdir: project.Path}, codexToolCall{Name: "mcp__fake__echo", Arguments: `{}`})
	if err != nil || !strings.Contains(result, "fake-mcp") {
		t.Fatalf("dynamic call = %s err=%v", result, err)
	}
}
