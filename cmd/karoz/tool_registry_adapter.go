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
		}
		specs := append(residentToolSpecs(), residentAgentManagementToolSpecs()...)
		for _, spec := range specs {
			definition := definitionFromResidentToolSpec(spec)
			if definition.Name == "bash" {
				continue
			}
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
	if call.Name == "bash" {
		result := runResidentBashTool(toolCtx.Workdir, call.Arguments)
		return toolJSON(result), nil
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
