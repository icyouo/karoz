package main

import (
	"fmt"
	"sort"
	"strings"
)

type residentToolAuthorization func(*app, ResidentToolContext) bool
type residentToolAdvertisement func(*app, ResidentToolContext) bool

type residentToolPolicy struct {
	Name           string
	DomainOwner    string
	AllowedTurns   map[string]bool
	Authorize      residentToolAuthorization
	Advertise      residentToolAdvertisement
	HasSideEffects bool
}

type residentToolPolicyRegistry struct {
	byName map[string]residentToolPolicy
}

var staticResidentToolPolicyRegistry = mustResidentToolPolicyRegistry(residentStaticToolPolicies())

func newResidentToolPolicyRegistry(policies []residentToolPolicy) (residentToolPolicyRegistry, error) {
	registry := residentToolPolicyRegistry{byName: make(map[string]residentToolPolicy, len(policies))}
	for _, policy := range policies {
		policy.Name = strings.TrimSpace(policy.Name)
		policy.DomainOwner = strings.TrimSpace(policy.DomainOwner)
		if policy.Name == "" {
			return residentToolPolicyRegistry{}, fmt.Errorf("resident tool policy name is required")
		}
		if policy.DomainOwner == "" {
			return residentToolPolicyRegistry{}, fmt.Errorf("resident tool policy %q has no domain owner", policy.Name)
		}
		if len(policy.AllowedTurns) == 0 {
			return residentToolPolicyRegistry{}, fmt.Errorf("resident tool policy %q has no allowed turns", policy.Name)
		}
		for turn := range policy.AllowedTurns {
			switch normalizeChatTurnType(turn) {
			case "ask", "plan", "dev":
			default:
				return residentToolPolicyRegistry{}, fmt.Errorf("resident tool policy %q has invalid turn %q", policy.Name, turn)
			}
		}
		if policy.Authorize == nil {
			return residentToolPolicyRegistry{}, fmt.Errorf("resident tool policy %q has no authorization rule", policy.Name)
		}
		if _, exists := registry.byName[policy.Name]; exists {
			return residentToolPolicyRegistry{}, fmt.Errorf("duplicate resident tool policy %q", policy.Name)
		}
		registry.byName[policy.Name] = policy
	}
	return registry, nil
}

func mustResidentToolPolicyRegistry(policies []residentToolPolicy) residentToolPolicyRegistry {
	registry, err := newResidentToolPolicyRegistry(policies)
	if err != nil {
		panic(err)
	}
	return registry
}

func residentPolicy(name, owner string, turns []string, sideEffects bool, authorize residentToolAuthorization, advertise residentToolAdvertisement) residentToolPolicy {
	allowedTurns := make(map[string]bool, len(turns))
	for _, turn := range turns {
		allowedTurns[normalizeChatTurnType(turn)] = true
	}
	return residentToolPolicy{
		Name:           name,
		DomainOwner:    owner,
		AllowedTurns:   allowedTurns,
		Authorize:      authorize,
		Advertise:      advertise,
		HasSideEffects: sideEffects,
	}
}

var (
	residentAllTurns                           = []string{"ask", "plan", "dev"}
	residentPlanTurn                           = []string{"plan"}
	residentDevTurn                            = []string{"dev"}
	residentAllowAll residentToolAuthorization = func(*app, ResidentToolContext) bool { return true }
)

func residentStaticToolPolicies() []residentToolPolicy {
	p := residentPolicy
	all := residentAllTurns
	dev := residentDevTurn
	plan := residentPlanTurn
	return []residentToolPolicy{
		p("bash", "runtime", all, true, residentAllowAll, nil),
		p("run_background", "runtime", all, true, residentAllowAll, nil),
		p("list_processes", "runtime", all, false, residentAllowAll, nil),
		p("read_process_log", "runtime", all, false, residentAllowAll, nil),
		p("stop_process", "runtime", all, true, residentAllowAll, nil),

		p("list_monitors", "monitoring", all, false, residentAllowAll, nil),
		p("prepare_monitor_probe", "monitoring", all, true, residentAllowAll, nil),
		p("create_monitor", "monitoring", dev, true, residentAllowAll, nil),
		p("update_monitor", "monitoring", dev, true, residentAllowAll, nil),
		p("pause_monitor", "monitoring", dev, true, residentAllowAll, nil),
		p("resume_monitor", "monitoring", dev, true, residentAllowAll, nil),
		p("delete_monitor", "monitoring", dev, true, residentAllowAll, nil),

		p("repo_list", "repository", all, false, residentAllowAll, nil),
		p("repo_read", "repository", all, false, residentAllowAll, nil),
		p("repo_search", "repository", all, false, residentAllowAll, nil),
		p("list_skills", "skills", all, false, residentAllowAll, nil),
		p("read_skill", "skills", all, false, residentAllowAll, nil),
		p("web_search", "web", all, false, residentAllowAll, nil),
		p("web_fetch", "web", all, false, residentAllowAll, nil),
		p("request_choice", "interaction", all, true, residentAllowAll, nil),

		p("remember_fact", "memory", all, true, residentAllowAll, nil),
		p("record_decision", "memory", all, true, residentAllowAll, nil),
		p("mark_done", "memory", all, true, residentAllowAll, nil),
		p("add_pending", "memory", all, true, residentAllowAll, nil),
		p("drop_pending", "memory", all, true, residentAllowAll, nil),
		p("search_archive", "memory", all, false, residentAllowAll, nil),
		p("list_pending", "memory", all, false, residentAllowAll, nil),
		p("get_messages", "memory", all, false, residentAllowAll, nil),

		p("write_workspace_file", "artifacts", dev, true, residentAllowAll, nil),
		p("show_preview", "artifacts", dev, true, residentAllowAll, nil),
		p("list_artifacts", "artifacts", all, false, residentAllowAll, nil),
		p("get_artifact", "artifacts", all, false, residentAllowAll, nil),
		p("submit_artifact", "artifacts", all, true, residentAllowAll, nil),
		p("review_artifact", "artifacts", all, true, residentCanReviewArtifacts, residentHasReviewableArtifact),

		p("create_task", "tasks", dev, true, residentAllowAll, nil),
		p("update_task_status", "tasks", dev, true, residentAllowAll, nil),

		p("send_to", "collaboration", all, true, residentAllowAll, nil),
		p("reply_to", "collaboration", all, true, residentAllowAll, residentHasPeerRequest),
		p("decline_handoff", "collaboration", all, true, residentAllowAll, residentHasPeerRequest),
		p("ack_inbox", "collaboration", all, true, residentAllowAll, residentHasAckableInbox),
		p("report_activity", "collaboration", all, true, residentAllowAll, nil),
		p("mark_activity", "collaboration", all, true, residentCanReconcileBacklog, residentHasActionableBlackboard),

		p("list_agent_templates", "agent-management", all, false, residentCanManageAgents, nil),
		p("add_agent", "agent-management", all, true, residentCanManageAgents, nil),
		p("create_agent_team", "agent-management", all, true, residentCanManageAgents, nil),
		p("delete_agent", "agent-management", all, true, residentCanManageAgents, nil),

		p("list_groups", "planning", all, false, residentAllowAll, nil),
		p("send_to_group", "planning", all, true, residentCanSendToGroup, nil),
		p("list_plans", "planning", all, false, residentAllowAll, nil),
		p("get_plan", "planning", all, false, residentAllowAll, nil),
		p("list_tasks", "planning", all, false, residentAllowAll, nil),
		p("get_task", "planning", all, false, residentAllowAll, nil),
		p("save_plan_draft", "planning", plan, true, residentAllowAll, nil),
		p("submit_plan", "planning", plan, true, residentAllowAll, nil),
		p("reconcile_plan_history", "planning", plan, true, residentAllowAll, nil),
		p("advance_plan", "planning", plan, true, residentAllowAll, residentOwnsActivePlan),
	}
}

func (registry residentToolPolicyRegistry) policy(name string) (residentToolPolicy, bool) {
	policy, ok := registry.byName[strings.TrimSpace(name)]
	return policy, ok
}

func (registry residentToolPolicyRegistry) names() []string {
	names := make([]string, 0, len(registry.byName))
	for name := range registry.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (a *app) residentToolAuthorized(toolCtx ResidentToolContext, name string) bool {
	if strings.HasPrefix(name, "mcp__") {
		return residentDynamicToolsAllowed(toolCtx)
	}
	if a == nil || a.residentToolRegistry() == nil {
		return false
	}
	policy, ok := staticResidentToolPolicyRegistry.policy(name)
	if !ok || !policy.AllowedTurns[normalizeChatTurnType(toolCtx.TurnType)] {
		return false
	}
	return policy.Authorize(a, toolCtx)
}

func (a *app) residentToolAdvertised(toolCtx ResidentToolContext, name string) bool {
	if !a.residentToolAuthorized(toolCtx, name) {
		return false
	}
	policy, _ := staticResidentToolPolicyRegistry.policy(name)
	return policy.Advertise == nil || policy.Advertise(a, toolCtx)
}

func residentCanManageAgents(_ *app, toolCtx ResidentToolContext) bool {
	return capabilitiesForAgent(toolCtx.Agent).CanManageAgents
}

func residentCanReconcileBacklog(_ *app, toolCtx ResidentToolContext) bool {
	return capabilitiesForAgent(toolCtx.Agent).CanReconcileBacklog
}

func residentCanReviewArtifacts(_ *app, toolCtx ResidentToolContext) bool {
	return capabilitiesForAgent(toolCtx.Agent).CanReviewArtifacts
}

func residentCanSendToGroup(a *app, toolCtx ResidentToolContext) bool {
	return toolCtx.Agent.ID == "karoz" || a.isGroupCoordinator(toolCtx.Project.ID, toolCtx.Agent.ID)
}

func residentHasPeerRequest(a *app, toolCtx ResidentToolContext) bool {
	return a.hasVisibleInboxMessage(toolCtx, func(message AgentInboxMessage) bool {
		return message.SourceAgentID != "karoz" && !handoffMessageIsTerminalDelivery(message)
	})
}

func residentHasAckableInbox(a *app, toolCtx ResidentToolContext) bool {
	return a.hasVisibleInboxMessage(toolCtx, func(message AgentInboxMessage) bool {
		return message.SourceAgentID != "karoz"
	})
}

func (a *app) hasVisibleInboxMessage(toolCtx ResidentToolContext, eligible func(AgentInboxMessage) bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, message := range a.collaborationServiceLocked().InboxFor(projectAgentKey(toolCtx.Project.ID, toolCtx.Agent.ID)) {
		if message.ProjectID == toolCtx.Project.ID &&
			message.TargetAgentID == toolCtx.Agent.ID &&
			handoffStatusOpen(message.Status) &&
			eligible(message) {
			return true
		}
	}
	return false
}

func residentHasActionableBlackboard(a *app, toolCtx ResidentToolContext) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, entry := range a.collaborationServiceLocked().BlackboardFor(toolCtx.Project.ID) {
		if !entry.Derived && blackboardEntryActionable(entry) {
			return true
		}
	}
	return false
}

func residentHasReviewableArtifact(a *app, toolCtx ResidentToolContext) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, artifact := range a.artifactCatalogLocked().artifacts[toolCtx.Project.ID] {
		if artifact.Status == ArtifactReviewing && artifact.AgentID != toolCtx.Agent.ID {
			return true
		}
	}
	return false
}

func residentOwnsActivePlan(a *app, toolCtx ResidentToolContext) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, plan := range a.collaborationServiceLocked().PlansFor(toolCtx.Project.ID) {
		if plan.Status == PlanActive && plan.OwnerAgentID == toolCtx.Agent.ID {
			return true
		}
	}
	return false
}

func validateResidentToolPolicyBijection(specs []map[string]any, handlers map[string]bool, policies residentToolPolicyRegistry) error {
	definitions := make(map[string]bool, len(specs))
	for _, spec := range specs {
		name := toolNameFromSpec(spec)
		if name == "" {
			return fmt.Errorf("resident tool definition has no name")
		}
		if definitions[name] {
			return fmt.Errorf("duplicate resident tool definition %q", name)
		}
		definitions[name] = true
	}
	for name := range definitions {
		if _, ok := handlers[name]; !ok {
			return fmt.Errorf("resident tool handler missing: %s", name)
		}
		if _, ok := policies.policy(name); !ok {
			return fmt.Errorf("resident tool policy missing: %s", name)
		}
	}
	for name := range handlers {
		if !definitions[name] {
			return fmt.Errorf("resident tool definition missing for handler: %s", name)
		}
	}
	for _, name := range policies.names() {
		if !definitions[name] {
			return fmt.Errorf("resident tool definition missing for policy: %s", name)
		}
	}
	return nil
}
