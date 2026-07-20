package main

import (
	"context"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestResidentAgentModelConfigPersistsAndRunSnapshotsIt(t *testing.T) {
	dataDir := t.TempDir()
	authPath := dataDir + "/auth.json"
	if err := os.WriteFile(authPath, []byte(`{"tokens":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAROZ_CODEX_AUTH_PATH", authPath)
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir(), DefaultBranch: "main"}
	a.agents[project.ID] = []Agent{{ID: "architect", ProjectID: project.ID, Name: "Architect"}}
	updated, err := a.updateProjectAgent(project, "architect", AgentUpdateRequest{Provider: ptrString("codex"), Model: ptrString("gpt-5.3-codex"), ThinkingEffort: ptrString("high")})
	if err != nil {
		t.Fatal(err)
	}
	run, started := a.beginAgentRun(AgentRunInput{ProjectID: project.ID, AgentID: updated.ID, Trigger: RunTriggerUserDirect, TurnType: "ask"})
	if !started || run.Provider != "codex" || run.Model != "gpt-5.3-codex" || run.ThinkingEffort != "high" || run.ModelConfigVersion != updated.ModelConfigVersion {
		t.Fatalf("run snapshot = %+v", run)
	}
	if _, err := a.updateProjectAgent(project, "architect", AgentUpdateRequest{Model: ptrString("gpt-5.2")}); err == nil {
		t.Fatal("expected active-run model update to fail")
	}
	a.finishAgentRun(project.ID, updated.ID, run.ID, RunStateDone, nil)
	after := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	if err := after.loadAgents(); err != nil {
		t.Fatal(err)
	}
	reloaded, ok := after.projectAgent(project, "architect")
	if !ok || reloaded.Provider != "codex" || reloaded.Model != "gpt-5.3-codex" || reloaded.ThinkingEffort != "high" {
		t.Fatalf("reloaded = %+v", reloaded)
	}
}

func TestResidentAgentCanSwitchToClaudeAndActiveRunReturnsConflict(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir(), DefaultBranch: "main"}
	a.agents[project.ID] = []Agent{{ID: "reviewer", ProjectID: project.ID, Name: "Reviewer"}}
	patch := httptest.NewRequest(http.MethodPatch, "/agents/reviewer", strings.NewReader(`{"provider":"claude","model":"claude-sonnet-4-6","thinking_effort":"high","expected_model_config_version":1}`))
	patch.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.handleAgents(response, patch, project, []string{"reviewer"})
	if response.Code != http.StatusOK {
		t.Fatalf("Claude switch status=%d body=%s", response.Code, response.Body.String())
	}
	configured, _ := a.projectAgent(project, "reviewer")
	run, started := a.beginAgentRun(AgentRunInput{ProjectID: project.ID, AgentID: configured.ID, Trigger: RunTriggerUserDirect, TurnType: "ask"})
	if !started || run.Provider != "claude" || run.Model != "claude-sonnet-4-6" || run.ThinkingEffort != "high" {
		t.Fatalf("Claude run snapshot=%+v", run)
	}
	patch = httptest.NewRequest(http.MethodPatch, "/agents/reviewer", strings.NewReader(`{"provider":"claude","model":"claude-opus-4-8","thinking_effort":"xhigh"}`))
	patch.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	a.handleAgents(response, patch, project, []string{"reviewer"})
	if response.Code != http.StatusConflict {
		t.Fatalf("active switch status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRuntimeProvidersEndpointReportsProviderAvailability(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KAROZ_CLAUDE_CLI_AUTH", "disabled")
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	response := httptest.NewRecorder()
	a.handleRuntimeProviders(response, httptest.NewRequest(http.MethodGet, "/api/runtime/providers", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"claude"`) || !strings.Contains(response.Body.String(), `"available":false`) {
		t.Fatalf("providers response=%d %s", response.Code, response.Body.String())
	}
}

func TestRuntimeProvidersPreferClaudeCLIOAuth(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KAROZ_CLAUDE_CLI_AUTH", "available")
	descriptor, ok := residentProviderDescriptor("claude")
	if !ok || !descriptor.Available || descriptor.Transport != "claude-cli-oauth" {
		t.Fatalf("Claude descriptor=%+v", descriptor)
	}
}

func ptrString(value string) *string { return &value }
