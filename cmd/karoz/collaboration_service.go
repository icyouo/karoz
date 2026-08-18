package main

import "sync"

// collaborationService owns mutable collaboration projections. Persistence
// remains an application adapter; maps and their mutation lock do not.
type collaborationService struct {
	mu             sync.RWMutex
	inbox          map[string][]AgentInboxMessage
	blackboard     map[string][]AgentBlackboardEntry
	groupInbox     map[string][]GroupInboxMessage
	plans          map[string][]WorkPlan
	routes         map[string][]AgentRoute
	groups         map[string][]AgentGroup
	handoffOpsMu   sync.Mutex
	handoffReplyMu sync.Mutex
}

func newCollaborationService() *collaborationService {
	return &collaborationService{inbox: map[string][]AgentInboxMessage{}, blackboard: map[string][]AgentBlackboardEntry{}, groupInbox: map[string][]GroupInboxMessage{}, plans: map[string][]WorkPlan{}, routes: map[string][]AgentRoute{}, groups: map[string][]AgentGroup{}}
}

func (service *collaborationService) GroupsFor(projectID string) []AgentGroup {
	if service == nil {
		return nil
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	return append([]AgentGroup{}, service.groups[projectID]...)
}

func (service *collaborationService) ReplaceGroups(projectID string, groups []AgentGroup) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.groups == nil {
		service.groups = map[string][]AgentGroup{}
	}
	service.groups[projectID] = append([]AgentGroup{}, groups...)
}

func (service *collaborationService) ResetGroups() {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.groups = map[string][]AgentGroup{}
}

func (service *collaborationService) RoutesFor(projectID string) []AgentRoute {
	if service == nil {
		return nil
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	return append([]AgentRoute{}, service.routes[projectID]...)
}

func (service *collaborationService) ReplaceRoutes(projectID string, routes []AgentRoute) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.routes == nil {
		service.routes = map[string][]AgentRoute{}
	}
	service.routes[projectID] = append([]AgentRoute{}, routes...)
}

func (service *collaborationService) RoutesSnapshot() map[string][]AgentRoute {
	if service == nil {
		return map[string][]AgentRoute{}
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	snapshot := make(map[string][]AgentRoute, len(service.routes))
	for key, items := range service.routes {
		snapshot[key] = append([]AgentRoute{}, items...)
	}
	return snapshot
}

func (service *collaborationService) ReplaceAllRoutes(routes map[string][]AgentRoute) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.routes = make(map[string][]AgentRoute, len(routes))
	for key, items := range routes {
		service.routes[key] = append([]AgentRoute{}, items...)
	}
}

func (service *collaborationService) PlansFor(projectID string) []WorkPlan {
	if service == nil {
		return nil
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	return append([]WorkPlan{}, service.plans[projectID]...)
}

func (service *collaborationService) PlanSnapshot() map[string][]WorkPlan {
	if service == nil {
		return map[string][]WorkPlan{}
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	snapshot := make(map[string][]WorkPlan, len(service.plans))
	for key, items := range service.plans {
		snapshot[key] = append([]WorkPlan{}, items...)
	}
	return snapshot
}

func (service *collaborationService) ReplacePlans(projectID string, plans []WorkPlan) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.plans == nil {
		service.plans = map[string][]WorkPlan{}
	}
	service.plans[projectID] = append([]WorkPlan{}, plans...)
}

func (service *collaborationService) ResetPlans() {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.plans = map[string][]WorkPlan{}
}

func (service *collaborationService) UpdatePlans(projectID string, update func([]WorkPlan) []WorkPlan) {
	if service == nil || update == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.plans == nil {
		service.plans = map[string][]WorkPlan{}
	}
	service.plans[projectID] = append([]WorkPlan{}, update(append([]WorkPlan{}, service.plans[projectID]...))...)
}

func (service *collaborationService) GroupInboxFor(projectID string) []GroupInboxMessage {
	if service == nil {
		return nil
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	return append([]GroupInboxMessage{}, service.groupInbox[projectID]...)
}

func (service *collaborationService) ReplaceGroupInbox(projectID string, items []GroupInboxMessage) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.groupInbox == nil {
		service.groupInbox = map[string][]GroupInboxMessage{}
	}
	service.groupInbox[projectID] = append([]GroupInboxMessage{}, items...)
}

func (service *collaborationService) ResetGroupInbox() {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.groupInbox = map[string][]GroupInboxMessage{}
}

func (service *collaborationService) AppendGroupInbox(projectID string, message GroupInboxMessage) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.groupInbox == nil {
		service.groupInbox = map[string][]GroupInboxMessage{}
	}
	service.groupInbox[projectID] = append(service.groupInbox[projectID], message)
}

func (service *collaborationService) UpdateGroupInbox(projectID string, update func(*GroupInboxMessage) bool) bool {
	if service == nil || update == nil {
		return false
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	for index := range service.groupInbox[projectID] {
		if update(&service.groupInbox[projectID][index]) {
			return true
		}
	}
	return false
}

func (service *collaborationService) BlackboardFor(projectID string) []AgentBlackboardEntry {
	if service == nil {
		return nil
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	return append([]AgentBlackboardEntry{}, service.blackboard[projectID]...)
}

func (service *collaborationService) BlackboardSnapshot() map[string][]AgentBlackboardEntry {
	if service == nil {
		return map[string][]AgentBlackboardEntry{}
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	snapshot := make(map[string][]AgentBlackboardEntry, len(service.blackboard))
	for key, items := range service.blackboard {
		snapshot[key] = append([]AgentBlackboardEntry{}, items...)
	}
	return snapshot
}

func (service *collaborationService) ReplaceBlackboard(snapshot map[string][]AgentBlackboardEntry) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.blackboard = make(map[string][]AgentBlackboardEntry, len(snapshot))
	for key, items := range snapshot {
		service.blackboard[key] = append([]AgentBlackboardEntry{}, items...)
	}
}

func (service *collaborationService) ReplaceProjectBlackboard(projectID string, items []AgentBlackboardEntry) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.blackboard == nil {
		service.blackboard = map[string][]AgentBlackboardEntry{}
	}
	service.blackboard[projectID] = append([]AgentBlackboardEntry{}, items...)
}

func (service *collaborationService) AppendBlackboard(projectID string, entry AgentBlackboardEntry) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.blackboard == nil {
		service.blackboard = map[string][]AgentBlackboardEntry{}
	}
	service.blackboard[projectID] = append(service.blackboard[projectID], entry)
}

func (service *collaborationService) MutateBlackboard(projectID string, mutate func([]AgentBlackboardEntry) []AgentBlackboardEntry) {
	if service == nil || mutate == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.blackboard == nil {
		service.blackboard = map[string][]AgentBlackboardEntry{}
	}
	service.blackboard[projectID] = append([]AgentBlackboardEntry{}, mutate(append([]AgentBlackboardEntry{}, service.blackboard[projectID]...))...)
}

func (service *collaborationService) InboxFor(key string) []AgentInboxMessage {
	if service == nil {
		return nil
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	return append([]AgentInboxMessage{}, service.inbox[key]...)
}

func (service *collaborationService) InboxSnapshot() map[string][]AgentInboxMessage {
	if service == nil {
		return map[string][]AgentInboxMessage{}
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	snapshot := make(map[string][]AgentInboxMessage, len(service.inbox))
	for key, items := range service.inbox {
		snapshot[key] = append([]AgentInboxMessage{}, items...)
	}
	return snapshot
}

func (service *collaborationService) ReplaceInbox(snapshot map[string][]AgentInboxMessage) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.inbox = make(map[string][]AgentInboxMessage, len(snapshot))
	for key, items := range snapshot {
		service.inbox[key] = append([]AgentInboxMessage{}, items...)
	}
}

func (service *collaborationService) AppendInbox(key string, message AgentInboxMessage) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.inbox == nil {
		service.inbox = map[string][]AgentInboxMessage{}
	}
	service.inbox[key] = append(service.inbox[key], message)
}

func (service *collaborationService) UpdateInbox(key, messageID string, update func(*AgentInboxMessage)) bool {
	if service == nil || update == nil {
		return false
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	for index := range service.inbox[key] {
		if service.inbox[key][index].ID == messageID {
			update(&service.inbox[key][index])
			return true
		}
	}
	return false
}

func (service *collaborationService) UpsertInbox(key string, message AgentInboxMessage) {
	if service == nil {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.inbox == nil {
		service.inbox = map[string][]AgentInboxMessage{}
	}
	for index := range service.inbox[key] {
		if service.inbox[key][index].ID == message.ID {
			service.inbox[key][index] = message
			return
		}
	}
	service.inbox[key] = append(service.inbox[key], message)
}

func (a *app) collaborationServiceLocked() *collaborationService {
	if a.collaboration == nil {
		a.collaboration = newCollaborationService()
	}
	return a.collaboration
}
