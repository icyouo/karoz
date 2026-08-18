package main

import (
	"context"
	agentdomain "github.com/karoz/karoz/internal/agent"
	executiondomain "github.com/karoz/karoz/internal/execution"
	httpapiadapter "github.com/karoz/karoz/internal/httpapi"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	taskdomain "github.com/karoz/karoz/internal/task"
	"net/http"
)

func newApp(settings Settings) *app {
	supervisorCtx, supervisorCancel := context.WithCancel(context.Background())
	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	a := &app{
		supervisorCtx:             supervisorCtx,
		supervisorCancel:          supervisorCancel,
		processRuntimeCoordinator: *newProcessRuntimeCoordinator(),
		monitorRuntimeCoordinator: *newMonitorRuntimeCoordinator(monitorCtx, monitorCancel),
		settings:                  settings,
		projectTasks:              newProjectTaskCoordinator(),
		agentDirectory:            newAgentDirectory(),
		memoryStore:               newMemoryStore(),
		artifactCatalog:           newArtifactCatalog(),
		conversation:              newConversationService(),
		collaboration:             newCollaborationService(),
		projectRegistry:           newProjectRegistry(),
		agentRuntime:              newAgentRuntimeCoordinator(),
		processTerminalOutbox:     newProcessTerminalOutboxCoordinator(),
		processOutputRuntime:      newProcessOutputCoordinator(),
	}
	a.modelProvider = cliModelProviderAdapter{app: a}
	a.dynamicTools = mcpDynamicToolAdapter{app: a}
	a.commandRunner = executiondomain.NewHostRunner()
	a.streamRunner = executiondomain.NewHostStreamRunner()
	a.sandboxEnforcer = executiondomain.UnsupportedSandboxEnforcer{}
	a.agentRuntime.lifecycle = runtimedomain.NewRunLifecycle(appAgentRunRepository{app: a})
	a.agentRuntime.control = runtimedomain.NewRunControl(appAgentRunRepository{app: a})
	a.agentService = agentdomain.NewService(appAgentRepository{app: a})
	a.taskService = taskdomain.NewService(appTaskRepository{app: a})
	// Static resident tools are part of the process contract, so validate the
	// definition/handler/policy bijection before the app can advertise or serve
	// any of them. Dynamic MCP tools retain their separate runtime contract.
	if err := a.initializeResidentToolRegistry(); err != nil {
		panic("initialize static resident tools: " + err.Error())
	}
	return a
}

func (a *app) httpHandler() http.Handler {
	mux := httpapiadapter.NewMux(httpapiadapter.Handlers{
		Index: a.handleIndex, Settings: a.handleSettings, FolderDialog: a.handleFolderDialog,
		AgentTemplates: a.handleAgentTemplates, AgentTeamTemplates: a.handleAgentTeamTemplates,
		Diagnostics: a.handleDiagnostics, CLI2API: a.handleCLI2API,
		RuntimeProviders: a.handleRuntimeProviders,
		Projects:         a.handleProjects, ProjectScoped: a.handleProjectScoped,
	})
	return withLocalStudioMutationGuard(mux)
}
