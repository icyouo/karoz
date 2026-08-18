package main

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

func (a *app) projectAgents(project Project) []Agent {
	agents, created := a.ensureAgentService().Ensure(project.ID, func() Agent {
		return newAgentFromTemplate(project, defaultKarozAgentTemplate(), "karoz", "Karoz")
	})
	if len(agents) == 0 {
		return []Agent{}
	}
	if created {
		if err := a.saveAgents(); err != nil {
			log.Printf("save agents: %v", err)
		}
	}
	for i := range agents {
		agents[i] = a.agentWithRuntimeState(project, agents[i])
	}
	sort.SliceStable(agents, func(i, j int) bool {
		if agents[i].ID == "karoz" {
			return true
		}
		if agents[j].ID == "karoz" {
			return false
		}
		return agents[i].CreatedAt.Before(agents[j].CreatedAt)
	})
	return agents
}

func (a *app) projectAgent(project Project, agentRef string) (Agent, bool) {
	agentRef = strings.TrimSpace(agentRef)
	if agentRef == "" {
		return Agent{}, false
	}
	agents := a.projectAgents(project)
	// Internal callers persist canonical IDs, so preserve exact-ID lookup first.
	for _, agent := range agents {
		if agent.ID == agentRef {
			return agent, true
		}
	}
	// Collaboration tools and prompts address agents by their unique nickname.
	// IDs remain an internal persistence detail and a compatibility fallback.
	for _, agent := range agents {
		if strings.EqualFold(strings.TrimSpace(agent.Nickname), agentRef) {
			return agent, true
		}
	}
	return Agent{}, false
}

func (a *app) agentNickname(project Project, agentID string) string {
	if agent, ok := a.projectAgent(project, agentID); ok {
		return firstNonEmpty(agent.Nickname, agent.DisplayName, agent.Name, agent.ID)
	}
	return agentID
}

func (a *app) agentWithRuntimeState(project Project, agent Agent) Agent {
	if strings.TrimSpace(agent.ID) == "" {
		agent.ID = "karoz"
	}
	if agent.ID == "karoz" && strings.TrimSpace(agent.Nickname) == "karoz" {
		agent.Nickname = "Karoz"
	}
	if agent.ID == "karoz" && strings.TrimSpace(agent.DisplayName) == "karoz" {
		agent.DisplayName = "Karoz"
	}
	if strings.TrimSpace(agent.ProjectID) == "" {
		agent.ProjectID = project.ID
	}
	if strings.TrimSpace(agent.Runtime) == "" {
		agent.Runtime = "resident"
	}
	agent.ChatMode = normalizeChatTurnType(agent.ChatMode)
	agent = normalizeAgentModelConfig(agent)
	if strings.TrimSpace(agent.State) == "" {
		agent.State = "idle"
	}
	if strings.TrimSpace(agent.StatusMessage) == "" {
		agent.StatusMessage = "ready"
	}
	if a.agentRunActive(project.ID, agent.ID) {
		agent.State = "working"
		agent.StatusMessage = "working"
	}
	messages := a.agentMessagesFor(project.ID, agent.ID)
	var lastSeen *time.Time
	if len(messages) > 0 {
		t := messages[len(messages)-1].CreatedAt
		lastSeen = &t
	}
	agent.SessionID = residentSessionID(project.ID, agent.ID)
	agent.MessageCount = len(messages)
	agent.LastSeenAt = lastSeen
	return agent
}

func normalizeAgentModelConfig(agent Agent) Agent {
	if strings.TrimSpace(agent.Provider) == "" {
		agent.Provider = normalizeResidentProvider(getenv("KAROZ_AGENT_PROVIDER", "auto"))
	} else {
		agent.Provider = normalizeResidentProvider(agent.Provider)
	}
	if strings.TrimSpace(agent.Model) == "" {
		if agent.Provider == "claude" {
			agent.Model = "claude-sonnet-4-6"
		} else {
			agent.Model = getenv("KAROZ_CODEX_MODEL", "gpt-5.6-luna")
		}
	}
	if strings.TrimSpace(agent.ThinkingEffort) == "" && agent.Model != "claude-haiku-4-5" {
		agent.ThinkingEffort = "medium"
	}
	if agent.ModelConfigVersion < 1 {
		agent.ModelConfigVersion = 1
	}
	return agent
}

func (a *app) createProjectAgent(project Project, req AgentCreateRequest) (Agent, error) {
	template, ok := residentAgentTemplateByID(req.TemplateID)
	if !ok {
		return Agent{}, errors.New("template_id is invalid")
	}
	nickname := strings.TrimSpace(req.Nickname)
	if nickname == "" {
		nickname = template.DisplayName
	}
	id := slugify(nickname)
	if id == "" {
		id = slugify(template.Name)
	}
	existing := a.projectAgents(project)
	seen := map[string]bool{}
	for _, agent := range existing {
		seen[agent.ID] = true
		if strings.EqualFold(firstNonEmpty(agent.Nickname, agent.DisplayName, agent.Name), nickname) {
			return Agent{}, fmt.Errorf("agent nickname %q already exists", nickname)
		}
	}
	baseID := id
	for i := 2; seen[id]; i++ {
		id = fmt.Sprintf("%s-%d", baseID, i)
	}
	agent := newAgentFromTemplate(project, template, id, nickname)
	agent.GroupID = strings.TrimSpace(req.GroupID)
	agent.GroupName = strings.TrimSpace(req.GroupName)
	agent.GroupRole = strings.TrimSpace(req.GroupRole)
	agent.GroupOrder = req.GroupOrder
	a.ensureAgentService().Insert(agent)
	if err := a.saveAgents(); err != nil {
		return Agent{}, err
	}
	return a.agentWithRuntimeState(project, agent), nil
}

func (a *app) createAgentTeam(project Project, req AgentTeamCreateRequest) (AgentTeamCreateResponse, error) {
	team, ok := residentAgentTeamByID(req.TemplateID)
	if !ok {
		return AgentTeamCreateResponse{}, errors.New("template_id is invalid")
	}
	groupID := slugify(firstNonEmpty(req.Instance, team.ID))
	if groupID == "" {
		groupID = team.ID
	}
	if !strings.HasPrefix(groupID, team.ID) {
		groupID = team.ID + "-" + groupID
	}
	existing := a.projectAgents(project)
	byGroupRole := map[string]Agent{}
	byID := map[string]Agent{}
	for _, agent := range existing {
		byID[agent.ID] = agent
		if agent.GroupID == groupID && agent.GroupRole != "" {
			byGroupRole[agent.GroupRole] = agent
		}
	}
	created := 0
	reused := 0
	agents := make([]Agent, 0, len(team.Agents))
	memberIDToAgentID := map[string]string{}
	for _, member := range team.Agents {
		if existingAgent, ok := byGroupRole[member.ID]; ok {
			reused++
			agents = append(agents, existingAgent)
			memberIDToAgentID[member.ID] = existingAgent.ID
			continue
		}
		nickname := teamAgentNickname(member, groupID)
		agent, err := a.createProjectAgent(project, AgentCreateRequest{
			TemplateID: member.TemplateID,
			Nickname:   nickname,
			GroupID:    groupID,
			GroupName:  team.Name,
			GroupRole:  member.ID,
			GroupOrder: member.StartupOrder,
		})
		if err != nil {
			if strings.Contains(err.Error(), "already exists") {
				if fallback, ok := byID[slugify(nickname)]; ok {
					reused++
					agents = append(agents, fallback)
					memberIDToAgentID[member.ID] = fallback.ID
					continue
				}
			}
			return AgentTeamCreateResponse{}, err
		}
		created++
		agents = append(agents, agent)
		memberIDToAgentID[member.ID] = agent.ID
	}
	coordinatorAgentID := memberIDToAgentID[team.CoordinatorMemberID]
	if coordinatorAgentID == "" && len(agents) > 0 {
		coordinatorAgentID = agents[0].ID
	}
	if _, err := a.upsertAgentGroup(project.ID, groupID, team.Name, team.ID, coordinatorAgentID, agents); err != nil {
		return AgentTeamCreateResponse{}, err
	}
	routes := a.routesForProject(project.ID)
	routeSeen := map[string]bool{}
	routeDirectionSeen := map[string]bool{}
	for _, route := range routes {
		routeSeen[route.FromAgentID+"->"+route.ToAgentID+"#"+route.Intent] = true
		routeDirectionSeen[route.FromAgentID+"->"+route.ToAgentID] = true
	}
	now := time.Now().UTC()
	appendRoute := func(from, to, intent string) {
		if from == "" || to == "" || from == to {
			return
		}
		directionKey := from + "->" + to
		if routeDirectionSeen[directionKey] {
			return
		}
		if intent == "" {
			intent = "request"
		}
		key := directionKey + "#" + intent
		if routeSeen[key] {
			return
		}
		routes = append(routes, AgentRoute{
			ID: randomID(), ProjectID: project.ID, FromAgentID: from, ToAgentID: to,
			Intent: intent, Enabled: true, CreatedAt: now, UpdatedAt: now,
		})
		routeSeen[key] = true
		routeDirectionSeen[directionKey] = true
	}
	appendRoute("karoz", coordinatorAgentID, "request")
	// Edge kinds provide the preferred semantic intent for the explicit workflow.
	for _, edge := range team.Edges {
		from := memberIDToAgentID[edge.From]
		to := memberIDToAgentID[edge.To]
		appendRoute(from, to, teamEdgeIntent(edge.Kind))
	}
	// Like ufoo, report_to and accept_from are also real collaboration topology,
	// not documentation-only metadata. Union them with explicit workflow edges.
	for _, member := range team.Agents {
		from := memberIDToAgentID[member.ID]
		for _, targetMemberID := range member.ReportTo {
			appendRoute(from, memberIDToAgentID[targetMemberID], "request")
		}
		to := memberIDToAgentID[member.ID]
		for _, sourceMemberID := range member.AcceptFrom {
			appendRoute(memberIDToAgentID[sourceMemberID], to, "request")
		}
	}
	routes, err := a.updateAgentRoutes(project, routes)
	if err != nil {
		return AgentTeamCreateResponse{}, err
	}
	return AgentTeamCreateResponse{GroupID: groupID, Team: team, Agents: agents, Routes: routes, Created: created, Reused: reused}, nil
}

func teamAgentNickname(member AgentTeamMember, groupID string) string {
	prefix := strings.TrimSpace(groupID)
	if prefix == "" {
		return firstNonEmpty(member.Nickname, member.ID)
	}
	return prefix + "-" + firstNonEmpty(member.Nickname, member.ID)
}

func teamEdgeIntent(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "review":
		return "handoff"
	case "report":
		return "status"
	case "status":
		return "status"
	case "note":
		return "note"
	default:
		return "request"
	}
}

func (a *app) updateProjectAgent(project Project, agentID string, req AgentUpdateRequest) (Agent, error) {
	var updated Agent
	var validationErr error
	_, ok := a.ensureAgentService().Mutate(project.ID, agentID, func(current *Agent) bool {
		candidate := normalizeAgentModelConfig(*current)
		if nickname := strings.TrimSpace(req.Nickname); nickname != "" {
			candidate.Nickname = nickname
			candidate.DisplayName = nickname
		}
		if req.SystemPrompt != nil {
			candidate.SystemPrompt = strings.TrimSpace(*req.SystemPrompt)
		}
		if req.ChatMode != nil {
			mode := strings.ToLower(strings.TrimSpace(*req.ChatMode))
			if mode != "ask" && mode != "plan" && mode != "dev" {
				validationErr = errors.New("chat_mode must be ask, plan, or dev")
				return false
			}
			candidate.ChatMode = mode
		}
		modelConfigChanged := req.Provider != nil || req.Model != nil || req.ThinkingEffort != nil
		if modelConfigChanged {
			key := projectAgentKey(project.ID, agentID)
			if run, ok := a.agentRuntimeLocked().runs[key]; ok && run.State.Active() {
				validationErr = &agentBusyModelConfigError{}
				return false
			}
			if req.ExpectedModelConfigVersion != nil && *req.ExpectedModelConfigVersion != candidate.ModelConfigVersion {
				validationErr = errors.New("model configuration changed; reload before updating")
				return false
			}
			provider, model, effort := candidate.Provider, candidate.Model, candidate.ThinkingEffort
			if req.Provider != nil {
				provider = normalizeResidentProvider(*req.Provider)
			}
			if req.Model != nil {
				model = strings.TrimSpace(*req.Model)
			}
			if req.ThinkingEffort != nil {
				effort = strings.ToLower(strings.TrimSpace(*req.ThinkingEffort))
			}
			if err := validateResidentModelConfig(provider, model, effort); err != nil {
				validationErr = err
				return false
			}
			candidate.Provider, candidate.Model, candidate.ThinkingEffort = provider, model, effort
			candidate.ModelConfigVersion++
		}
		candidate.UpdatedAt = time.Now().UTC()
		*current = candidate
		updated = candidate
		return true
	})
	if validationErr != nil {
		return Agent{}, validationErr
	}
	if !ok {
		return Agent{}, fmt.Errorf("agent %s not found", agentID)
	}
	if err := a.saveAgents(); err != nil {
		return Agent{}, err
	}
	return a.agentWithRuntimeState(project, updated), nil
}

type agentBusyModelConfigError struct{}

func (*agentBusyModelConfigError) Error() string {
	return "stop the active agent run before changing provider or model"
}

func (a *app) deleteProjectAgent(project Project, agentID string) error {
	a.agentRuntimeLocked().backgroundOwnerMu.Lock()
	defer a.agentRuntimeLocked().backgroundOwnerMu.Unlock()

	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return fmt.Errorf("agent id is required")
	}
	if agentID == "karoz" {
		return fmt.Errorf("default agent cannot be deleted")
	}
	if group, ok := a.groupForAgent(project.ID, agentID); ok && group.CoordinatorAgentID == agentID {
		return fmt.Errorf("group coordinator cannot be deleted before coordination is transferred")
	}
	key := projectAgentKey(project.ID, agentID)
	a.mu.Lock()
	agents := a.agentDirectoryLocked().agents[project.ID]
	found := false
	for _, agent := range agents {
		if agent.ID == agentID {
			found = true
			break
		}
	}
	if !found {
		a.mu.Unlock()
		return fmt.Errorf("agent %s not found", agentID)
	}
	runtime := a.agentRuntimeLocked()
	if runtime.backgroundOwnerDeleting == nil {
		runtime.backgroundOwnerDeleting = map[string]bool{}
	}
	if runtime.backgroundOwnerDeleting[key] {
		a.mu.Unlock()
		return fmt.Errorf("agent %s deletion is already in progress", agentID)
	}
	runtime.backgroundOwnerDeleting[key] = true
	a.revokeResidentBashApprovalsForOwnerLocked(project.ID, agentID)
	cancel := a.agentRuntimeLocked().cancels[key]
	queue := a.agentRuntimeLocked().schedulerQueue
	a.mu.Unlock()

	ownerRemoved := false
	defer func() {
		if ownerRemoved {
			return
		}
		a.mu.Lock()
		delete(a.agentRuntimeLocked().backgroundOwnerDeleting, key)
		a.mu.Unlock()
	}()
	if cancel != nil {
		cancel()
	}
	if queue != nil {
		queue.CancelAgent(project.ID, agentID, "agent deleted", time.Now().UTC())
	}
	if err := a.saveScheduledRuns(); err != nil {
		return err
	}
	if err := a.stopOwnedProcesses(project.ID, agentID); err != nil {
		return fmt.Errorf("stop background processes owned by %s: %w", agentID, err)
	}

	a.mu.Lock()
	agents = a.agentDirectoryLocked().agents[project.ID]
	next := make([]Agent, 0, len(agents))
	found = false
	for _, agent := range agents {
		if agent.ID == agentID {
			found = true
			continue
		}
		next = append(next, agent)
	}
	if !found {
		a.mu.Unlock()
		return fmt.Errorf("agent %s not found", agentID)
	}
	a.agentDirectoryLocked().agents[project.ID] = next
	// A deleted owner must not leave a latent monitor able to wake a recreated
	// same-ID agent.  Pending work is discarded under the same deletion fence.
	probeMonitorIDs := make([]string, 0)
	for i := range a.monitors[project.ID] {
		item := &a.monitors[project.ID][i]
		if item.AgentID == agentID || (item.Action.Kind == "notify_agent" && item.Action.AgentID == agentID) {
			if item.Trigger.Kind == "script_probe" {
				probeMonitorIDs = append(probeMonitorIDs, item.ID)
			}
			item.State = "disabled"
			item.ErrorCode = "owner_deleted"
			item.LastError = "monitor owner or target agent was deleted"
			item.PendingFires = nil
			item.UpdatedAt = time.Now().UTC()
		}
	}
	now := time.Now().UTC()
	for id, reservation := range a.monitorProbeReservations {
		if reservation.ProjectID == project.ID &&
			reservation.AgentID == agentID {
			delete(a.monitorProbeReservations, id)
		}
	}
	for id, challenge := range a.monitorProbeChallenges {
		if challenge.ProjectID == project.ID &&
			challenge.AgentID == agentID &&
			challenge.ConsumedReceiptID == "" {
			delete(a.monitorProbeChallenges, id)
		}
	}
	for id, receipt := range a.monitorProbeReceipts {
		if receipt.ProjectID == project.ID &&
			receipt.AgentID == agentID &&
			receipt.RevokedAt == nil {
			receipt.RevokedAt = timePointer(now)
			a.monitorProbeReceipts[id] = receipt
		}
	}
	var routes []AgentRoute
	for _, route := range a.collaborationServiceLocked().RoutesFor(project.ID) {
		if route.FromAgentID == agentID || route.ToAgentID == agentID {
			continue
		}
		routes = append(routes, route)
	}
	a.collaborationServiceLocked().ReplaceRoutes(project.ID, routes)
	delete(runtime.runs, key)
	delete(runtime.cancels, key)
	a.mu.Unlock()
	for _, monitorID := range probeMonitorIDs {
		a.cancelMonitorProbe(project.ID, monitorID)
	}
	ownerRemoved = true
	if err := a.saveMonitors(); err != nil {
		return err
	}
	if err := a.saveAgents(); err != nil {
		return err
	}
	if err := a.saveAgentRoutes(); err != nil {
		return err
	}
	if err := a.reconcileAgentGroups(); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.agentRuntimeLocked().backgroundOwnerDeleting, key)
	a.mu.Unlock()
	return nil
}

func (a *app) routesForProject(projectID string) []AgentRoute {
	out := a.collaborationServiceLocked().RoutesFor(projectID)
	if out == nil {
		return []AgentRoute{}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FromAgentID == out[j].FromAgentID {
			return out[i].ToAgentID < out[j].ToAgentID
		}
		return out[i].FromAgentID < out[j].FromAgentID
	})
	return out
}

func (a *app) updateAgentRoutes(project Project, routes []AgentRoute) ([]AgentRoute, error) {
	known := map[string]bool{}
	for _, agent := range a.projectAgents(project) {
		known[agent.ID] = true
	}
	now := time.Now().UTC()
	normalized := make([]AgentRoute, 0, len(routes))
	seen := map[string]bool{}
	for _, route := range routes {
		from := strings.TrimSpace(route.FromAgentID)
		to := strings.TrimSpace(route.ToAgentID)
		intent := strings.TrimSpace(route.Intent)
		if intent == "" {
			intent = "request"
		}
		if from == "" || to == "" || from == to || !known[from] || !known[to] {
			return nil, fmt.Errorf("invalid route %s -> %s", from, to)
		}
		if !validAgentIntent(intent) {
			return nil, fmt.Errorf("invalid route intent %s", intent)
		}
		key := from + "/" + to + "/" + intent
		if seen[key] {
			continue
		}
		seen[key] = true
		id := route.ID
		if strings.TrimSpace(id) == "" {
			id = randomID()
		}
		created := route.CreatedAt
		if created.IsZero() {
			created = now
		}
		normalized = append(normalized, AgentRoute{
			ID:          id,
			ProjectID:   project.ID,
			FromAgentID: from,
			ToAgentID:   to,
			Intent:      intent,
			Enabled:     route.Enabled,
			CreatedAt:   created,
			UpdatedAt:   now,
		})
	}
	a.collaborationServiceLocked().ReplaceRoutes(project.ID, normalized)
	if err := a.saveAgentRoutes(); err != nil {
		return nil, err
	}
	return a.routesForProject(project.ID), nil
}

func newAgentFromTemplate(project Project, template AgentTemplate, id, nickname string) Agent {
	now := time.Now().UTC()
	return Agent{
		ID:            id,
		ProjectID:     project.ID,
		TemplateID:    template.ID,
		Name:          template.Name,
		Nickname:      nickname,
		DisplayName:   firstNonEmpty(nickname, template.DisplayName, template.Name),
		ShortName:     template.ShortName,
		Role:          template.Role,
		SystemPrompt:  template.SystemPrompt,
		Emoji:         template.Emoji,
		Summary:       template.Summary,
		Type:          "general",
		Runtime:       "resident",
		SessionID:     residentSessionID(project.ID, id),
		State:         "idle",
		StatusMessage: "ready",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}
