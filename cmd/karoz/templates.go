package main

import (
	agentdomain "github.com/karoz/karoz/internal/agent"
)

func residentAgentTemplates() []AgentTemplate {
	return []AgentTemplate{
		agentTemplate("pmo-coordinator", "PMO", "PMO", "coordinate builders, track progress, enforce delivery cadence", "Coordinate implementation work, keep scope clear, track progress, and hand off concrete next steps.", "📋", "Coordinates builders, tracks delivery, and keeps execution aligned."),
		agentTemplate("implementation-lead", "Build", "Build", "implement approved plans with minimal unnecessary churn", "Turn approved plans into working code in small, verifiable slices. Preserve repo conventions and surface blockers early.", "🛠️", "Implements approved plans with narrow, verifiable code changes."),
		agentTemplate("review-critic", "Review", "Review", "review correctness and risk", "Review code for behavior defects, correctness risks, missing tests, and regressions. Lead with actionable findings.", "🔍", "Finds correctness issues, risks, and missing verification."),
		agentTemplate("release-coordinator", "Release", "Release", "coordinate release, deployment, rollback, and environment status", "Coordinate releases, verify deployment readiness, track environment state, and escalate rollback risks.", "🚀", "Coordinates deployment readiness and release follow-through."),
		agentTemplate("qa-driver", "QA", "QA", "drive acceptance, regression testing, and bug creation", "Drive acceptance and regression testing. Convert failures into clear reproduction steps and actionable bug tasks.", "✅", "Runs acceptance thinking and turns failures into bug work."),
		agentTemplate("debug-investigator", "Debug", "Debug", "investigate runtime failures and diagnostics", "Investigate failures from logs, traces, and runtime state. Isolate likely causes before recommending fixes.", "🧭", "Investigates failures and narrows root causes."),
		agentTemplate("product-designer", "Designer", "Design", "design and iterate concrete product interfaces and visual artifacts", "Turn product intent and user feedback into concrete design artifacts. Inspect the existing product when relevant, create complete mockups instead of only describing them, and maintain a coherent visual and interaction direction across turns.", "🎨", "Creates and iterates product design drafts, mockups, and implementation handoffs."),
		agentTemplate("design-critic", "Design Critic", "Critic", "review product design and UX quality", "Evaluate UX quality, visual hierarchy, interaction clarity, and product fit. Call out concrete design risks.", "◐", "Reviews UX quality and design implementation risk."),
		agentTemplate("research-scan", "Research", "Research", "scan product, technical, and project context", "Search and synthesize relevant project context, prior decisions, tasks, and external research when needed.", "📚", "Finds and summarizes relevant context for decisions."),
		agentTemplate("architect", "Architect", "Arch", "shape technical architecture and integration contracts", "Define architecture, trade-offs, and integration contracts. Keep decisions stable and implementation-ready.", "🏗️", "Shapes architecture and implementation constraints."),
		agentTemplate("product-strategist", "Product", "Product", "clarify product goals, scope, and user value", "Clarify product goals, user value, scope boundaries, and prioritization trade-offs.", "🧩", "Clarifies product scope and user value."),
		agentTemplate("security-reviewer", "Security", "Sec", "review security, privacy, and permission boundaries", "Review authentication, authorization, data exposure, secrets, and privacy-sensitive behavior.", "🔐", "Reviews security and permission boundaries."),
		agentTemplate("data-analyst", "Data", "Data", "analyze data, metrics, and operational signals", "Analyze metrics, database state, and operational signals to support evidence-based decisions.", "📊", "Analyzes data and operational signals."),
		agentTemplate("frontend-specialist", "Frontend", "FE", "implement and review frontend experience", "Implement and review frontend UI with attention to workflow ergonomics, responsiveness, and existing design patterns.", "🖥️", "Builds and reviews frontend experience."),
		agentTemplate("backend-specialist", "Backend", "BE", "implement and review backend services and data flows", "Implement and review backend services, data models, migrations, and integration boundaries.", "⚙️", "Builds and reviews backend services and data flow."),
		agentTemplate("ops-sentinel", "Ops", "Ops", "monitor operational health and infrastructure risk", "Monitor operational health, infrastructure risk, deployment state, and incident follow-up.", "📡", "Tracks operational health and infrastructure risks."),
		agentTemplate("docs-writer", "Docs", "Docs", "write and maintain user and developer documentation", "Write clear documentation, release notes, and handoff material grounded in implemented behavior.", "✍️", "Maintains developer and user-facing documentation."),
	}
}

func agentTemplate(name, displayName, shortName, role, systemPrompt, emoji, summary string) AgentTemplate {
	return AgentTemplate{
		ID:           name,
		Name:         name,
		DisplayName:  displayName,
		ShortName:    shortName,
		Role:         role,
		SystemPrompt: systemPrompt,
		Emoji:        emoji,
		Summary:      summary,
		Config:       map[string]any{},
		Source:       "builtin",
	}
}

func residentAgentTemplateByID(id string) (AgentTemplate, bool) {
	for _, template := range residentAgentTemplates() {
		if template.ID == id || template.Name == id {
			return template, true
		}
	}
	return AgentTemplate{}, false
}

func defaultKarozAgentTemplate() AgentTemplate {
	return agentTemplate("karoz", "Karoz", "Karoz", "route project goals, manage resident capabilities, and coordinate groups", "Act as the project's control-plane coordinator. Receive user intent, inspect project-level state, reuse or provision the smallest suitable agent or group, and route business execution to that owner. Manage agents, groups, plans, tasks, conflicts, and escalations, but do not become the ordinary executor or WorkPlan owner. Communicate with grouped agents through their group coordinator.", "K", "Routes project work, manages groups, and coordinates execution without owning ordinary business tasks.")
}

func residentAgentTeams() []AgentTeam {
	return []AgentTeam{
		{
			ID:                  "build-lane",
			Name:                "Build Lane",
			Description:         "Default engineering lane: architect plans, builder implements, reviewer validates.",
			CoordinatorMemberID: "architect",
			Agents: []AgentTeamMember{
				{ID: "architect", Nickname: "architect", TemplateID: "architect", Role: "design phased work, ownership, dependencies, and handoffs", AcceptFrom: []string{"builder", "reviewer"}, ReportTo: []string{"builder", "reviewer"}, StartupOrder: 1},
				{ID: "builder", Nickname: "builder", TemplateID: "implementation-lead", Role: "implement approved slices", AcceptFrom: []string{"architect", "reviewer"}, ReportTo: []string{"reviewer"}, StartupOrder: 2, DependsOn: []string{"architect"}},
				{ID: "reviewer", Nickname: "reviewer", TemplateID: "review-critic", Role: "review correctness, release risk, regression coverage, and user-flow QA", AcceptFrom: []string{"architect", "builder"}, ReportTo: []string{"builder", "architect"}, StartupOrder: 3, DependsOn: []string{"architect", "builder"}},
			},
			Edges: []AgentTeamEdge{{From: "architect", To: "builder", Kind: "task"}, {From: "builder", To: "reviewer", Kind: "review"}, {From: "reviewer", To: "builder", Kind: "task"}, {From: "reviewer", To: "architect", Kind: "task"}},
		},
		{
			ID:                  "build-ultra",
			Name:                "Build Ultra",
			Description:         "High-parallelism build lane for large, explicitly splittable engineering work.",
			CoordinatorMemberID: "pmo",
			Agents: []AgentTeamMember{
				{ID: "pmo", Nickname: "pmo", TemplateID: "pmo-coordinator", Role: "coordinate builders, track progress, enforce delivery cadence", AcceptFrom: []string{"builder-1", "builder-2", "builder-3", "builder-4", "reviewer"}, ReportTo: []string{"builder-1", "builder-2", "builder-3", "builder-4", "reviewer"}, StartupOrder: 1},
				{ID: "builder-1", Nickname: "builder-1", TemplateID: "implementation-lead", Role: "implement PMO-assigned slice 1 without duplicating other builders", AcceptFrom: []string{"pmo", "reviewer"}, ReportTo: []string{"pmo", "reviewer"}, StartupOrder: 2, DependsOn: []string{"pmo"}},
				{ID: "builder-2", Nickname: "builder-2", TemplateID: "implementation-lead", Role: "implement PMO-assigned slice 2 without duplicating other builders", AcceptFrom: []string{"pmo", "reviewer"}, ReportTo: []string{"pmo", "reviewer"}, StartupOrder: 2, DependsOn: []string{"pmo"}},
				{ID: "builder-3", Nickname: "builder-3", TemplateID: "implementation-lead", Role: "implement PMO-assigned slice 3 without duplicating other builders", AcceptFrom: []string{"pmo", "reviewer"}, ReportTo: []string{"pmo", "reviewer"}, StartupOrder: 2, DependsOn: []string{"pmo"}},
				{ID: "builder-4", Nickname: "builder-4", TemplateID: "implementation-lead", Role: "implement PMO-assigned slice 4 without duplicating other builders", AcceptFrom: []string{"pmo", "reviewer"}, ReportTo: []string{"pmo", "reviewer"}, StartupOrder: 2, DependsOn: []string{"pmo"}},
				{ID: "reviewer", Nickname: "reviewer", TemplateID: "review-critic", Role: "review correctness and risk", AcceptFrom: []string{"pmo", "builder-1", "builder-2", "builder-3", "builder-4"}, ReportTo: []string{"pmo", "builder-1", "builder-2", "builder-3", "builder-4"}, StartupOrder: 3, DependsOn: []string{"pmo"}},
			},
			Edges: []AgentTeamEdge{
				{From: "pmo", To: "builder-1", Kind: "task"}, {From: "pmo", To: "builder-2", Kind: "task"}, {From: "pmo", To: "builder-3", Kind: "task"}, {From: "pmo", To: "builder-4", Kind: "task"},
				{From: "builder-1", To: "reviewer", Kind: "review"}, {From: "builder-2", To: "reviewer", Kind: "review"}, {From: "builder-3", To: "reviewer", Kind: "review"}, {From: "builder-4", To: "reviewer", Kind: "review"},
				{From: "reviewer", To: "builder-1", Kind: "task"}, {From: "reviewer", To: "builder-2", Kind: "task"}, {From: "reviewer", To: "builder-3", Kind: "task"}, {From: "reviewer", To: "builder-4", Kind: "task"},
				{From: "reviewer", To: "pmo", Kind: "report"}, {From: "builder-1", To: "pmo", Kind: "report"}, {From: "builder-2", To: "pmo", Kind: "report"}, {From: "builder-3", To: "pmo", Kind: "report"}, {From: "builder-4", To: "pmo", Kind: "report"},
			},
		},
		{
			ID:                  "product-discovery",
			Name:                "Product Discovery",
			Description:         "Pre-implementation lane that turns vague product intent into scoped technical plans.",
			CoordinatorMemberID: "facilitator",
			Agents: []AgentTeamMember{
				{ID: "facilitator", Nickname: "facilitator", TemplateID: "product-strategist", Role: "clarify the real problem and define a narrow wedge", AcceptFrom: []string{"challenger", "architect"}, ReportTo: []string{"challenger", "architect"}, StartupOrder: 1},
				{ID: "challenger", Nickname: "challenger", TemplateID: "research-scan", Role: "challenge scope and leverage", AcceptFrom: []string{"facilitator", "architect"}, ReportTo: []string{"architect"}, StartupOrder: 2, DependsOn: []string{"facilitator"}},
				{ID: "architect", Nickname: "architect", TemplateID: "architect", Role: "translate approved scope into an execution-ready technical plan, not code", AcceptFrom: []string{"facilitator", "challenger"}, StartupOrder: 3, DependsOn: []string{"facilitator", "challenger"}},
			},
			Edges: []AgentTeamEdge{{From: "facilitator", To: "challenger", Kind: "task"}, {From: "facilitator", To: "architect", Kind: "task"}, {From: "challenger", To: "architect", Kind: "review"}},
		},
		{
			ID:                  "ui-polish",
			Name:                "UI Polish",
			Description:         "UI execution lane: audit, focused frontend polish, and UX regression validation.",
			CoordinatorMemberID: "designer",
			Agents: []AgentTeamMember{
				{ID: "designer", Nickname: "designer", TemplateID: "product-designer", Role: "audit the interface and produce concrete design refinements", AcceptFrom: []string{"refiner", "qa"}, ReportTo: []string{"refiner", "qa"}, StartupOrder: 1},
				{ID: "refiner", Nickname: "refiner", TemplateID: "frontend-specialist", Role: "implement focused UI and interaction refinements", AcceptFrom: []string{"designer", "qa"}, ReportTo: []string{"qa"}, StartupOrder: 2, DependsOn: []string{"designer"}},
				{ID: "qa", Nickname: "qa", TemplateID: "qa-driver", Role: "validate polish changes and catch UX regressions", AcceptFrom: []string{"designer", "refiner"}, ReportTo: []string{"refiner"}, StartupOrder: 3, DependsOn: []string{"designer", "refiner"}},
			},
			Edges: []AgentTeamEdge{{From: "designer", To: "refiner", Kind: "task"}, {From: "designer", To: "qa", Kind: "review"}, {From: "refiner", To: "qa", Kind: "task"}, {From: "qa", To: "refiner", Kind: "review"}},
		},
		{
			ID:                  "verify-ship",
			Name:                "Verify Ship",
			Description:         "Post-change verification lane for an existing implementation before release or merge.",
			CoordinatorMemberID: "release",
			Agents: []AgentTeamMember{
				{ID: "qa", Nickname: "qa", TemplateID: "qa-driver", Role: "validate existing implementation user flows and capture reproducible defects", AcceptFrom: []string{"debugger", "release"}, ReportTo: []string{"debugger", "release"}, StartupOrder: 1},
				{ID: "debugger", Nickname: "debugger", TemplateID: "debug-investigator", Role: "identify root cause for hard failures", AcceptFrom: []string{"qa", "release"}, ReportTo: []string{"release"}, StartupOrder: 2, DependsOn: []string{"qa"}},
				{ID: "release", Nickname: "release", TemplateID: "release-coordinator", Role: "judge release readiness and blockers", AcceptFrom: []string{"qa", "debugger"}, StartupOrder: 3, DependsOn: []string{"qa", "debugger"}},
			},
			Edges: []AgentTeamEdge{{From: "qa", To: "debugger", Kind: "task"}, {From: "qa", To: "release", Kind: "task"}, {From: "debugger", To: "release", Kind: "review"}},
		},
	}
}

func residentAgentTeamByID(id string) (AgentTeam, bool) {
	for _, team := range residentAgentTeams() {
		if team.ID == id {
			return team, true
		}
	}
	return AgentTeam{}, false
}

func residentAgentIsDesign(agent Agent) bool {
	return agentdomain.IsDesign(agent)
}

func residentDesignAgentPrompt() string {
	return "\n### Design resident rules\n" +
		"- When the user asks for a design draft, mockup, screen, or HTML design artifact, produce a concrete artifact instead of only explaining.\n" +
		"- A workspace HTML mockup must be complete: include document structure, responsive CSS, and a complete body. Do not paste the full file into chat after writing it.\n" +
		"- When the user asks to match, refine, or reproduce the current project's UI, first inspect relevant repository entry points, components, routes, styles, or assets with repo_list, repo_read, and repo_search; do not rely only on an attached mockup.\n" +
		"- Inspect the smallest set of files needed to understand visual language, routes, and existing components, then stop scanning and draft.\n" +
		"- Set artifact_kind and a clear title on write_workspace_file. Rewriting the same path creates a new revision. When the draft is ready, call submit_artifact and hand its artifact_id to a reviewer.\n"
}

func residentAgentIsReviewer(agent Agent) bool {
	return agentdomain.IsReviewer(agent)
}

func residentReviewerAgentPrompt() string {
	return "\n### Artifact review rules\n" +
		"- When a handoff references artifact_ids, inspect each Artifact with get_artifact and show_preview before judging it.\n" +
		"- Use review_artifact to approve or request changes. Do not claim approval only in prose. Include concrete review notes.\n" +
		"- For implementation review, compare the result against the referenced approved Artifact revision and record a review_report Artifact when the findings need to persist.\n"
}

func residentAgentIsBuilder(agent Agent) bool {
	return agentdomain.IsBuilder(agent)
}

func residentBuilderAgentPrompt() string {
	return "\n### Artifact implementation rules\n" +
		"- Resolve referenced artifact_ids before implementation and treat approved design Artifacts as the acceptance contract.\n" +
		"- Do not create an implementation task from an unapproved design Artifact. Preserve Artifact IDs on the Task so a reviewer can compare the result.\n"
}
