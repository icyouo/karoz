package main

import (
	"os"
	"path/filepath"
	"testing"
)

func planTestApp(t *testing.T) (*app, Project, Agent, Agent) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: projectID(path), Name: "demo", Path: path, DefaultBranch: "main"}
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	coordinator := Agent{ID: "lead", ProjectID: project.ID, Nickname: "Lead", GroupID: "build", GroupName: "Build", GroupRole: "coordinator", GroupOrder: 1}
	builder := Agent{ID: "builder", ProjectID: project.ID, Nickname: "Builder", GroupID: "build", GroupName: "Build", GroupRole: "builder", GroupOrder: 2}
	a.agents[project.ID] = []Agent{{ID: "karoz", ProjectID: project.ID, Nickname: "Karoz"}, coordinator, builder}
	a.agentRoutes[project.ID] = []AgentRoute{{ID: "karoz-build", ProjectID: project.ID, FromAgentID: "karoz", ToAgentID: coordinator.ID, Intent: "request", Enabled: true}}
	if _, err := a.upsertAgentGroup(project.ID, "build", "Build", "", coordinator.ID, []Agent{coordinator, builder}); err != nil {
		t.Fatal(err)
	}
	return a, project, coordinator, builder
}

func TestWorkPlanOwnershipAndTaskCompletionRequiresDecision(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	t.Setenv("KAROZ_TASK_AUTO_RUN", "0")
	a, project, coordinator, builder := planTestApp(t)
	plan, err := a.createPlanDraft(project, builder, builder.ID, WorkPlanDraftRequest{
		Title: "Ship runtime", Goal: "Deliver verified runtime", Steps: []PlanStep{
			{ID: "implement", Title: "Implement", Description: "Implement the runtime", AcceptanceCriteria: []string{"tests pass"}},
			{ID: "release", Title: "Release", Description: "Release it", Dependencies: []string{"implement"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.OwnerType != "group" || plan.OwnerGroupID != "build" || plan.OwnerAgentID != coordinator.ID {
		t.Fatalf("group plan owner = %+v", plan)
	}
	plan, err = a.activatePlan(project, plan.ID, "user", plan.Version)
	if err != nil || plan.Steps[0].Status != PlanStepReady || plan.Steps[1].Status != PlanStepPending {
		t.Fatalf("activated plan = %+v err=%v", plan, err)
	}
	plan, err = a.advancePlan(project, coordinator, plan.ID, PlanActionRequest{Action: "dispatch_task", StepID: "implement", ExpectedVersion: plan.Version, AgentID: builder.ID})
	if err != nil || len(plan.Steps[0].TaskAttempts) != 1 {
		t.Fatalf("dispatch = %+v err=%v", plan, err)
	}
	task, ok := a.findTask(project.ID, plan.Steps[0].TaskAttempts[0].TaskID)
	if !ok || task.PlanID != plan.ID || task.PlanStepID != "implement" {
		t.Fatalf("linked task = %+v", task)
	}
	task.Status = "done"
	task.Result = "tests pass"
	a.updateTask(project.ID, task)
	a.notifyTaskRuntimeHooks(project, task)
	plan, _ = a.planByID(project.ID, plan.ID)
	if plan.Steps[0].Status != PlanStepAwaitingDecision || plan.Steps[1].Status != PlanStepPending {
		t.Fatalf("task completion incorrectly completed todo: %+v", plan.Steps)
	}
	plan, err = a.advancePlan(project, coordinator, plan.ID, PlanActionRequest{Action: "accept_step", StepID: "implement", Result: "verified", ExpectedVersion: plan.Version})
	if err != nil || plan.Steps[0].Status != PlanStepCompleted || plan.Steps[1].Status != PlanStepReady {
		t.Fatalf("accept = %+v err=%v", plan.Steps, err)
	}
}

func TestKarozCannotOwnPlanAndGroupedWorkUsesGroupInbox(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	a, project, _, builder := planTestApp(t)
	karoz, _ := a.projectAgent(project, "karoz")
	if _, err := a.createPlanDraft(project, karoz, karoz.ID, WorkPlanDraftRequest{Title: "Wrong owner", Goal: "execute", Steps: []PlanStep{{Title: "Do", Description: "Do it"}}}); err == nil {
		t.Fatal("Karoz should not own an ordinary WorkPlan")
	}
	direct := a.sendToAgent(project.ID, karoz.ID, "", map[string]any{"target_agent_id": builder.ID, "body": "implement"})
	if !containsToolError(direct, "group_route_required") {
		t.Fatalf("direct grouped route = %s", direct)
	}
	routed := a.sendToGroup(project.ID, karoz.ID, "", map[string]any{"group_id": "build", "body": "implement", "subject": "Runtime"})
	if containsToolError(routed, "") || len(a.groupInbox[project.ID]) != 1 {
		t.Fatalf("group route = %s inbox=%+v", routed, a.groupInbox[project.ID])
	}
}

func containsToolError(result, code string) bool {
	var payload map[string]any
	if jsonUnmarshalToolResult(result, &payload) != nil || payload["error"] == nil {
		return false
	}
	return code == "" || payload["error"] == code
}
