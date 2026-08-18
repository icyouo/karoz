package main

import (
	"context"
	"sync"
	"time"

	runtimedomain "github.com/karoz/karoz/internal/runtime"
)

// agentRuntimeCoordinator owns the ephemeral lifecycle/control state of
// resident Agent Runs. Durable Run facts stay in the session-event log; these
// handles exist only while a local worker is alive.
type agentRuntimeCoordinator struct {
	runs                               map[string]AgentRun
	cancels                            map[string]context.CancelFunc
	contexts                           map[string]context.Context
	workers                            map[string]string
	cancelling                         map[string]string
	resultCommitted                    map[string]string
	ledgers                            map[string]*agentRunLedger
	finishedWatchers                   map[string]map[chan struct{}]struct{}
	residentBashApprovals              map[string]ResidentBashApproval
	runtimeHooks                       map[string]bool
	runtimeWatchers                    map[string]map[chan RuntimeEvent]bool
	schedulerQueue                     *runtimedomain.SchedulerQueue
	schedulerExecutors                 map[ScheduledRunKind]ScheduledRunExecutor
	checkpointClaims                   map[string]agentCheckpointClaim
	checkpointRetryNotBefore           map[string]time.Time
	scheduledRunBeforeBindHook         func()
	scheduledRunBeforeResultCommitHook func()
	backgroundOwnerDeleting            map[string]bool
	backgroundOwnerMu                  sync.Mutex
	schedulerPersistMu                 sync.Mutex
	scheduledRunsSaveOverride          func(scheduledRunSnapshot) error
	agentRunAfterProviderHook          func()
	agentRunAfterSuccessHook           func()
	lifecycle                          *runtimedomain.RunLifecycle
	control                            *runtimedomain.RunControl
}

func newAgentRuntimeCoordinator() *agentRuntimeCoordinator {
	return &agentRuntimeCoordinator{
		runs:                     map[string]AgentRun{},
		cancels:                  map[string]context.CancelFunc{},
		contexts:                 map[string]context.Context{},
		workers:                  map[string]string{},
		cancelling:               map[string]string{},
		resultCommitted:          map[string]string{},
		ledgers:                  map[string]*agentRunLedger{},
		finishedWatchers:         map[string]map[chan struct{}]struct{}{},
		residentBashApprovals:    map[string]ResidentBashApproval{},
		runtimeHooks:             map[string]bool{},
		runtimeWatchers:          map[string]map[chan RuntimeEvent]bool{},
		schedulerQueue:           runtimedomain.NewSchedulerQueue(),
		schedulerExecutors:       map[ScheduledRunKind]ScheduledRunExecutor{},
		checkpointClaims:         map[string]agentCheckpointClaim{},
		checkpointRetryNotBefore: map[string]time.Time{},
		backgroundOwnerDeleting:  map[string]bool{},
	}
}

// agentRuntimeLocked returns the sole owner of resident Run state. Callers
// must hold app.mu; lazy initialization keeps small unit-test app fixtures
// valid without restoring app-owned maps.
func (a *app) agentRuntimeLocked() *agentRuntimeCoordinator {
	if a.agentRuntime == nil {
		a.agentRuntime = newAgentRuntimeCoordinator()
	}
	return a.agentRuntime
}
