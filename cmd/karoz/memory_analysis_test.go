package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const memorySectionHeading = "### Relevant remembered facts and decisions"

// recordingMemoryAnalyzer stubs the side-channel analyzer and records calls.
type recordingMemoryAnalyzer struct {
	calls   int
	lastMsg string
	result  memoryGateResult
	err     error
	block   bool // block until ctx is done, then return ctx.Err()
}

func (r *recordingMemoryAnalyzer) analyze(ctx context.Context, _ Agent, msg string) (memoryGateResult, error) {
	r.calls++
	r.lastMsg = msg
	if r.block {
		<-ctx.Done()
		return memoryGateResult{}, ctx.Err()
	}
	return r.result, r.err
}

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

func TestMemoryGateRuleFilterSkipsAnalysis(t *testing.T) {
	a, project, agent := newMemoryGateTestApp(t)
	stub := &recordingMemoryAnalyzer{}
	a.memoryAnalyzer = stub.analyze
	skipMessages := []string{
		"/compact",                   // slash command
		"/postgres status please",    // slash command containing a memory keyword
		"Selected: Postgres cluster", // choice-answer submission
		"Selected: 1",                // numbered choice-answer submission
		"hi there",                   // trivially short (< 3 words)
		"好的",                         // trivially short Chinese ack
	}
	for _, msg := range skipMessages {
		query := a.memoryRetrievalQueryFor(context.Background(), agent, msg)
		if query != "" {
			t.Fatalf("skip message %q should disable retrieval, got query %q", msg, query)
		}
		if stub.calls != 0 {
			t.Fatalf("skip message %q triggered %d analyzer calls", msg, stub.calls)
		}
		prompt := a.buildResidentAgentPromptWithMemoryQuery(project, agent, msg, "ask", query)
		if strings.Contains(prompt, memorySectionHeading) {
			t.Fatalf("skip message %q should not inject memory even when keywords match:\n%s", msg, prompt)
		}
	}
}

func TestMemoryGateCueWordsForceRetrieval(t *testing.T) {
	a, project, agent := newMemoryGateTestApp(t)
	stub := &recordingMemoryAnalyzer{result: memoryGateResult{Retrieve: false}}
	a.memoryAnalyzer = stub.analyze
	cueMessages := []string{
		"记得",
		"请记住这个约定",
		"上次我们怎么配置的",
		"之前讨论过的方案是什么",
		"what did we decide previously about storage",
		"remember the deployment window",
		"Recall our last discussion",
		"LAST TIME we configured Postgres",
	}
	for _, msg := range cueMessages {
		query := a.memoryRetrievalQueryFor(context.Background(), agent, msg)
		if query != msg {
			t.Fatalf("cue message %q should force retrieval with the raw text, got %q", msg, query)
		}
	}
	if stub.calls != 0 {
		t.Fatalf("cue words must skip the analyzer, got %d calls", stub.calls)
	}
	// A cue message also forces retrieval when the stub would have said false.
	prompt := a.buildResidentAgentPromptWithMemoryQuery(project, agent, "LAST TIME we configured Postgres", "ask", "LAST TIME we configured Postgres")
	if !strings.Contains(promptSection(prompt, memorySectionHeading), "fact-pg") {
		t.Fatalf("cue-forced retrieval should inject the matching fact:\n%s", prompt)
	}
}

func TestMemoryGateFallbackOnAnalyzerError(t *testing.T) {
	a, project, agent := newMemoryGateTestApp(t)
	stub := &recordingMemoryAnalyzer{err: errors.New("boom")}
	a.memoryAnalyzer = stub.analyze
	msg := "How is Postgres configured?"
	query := a.memoryRetrievalQueryFor(context.Background(), agent, msg)
	if query != msg {
		t.Fatalf("analyzer error must fall back to the raw text query, got %q", query)
	}
	if stub.calls != 1 {
		t.Fatalf("expected 1 analyzer call, got %d", stub.calls)
	}
	prompt := a.buildResidentAgentPromptWithMemoryQuery(project, agent, msg, "ask", query)
	if !strings.Contains(promptSection(prompt, memorySectionHeading), "fact-pg") {
		t.Fatalf("fallback should keep keyword-based injection:\n%s", prompt)
	}
}

func TestMemoryGateFallbackOnTimeout(t *testing.T) {
	previous := memoryAnalysisTimeout
	memoryAnalysisTimeout = 50 * time.Millisecond
	t.Cleanup(func() { memoryAnalysisTimeout = previous })
	a, project, agent := newMemoryGateTestApp(t)
	stub := &recordingMemoryAnalyzer{block: true}
	a.memoryAnalyzer = stub.analyze
	msg := "How is Postgres configured?"
	start := time.Now()
	query := a.memoryRetrievalQueryFor(context.Background(), agent, msg)
	elapsed := time.Since(start)
	if query != msg {
		t.Fatalf("analyzer timeout must fall back to the raw text query, got %q", query)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout fallback took too long: %s", elapsed)
	}
	prompt := a.buildResidentAgentPromptWithMemoryQuery(project, agent, msg, "ask", query)
	if !strings.Contains(promptSection(prompt, memorySectionHeading), "fact-pg") {
		t.Fatalf("timeout fallback should keep keyword-based injection:\n%s", prompt)
	}
}

func TestMemoryGateRetrieveFalseSuppressesInjection(t *testing.T) {
	a, project, agent := newMemoryGateTestApp(t)
	stub := &recordingMemoryAnalyzer{result: memoryGateResult{Retrieve: false, Queries: []string{"postgres"}}}
	a.memoryAnalyzer = stub.analyze
	msg := "How is Postgres configured?"
	query := a.memoryRetrievalQueryFor(context.Background(), agent, msg)
	if query != "" {
		t.Fatalf("retrieve=false should disable retrieval, got query %q", query)
	}
	if stub.calls != 1 || stub.lastMsg != msg {
		t.Fatalf("analyzer calls=%d lastMsg=%q", stub.calls, stub.lastMsg)
	}
	prompt := a.buildResidentAgentPromptWithMemoryQuery(project, agent, msg, "ask", query)
	if strings.Contains(prompt, memorySectionHeading) {
		t.Fatalf("retrieve=false must suppress the memory section even when keywords match:\n%s", prompt)
	}
}

func TestMemoryGateGeneratedQueriesUnion(t *testing.T) {
	a, project, agent := newMemoryGateTestApp(t)
	msg := "zzz qqq www" // matches nothing by itself
	// Without generated queries, retrieve=true still finds nothing.
	a.memoryAnalyzer = (&recordingMemoryAnalyzer{result: memoryGateResult{Retrieve: true}}).analyze
	query := a.memoryRetrievalQueryFor(context.Background(), agent, msg)
	prompt := a.buildResidentAgentPromptWithMemoryQuery(project, agent, msg, "ask", query)
	if strings.Contains(prompt, memorySectionHeading) {
		t.Fatalf("raw text alone should not match any memory:\n%s", prompt)
	}
	// A fact matching only the generated query must be injected.
	stub := &recordingMemoryAnalyzer{result: memoryGateResult{Retrieve: true, Queries: []string{"Postgres configuration"}}}
	a.memoryAnalyzer = stub.analyze
	query = a.memoryRetrievalQueryFor(context.Background(), agent, msg)
	if !strings.Contains(query, msg) || !strings.Contains(query, "Postgres configuration") {
		t.Fatalf("query should union raw text and generated queries, got %q", query)
	}
	prompt = a.buildResidentAgentPromptWithMemoryQuery(project, agent, msg, "ask", query)
	section := promptSection(prompt, memorySectionHeading)
	if !strings.Contains(section, "fact-pg") {
		t.Fatalf("fact matching only the generated query should be injected:\n%s", prompt)
	}
}

func TestMemoryGateKillSwitch(t *testing.T) {
	for _, value := range []string{"0", "off", "false", "OFF", "False"} {
		t.Setenv("KAROZ_MEMORY_ANALYSIS", value)
		if !memoryAnalysisDisabled() {
			t.Fatalf("KAROZ_MEMORY_ANALYSIS=%q should disable analysis", value)
		}
	}
	t.Setenv("KAROZ_MEMORY_ANALYSIS", "0")
	a, project, agent := newMemoryGateTestApp(t)
	stub := &recordingMemoryAnalyzer{}
	a.memoryAnalyzer = stub.analyze
	msg := "How is Postgres configured?"
	query := a.memoryRetrievalQueryFor(context.Background(), agent, msg)
	if query != msg || stub.calls != 0 {
		t.Fatalf("kill switch should keep the keyword baseline without analysis: query=%q calls=%d", query, stub.calls)
	}
	prompt := a.buildResidentAgentPromptWithMemoryQuery(project, agent, msg, "ask", query)
	if !strings.Contains(promptSection(prompt, memorySectionHeading), "fact-pg") {
		t.Fatalf("kill switch baseline should keep keyword injection:\n%s", prompt)
	}
}

func TestParseMemoryGateResult(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantErr  bool
		retrieve bool
		queryLen int
	}{
		{name: "plain", text: `{"retrieve": true, "queries": ["postgres config"]}`, retrieve: true, queryLen: 1},
		{name: "fenced", text: "```json\n{\"retrieve\": false, \"queries\": []}\n```", retrieve: false, queryLen: 0},
		{name: "prose wrapped", text: `Here is the verdict: {"retrieve": true, "queries": ["a", "b"]} done`, retrieve: true, queryLen: 2},
		{name: "queries capped at four", text: `{"retrieve": true, "queries": ["a", "b", "c", "d", "e", ""]}`, retrieve: true, queryLen: 4},
		{name: "garbage", text: "I cannot decide", wantErr: true},
		{name: "broken json", text: `{"retrieve": yes}`, wantErr: true},
		{name: "empty", text: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseMemoryGateResult(tc.text)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.text)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.text, err)
			}
			if result.Retrieve != tc.retrieve || len(result.Queries) != tc.queryLen {
				t.Fatalf("parsed %+v, want retrieve=%v queries=%d", result, tc.retrieve, tc.queryLen)
			}
		})
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
			t.Fatalf("memoryWordCount(%q) = %d, want %d", text, got, want)
		}
	}
}
