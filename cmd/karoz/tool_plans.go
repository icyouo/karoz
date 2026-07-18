package main

import (
	"encoding/json"
	"strings"
)

func residentPlanToolSpecs() []map[string]any {
	step := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":                  map[string]any{"type": "string"},
			"title":               map[string]any{"type": "string"},
			"description":         map[string]any{"type": "string"},
			"dependencies":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"acceptance_criteria": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"assigned_agent_id":   map[string]any{"type": "string"},
			"assigned_group_id":   map[string]any{"type": "string"},
		},
		"required": []string{"title", "description"},
	}
	return []map[string]any{
		residentToolSpec("list_groups", "List project groups, their coordinator, and members. Karoz uses this before routing business work.", nil, nil),
		residentToolSpec("send_to_group", "Deliver work to a group inbox. The group's coordinator receives it and decides internal assignment. Karoz and group coordinators use this for cross-group communication.", map[string]any{
			"group_id": map[string]any{"type": "string"}, "intent": map[string]any{"type": "string", "enum": []string{"note", "request", "handoff", "status", "question", "result"}},
			"subject": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}, "objective": map[string]any{"type": "string"}, "expected_output": map[string]any{"type": "string"},
			"preferred_member_id": map[string]any{"type": "string"}, "parent_plan_id": map[string]any{"type": "string"}, "parent_step_id": map[string]any{"type": "string"},
		}, []string{"group_id", "body"}),
		residentToolSpec("list_plans", "List durable WorkPlans for this project.", nil, nil),
		residentToolSpec("get_plan", "Read one durable WorkPlan including todo state, task attempts, and reviews.", map[string]any{"plan_id": map[string]any{"type": "string"}}, []string{"plan_id"}),
		residentToolSpec("save_plan_draft", "Persist the Plan-mode todo list as a durable WorkPlan draft. A grouped author assigns ownership to its group coordinator; a standalone author owns it directly. Karoz cannot author ordinary WorkPlans.", map[string]any{
			"title": map[string]any{"type": "string"}, "goal": map[string]any{"type": "string"}, "max_concurrency": map[string]any{"type": "integer"},
			"steps": map[string]any{"type": "array", "items": step},
		}, []string{"title", "goal", "steps"}),
		residentToolSpec("submit_plan", "Submit a persisted WorkPlan draft for user approval.", map[string]any{"plan_id": map[string]any{"type": "string"}, "expected_version": map[string]any{"type": "integer"}}, []string{"plan_id"}),
		residentToolSpec("advance_plan", "Advance one active WorkPlan through an explicit decision. Task completion is evidence only; use accept_step, delegate_review, or request_rework before completing a todo.", map[string]any{
			"plan_id": map[string]any{"type": "string"}, "action": map[string]any{"type": "string", "enum": []string{"dispatch_task", "delegate_group", "accept_step", "request_rework", "delegate_review", "block_step", "skip_step", "add_step", "pause", "cancel", "complete_plan"}},
			"step_id": map[string]any{"type": "string"}, "agent_id": map[string]any{"type": "string"}, "group_id": map[string]any{"type": "string"}, "reviewer_agent_id": map[string]any{"type": "string"},
			"task_type": map[string]any{"type": "string", "enum": []string{"bug", "feature", "deploy"}}, "result": map[string]any{"type": "string"}, "reason": map[string]any{"type": "string"},
			"title": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}, "goal": map[string]any{"type": "string"}, "dependencies": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"expected_version": map[string]any{"type": "integer"},
		}, []string{"plan_id", "action"}),
	}
}

func (a *app) listGroupsFromResidentTool(projectID string) string {
	return toolJSON(map[string]any{"groups": a.groupsForProject(projectID)})
}

func (a *app) listPlansFromResidentTool(projectID string) string {
	return toolJSON(map[string]any{"plans": a.plansForProject(projectID)})
}

func (a *app) getPlanFromResidentTool(projectID string, args map[string]any) string {
	plan, ok := a.planByID(projectID, toolStringArg(args, "plan_id", 128))
	if !ok {
		return toolJSON(map[string]any{"error": "not_found", "message": "plan not found"})
	}
	return toolJSON(map[string]any{"plan": plan})
}

func decodeToolArgs(args map[string]any, target any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func (a *app) savePlanDraftFromResidentTool(project Project, author Agent, args map[string]any) string {
	var req WorkPlanDraftRequest
	if err := decodeToolArgs(args, &req); err != nil {
		return toolJSON(map[string]any{"error": "validation_error", "message": err.Error()})
	}
	plan, err := a.createPlanDraft(project, author, author.ID, req)
	if err != nil {
		code := "create_failed"
		if author.ID == "karoz" {
			code = "route_required"
		}
		return toolJSON(map[string]any{"error": code, "message": err.Error(), "groups": a.groupsForProject(project.ID)})
	}
	return toolJSON(map[string]any{"plan": plan, "next_action": "submit_plan"})
}

func (a *app) submitPlanFromResidentTool(projectID string, actor Agent, args map[string]any) string {
	plan, err := a.submitPlan(projectID, toolStringArg(args, "plan_id", 128), actor.ID, int64(clampToolInt(args, "expected_version", 0, 0, 1_000_000_000)))
	if err != nil {
		return toolJSON(map[string]any{"error": "submit_failed", "message": err.Error()})
	}
	return toolJSON(map[string]any{"plan": plan, "awaiting_user_approval": true})
}

func (a *app) advancePlanFromResidentTool(project Project, actor Agent, args map[string]any) string {
	planID := strings.TrimSpace(toolStringArg(args, "plan_id", 128))
	var req PlanActionRequest
	if err := decodeToolArgs(args, &req); err != nil {
		return toolJSON(map[string]any{"error": "validation_error", "message": err.Error()})
	}
	plan, err := a.advancePlan(project, actor, planID, req)
	if err != nil {
		return toolJSON(map[string]any{"error": "advance_failed", "message": err.Error()})
	}
	return toolJSON(map[string]any{"plan": plan})
}
