package main

import (
	"fmt"
	"strings"
)

func (a *app) buildResidentAgentPrompt(project Project, agent Agent, userText, turnType string) string {
	turnType = normalizeChatTurnType(turnType)
	agentID := agent.ID
	a.maybeCheckpointAgentSession(project.ID, agentID, false)
	state := a.agentSessionState(project.ID, agentID)
	history := a.agentMessagesFor(project.ID, agentID)
	var delta []AgentMessage
	for _, msg := range history {
		if msg.Seq >= state.ShortWindowStartSeq || msg.Seq == 0 {
			delta = append(delta, msg)
		}
	}
	if len(delta) > 50 {
		delta = delta[len(delta)-50:]
	}
	var b strings.Builder
	b.WriteString("## Session mode: Resident agent\n")
	b.WriteString("## Current chat turn type: " + turnType + "\n")
	b.WriteString("- You are a project resident agent. Keep continuity across turns and work from durable project context, not just the latest message.\n")
	b.WriteString("- Treat the visible conversation as the short-term window. Earlier full messages are archived; use memory/archive tools by id, sequence range, or query when details are needed.\n")
	b.WriteString("- When important facts, decisions, completed work, or pending work appear, preserve them through resident memory tools.\n")
	b.WriteString("- Use project tools to ground answers in the real project before making claims. Prefer focused, verifiable steps over broad speculation.\n")
	b.WriteString("- Use web_search and web_fetch for current external facts, docs, releases, prices, policies, or anything likely to have changed. Summarize sources with URLs when web tools inform the answer.\n")
	b.WriteString("- If MCP tools are available, use tools named mcp__server__tool for external systems. For figma.com URLs or design implementation tasks, prefer the Figma MCP tools exposed by the figma server; parse file key and node-id from the URL, use get_design_context/get_screenshot/get_metadata when available, and do not claim Figma is unavailable until the MCP tool call itself fails.\n")
	b.WriteString("- Coordinate with teammates through the collaboration loop: send_to creates a request/handoff, reply_to answers a non-Karoz requester, report_activity reports one-way state to Karoz, and ack_inbox only confirms low-information receipt/completion.\n")
	b.WriteString("- Collaboration rule: never reply_to Karoz. For a Karoz-originated handoff, use report_activity with its inbox_message_id; activity_kind done or error closes it, while progress/blocker/decision reports keep it open. Reports never trigger a Karoz response. For other original requests use reply_to, decline_handoff, or ack_inbox exactly once. A reply is terminal: consume it without replying. If more work is required, create a new send_to handoff instead of replying to a reply.\n")
	b.WriteString("- Escalation rule: send_to karoz only for decisions, conflicts, resource requests, or user-facing coordination. Use report_activity only for project-level blockers, decisions, or milestones that are not already represented by a Run, Handoff, or Task.\n")
	b.WriteString("- Use create_task when a requested development or deployment task should be tracked as a project task. Use update_task_status when you have a concrete status change for an existing task.\n")
	b.WriteString("- Use request_choice when you need the user to confirm yes/no or choose one option from a numbered list. After requesting a choice, wait for the user's next message and do not assume the answer.\n")
	b.WriteString("- At the start of each new user turn, if you need tools, first emit one short visible sentence describing what you will inspect or do, then call the first tool. Do not begin a user turn with a tool-only response.\n")
	b.WriteString("- You have a bash tool that runs in the selected project workspace. Use it when you need real repository or runtime evidence. Prefer read-only inspection unless the user asks for changes.\n")
	b.WriteString("- Use write_workspace_file for generated artifacts such as requirements, development plans, and HTML mockups. Use show_preview after writing an HTML design draft that should open in the side preview.\n")
	b.WriteString("- Workspace writes create versioned Artifacts. Use list_artifacts/get_artifact for metadata, submit_artifact for review, and review_artifact for approval or change requests. Reference Artifact IDs in send_to and create_task instead of copying their full contents.\n")
	b.WriteString("- This local OSS version has no sandbox boundary. Do not pretend files, git operations, or shell commands are isolated.\n")
	b.WriteString("- Respond in the user's language. Use concise, concrete answers. Do not claim that you created a task unless an explicit tool call or API action has created one.\n\n")
	if capabilitiesForAgent(agent).CanManageAgents {
		b.WriteString("### Karoz coordination tools\n")
		b.WriteString("- You can manage resident agents with list_agent_templates, add_agent, create_agent_team, and delete_agent.\n")
		b.WriteString("- When the user asks to create an agent or team by role/natural language, call list_agent_templates first, then choose the exact template_id from that result.\n")
		b.WriteString("- When the user asks you to let, ask, notify, route to, or have one or more resident agents discuss/review/output something, you must call send_to for each named or relevant target agent in the same turn before claiming it was sent. Do not answer only with a plan such as \"I will ask them\".\n")
		b.WriteString("- For multi-agent product/design/architecture coordination, send concise requests to the responsible agents and tell the user which agents were queued. Use the visible Resident teammates list as the routing source.\n")
		b.WriteString("- Do not use bash or external project files to discover Karoz resident template IDs; the list_agent_templates tool is authoritative.\n\n")
	}
	switch turnType {
	case "ask":
		b.WriteString("### Turn contract: ask\n")
		b.WriteString("- Answer questions and inspect context when useful.\n")
		b.WriteString("- Do not create tasks, write files, or make repository changes unless the user explicitly asks you to switch into development work.\n\n")
	case "plan":
		b.WriteString("### Turn contract: plan\n")
		b.WriteString("- Produce a concrete plan, requirement draft, design direction, or implementation outline.\n")
		b.WriteString("- You may write generated artifacts such as requirements, plans, and HTML design drafts when requested.\n")
		b.WriteString("- Do not execute coding changes directly; create a task only if the user asks to track the work.\n\n")
	case "dev":
		b.WriteString("### Turn contract: dev\n")
		b.WriteString("- You may inspect the repo, create bug/feature/deploy tasks, and use tools needed to advance implementation.\n")
		b.WriteString("- Prefer creating a task for coding/deployment execution rather than pretending long-running work happened inside the chat turn.\n\n")
	}
	b.WriteString("### Current project\n")
	b.WriteString("- name: " + project.Name + "\n")
	b.WriteString("- path: " + project.Path + "\n")
	b.WriteString("- branch: " + project.DefaultBranch + "\n")
	b.WriteString("- resident_agent: " + agentID + "\n")
	b.WriteString("\n### Resident identity\n")
	b.WriteString("- nickname: " + firstNonEmpty(agent.Nickname, agent.DisplayName, agent.Name) + "\n")
	b.WriteString("- template: " + agent.Name + "\n")
	b.WriteString("- display_name: " + agent.DisplayName + "\n")
	b.WriteString("- role: " + agent.Role + "\n")
	if strings.TrimSpace(agent.GroupID) != "" {
		b.WriteString("- group_id: " + agent.GroupID + "\n")
		b.WriteString("- group_name: " + agent.GroupName + "\n")
		b.WriteString("- group_role: " + agent.GroupRole + "\n")
		b.WriteString("- group_order: " + fmt.Sprintf("%d", agent.GroupOrder) + "\n")
		b.WriteString("- Group contract: follow the team's handoff order. Use send_to for explicit handoffs to the next responsible group member instead of broadcasting.\n")
	}
	if strings.TrimSpace(agent.Summary) != "" {
		b.WriteString("- summary: " + agent.Summary + "\n")
	}
	if strings.TrimSpace(agent.SystemPrompt) != "" {
		b.WriteString("- Template instructions:\n")
		b.WriteString(indentPrompt(limitString(strings.TrimSpace(agent.SystemPrompt), 2400), "  "))
		b.WriteString("\n")
	}
	if skillPrompt := a.renderSkillsPrompt(project); skillPrompt != "" {
		b.WriteString(skillPrompt)
	}
	if skillInjection := a.injectMentionedSkills(project, userText); skillInjection != "" {
		b.WriteString("\n### Selected skill instructions\n")
		b.WriteString(skillInjection)
	}
	if residentAgentIsDesign(agent) {
		b.WriteString(residentDesignAgentPrompt())
	}
	if residentAgentIsReviewer(agent) {
		b.WriteString(residentReviewerAgentPrompt())
	}
	if residentAgentIsBuilder(agent) {
		b.WriteString(residentBuilderAgentPrompt())
	}
	if peers := a.projectAgents(project); len(peers) > 1 {
		b.WriteString("\n### Resident teammates\n")
		for _, peer := range peers {
			if peer.ID == agent.ID {
				continue
			}
			b.WriteString("- id: ")
			b.WriteString(peer.ID)
			b.WriteString("; name: ")
			b.WriteString(firstNonEmpty(peer.Nickname, peer.DisplayName, peer.Name))
			b.WriteString("; role: ")
			b.WriteString(peer.Role)
			if strings.TrimSpace(peer.GroupID) != "" {
				b.WriteString("; group: ")
				b.WriteString(peer.GroupID)
				b.WriteString("/")
				b.WriteString(peer.GroupRole)
			}
			b.WriteString("\n")
		}
	}
	if pending := a.pendingInboxFor(project.ID, agent.ID, 8); len(pending) > 0 {
		b.WriteString("\n### Pending handoffs for this agent\n")
		for _, msg := range pending {
			b.WriteString("- id: ")
			b.WriteString(msg.ID)
			b.WriteString("; from: ")
			b.WriteString(msg.SourceAgentID)
			if strings.TrimSpace(msg.MessageType) != "" {
				b.WriteString("; type: ")
				b.WriteString(msg.MessageType)
			}
			b.WriteString("; intent: ")
			b.WriteString(msg.Intent)
			if strings.TrimSpace(msg.ReplyToID) != "" {
				b.WriteString("; reply_to: ")
				b.WriteString(msg.ReplyToID)
			}
			b.WriteString("; subject: ")
			b.WriteString(msg.Subject)
			b.WriteString("; correlation: ")
			b.WriteString(msg.CorrelationID)
			if strings.TrimSpace(msg.ParentRunID) != "" {
				b.WriteString("; parent_run: ")
				b.WriteString(msg.ParentRunID)
			}
			b.WriteString("; objective: ")
			b.WriteString(msg.Objective)
			b.WriteString("; expected_output: ")
			b.WriteString(msg.ExpectedOutput)
			if len(msg.ArtifactIDs) > 0 {
				b.WriteString("; artifact_ids: ")
				b.WriteString(strings.Join(msg.ArtifactIDs, ", "))
			}
			b.WriteString("; body: ")
			b.WriteString(limitString(msg.Body, 500))
			b.WriteString("\n")
		}
	}
	if pending := a.activeMemoriesFor(project.ID, agent.ID, "pending", 8); len(pending) > 0 {
		b.WriteString("\n### Active pending memory\n")
		for _, entry := range pending {
			b.WriteString("- id: ")
			b.WriteString(entry.ID)
			b.WriteString("; priority: ")
			b.WriteString(fmt.Sprintf("%d", entry.Priority))
			b.WriteString("; ")
			b.WriteString(entry.Summary)
			b.WriteString("\n")
		}
	}
	if entries := a.blackboardFor(project.ID, 8); len(entries) > 0 {
		b.WriteString("\n### Latest blackboard\n")
		for _, entry := range entries {
			b.WriteString("- ")
			b.WriteString(entry.ActivityKind)
			b.WriteString(" by ")
			b.WriteString(entry.AgentName)
			b.WriteString(": ")
			b.WriteString(entry.Summary)
			if strings.TrimSpace(entry.Detail) != "" {
				b.WriteString(" — ")
				b.WriteString(limitString(entry.Detail, 360))
			}
			b.WriteString("\n")
		}
	}
	if strings.TrimSpace(state.ResidentSummary) != "" {
		b.WriteString("\nResident rolling summary:\n")
		b.WriteString(strings.TrimSpace(state.ResidentSummary))
		b.WriteString("\n\nEarlier full messages are archived. Use search_archive or get_messages for exact details.")
		b.WriteString("\n")
	}
	if len(delta) > 0 {
		b.WriteString("\nRecent resident conversation delta:\n")
		for _, line := range renderAgentPromptDelta(delta, 50, 24000) {
			b.WriteString(strings.ToUpper(line.Role))
			b.WriteString(": ")
			b.WriteString(line.Body)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nLatest user message:\n")
	b.WriteString(userText)
	return b.String()
}
