package main

import (
	"strings"
	"testing"
	"time"
)

func TestResidentPromptInjectsRelevantFactsAndDecisions(t *testing.T) {
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir(), DefaultBranch: "main"}
	a := &app{
		settings:      Settings{DataDir: t.TempDir()},
		agents:        map[string][]Agent{"p1": {{ID: "karoz", ProjectID: "p1", Nickname: "Karoz"}}},
		agentMessages: map[string][]AgentMessage{},
		agentSessions: map[string]AgentSessionState{},
		memories:      map[string][]AgentMemoryEntry{},
		archives:      map[string][]AgentArchiveMessage{},
		blackboard:    map[string][]AgentBlackboardEntry{},
		inbox:         map[string][]AgentInboxMessage{},
	}
	now := time.Now().UTC()
	archivedAt := now
	a.memories[projectAgentKey("p1", "karoz")] = []AgentMemoryEntry{
		{ID: "fact-pg", ProjectID: "p1", AgentID: "karoz", Layer: "fact", State: "active", Summary: "Postgres is the durable store", Detail: "All project state lives in Postgres 16.", CreatedAt: now, UpdatedAt: now},
		{ID: "decision-vue", ProjectID: "p1", AgentID: "karoz", Layer: "decision", State: "active", Summary: "Dashboard framework choice", Detail: "The dashboard uses Vue.", CreatedAt: now, UpdatedAt: now},
		{ID: "pending-pg", ProjectID: "p1", AgentID: "karoz", Layer: "pending", State: "active", Priority: 3, Summary: "Verify Postgres backup", Detail: "Postgres backups need verification.", CreatedAt: now, UpdatedAt: now},
		{ID: "archived-pg", ProjectID: "p1", AgentID: "karoz", Layer: "fact", State: "archived", Summary: "Old Postgres tuning notes", Detail: "Archived Postgres detail.", CreatedAt: now, UpdatedAt: now, ArchivedAt: &archivedAt},
	}

	prompt := a.buildResidentAgentPrompt(project, a.agents["p1"][0], "How is Postgres configured?", "ask")
	section := promptSection(prompt, "### Relevant remembered facts and decisions")
	if section == "" {
		t.Fatalf("prompt missing relevant memory section:\n%s", prompt)
	}
	if !strings.Contains(section, "[fact; id: fact-pg] Postgres is the durable store — All project state lives in Postgres 16.") {
		t.Fatalf("matching fact memory not rendered in section:\n%s", section)
	}
	if strings.Contains(section, "decision-vue") {
		t.Fatalf("non-matching memory leaked into section:\n%s", section)
	}
	if strings.Contains(section, "pending-pg") {
		t.Fatalf("pending-layer memory leaked into relevant section:\n%s", section)
	}
	if strings.Contains(section, "archived-pg") {
		t.Fatalf("archived memory leaked into section:\n%s", section)
	}

	unrelated := a.buildResidentAgentPrompt(project, a.agents["p1"][0], "zzz qqq", "ask")
	if strings.Contains(unrelated, "### Relevant remembered facts and decisions") {
		t.Fatalf("section should be skipped when no memory matches:\n%s", unrelated)
	}
}

func promptSection(prompt, heading string) string {
	start := strings.Index(prompt, heading)
	if start < 0 {
		return ""
	}
	rest := prompt[start:]
	if next := strings.Index(rest[len(heading):], "\n### "); next >= 0 {
		return rest[:len(heading)+next]
	}
	return rest
}
