package main

import collaborationdomain "github.com/karoz/karoz/internal/collaboration"

type appHandoffRepository struct{ app *app }

func (repository appHandoffRepository) Get(projectID, agentID, messageID string) (AgentInboxMessage, bool, error) {
	message, ok := repository.app.inboxMessage(projectID, agentID, messageID)
	if !ok {
		return AgentInboxMessage{}, false, nil
	}
	message, _ = normalizeHandoffMessage(message)
	return message, true, nil
}

func (repository appHandoffRepository) Save(message AgentInboxMessage) error {
	key := projectAgentKey(message.ProjectID, message.TargetAgentID)
	repository.app.collaborationServiceLocked().UpsertInbox(key, message)
	return repository.app.saveInbox()
}

type appHandoffEvents struct{ app *app }

func (events appHandoffEvents) HandoffChanged(change collaborationdomain.HandoffChange) {
	events.app.appendAgentHandoffStateEvent(change.Handoff, change.From, change.Reason)
	events.app.emitRuntimeStateChanged(RuntimeEvent{
		ID: randomID(), ProjectID: change.Handoff.ProjectID, Kind: "handoff_changed", EntityID: change.Handoff.ID,
		From: change.From, To: change.To, Reason: change.Reason, CreatedAt: change.At,
		FromAgentID: change.Handoff.SourceAgentID, ToAgentID: change.Handoff.TargetAgentID,
	})
}

func (a *app) handoffService() collaborationdomain.HandoffService {
	return collaborationdomain.HandoffService{
		Repository: appHandoffRepository{app: a},
		Events:     appHandoffEvents{app: a},
	}
}
