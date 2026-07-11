package main

import (
	"context"
	tooldomain "github.com/karoz/karoz/internal/tool"
)

func bashToolSpec() map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        "bash",
		"description": "Run a bash command in the current project workspace. Use it to inspect files, run tests, and gather local evidence. The command runs on the host workspace without sandbox isolation.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":     map[string]any{"type": "string", "description": "Bash command to execute."},
				"timeout_ms":  map[string]any{"type": "integer", "description": "Optional timeout in milliseconds. Default 60000, max 300000."},
				"max_output":  map[string]any{"type": "integer", "description": "Optional maximum combined stdout/stderr characters. Default 20000."},
				"description": map[string]any{"type": "string", "description": "Short reason for running the command."},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
}

func residentToolSpecs() []map[string]any {
	return []map[string]any{
		bashToolSpec(),
		residentToolSpec("list_skills", "List local Karoz/Codex-style skills discovered for the current project.", map[string]any{
			"query": map[string]any{"type": "string", "description": "Optional case-insensitive name or description filter."},
		}, nil),
		residentToolSpec("read_skill", "Read the full SKILL.md content for a discovered local skill by name.", map[string]any{
			"name": map[string]any{"type": "string", "description": "Discovered skill name."},
		}, []string{"name"}),
		residentToolSpec("web_search", "Search the web for current external information. Use this when facts may have changed, or when the user asks to look something up. Returns result titles, URLs, and snippets.", map[string]any{
			"query": map[string]any{"type": "string"},
			"limit": map[string]any{"type": "integer", "description": "Optional result limit from 1 to 10. Default 5."},
		}, []string{"query"}),
		residentToolSpec("web_fetch", "Fetch and extract readable text from an HTTP or HTTPS URL. Use after web_search when source details are needed.", map[string]any{
			"url":       map[string]any{"type": "string"},
			"max_chars": map[string]any{"type": "integer", "description": "Optional maximum extracted text characters from 1000 to 50000. Default 12000."},
		}, []string{"url"}),
		residentToolSpec("request_choice", "Ask the user to choose from structured options. Use mode yes_no for confirmation, or single for numbered choices. After calling this tool, wait for the user's next message instead of assuming an answer.", map[string]any{
			"question": map[string]any{"type": "string"},
			"mode":     map[string]any{"type": "string", "enum": []string{"yes_no", "single"}},
			"choices": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":          map[string]any{"type": "string"},
						"label":       map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
					},
					"required":             []string{"label"},
					"additionalProperties": false,
				},
			},
		}, []string{"question", "mode"}),
		residentToolSpec("remember_fact", "Store a durable fact for this resident agent session.", map[string]any{
			"summary": map[string]any{"type": "string"},
			"detail":  map[string]any{"type": "string"},
		}, []string{"summary", "detail"}),
		residentToolSpec("record_decision", "Store a durable decision for this resident agent session, including rationale.", map[string]any{
			"summary":   map[string]any{"type": "string"},
			"detail":    map[string]any{"type": "string"},
			"rationale": map[string]any{"type": "string"},
		}, []string{"summary", "detail", "rationale"}),
		residentToolSpec("mark_done", "Store a completed work item for this resident agent session.", map[string]any{
			"summary": map[string]any{"type": "string"},
			"detail":  map[string]any{"type": "string"},
		}, []string{"summary", "detail"}),
		residentToolSpec("add_pending", "Store an active pending work item for this resident agent session.", map[string]any{
			"summary":  map[string]any{"type": "string"},
			"detail":   map[string]any{"type": "string"},
			"priority": map[string]any{"type": "integer", "description": "Priority from 0 to 100."},
		}, []string{"summary", "detail"}),
		residentToolSpec("drop_pending", "Archive a pending memory item by id.", map[string]any{
			"id": map[string]any{"type": "string"},
		}, []string{"id"}),
		residentToolSpec("search_archive", "Search this resident agent session's memory archive and archived messages.", map[string]any{
			"query": map[string]any{"type": "string"},
			"limit": map[string]any{"type": "integer", "description": "Optional result limit from 1 to 200."},
		}, []string{"query"}),
		residentToolSpec("list_pending", "List active pending memory items for this resident agent session.", map[string]any{
			"limit": map[string]any{"type": "integer", "description": "Optional result limit from 1 to 200."},
		}, nil),
		residentToolSpec("get_messages", "Retrieve compact archived/current messages by focused sequence range. Prefer search_archive first. Avoid broad ranges; tool results are summarized for loop safety.", map[string]any{
			"start_seq": map[string]any{"type": "integer", "description": "First sequence number to inspect."},
			"end_seq":   map[string]any{"type": "integer", "description": "Last sequence number to inspect."},
			"limit":     map[string]any{"type": "integer", "description": "Optional result limit from 1 to 80. Use small focused limits."},
		}, nil),
		residentToolSpec("write_workspace_file", "Write or revise a registered Artifact in this resident session workspace. Rewriting the same path increments its revision and returns it to draft.", map[string]any{
			"path":          map[string]any{"type": "string", "description": "Relative workspace path, for example mockup.html or docs/plan.md."},
			"content":       map[string]any{"type": "string"},
			"description":   map[string]any{"type": "string"},
			"artifact_kind": map[string]any{"type": "string", "enum": []string{"requirements", "design_brief", "user_flow", "wireframe", "mockup_html", "mockup_svg", "mockup_image", "design_system", "technical_plan", "implementation_handoff", "review_report"}},
			"title":         map[string]any{"type": "string"},
		}, []string{"path", "content"}),
		residentToolSpec("show_preview", "Open an Artifact or workspace file in the side preview.", map[string]any{
			"artifact_id": map[string]any{"type": "string"},
			"path":        map[string]any{"type": "string"},
		}, nil),
		residentToolSpec("list_artifacts", "List registered project Artifacts, optionally filtered by owner, kind, or status.", map[string]any{
			"agent_id": map[string]any{"type": "string"},
			"kind":     map[string]any{"type": "string"},
			"status":   map[string]any{"type": "string", "enum": []string{"draft", "reviewing", "approved", "superseded"}},
		}, nil),
		residentToolSpec("get_artifact", "Get Artifact metadata and revision history by ID.", map[string]any{
			"artifact_id": map[string]any{"type": "string"},
		}, []string{"artifact_id"}),
		residentToolSpec("submit_artifact", "Submit one draft Artifact for review.", map[string]any{
			"artifact_id": map[string]any{"type": "string"},
			"note":        map[string]any{"type": "string"},
		}, []string{"artifact_id"}),
		residentToolSpec("review_artifact", "Review an Artifact in reviewing state. Approve it or request changes; authors cannot approve their own Artifact.", map[string]any{
			"artifact_id": map[string]any{"type": "string"},
			"decision":    map[string]any{"type": "string", "enum": []string{"approved", "changes_requested"}},
			"note":        map[string]any{"type": "string"},
		}, []string{"artifact_id", "decision"}),
		residentToolSpec("create_task", "Create a project task from the current resident agent session. Task type must be bug, feature, or deploy. bugfix and deployment are accepted as compatibility aliases.", map[string]any{
			"title":        map[string]any{"type": "string"},
			"description":  map[string]any{"type": "string"},
			"goal":         map[string]any{"type": "string"},
			"type":         map[string]any{"type": "string", "enum": []string{"bug", "feature", "deploy"}},
			"assignee":     map[string]any{"type": "string", "description": "Optional compatibility hint; ignored in local OSS version."},
			"artifact_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Referenced approved design or planning Artifacts."},
		}, []string{"title", "description"}),
		residentToolSpec("update_task_status", "Update the status of a project task in the current resident agent session project.", map[string]any{
			"task_id": map[string]any{"type": "string"},
			"status":  map[string]any{"type": "string"},
			"result":  map[string]any{"type": "string"},
		}, []string{"task_id", "status"}),
		residentToolSpec("send_to", "Queue one asynchronous peer handoff/request. Address peers by the unique nickname shown in collaboration topology; internal IDs are accepted only for compatibility.", map[string]any{
			"target_agent_id":       map[string]any{"type": "string", "description": "Unique target nickname from collaboration topology (preferred), or an internal agent ID for compatibility."},
			"intent":                map[string]any{"type": "string", "enum": []string{"note", "request", "handoff", "status", "question", "result", "reply"}},
			"subject":               map[string]any{"type": "string"},
			"body":                  map[string]any{"type": "string"},
			"objective":             map[string]any{"type": "string", "description": "Concrete objective the target agent should complete."},
			"expected_output":       map[string]any{"type": "string", "description": "Definition of the result required to close this handoff."},
			"artifact_ids":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional referenced artifact IDs."},
			"correlation_id":        map[string]any{"type": "string", "description": "Optional workflow correlation ID; generated when omitted."},
			"thread_key":            map[string]any{"type": "string"},
			"priority":              map[string]any{"type": "integer"},
			"result_owner_agent_id": map[string]any{"type": "string", "description": "Karoz-only optional unique nickname (preferred) or ID that receives a delegated result. Peer agents must omit this because peer results return directly to the source."},
		}, []string{"target_agent_id", "body"}),
		residentToolSpec("reply_to", "Reply once with a substantive answer/result to the source agent for an original peer request. The receiver reviews and acknowledges this delivery; any genuinely new follow-up uses send_to, never reply-to-reply.", map[string]any{
			"inbox_message_id": map[string]any{"type": "string"},
			"subject":          map[string]any{"type": "string"},
			"body":             map[string]any{"type": "string"},
		}, []string{"inbox_message_id", "body"}),
		residentToolSpec("decline_handoff", "Decline an inbox handoff that cannot or should not be completed, with a concrete reason for the requester.", map[string]any{
			"inbox_message_id": map[string]any{"type": "string"},
			"reason":           map[string]any{"type": "string"},
		}, []string{"inbox_message_id", "reason"}),
		residentToolSpec("ack_inbox", "Silently consume one inbox delivery after handling it when there is no useful detail to send back. Ack is internal state only: it never creates a peer message and must not be used for substantive results.", map[string]any{
			"inbox_message_id": map[string]any{"type": "string"},
			"note":             map[string]any{"type": "string"},
		}, []string{"inbox_message_id"}),
		residentToolSpec("report_activity", "Send a one-way progress or completion report to Karoz. For a Karoz-originated handoff, done/error with inbox_message_id closes it and delivers the result to its result owner without triggering a Karoz response.", map[string]any{
			"activity_kind":    map[string]any{"type": "string", "enum": []string{"focus", "start", "progress", "blocker", "handoff", "done", "error", "next_step", "decision_needed"}},
			"summary":          map[string]any{"type": "string"},
			"detail":           map[string]any{"type": "string"},
			"inbox_message_id": map[string]any{"type": "string"},
		}, []string{"summary"}),
		residentToolSpec("mark_activity", "Mark an existing blackboard signal as consumed after routing, creating a task, asking the user, ignoring, or expiring it. This does not track work execution; inbox/task state does that.", map[string]any{
			"activity_id":        map[string]any{"type": "string"},
			"handling_result":    map[string]any{"type": "string", "enum": []string{"routed_to_inbox", "created_task", "asked_user", "ignored", "expired", "no_action"}},
			"note":               map[string]any{"type": "string"},
			"routed_to_agent_id": map[string]any{"type": "string"},
			"created_task_id":    map[string]any{"type": "string"},
			"requires_action":    map[string]any{"type": "boolean"},
		}, []string{"activity_id", "handling_result"}),
	}
}

func residentToolSpec(name, description string, properties map[string]any, required []string) map[string]any {
	return (tooldomain.Definition{Name: name, Description: description, Properties: properties, Required: required}).FunctionSpec()
}

func residentAgentManagementToolSpecs() []map[string]any {
	return []map[string]any{
		residentToolSpec("list_agent_templates", "List built-in resident agent role templates and team templates. Use this before add_agent or create_agent_team when the user describes a role in natural language.", map[string]any{
			"query": map[string]any{"type": "string", "description": "Optional case-insensitive filter, for example product, frontend, review, or discovery."},
		}, nil),
		residentToolSpec("add_agent", "Create one resident agent in the current project. Only the default Karoz agent can use this tool. Use list_agent_templates first if you are not certain of the exact template_id.", map[string]any{
			"template_id": map[string]any{"type": "string", "description": "Exact built-in resident template id from list_agent_templates, for example product-strategist, research-scan, architect, frontend-specialist, implementation-lead, review-critic."},
			"nickname":    map[string]any{"type": "string", "description": "Optional agent nickname. Defaults to the template display name."},
		}, []string{"template_id"}),
		residentToolSpec("create_agent_team", "Create a built-in resident agent team in the current project. Only the default Karoz agent can use this tool. Prefer this when the user asks for a group/lane such as product discovery.", map[string]any{
			"template_id": map[string]any{"type": "string", "description": "Exact team template id from list_agent_templates, for example product-discovery, build-lane, build-ultra, ui-polish, verify-ship."},
			"instance":    map[string]any{"type": "string", "description": "Optional team instance suffix/name."},
		}, []string{"template_id"}),
		residentToolSpec("delete_agent", "Delete a resident agent from the current project by id. Only the default Karoz agent can use this tool. The default karoz agent cannot be deleted.", map[string]any{
			"agent_id": map[string]any{"type": "string", "description": "Agent id to delete."},
		}, []string{"agent_id"}),
	}
}

func (a *app) residentToolSpecsForContext(ctx context.Context, workdir string, agent Agent) []map[string]any {
	specs := residentToolSpecs()
	if capabilitiesForAgent(agent).CanManageAgents {
		specs = append(specs, residentAgentManagementToolSpecs()...)
	}
	specs = append(specs, a.dynamicToolProvider().Specs(ctx, workdir)...)
	return specs
}
