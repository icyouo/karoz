package main

import (
	"context"
	httpapiadapter "github.com/karoz/karoz/internal/httpapi"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	"net/http"
	"sync"
)

func newApp(settings Settings) *app {
	supervisorCtx, supervisorCancel := context.WithCancel(context.Background())
	a := &app{
		supervisorCtx:            supervisorCtx,
		supervisorCancel:         supervisorCancel,
		settings:                 settings,
		tasks:                    map[string][]Task{},
		taskRunCancels:           map[string]taskRun{},
		taskIntegrationLocks:     map[string]*sync.Mutex{},
		agents:                   map[string][]Agent{},
		archives:                 map[string][]AgentArchiveMessage{},
		memories:                 map[string][]AgentMemoryEntry{},
		blackboard:               map[string][]AgentBlackboardEntry{},
		artifacts:                map[string][]Artifact{},
		groups:                   map[string][]AgentGroup{},
		groupInbox:               map[string][]GroupInboxMessage{},
		plans:                    map[string][]WorkPlan{},
		inbox:                    map[string][]AgentInboxMessage{},
		taskHooks:                map[string][]TaskRuntimeHook{},
		agentRoutes:              map[string][]AgentRoute{},
		agentMessages:            map[string][]AgentMessage{},
		agentTranscripts:         map[string][]AgentTranscriptItem{},
		agentSessions:            map[string]AgentSessionState{},
		projectAliases:           map[string]string{},
		agentRuns:                map[string]AgentRun{},
		agentRunCancels:          map[string]context.CancelFunc{},
		agentRunContexts:         map[string]context.Context{},
		agentRunWorkers:          map[string]string{},
		agentRunCancelling:       map[string]string{},
		agentRunResultCommitted:  map[string]string{},
		agentRunLedgers:          map[string]*agentRunLedger{},
		agentRunFinishedWatchers: map[string]map[chan struct{}]struct{}{},
		residentBashApprovals:    map[string]ResidentBashApproval{},
		backgroundOwnerDeleting:  map[string]bool{},
		processTerminalWake:      make(chan struct{}, 1),
		schedulerQueue:           runtimedomain.NewSchedulerQueue(),
		schedulerExecutors:       map[ScheduledRunKind]ScheduledRunExecutor{},
		runtimeHooks:             map[string]bool{},
		runtimeWatchers:          map[string]map[chan RuntimeEvent]bool{},
	}
	a.modelProvider = cliModelProviderAdapter{app: a}
	a.dynamicTools = mcpDynamicToolAdapter{app: a}
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
