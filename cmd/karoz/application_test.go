package main

import (
	"context"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeModelProvider struct{}

func (fakeModelProvider) Capabilities(CLI2APIRequest) runtimedomain.ProviderCapabilities {
	return runtimedomain.ProviderCapabilities{Streaming: true, Tools: true, Interrupts: true}
}

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
	toolCtx := ResidentToolContext{Project: project, Agent: agent, Workdir: project.Path, TurnType: "dev"}
	specs := toolSpecNames(a.residentToolSpecsForContext(context.Background(), toolCtx))
	if !specs["mcp__fake__echo"] {
		t.Fatalf("dynamic specs = %+v", specs)
	}
	result, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{Name: "mcp__fake__echo", Arguments: `{}`})
	if err != nil || !strings.Contains(result, "fake-mcp") {
		t.Fatalf("dynamic call = %s err=%v", result, err)
	}
}

func TestAgentMessagePostAlwaysUsesResidentSSEStream(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	a.modelProvider = fakeModelProvider{}
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir(), DefaultBranch: "main"}
	agent := Agent{ID: "designer", ProjectID: project.ID, Name: "Designer", Role: "design"}
	a.agents[project.ID] = []Agent{agent}

	request := httptest.NewRequest(http.MethodPost, "/agents/designer/messages", strings.NewReader(`{"message":"hello","type":"ask"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	a.handleAgents(response, request, project, []string{"designer", "messages"})

	if response.Code != http.StatusOK {
		t.Fatalf("agent message status = %d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
	body := response.Body.String()
	for _, expected := range []string{"event: meta", "event: delta", "provider output", "event: done"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("SSE body missing %q: %s", expected, body)
		}
	}
}

func TestResidentAgentChatModePersistsAndDefaultsMessageMode(t *testing.T) {
	dataDir := t.TempDir()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	a.modelProvider = fakeModelProvider{}
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir(), DefaultBranch: "main"}
	a.agents[project.ID] = []Agent{{ID: "designer", ProjectID: project.ID, Name: "Designer", Role: "design"}}

	patch := httptest.NewRequest(http.MethodPatch, "/agents/designer", strings.NewReader(`{"chat_mode":"plan"}`))
	patch.Header.Set("Content-Type", "application/json")
	patchResponse := httptest.NewRecorder()
	a.handleAgents(patchResponse, patch, project, []string{"designer"})
	if patchResponse.Code != http.StatusOK {
		t.Fatalf("chat mode patch status = %d body=%s", patchResponse.Code, patchResponse.Body.String())
	}
	stored, ok := a.projectAgent(project, "designer")
	if !ok || stored.ChatMode != "plan" {
		t.Fatalf("stored agent mode = %+v", stored)
	}

	request := httptest.NewRequest(http.MethodPost, "/agents/designer/messages", strings.NewReader(`{"message":"plan this"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.handleAgents(response, request, project, []string{"designer", "messages"})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"type":"plan"`) {
		t.Fatalf("saved mode was not used by default: status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/agents/designer/messages", strings.NewReader(`{"message":"implement this","type":"dev"}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	a.handleAgents(response, request, project, []string{"designer", "messages"})
	stored, _ = a.projectAgent(project, "designer")
	if response.Code != http.StatusOK || stored.ChatMode != "dev" {
		t.Fatalf("explicit message mode was not persisted: status=%d agent=%+v", response.Code, stored)
	}

	after := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	if err := after.loadAgents(); err != nil {
		t.Fatal(err)
	}
	reloaded, ok := after.projectAgent(project, "designer")
	if !ok || reloaded.ChatMode != "dev" {
		t.Fatalf("reloaded agent mode = %+v", reloaded)
	}

	invalid := httptest.NewRequest(http.MethodPatch, "/agents/designer", strings.NewReader(`{"chat_mode":"execute"}`))
	invalid.Header.Set("Content-Type", "application/json")
	invalidResponse := httptest.NewRecorder()
	a.handleAgents(invalidResponse, invalid, project, []string{"designer"})
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid chat mode status = %d body=%s", invalidResponse.Code, invalidResponse.Body.String())
	}
}
