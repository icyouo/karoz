package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

const memorySectionHeading = "### Relevant remembered facts and decisions"

func newMemoryGateTestApp(t *testing.T) (*app, Project, Agent) {
	t.Helper()
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir(), DefaultBranch: "main"}
	agent := Agent{ID: "karoz", ProjectID: "p1", Nickname: "Karoz"}
	a := &app{
		settings:      Settings{DataDir: t.TempDir()},
		agents:        map[string][]Agent{"p1": {agent}},
		agentMessages: map[string][]AgentMessage{},
		agentSessions: map[string]AgentSessionState{},
		memories:      map[string][]AgentMemoryEntry{},
		archives:      map[string][]AgentArchiveMessage{},
		blackboard:    map[string][]AgentBlackboardEntry{},
		inbox:         map[string][]AgentInboxMessage{},
	}
	now := time.Now().UTC()
	a.memories[projectAgentKey("p1", "karoz")] = []AgentMemoryEntry{
		{ID: "fact-pg", ProjectID: "p1", AgentID: "karoz", Layer: "fact", State: "active", Summary: "Postgres is the durable store", Detail: "All project state lives in Postgres 16.", CreatedAt: now, UpdatedAt: now},
	}
	return a, project, agent
}

func TestMemoryRetrievalUsesRawTextAndCheapSkipRules(t *testing.T) {
	a, _, agent := newMemoryGateTestApp(t)
	for _, msg := range []string{"/compact", "/postgres status please", "Selected: Postgres cluster", "hi there", "好的"} {
		if query := a.memoryRetrievalQueryFor(context.Background(), agent, msg); query != "" {
			t.Fatalf("skip message %q should not retrieve, got %q", msg, query)
		}
	}
	for _, msg := range []string{"记得", "上次我们怎么配置的", "Recall our last discussion", "LAST TIME we configured Postgres"} {
		if query := a.memoryRetrievalQueryFor(context.Background(), agent, msg); query != msg {
			t.Fatalf("cue message %q query = %q", msg, query)
		}
	}
	const ordinary = "How is Postgres configured?"
	if query := a.memoryRetrievalQueryFor(context.Background(), agent, ordinary); query != ordinary {
		t.Fatalf("ordinary lexical query = %q", query)
	}
}

func TestMemoryRetrievalDoesNotDelayCurrentTurn(t *testing.T) {
	a, _, agent := newMemoryGateTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	query := a.memoryRetrievalQueryFor(ctx, agent, "How is Postgres configured?")
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("lexical retrieval waited on a side channel: %s", elapsed)
	}
	if query != "How is Postgres configured?" {
		t.Fatalf("cancelled context changed current-turn retrieval query: %q", query)
	}
}

func TestExplicitAndOrdinaryLexicalRetrievalInjectExpectedMemory(t *testing.T) {
	a, project, agent := newMemoryGateTestApp(t)
	for _, message := range []string{"LAST TIME we configured Postgres", "How is Postgres configured?"} {
		query := a.memoryRetrievalQueryFor(context.Background(), agent, message)
		prompt := a.buildResidentAgentPromptWithMemoryQuery(project, agent, message, "ask", query)
		if !strings.Contains(promptSection(prompt, memorySectionHeading), "fact-pg") {
			t.Fatalf("memory %q was not injected:\n%s", message, prompt)
		}
	}
}

func TestMemoryWordCount(t *testing.T) {
	cases := map[string]int{
		"hi there":          2,
		"ok":                1,
		"what is postgres":  3,
		"数据库":               3,
		"数据库呢":              4,
		"好的":                2,
		"数据库 connection 配置": 6,
	}
	for text, want := range cases {
		if got := memoryWordCount(text); got != want {
			t.Errorf("memoryWordCount(%q) = %d, want %d", text, got, want)
		}
	}
}
