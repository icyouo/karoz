package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func initializeResidentToolsForTest(t *testing.T, a *app) {
	t.Helper()
	if err := a.initializeResidentToolRegistry(); err != nil {
		t.Fatalf("initialize resident tools: %v", err)
	}
}

func TestResidentToolPolicyRegistryIsExactFailClosedBijection(t *testing.T) {
	specs := residentStaticToolSpecs()
	handlerNames := map[string]bool{}
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	for _, definition := range a.residentToolRegistry().Definitions() {
		handlerNames[definition.Name] = true
	}
	if err := validateResidentToolPolicyBijection(specs, handlerNames, staticResidentToolPolicyRegistry); err != nil {
		t.Fatal(err)
	}
	if len(specs) != len(handlerNames) || len(specs) != len(staticResidentToolPolicyRegistry.byName) {
		t.Fatalf("definitions=%d handlers=%d policies=%d", len(specs), len(handlerNames), len(staticResidentToolPolicyRegistry.byName))
	}
	const establishedNames = "ack_inbox,add_agent,add_pending,advance_plan,bash,create_agent_team,create_monitor,create_task,decline_handoff,delete_agent,delete_monitor,drop_pending,get_artifact,get_messages,get_plan,get_task,list_agent_templates,list_artifacts,list_groups,list_monitors,list_pending,list_plans,list_processes,list_skills,list_tasks,mark_activity,mark_done,pause_monitor,prepare_monitor_probe,read_process_log,read_skill,reconcile_plan_history,record_decision,remember_fact,reply_to,repo_list,repo_read,repo_search,report_activity,request_choice,resume_monitor,review_artifact,run_background,save_plan_draft,search_archive,send_to,send_to_group,show_preview,stop_process,submit_artifact,submit_plan,update_monitor,update_task_status,web_fetch,web_search,write_workspace_file"
	if got := strings.Join(staticResidentToolPolicyRegistry.names(), ","); got != establishedNames {
		t.Fatalf("static resident tool names drifted:\n got %s\nwant %s", got, establishedNames)
	}
	for _, policy := range staticResidentToolPolicyRegistry.byName {
		if policy.DomainOwner == "" || policy.Authorize == nil || len(policy.AllowedTurns) == 0 {
			t.Fatalf("incomplete policy: %+v", policy)
		}
	}

	_, err := newResidentToolPolicyRegistry([]residentToolPolicy{residentPolicy("x", "", residentAllTurns, true, residentAllowAll, nil)})
	if err == nil || !strings.Contains(err.Error(), "domain owner") {
		t.Fatalf("empty owner error = %v", err)
	}
	_, err = newResidentToolPolicyRegistry([]residentToolPolicy{
		residentPolicy("x", "one", residentAllTurns, true, residentAllowAll, nil),
		residentPolicy("x", "two", residentAllTurns, true, residentAllowAll, nil),
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate error = %v", err)
	}

	a, project := newHandlerTestApp(t)
	ctx := ResidentToolContext{Project: project, Agent: Agent{ID: "worker-a"}, TurnType: "ask", EnforcePolicy: true}
	if a.residentToolAuthorized(ctx, "unknown_static_tool") || a.residentToolAdvertised(ctx, "unknown_static_tool") {
		t.Fatal("unknown static tool was allowed")
	}
	if !residentToolHasSideEffects("unknown_static_tool") {
		t.Fatal("unknown static tool must be effectful fail-safe")
	}
	result, err := a.executeResidentTool(context.Background(), ctx, codexToolCall{Name: "unknown_static_tool", Arguments: `{}`})
	if err != nil || !strings.Contains(result, `"tool_forbidden"`) {
		t.Fatalf("unknown execution = %s err=%v", result, err)
	}
	if !residentToolHasSideEffects("mcp__server__tool") {
		t.Fatal("dynamic MCP tools must remain effectful")
	}
}

func TestNewAppCompletesStaticResidentToolValidation(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	if a.residentTools == nil {
		t.Fatal("newApp returned before the static resident tool registry was initialized")
	}
	definitions := a.residentTools.Definitions()
	if got, want := len(definitions), 56; got != want {
		t.Fatalf("initialized resident definitions = %d, want %d", got, want)
	}
	for _, definition := range definitions {
		if _, ok := staticResidentToolPolicyRegistry.policy(definition.Name); !ok {
			t.Fatalf("initialized definition %q has no policy", definition.Name)
		}
	}
}

func TestStaticResidentToolInitializationFailsBeforeAdvertisement(t *testing.T) {
	base := &app{}
	specs := residentStaticToolSpecs()
	toolCtx := ResidentToolContext{Project: Project{ID: "p1"}, Agent: Agent{ID: "karoz"}, TurnType: "ask"}

	t.Run("missing handler", func(t *testing.T) {
		a := &app{}
		handlers := base.residentStaticToolHandlers()
		delete(handlers, "repo_read")
		registry, err := initializeResidentToolRegistry(specs, handlers, staticResidentToolPolicyRegistry)
		if err == nil || registry != nil || !strings.Contains(err.Error(), "handler missing") {
			t.Fatalf("registry=%v err=%v", registry, err)
		}
		if advertised := a.residentToolSpecsForContext(context.Background(), toolCtx); len(advertised) != 0 {
			t.Fatalf("unvalidated app advertised tools: %v", toolSpecNames(advertised))
		}
		if provider := a.residentToolContractForProvider(context.Background(), toolCtx, "codex-direct"); len(provider) != 0 {
			t.Fatalf("unvalidated provider contract exposed tools: %v", toolSpecNames(provider))
		}
		if a.residentToolAuthorized(toolCtx, "repo_read") {
			t.Fatal("unvalidated app authorized a static tool")
		}
	})

	t.Run("missing policy", func(t *testing.T) {
		a := &app{}
		policies := residentToolPolicyRegistry{byName: map[string]residentToolPolicy{}}
		for name, policy := range staticResidentToolPolicyRegistry.byName {
			if name != "repo_read" {
				policies.byName[name] = policy
			}
		}
		registry, err := initializeResidentToolRegistry(specs, base.residentStaticToolHandlers(), policies)
		if err == nil || registry != nil || !strings.Contains(err.Error(), "policy missing") {
			t.Fatalf("registry=%v err=%v", registry, err)
		}
		if advertised := a.residentToolSpecsForContext(context.Background(), toolCtx); len(advertised) != 0 {
			t.Fatalf("unvalidated app advertised tools: %v", toolSpecNames(advertised))
		}
	})
}

func TestResidentToolPolicySideEffectsMatchEstablishedContract(t *testing.T) {
	readOnly := map[string]bool{}
	for _, name := range []string{
		"repo_list", "repo_read", "repo_search", "list_skills", "read_skill", "list_monitors",
		"web_search", "web_fetch", "search_archive", "list_pending", "get_messages",
		"list_artifacts", "get_artifact", "list_agent_templates", "list_groups", "list_plans",
		"get_plan", "list_tasks", "get_task", "list_processes", "read_process_log",
	} {
		readOnly[name] = true
	}
	for _, name := range staticResidentToolPolicyRegistry.names() {
		if got, want := residentToolHasSideEffects(name), !readOnly[name]; got != want {
			t.Errorf("%s side effects = %v, want %v", name, got, want)
		}
	}
}

func TestResidentToolPolicyCapabilitiesAdvertiseAndAuthorize(t *testing.T) {
	a, project := newHandlerTestApp(t)
	ordinary := Agent{ID: "ordinary", ProjectID: project.ID, Name: "ordinary"}
	reviewer := Agent{ID: "reviewer", ProjectID: project.ID, Name: "quality-reviewer", Role: "review"}
	coordinator := Agent{ID: "coordinator", ProjectID: project.ID, Name: "coordinator"}
	karoz := Agent{ID: "karoz", ProjectID: project.ID, Name: "Karoz"}
	replaceGroupsForTest(a, project.ID, []AgentGroup{{
		ID: "group-1", ProjectID: project.ID, CoordinatorAgentID: coordinator.ID,
		MemberAgentIDs: []string{coordinator.ID},
	}})
	replaceBlackboardForTest(a, map[string][]AgentBlackboardEntry{project.ID: {{
		ID: "action-1", ProjectID: project.ID, ActivityKind: "blocker", Summary: "blocked", Status: "active",
	}}})
	a.artifactCatalogLocked().artifacts[project.ID] = []Artifact{{
		ID: "artifact-1", ProjectID: project.ID, AgentID: ordinary.ID, Status: ArtifactReviewing,
	}}

	type expectation struct {
		agent                   Agent
		mark, review, sendGroup bool
	}
	for _, tc := range []expectation{
		{agent: ordinary},
		{agent: karoz, mark: true, sendGroup: true},
		{agent: reviewer, review: true},
		{agent: coordinator, sendGroup: true},
	} {
		t.Run(tc.agent.ID, func(t *testing.T) {
			toolCtx := ResidentToolContext{Project: project, Agent: tc.agent, TurnType: "ask", EnforcePolicy: true}
			specs := toolSpecNames(a.residentToolSpecsForContext(context.Background(), toolCtx))
			for name, want := range map[string]bool{
				"mark_activity": tc.mark, "review_artifact": tc.review, "send_to_group": tc.sendGroup,
			} {
				if specs[name] != want {
					t.Errorf("%s advertised = %v, want %v; specs=%v", name, specs[name], want, specs)
				}
				result, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{Name: name, Arguments: `{}`})
				if err != nil {
					t.Fatalf("%s execute error: %v", name, err)
				}
				forbidden := strings.Contains(result, `"tool_forbidden"`)
				if forbidden == want {
					t.Errorf("%s forbidden=%v want allowed=%v result=%s", name, forbidden, want, result)
				}
			}
		})
	}
}

func TestResidentToolPolicyInboxAdvertisementMatrix(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent := Agent{ID: "worker-a", ProjectID: project.ID}
	names := func() map[string]bool {
		return toolSpecNames(a.residentToolSpecsForContext(context.Background(), ResidentToolContext{
			Project: project, Agent: agent, TurnType: "ask",
		}))
	}
	assertTerminal := func(label string, reply, decline, ack bool) {
		t.Helper()
		got := names()
		if got["reply_to"] != reply || got["decline_handoff"] != decline || got["ack_inbox"] != ack {
			t.Fatalf("%s: reply=%v decline=%v ack=%v specs=%v", label, got["reply_to"], got["decline_handoff"], got["ack_inbox"], got)
		}
	}

	assertTerminal("empty", false, false, false)
	replaceInboxForTest(a, map[string][]AgentInboxMessage{projectAgentKey(project.ID, agent.ID): {{
		ID: "peer-request", ProjectID: project.ID, SourceAgentID: "worker-b", TargetAgentID: agent.ID,
		MessageType: "handoff", Intent: "request", Status: HandoffDelivered,
	}}})
	assertTerminal("peer request", true, true, true)
	replaceInboxForTest(a, map[string][]AgentInboxMessage{projectAgentKey(project.ID, agent.ID): {{
		ID: "karoz-request", ProjectID: project.ID, SourceAgentID: "karoz", TargetAgentID: agent.ID,
		MessageType: "handoff", Intent: "request", Status: HandoffDelivered,
	}}})
	assertTerminal("karoz request", false, false, false)
	if !names()["report_activity"] {
		t.Fatal("Karoz-originated handoff must retain report_activity")
	}
	replaceInboxForTest(a, map[string][]AgentInboxMessage{projectAgentKey(project.ID, agent.ID): {{
		ID: "peer-result", ProjectID: project.ID, SourceAgentID: "worker-b", TargetAgentID: agent.ID,
		MessageType: "result", Intent: "reply", Status: HandoffDelivered,
	}}})
	assertTerminal("terminal peer delivery", false, false, true)
}

func TestInboxAdvertisementUsesOnlyExactProjectAgentKey(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent := Agent{ID: "worker-a", ProjectID: project.ID}
	for i := 0; i < 500; i++ {
		key := projectAgentKey("foreign-project", "foreign-agent-"+string(rune(i)))
		appendInboxForTest(a, key, AgentInboxMessage{
			ID: "foreign", ProjectID: "foreign-project", SourceAgentID: "worker-b",
			TargetAgentID: agent.ID, MessageType: "handoff", Intent: "request", Status: HandoffDelivered,
		})
	}
	visited := 0
	if a.hasVisibleInboxMessage(ResidentToolContext{Project: project, Agent: agent}, func(AgentInboxMessage) bool {
		visited++
		return true
	}) {
		t.Fatal("foreign inbox volume affected the target predicate")
	}
	if visited != 0 {
		t.Fatalf("predicate scanned %d foreign messages", visited)
	}
	appendInboxForTest(a, projectAgentKey(project.ID, agent.ID), AgentInboxMessage{
		ID: "local", ProjectID: project.ID, SourceAgentID: "worker-b", TargetAgentID: agent.ID,
		MessageType: "handoff", Intent: "request", Status: HandoffDelivered,
	})
	if !a.hasVisibleInboxMessage(ResidentToolContext{Project: project, Agent: agent}, func(AgentInboxMessage) bool {
		visited++
		return true
	}) {
		t.Fatal("exact keyed inbox message was not found")
	}
	if visited != 1 {
		t.Fatalf("visited messages = %d, want exactly one keyed message", visited)
	}
}

func TestResidentToolStateHintsPreserveSameRunWorkflows(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent := Agent{ID: "ordinary", ProjectID: project.ID}

	ask := toolSpecNames(a.residentToolSpecsForContext(context.Background(), ResidentToolContext{Project: project, Agent: agent, TurnType: "ask"}))
	for _, name := range []string{"add_pending", "drop_pending", "remember_fact", "record_decision"} {
		if !ask[name] {
			t.Errorf("ask same-run workflow tool %s was hidden", name)
		}
	}
	dev := toolSpecNames(a.residentToolSpecsForContext(context.Background(), ResidentToolContext{Project: project, Agent: agent, TurnType: "dev"}))
	for _, name := range []string{"create_monitor", "update_monitor", "pause_monitor", "resume_monitor", "delete_monitor"} {
		if !dev[name] {
			t.Errorf("dev same-run workflow tool %s was hidden", name)
		}
	}
	plan := toolSpecNames(a.residentToolSpecsForContext(context.Background(), ResidentToolContext{Project: project, Agent: agent, TurnType: "plan"}))
	for _, name := range []string{"save_plan_draft", "submit_plan", "reconcile_plan_history"} {
		if !plan[name] {
			t.Errorf("plan same-run workflow tool %s was hidden", name)
		}
	}
	if plan["advance_plan"] {
		t.Fatal("advance_plan should be hidden without an owned active plan")
	}
}

func TestResidentToolStateAdvertisementPrerequisites(t *testing.T) {
	a, project := newHandlerTestApp(t)
	karoz := Agent{ID: "karoz", ProjectID: project.ID, Name: "Karoz"}
	reviewer := Agent{ID: "reviewer", ProjectID: project.ID, Name: "quality-reviewer"}
	planner := Agent{ID: "planner", ProjectID: project.ID}
	has := func(agent Agent, turn, name string) bool {
		return toolSpecNames(a.residentToolSpecsForContext(context.Background(), ResidentToolContext{
			Project: project, Agent: agent, TurnType: turn,
		}))[name]
	}

	if has(karoz, "ask", "mark_activity") {
		t.Fatal("mark_activity advertised without actionable blackboard state")
	}
	replaceBlackboardForTest(a, map[string][]AgentBlackboardEntry{project.ID: {{
		ID: "derived", ProjectID: project.ID, ActivityKind: "blocker", Summary: "derived", Derived: true,
	}}})
	if has(karoz, "ask", "mark_activity") {
		t.Fatal("mark_activity advertised for derived blackboard state")
	}
	appendBlackboardForTest(a, project.ID, AgentBlackboardEntry{
		ID: "actionable", ProjectID: project.ID, ActivityKind: "decision_needed", Summary: "choose", Status: "active",
	})
	if !has(karoz, "ask", "mark_activity") {
		t.Fatal("mark_activity hidden with actionable non-derived state")
	}

	if has(reviewer, "ask", "review_artifact") {
		t.Fatal("review_artifact advertised without a submitted artifact")
	}
	a.artifactCatalogLocked().artifacts[project.ID] = []Artifact{{
		ID: "self", ProjectID: project.ID, AgentID: reviewer.ID, Status: ArtifactReviewing,
	}}
	if has(reviewer, "ask", "review_artifact") {
		t.Fatal("review_artifact advertised for self-authored artifact only")
	}
	a.artifactCatalogLocked().artifacts[project.ID] = append(a.artifactCatalogLocked().artifacts[project.ID], Artifact{
		ID: "peer", ProjectID: project.ID, AgentID: "author", Status: ArtifactReviewing,
	})
	if !has(reviewer, "ask", "review_artifact") {
		t.Fatal("review_artifact hidden with peer submitted artifact")
	}

	replacePlansForTest(a, project.ID, []WorkPlan{{
		ID: "other", ProjectID: project.ID, OwnerAgentID: "someone-else", Status: PlanActive,
	}})
	if has(planner, "plan", "advance_plan") {
		t.Fatal("advance_plan advertised for another actor's plan")
	}
	plans := a.collaboration.PlansFor(project.ID)
	replacePlansForTest(a, project.ID, append(plans, WorkPlan{
		ID: "own", ProjectID: project.ID, OwnerAgentID: planner.ID, Status: PlanActive,
	}))
	if !has(planner, "plan", "advance_plan") {
		t.Fatal("advance_plan hidden for actor-owned active plan")
	}
}

type invariantToolWire struct {
	seen   [][]byte
	rounds int
}

func (w *invariantToolWire) step(_ context.Context, tools []map[string]any, _ AgentStreamCallbacks) (residentStepOutput, []AgentInterrupt, error) {
	encoded, _ := json.Marshal(tools)
	w.seen = append(w.seen, encoded)
	w.rounds++
	if w.rounds == 1 {
		return residentStepOutput{ToolCalls: []codexToolCall{{ID: "call-1", Name: "save_plan_draft", Arguments: `{}`}}}, nil, nil
	}
	return residentStepOutput{Text: "done"}, nil, nil
}

func (*invariantToolWire) appendAssistantTurn(residentStepOutput)                   {}
func (*invariantToolWire) appendInterruptTurn(residentStepOutput, []AgentInterrupt) {}
func (*invariantToolWire) appendToolCall(codexToolCall)                             {}
func (*invariantToolWire) appendToolResult(codexToolCall, string, bool)             {}
func (*invariantToolWire) appendInlineInterrupts([]AgentInterrupt)                  {}
func (*invariantToolWire) flushToolResults()                                        {}
func (*invariantToolWire) appendLimitMessage(string)                                {}
func (*invariantToolWire) finalize(context.Context, context.Context, AgentStreamCallbacks) error {
	return nil
}

func TestResidentToolListIsInvariantAcrossProviderRounds(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent := Agent{ID: "planner", ProjectID: project.ID}
	toolCtx := ResidentToolContext{Project: project, Agent: agent, TurnType: "plan"}
	tools := a.residentToolContractForProvider(context.Background(), toolCtx, "codex-direct")
	if toolSpecNames(tools)["advance_plan"] {
		t.Fatal("advance_plan unexpectedly present before the run")
	}
	wire := &invariantToolWire{}
	budget := ResidentTurnBudget{
		TotalDuration: 5 * time.Second, ToolPhaseDuration: 4 * time.Second, FinalResponseReserve: time.Second,
		MaxModelRounds: 3, MaxToolRounds: 2, MaxToolOutputChars: 1000,
	}
	err := invokeResidentToolLoop(context.Background(), wire, tools, AgentStreamCallbacks{}, budget, func(context.Context, codexToolCall) (string, error) {
		replacePlansForTest(a, project.ID, []WorkPlan{{ID: "new-active", ProjectID: project.ID, OwnerAgentID: agent.ID, Status: PlanActive}})
		return `{}`, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.seen) != 2 || string(wire.seen[0]) != string(wire.seen[1]) {
		t.Fatalf("tool list changed across rounds: %q != %q", wire.seen[0], wire.seen[1])
	}
}

func TestResidentProviderSpecsPreserveEstablishedStaticOrderAndSchema(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent := Agent{ID: "karoz", ProjectID: project.ID, Name: "Karoz"}
	replaceBlackboardForTest(a, map[string][]AgentBlackboardEntry{project.ID: {{
		ID: "actionable", ProjectID: project.ID, ActivityKind: "blocker", Summary: "blocked", Status: "active",
	}}})
	appendInboxForTest(a, projectAgentKey(project.ID, agent.ID), AgentInboxMessage{
		ID: "peer-request", ProjectID: project.ID, SourceAgentID: "worker-a", TargetAgentID: agent.ID,
		MessageType: "handoff", Intent: "request", Status: HandoffDelivered,
	})
	replacePlansForTest(a, project.ID, []WorkPlan{{
		ID: "owned-plan", ProjectID: project.ID, OwnerAgentID: agent.ID, Status: PlanActive,
	}})

	for _, turnType := range []string{"ask", "plan", "dev"} {
		t.Run(turnType, func(t *testing.T) {
			toolCtx := ResidentToolContext{Project: project, Agent: agent, Workdir: project.Path, TurnType: turnType, EnforcePolicy: true}
			var expected []map[string]any
			for _, spec := range residentStaticToolSpecs() {
				if a.residentToolAdvertised(toolCtx, toolNameFromSpec(spec)) {
					expected = append(expected, spec)
				}
			}
			expectedJSON, err := json.Marshal(expected)
			if err != nil {
				t.Fatal(err)
			}
			actualJSON, err := json.Marshal(a.residentToolSpecsForContext(context.Background(), toolCtx))
			if err != nil {
				t.Fatal(err)
			}
			if string(actualJSON) != string(expectedJSON) {
				t.Fatalf("provider specs changed established order/schema\nactual=%s\nexpected=%s", actualJSON, expectedJSON)
			}
			for _, provider := range []string{"codex-direct", "claude-api"} {
				providerJSON, err := json.Marshal(a.residentToolContractForProvider(context.Background(), toolCtx, provider))
				if err != nil {
					t.Fatal(err)
				}
				if string(providerJSON) != string(expectedJSON) {
					t.Fatalf("%s contract changed established order/schema\nactual=%s\nexpected=%s", provider, providerJSON, expectedJSON)
				}
			}
		})
	}
}
