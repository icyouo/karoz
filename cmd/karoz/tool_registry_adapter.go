package main

import (
	"context"
	"fmt"
	tooldomain "github.com/karoz/karoz/internal/tool"
	"strings"
)

func definitionFromResidentToolSpec(spec map[string]any) tooldomain.Definition {
	parameters, _ := spec["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	required, _ := parameters["required"].([]string)
	return tooldomain.Definition{
		Name:        fmt.Sprint(spec["name"]),
		Description: fmt.Sprint(spec["description"]),
		Properties:  properties,
		Required:    required,
	}
}

func (a *app) residentToolRegistry() *tooldomain.Registry[ResidentToolContext] {
	a.residentToolsOnce.Do(func() {
		registry := tooldomain.NewRegistry[ResidentToolContext]()
		handlers := map[string]tooldomain.Handler[ResidentToolContext]{
			"bash": func(ctx context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.executeResidentBashTool(ctx, toolCtx, args)
			},
			"repo_list": func(ctx context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				result := repoListTool(ctx, toolCtx.Workdir, args)
				return result, ctx.Err()
			},
			"repo_read": func(ctx context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				result := repoReadTool(ctx, toolCtx.Workdir, args)
				return result, ctx.Err()
			},
			"repo_search": func(ctx context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				result := repoSearchTool(ctx, toolCtx.Workdir, args)
				return result, ctx.Err()
			},
			"list_skills": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.listSkillsTool(toolCtx.Project, toolStringArg(args, "query", 200)), nil
			},
			"read_skill": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.readSkillTool(toolCtx.Project, toolStringArg(args, "name", 200)), nil
			},
			"web_search": func(ctx context.Context, _ ResidentToolContext, args map[string]any) (string, error) {
				return webSearchTool(ctx, args), nil
			},
			"web_fetch": func(ctx context.Context, _ ResidentToolContext, args map[string]any) (string, error) {
				return webFetchTool(ctx, args), nil
			},
			"request_choice": func(_ context.Context, _ ResidentToolContext, args map[string]any) (string, error) {
				return requestChoiceTool(args), nil
			},
			"remember_fact": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.createMemory(toolCtx.Project.ID, toolCtx.Agent.ID, "fact", args, 0, nil)
			},
			"record_decision": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				rationale := toolStringArg(args, "rationale", 8000)
				if rationale == "" {
					return toolJSON(map[string]any{"error": "validation_error", "message": "rationale is required"}), nil
				}
				return a.createMemory(toolCtx.Project.ID, toolCtx.Agent.ID, "decision", args, 0, map[string]any{"rationale": rationale})
			},
			"mark_done": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.createMemory(toolCtx.Project.ID, toolCtx.Agent.ID, "done", args, 0, nil)
			},
			"add_pending": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.createMemory(toolCtx.Project.ID, toolCtx.Agent.ID, "pending", args, clampToolInt(args, "priority", 0, 0, 100), nil)
			},
			"drop_pending": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.dropPendingMemory(toolCtx.Project.ID, toolCtx.Agent.ID, toolStringArg(args, "id", 128)), nil
			},
			"search_archive": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.searchArchive(toolCtx.Project.ID, toolCtx.Agent.ID, toolStringArg(args, "query", 1000), clampToolInt(args, "limit", 20, 1, 200)), nil
			},
			"list_pending": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return toolJSON(map[string]any{"entries": a.memorySummaries(a.activeMemoriesFor(toolCtx.Project.ID, toolCtx.Agent.ID, "pending", clampToolInt(args, "limit", 50, 1, 200)))}), nil
			},
			"get_messages": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.getArchivedMessages(toolCtx.Project.ID, toolCtx.Agent.ID, int64(clampToolInt(args, "start_seq", 1, 1, 1_000_000_000)), int64(clampToolInt(args, "end_seq", 1_000_000_000, 1, 1_000_000_000)), clampToolInt(args, "limit", 40, 1, 80)), nil
			},
			"write_workspace_file": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.writeWorkspaceFileFromTool(toolCtx.Project.ID, toolCtx.Agent.ID, toolCtx.RunID, args), nil
			},
			"show_preview": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.showWorkspacePreviewFromTool(toolCtx.Project.ID, toolCtx.Agent.ID, args), nil
			},
			"list_artifacts": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return toolJSON(map[string]any{"artifacts": a.artifactsForProject(toolCtx.Project.ID, toolStringArg(args, "agent_id", 128), toolStringArg(args, "kind", 64), toolStringArg(args, "status", 64))}), nil
			},
			"get_artifact": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.getArtifactFromTool(toolCtx.Project.ID, args), nil
			},
			"submit_artifact": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.submitArtifactFromTool(toolCtx.Project.ID, toolCtx.Agent, args), nil
			},
			"review_artifact": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.reviewArtifactFromTool(toolCtx.Project.ID, toolCtx.Agent, args), nil
			},
			"create_task": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.createTaskFromResidentTool(toolCtx.Project, toolCtx.Agent, args), nil
			},
			"update_task_status": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.updateTaskStatusFromResidentTool(toolCtx.Project.ID, toolCtx.Agent.ID, args), nil
			},
			"list_agent_templates": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.listAgentTemplatesFromResidentTool(toolCtx.Agent, args), nil
			},
			"add_agent": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.addAgentFromResidentTool(toolCtx.Project, toolCtx.Agent, args), nil
			},
			"create_agent_team": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.createAgentTeamFromResidentTool(toolCtx.Project, toolCtx.Agent, args), nil
			},
			"delete_agent": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.deleteAgentFromResidentTool(toolCtx.Project, toolCtx.Agent, args), nil
			},
			"send_to": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.sendToAgent(toolCtx.Project.ID, toolCtx.Agent.ID, toolCtx.RunID, args), nil
			},
			"reply_to": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.replyToInboxMessage(toolCtx.Project.ID, toolCtx.Agent.ID, toolCtx.RunID, args), nil
			},
			"decline_handoff": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.declineInboxHandoff(toolCtx.Project.ID, toolCtx.Agent.ID, args), nil
			},
			"ack_inbox": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.ackInboxMessage(toolCtx.Project.ID, toolCtx.Agent.ID, args), nil
			},
			"report_activity": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.reportActivity(toolCtx.Project.ID, toolCtx.Agent, args), nil
			},
			"mark_activity": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.markBlackboardActivity(toolCtx.Project.ID, toolCtx.Agent, args), nil
			},
			"list_groups": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.listGroupsFromResidentTool(toolCtx.Project.ID), nil
			},
			"send_to_group": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.sendToGroup(toolCtx.Project.ID, toolCtx.Agent.ID, toolCtx.RunID, args), nil
			},
			"list_plans": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.listPlansFromResidentTool(toolCtx.Project.ID), nil
			},
			"get_plan": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.getPlanFromResidentTool(toolCtx.Project.ID, args), nil
			},
			"save_plan_draft": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.savePlanDraftFromResidentTool(toolCtx.Project, toolCtx.Agent, args), nil
			},
			"submit_plan": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.submitPlanFromResidentTool(toolCtx.Project.ID, toolCtx.Agent, args), nil
			},
			"advance_plan": func(_ context.Context, toolCtx ResidentToolContext, args map[string]any) (string, error) {
				return a.advancePlanFromResidentTool(toolCtx.Project, toolCtx.Agent, args), nil
			},
		}
		specs := append(residentToolSpecs(), residentPlanToolSpecs()...)
		specs = append(specs, residentAgentManagementToolSpecs()...)
		for _, spec := range specs {
			definition := definitionFromResidentToolSpec(spec)
			handler, exists := handlers[definition.Name]
			if !exists {
				panic("resident tool handler missing: " + definition.Name)
			}
			if err := registry.Register(definition, handler); err != nil {
				panic(err)
			}
			delete(handlers, definition.Name)
		}
		if len(handlers) > 0 {
			panic(fmt.Sprintf("resident tool definitions missing for handlers: %v", handlers))
		}
		a.residentTools = registry
	})
	return a.residentTools
}

func (a *app) executeResidentTool(ctx context.Context, toolCtx ResidentToolContext, call codexToolCall) (string, error) {
	if err := ctx.Err(); err != nil {
		return toolJSON(map[string]any{"error": "cancelled", "message": err.Error()}), err
	}
	if toolCtx.EnforceRunScope {
		if run, active := a.activeAgentRun(toolCtx.Project.ID, toolCtx.Agent.ID); !active || run.ID != toolCtx.RunID {
			return toolJSON(map[string]any{"error": "stale_run", "message": "the resident run is no longer active"}), context.Canceled
		}
	}
	if toolCtx.EnforcePolicy && !residentToolAllowed(toolCtx, call.Name) {
		return toolJSON(map[string]any{"error": "tool_forbidden", "message": call.Name + " is not allowed for this resident turn"}), nil
	}
	if residentToolHasSideEffects(call.Name) && call.Name != "bash" {
		if err := a.markScheduledRunEffectsStarted(toolCtx.RunID); err != nil {
			return toolJSON(map[string]any{"error": "effect_barrier_failed", "message": err.Error()}), err
		}
	}
	if strings.HasPrefix(call.Name, "mcp__") {
		return a.dynamicToolProvider().Call(ctx, toolCtx.Workdir, call.Name, call.Arguments)
	}
	args, err := parseToolArgs(call.Arguments)
	if err != nil {
		return toolJSON(map[string]any{"error": "validation_error", "message": err.Error()}), nil
	}
	result, found, err := a.residentToolRegistry().Execute(ctx, toolCtx, call.Name, args)
	if !found {
		return toolJSON(map[string]any{"error": "unknown_tool", "message": call.Name}), nil
	}
	return result, err
}

func residentToolHasSideEffects(name string) bool {
	if strings.HasPrefix(name, "mcp__") {
		return true
	}
	switch name {
	case "repo_list", "repo_read", "repo_search", "list_skills", "read_skill",
		"web_search", "web_fetch", "search_archive", "list_pending", "get_messages",
		"list_artifacts", "get_artifact", "list_agent_templates", "list_groups", "list_plans", "get_plan":
		return false
	default:
		return true
	}
}
