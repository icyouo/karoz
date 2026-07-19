package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestGroupAllowsOnlyOneExecutionPlan(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	a, project, coordinator, builder := planTestApp(t)
	draft := func(author Agent, title string) WorkPlan {
		t.Helper()
		plan, err := a.createPlanDraft(project, author, author.ID, WorkPlanDraftRequest{Title: title, Goal: "Ship it", Steps: []PlanStep{{ID: "ship", Title: "Ship", Description: "Ship the work"}}})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}

	first := draft(builder, "First group plan")
	second := draft(builder, "Second group plan")
	first, err := a.activatePlan(project, first.ID, "user", first.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.activatePlan(project, second.ID, "user", second.Version); err == nil || !strings.Contains(err.Error(), first.ID) {
		t.Fatalf("second group activation should identify the conflict, err=%v", err)
	}
	unchanged, _ := a.planByID(project.ID, second.ID)
	if unchanged.Status != PlanDraft || unchanged.Version != second.Version {
		t.Fatalf("conflicting plan changed: %+v", unchanged)
	}

	first, err = a.advancePlan(project, coordinator, first.ID, PlanActionRequest{Action: "pause", ExpectedVersion: first.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.activatePlan(project, second.ID, "user", second.Version); err == nil {
		t.Fatal("a paused group plan must continue reserving the execution slot")
	}
	first, err = a.advancePlan(project, coordinator, first.ID, PlanActionRequest{Action: "cancel", ExpectedVersion: first.Version})
	if err != nil {
		t.Fatal(err)
	}
	second, err = a.activatePlan(project, second.ID, "user", second.Version)
	if err != nil || second.Status != PlanActive {
		t.Fatalf("terminal plan should release the group slot: plan=%+v err=%v", second, err)
	}

	otherLead := Agent{ID: "other-lead", ProjectID: project.ID, Nickname: "Other Lead", GroupID: "other", GroupName: "Other", GroupRole: "coordinator"}
	otherBuilder := Agent{ID: "other-builder", ProjectID: project.ID, Nickname: "Other Builder", GroupID: "other", GroupName: "Other", GroupRole: "builder"}
	a.agents[project.ID] = append(a.agents[project.ID], otherLead, otherBuilder)
	if _, err := a.upsertAgentGroup(project.ID, "other", "Other", "", otherLead.ID, []Agent{otherLead, otherBuilder}); err != nil {
		t.Fatal(err)
	}
	other := draft(otherBuilder, "Independent group plan")
	if _, err := a.activatePlan(project, other.ID, "user", other.Version); err != nil {
		t.Fatalf("an independent group should have its own execution slot: %v", err)
	}

	standalone := Agent{ID: "solo", ProjectID: project.ID, Nickname: "Solo"}
	a.agents[project.ID] = append(a.agents[project.ID], standalone)
	soloFirst := draft(standalone, "Solo first")
	soloSecond := draft(standalone, "Solo second")
	if _, err := a.activatePlan(project, soloFirst.ID, "user", soloFirst.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := a.activatePlan(project, soloSecond.ID, "user", soloSecond.Version); err != nil {
		t.Fatalf("standalone agents are not subject to the group execution slot: %v", err)
	}
}

func TestGroupExecutionPlanActivationIsAtomic(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	a, project, _, builder := planTestApp(t)
	makePlan := func(title string) WorkPlan {
		plan, err := a.createPlanDraft(project, builder, builder.ID, WorkPlanDraftRequest{Title: title, Goal: "Ship it", Steps: []PlanStep{{ID: "ship", Title: "Ship", Description: "Ship the work"}}})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	first := makePlan("Concurrent first")
	second := makePlan("Concurrent second")
	start := make(chan struct{})
	results := make(chan error, 2)
	activate := func(plan WorkPlan) {
		<-start
		_, err := a.activatePlan(project, plan.ID, "user", plan.Version)
		results <- err
	}
	go activate(first)
	go activate(second)
	close(start)
	firstErr, secondErr := <-results, <-results
	if (firstErr == nil) == (secondErr == nil) {
		t.Fatalf("exactly one concurrent activation must succeed: first=%v second=%v", firstErr, secondErr)
	}
	active := 0
	for _, plan := range a.plansForProject(project.ID) {
		if plan.OwnerGroupID == "build" && plan.Status == PlanActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active group plans = %d, want 1", active)
	}
}

func TestAcceptedPlanStepSchedulesOwnerContinuation(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "1")
	a, project, coordinator, _ := planTestApp(t)
	plan := WorkPlan{
		ID: "plan-follow-up", ProjectID: project.ID, Title: "Continue the plan", Goal: "Finish both steps", Status: PlanActive,
		OwnerType: "group", OwnerGroupID: "build", OwnerAgentID: coordinator.ID, MaxConcurrency: 1, Version: 7,
		Steps: []PlanStep{
			{ID: "step-1", Title: "First", Status: PlanStepAwaitingDecision, Version: 2},
			{ID: "step-2", Title: "Second", Status: PlanStepPending, Dependencies: []string{"step-1"}, Version: 1},
		},
	}
	a.plans[project.ID] = []WorkPlan{plan}
	jobs := make(chan ScheduledRun, 1)
	a.schedulerExecutors[ScheduledRunPlanEvent] = func(_ context.Context, job ScheduledRun) error {
		jobs <- job
		return nil
	}

	updated, err := a.advancePlan(project, coordinator, plan.ID, PlanActionRequest{Action: "accept_step", StepID: "step-1", Result: "verified", ExpectedVersion: plan.Version})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Steps[1].Status != PlanStepReady {
		t.Fatalf("next step status = %s, want ready", updated.Steps[1].Status)
	}
	select {
	case job := <-jobs:
		var payload PlanEventRunPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Event != "plan_advanced" || payload.StepID != "step-2" || payload.PlanVersion != updated.Version {
			t.Fatalf("follow-up payload = %+v, updated version=%d", payload, updated.Version)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("plan owner did not receive a follow-up event")
	}
	deadline := time.Now().Add(2 * time.Second)
	for a.agentRunActive(project.ID, coordinator.ID) || a.scheduledAgentRunCount(project.ID, coordinator.ID) > 0 || a.scheduledAgentWorkerActive(project.ID, coordinator.ID) {
		if time.Now().After(deadline) {
			t.Fatal("plan follow-up worker did not become idle")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPlanOwnerContinuationOnlyWhenActionable(t *testing.T) {
	cases := []struct {
		name string
		plan WorkPlan
		want bool
	}{
		{name: "ready step", plan: WorkPlan{Status: PlanActive, MaxConcurrency: 1, Steps: []PlanStep{{ID: "next", Status: PlanStepReady}}}, want: true},
		{name: "capacity available", plan: WorkPlan{Status: PlanActive, MaxConcurrency: 2, Steps: []PlanStep{{ID: "running", Status: PlanStepRunning}, {ID: "next", Status: PlanStepReady}}}, want: true},
		{name: "capacity full", plan: WorkPlan{Status: PlanActive, MaxConcurrency: 1, Steps: []PlanStep{{ID: "running", Status: PlanStepRunning}, {ID: "next", Status: PlanStepReady}}}, want: false},
		{name: "task decision", plan: WorkPlan{Status: PlanActive, Steps: []PlanStep{{ID: "review", Status: PlanStepAwaitingDecision}}}, want: true},
		{name: "delivered review", plan: WorkPlan{Status: PlanActive, Steps: []PlanStep{{ID: "review", Status: PlanStepReviewing, Reviews: []PlanReview{{Status: "delivered"}}}}}, want: true},
		{name: "pending review", plan: WorkPlan{Status: PlanActive, Steps: []PlanStep{{ID: "review", Status: PlanStepReviewing, Reviews: []PlanReview{{Status: "pending"}}}}}, want: false},
		{name: "complete plan", plan: WorkPlan{Status: PlanActive, Steps: []PlanStep{{ID: "done", Status: PlanStepCompleted}}}, want: true},
		{name: "paused plan", plan: WorkPlan{Status: PlanPaused, Steps: []PlanStep{{ID: "next", Status: PlanStepReady}}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := planOwnerContinuation(tc.plan)
			if got != tc.want {
				t.Fatalf("planOwnerContinuation() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestResumeActionablePlansSchedulesRecoveredOwner(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "1")
	a, project, coordinator, _ := planTestApp(t)
	plan := WorkPlan{
		ID: "plan-recovered", ProjectID: project.ID, Title: "Recovered plan", Goal: "Continue after restart", Status: PlanActive,
		OwnerType: "group", OwnerGroupID: "build", OwnerAgentID: coordinator.ID, MaxConcurrency: 1, Version: 4,
		Steps: []PlanStep{{ID: "next", Title: "Next", Status: PlanStepReady, Version: 2}},
	}
	a.plans[project.ID] = []WorkPlan{plan}
	jobs := make(chan ScheduledRun, 1)
	a.schedulerExecutors[ScheduledRunPlanEvent] = func(_ context.Context, job ScheduledRun) error {
		jobs <- job
		return nil
	}

	a.resumeActionablePlans()
	select {
	case job := <-jobs:
		var payload PlanEventRunPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Event != "plan_recovered" || payload.StepID != "next" || payload.PlanVersion != plan.Version {
			t.Fatalf("recovery payload = %+v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("actionable plan was not resumed")
	}
	deadline := time.Now().Add(2 * time.Second)
	for a.agentRunActive(project.ID, coordinator.ID) || a.scheduledAgentRunCount(project.ID, coordinator.ID) > 0 || a.scheduledAgentWorkerActive(project.ID, coordinator.ID) {
		if time.Now().After(deadline) {
			t.Fatal("recovered plan worker did not become idle")
		}
		time.Sleep(5 * time.Millisecond)
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

func TestPlanOwnerReconcilesTerminalTasksCreatedBeforePlan(t *testing.T) {
	a, project, coordinator, builder := planTestApp(t)
	now := time.Now().UTC()
	a.tasks[project.ID] = []Task{
		{ID: "task-m0", ProjectID: project.ID, Title: "M0 data probe", Status: "done", Result: "merged and independently verified", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)},
		{ID: "task-m1", ProjectID: project.ID, Title: "M1 watchlist", Status: "completed", Result: "tests pass; final review pending", CreatedAt: now.Add(-time.Hour), UpdatedAt: now},
	}
	plan, err := a.createPlanDraft(project, builder, builder.ID, WorkPlanDraftRequest{Title: "Milestones", Goal: "Finish", Steps: []PlanStep{
		{ID: "m0", Title: "M0", Description: "Probe"},
		{ID: "m1", Title: "M1", Description: "Watchlist", Dependencies: []string{"m0"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = a.submitPlan(project.ID, plan.ID, coordinator.ID, plan.Version)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = a.reconcilePlanHistory(project, coordinator, PlanHistoryReconciliationRequest{
		PlanID: plan.ID, ExpectedVersion: plan.Version, Steps: []PlanHistoryReconciliationStep{
			{StepID: "m0", TaskIDs: []string{"task-m0"}, Decision: "accepted", Evidence: "Task merged and reviewer accepted it."},
			{StepID: "m1", TaskIDs: []string{"task-m1"}, Decision: "needs_review", Evidence: "Implementation is terminal but still needs owner review."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Steps[0].Status != PlanStepCompleted || plan.Steps[1].Status != PlanStepAwaitingDecision {
		t.Fatalf("reconciled steps = %+v", plan.Steps)
	}
	if len(plan.Steps[0].TaskAttempts) != 1 || plan.Steps[0].TaskAttempts[0].TaskID != "task-m0" {
		t.Fatalf("historical attempts = %+v", plan.Steps[0].TaskAttempts)
	}
	result := a.listTasksFromResidentTool(project.ID, map[string]any{"query": "M0 M1", "limit": 10})
	if !strings.Contains(result, "task-m0") || !strings.Contains(result, "task-m1") {
		t.Fatalf("task history result = %s", result)
	}
}

func containsToolError(result, code string) bool {
	var payload map[string]any
	if jsonUnmarshalToolResult(result, &payload) != nil || payload["error"] == nil {
		return false
	}
	return code == "" || payload["error"] == code
}
