package main

import runtimedomain "github.com/karoz/karoz/internal/runtime"

// agentSessionsForTest exposes a detached projection for assertions without
// giving tests a second mutable owner for conversation state.
func agentSessionsForTest(a *app) map[string]AgentSessionState {
	if a == nil || a.conversation == nil {
		return map[string]AgentSessionState{}
	}
	return a.conversation.SessionSnapshot()
}

func setAgentSessionForTest(a *app, key string, state AgentSessionState) {
	if a.conversation == nil {
		a.conversation = newConversationService()
	}
	a.conversation.SetSession(key, state)
}

func deleteAgentSessionForTest(a *app, key string) {
	if a.conversation != nil {
		a.conversation.DeleteSession(key)
	}
}

func replaceAgentMessagesForTest(a *app, key string, messages []AgentMessage) {
	if a.conversation == nil {
		a.conversation = newConversationService()
	}
	a.conversation.ReplaceMessages(key, messages)
}

func appendAgentMessageForTest(a *app, key string, message AgentMessage) {
	if a.conversation == nil {
		a.conversation = newConversationService()
	}
	a.conversation.AppendMessage(key, message)
}

func replaceProjectArchivesForTest(a *app, key string, archives []AgentArchiveMessage) {
	if a.conversation == nil {
		a.conversation = newConversationService()
	}
	messages := make([]AgentMessage, 0, len(archives))
	var coveredSeqEnd int64
	for _, archive := range archives {
		messages = append(messages, AgentMessage{
			ID: archive.ID, ProjectID: archive.ProjectID, AgentID: archive.AgentID,
			SessionID: archive.SessionID, Seq: archive.Seq, Role: archive.Role,
			Intent: archive.Intent, Body: archive.Body, CreatedAt: archive.CreatedAt,
		})
		if archive.Seq > coveredSeqEnd {
			coveredSeqEnd = archive.Seq
		}
	}
	a.conversation.ReplaceMessages(key, messages)
	a.conversation.SetSession(key, AgentSessionState{CoveredSeqEnd: coveredSeqEnd, BoundarySeq: coveredSeqEnd, ShortWindowStartSeq: coveredSeqEnd + 1})
}

func agentRuntimeForTest(runs map[string]AgentRun) *agentRuntimeCoordinator {
	return agentRuntimeForTestWithRuntimeState(runs, nil, nil)
}

func agentRuntimeForTestWithRuntimeState(runs map[string]AgentRun, hooks map[string]bool, watchers map[string]map[chan RuntimeEvent]bool) *agentRuntimeCoordinator {
	runtime := newAgentRuntimeCoordinator()
	if runs != nil {
		runtime.runs = runs
	}
	if hooks != nil {
		runtime.runtimeHooks = hooks
	}
	if watchers != nil {
		runtime.runtimeWatchers = watchers
	}
	return runtime
}

func agentRuntimeForTestWithScheduler(queue *runtimedomain.SchedulerQueue, executors map[ScheduledRunKind]ScheduledRunExecutor) *agentRuntimeCoordinator {
	runtime := newAgentRuntimeCoordinator()
	if queue != nil {
		runtime.schedulerQueue = queue
	}
	if executors != nil {
		runtime.schedulerExecutors = executors
	}
	return runtime
}

func projectTasksForTest(tasks map[string][]Task) *projectTaskCoordinator {
	coordinator := newProjectTaskCoordinator()
	if tasks != nil {
		coordinator.tasks = tasks
	}
	return coordinator
}

func agentDirectoryForTest(agents map[string][]Agent) *agentDirectory {
	directory := newAgentDirectory()
	if agents != nil {
		directory.agents = agents
	}
	return directory
}

func artifactCatalogForTest(artifacts map[string][]Artifact) *artifactCatalog {
	catalog := newArtifactCatalog()
	if artifacts != nil {
		catalog.artifacts = artifacts
	}
	return catalog
}

func memoryStoreForTest(entries map[string][]AgentMemoryEntry) *memoryStore {
	store := newMemoryStore()
	if entries != nil {
		store.entries = entries
	}
	return store
}

func replaceInboxForTest(a *app, inbox map[string][]AgentInboxMessage) {
	if a.collaboration == nil {
		a.collaboration = newCollaborationService()
	}
	a.collaboration.ReplaceInbox(inbox)
}

func appendInboxForTest(a *app, key string, message AgentInboxMessage) {
	if a.collaboration == nil {
		a.collaboration = newCollaborationService()
	}
	a.collaboration.AppendInbox(key, message)
}

func replaceBlackboardForTest(a *app, entries map[string][]AgentBlackboardEntry) {
	if a.collaboration == nil {
		a.collaboration = newCollaborationService()
	}
	a.collaboration.ReplaceBlackboard(entries)
}

func appendBlackboardForTest(a *app, projectID string, entry AgentBlackboardEntry) {
	if a.collaboration == nil {
		a.collaboration = newCollaborationService()
	}
	a.collaboration.AppendBlackboard(projectID, entry)
}

func replacePlansForTest(a *app, projectID string, plans []WorkPlan) {
	if a.collaboration == nil {
		a.collaboration = newCollaborationService()
	}
	a.collaboration.ReplacePlans(projectID, plans)
}

func replaceRoutesForTest(a *app, projectID string, routes []AgentRoute) {
	if a.collaboration == nil {
		a.collaboration = newCollaborationService()
	}
	a.collaboration.ReplaceRoutes(projectID, routes)
}

func replaceGroupsForTest(a *app, projectID string, groups []AgentGroup) {
	if a.collaboration == nil {
		a.collaboration = newCollaborationService()
	}
	a.collaboration.ReplaceGroups(projectID, groups)
}
