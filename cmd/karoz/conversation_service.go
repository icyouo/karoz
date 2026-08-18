package main

import "sync"

// conversationService owns the canonical session-event store and the derived
// transcript/session projections. No other app state owns or persists these
// conversation facts.
type conversationService struct {
	mu                            sync.RWMutex
	events                        map[string][]AgentSessionEvent
	messages                      map[string][]AgentMessage
	transcripts                   map[string][]AgentTranscriptItem
	sessions                      map[string]AgentSessionState
	checkpointSessionSaveOverride func(map[string]AgentSessionState) error
}

func newConversationService() *conversationService {
	return &conversationService{
		events:      map[string][]AgentSessionEvent{},
		messages:    map[string][]AgentMessage{},
		transcripts: map[string][]AgentTranscriptItem{},
		sessions:    map[string]AgentSessionState{},
	}
}

// ArchivedMessagesFor derives the compacted portion of a session directly
// from canonical message events. It is intentionally not a second persisted
// archive: a checkpoint boundary is itself a durable session event.
func (service *conversationService) ArchivedMessagesFor(key string) []AgentArchiveMessage {
	if service == nil {
		return nil
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	state, ok := service.sessions[key]
	if !ok || state.CoveredSeqEnd <= 0 {
		return nil
	}
	items := make([]AgentArchiveMessage, 0, len(service.messages[key]))
	for _, message := range service.messages[key] {
		if message.Seq <= 0 || message.Seq > state.CoveredSeqEnd {
			continue
		}
		items = append(items, AgentArchiveMessage{
			ID: message.ID, ProjectID: message.ProjectID, AgentID: message.AgentID,
			SessionID: message.SessionID, Seq: message.Seq, Role: message.Role,
			Intent: message.Intent, Body: message.Body, CreatedAt: message.CreatedAt,
			ArchivedAt: state.LastCheckpointAt,
		})
	}
	return items
}

func (service *conversationService) MessagesFor(key string) []AgentMessage {
	if service == nil {
		return nil
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	return append([]AgentMessage{}, service.messages[key]...)
}

func (service *conversationService) AppendMessage(key string, message AgentMessage) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.messages == nil {
		service.messages = map[string][]AgentMessage{}
	}
	service.messages[key] = append(service.messages[key], message)
}

func (service *conversationService) ReplaceMessages(key string, messages []AgentMessage) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.messages == nil {
		service.messages = map[string][]AgentMessage{}
	}
	service.messages[key] = append([]AgentMessage{}, messages...)
}

func (service *conversationService) MessageSnapshot() map[string][]AgentMessage {
	if service == nil {
		return map[string][]AgentMessage{}
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	snapshot := make(map[string][]AgentMessage, len(service.messages))
	for key, messages := range service.messages {
		snapshot[key] = append([]AgentMessage{}, messages...)
	}
	return snapshot
}

func (service *conversationService) ResetMessages() {
	if service == nil {
		return
	}
	service.mu.Lock()
	service.messages = map[string][]AgentMessage{}
	service.mu.Unlock()
}

func (service *conversationService) Session(key string) (AgentSessionState, bool) {
	if service == nil {
		return AgentSessionState{}, false
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	state, ok := service.sessions[key]
	return state, ok
}

func (service *conversationService) SetSession(key string, state AgentSessionState) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.sessions == nil {
		service.sessions = map[string]AgentSessionState{}
	}
	service.sessions[key] = state
}

func (service *conversationService) DeleteSession(key string) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	delete(service.sessions, key)
}

func (service *conversationService) ReplaceSessions(sessions map[string]AgentSessionState) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.sessions = cloneAgentSessions(sessions)
}

// SessionSnapshot is the observation boundary used by checkpoint and memory
// transactions. The returned map is detached from the conversation owner.
func (service *conversationService) SessionSnapshot() map[string]AgentSessionState {
	if service == nil {
		return map[string]AgentSessionState{}
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	out := make(map[string]AgentSessionState, len(service.sessions))
	for key, state := range service.sessions {
		out[key] = state
	}
	return out
}

func (service *conversationService) ResetSessions() {
	if service == nil {
		return
	}
	service.mu.Lock()
	service.sessions = map[string]AgentSessionState{}
	service.mu.Unlock()
}

func (service *conversationService) Append(key string, event AgentSessionEvent) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.events == nil {
		service.events = map[string][]AgentSessionEvent{}
	}
	service.events[key] = append(service.events[key], cloneAgentSessionEvent(event))
}

func (service *conversationService) EventsFor(key string) []AgentSessionEvent {
	if service == nil {
		return nil
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	return append([]AgentSessionEvent{}, service.events[key]...)
}

func (service *conversationService) Replace(key string, events []AgentSessionEvent) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.events == nil {
		service.events = map[string][]AgentSessionEvent{}
	}
	service.events[key] = append([]AgentSessionEvent{}, events...)
}

func (service *conversationService) Reset() {
	if service == nil {
		return
	}
	service.mu.Lock()
	service.events = map[string][]AgentSessionEvent{}
	service.mu.Unlock()
}

func (service *conversationService) Snapshot() map[string][]AgentSessionEvent {
	if service == nil {
		return map[string][]AgentSessionEvent{}
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	snapshot := make(map[string][]AgentSessionEvent, len(service.events))
	for key, events := range service.events {
		snapshot[key] = append([]AgentSessionEvent{}, events...)
	}
	return snapshot
}

func (service *conversationService) AppendTranscript(key string, item AgentTranscriptItem) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.transcripts == nil {
		service.transcripts = map[string][]AgentTranscriptItem{}
	}
	service.transcripts[key] = append(service.transcripts[key], item)
}

func (service *conversationService) TranscriptsFor(key string) []AgentTranscriptItem {
	if service == nil {
		return nil
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	return append([]AgentTranscriptItem{}, service.transcripts[key]...)
}

func (service *conversationService) ReplaceTranscripts(key string, items []AgentTranscriptItem) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.transcripts == nil {
		service.transcripts = map[string][]AgentTranscriptItem{}
	}
	service.transcripts[key] = append([]AgentTranscriptItem{}, items...)
}

func (service *conversationService) ResetTranscripts() {
	if service == nil {
		return
	}
	service.mu.Lock()
	service.transcripts = map[string][]AgentTranscriptItem{}
	service.mu.Unlock()
}

// conversationServiceLocked returns the event-store owner while a.mu is held.
// Lazy initialization keeps small, focused application tests valid without
// reintroducing a second event store on app.
func (a *app) conversationServiceLocked() *conversationService {
	if a.conversation == nil {
		a.conversation = newConversationService()
	}
	return a.conversation
}
