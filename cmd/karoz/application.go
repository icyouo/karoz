package main

import (
	"context"
	httpapiadapter "github.com/karoz/karoz/internal/httpapi"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	"net/http"
)

func newApp(settings Settings) *app {
	a := &app{
		settings:           settings,
		tasks:              map[string][]Task{},
		agents:             map[string][]Agent{},
		archives:           map[string][]AgentArchiveMessage{},
		memories:           map[string][]AgentMemoryEntry{},
		blackboard:         map[string][]AgentBlackboardEntry{},
		artifacts:          map[string][]Artifact{},
		inbox:              map[string][]AgentInboxMessage{},
		taskHooks:          map[string][]TaskRuntimeHook{},
		agentRoutes:        map[string][]AgentRoute{},
		agentMessages:      map[string][]AgentMessage{},
		agentSessions:      map[string]AgentSessionState{},
		projectAliases:     map[string]string{},
		agentRuns:          map[string]AgentRun{},
		agentRunCancels:    map[string]context.CancelFunc{},
		schedulerQueue:     runtimedomain.NewSchedulerQueue(),
		schedulerExecutors: map[ScheduledRunKind]ScheduledRunExecutor{},
		runtimeHooks:       map[string]bool{},
		runtimeWatchers:    map[string]map[chan RuntimeEvent]bool{},
	}
	a.modelProvider = cliModelProviderAdapter{app: a}
	a.dynamicTools = mcpDynamicToolAdapter{app: a}
	return a
}

func (a *app) httpHandler() http.Handler {
	return httpapiadapter.NewMux(httpapiadapter.Handlers{
		Index: a.handleIndex, Settings: a.handleSettings, FolderDialog: a.handleFolderDialog,
		AgentTemplates: a.handleAgentTemplates, AgentTeamTemplates: a.handleAgentTeamTemplates,
		Diagnostics: a.handleDiagnostics, CLI2API: a.handleCLI2API,
		Projects: a.handleProjects, ProjectScoped: a.handleProjectScoped,
	})
}
