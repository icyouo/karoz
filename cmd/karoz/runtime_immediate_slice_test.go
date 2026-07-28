package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCJKLexicalMemoryBehaviorTable(t *testing.T) {
	chinese := "决定：登录页的主按钮统一使用品牌蓝 #1A73E8，不再使用绿色。"
	english := "Decision: the login page button uses brand blue, not green."
	tests := []struct {
		name  string
		query string
		text  string
	}{
		{"Chinese natural question", "登录页的按钮用什么颜色", chinese},
		{"Chinese recall cue", "记得我们之前定的登录页按钮颜色吗", chinese},
		{"Chinese exact phrase", "登录页的主按钮", chinese},
		{"English unchanged", "what color is the login page button", english},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if score := memoryMatchScore(newMemoryLexicalQuery(tt.query), tt.text); score == 0 {
				t.Fatalf("query %q did not retrieve %q", tt.query, tt.text)
			}
		})
	}
	if score := memoryMatchScore(newMemoryLexicalQuery("登录系统设计"), "日志里提到了登录"); score != 0 {
		t.Fatalf("a single common CJK bigram produced a false positive: %d", score)
	}
	exact := memoryMatchScore(newMemoryLexicalQuery("登录页的主按钮"), chinese)
	partial := memoryMatchScore(newMemoryLexicalQuery("登录页按钮颜色"), chinese)
	if exact <= partial {
		t.Fatalf("exact phrase priority lost: exact=%d partial=%d", exact, partial)
	}
}

func TestCJKLexicalQuerySharedByRelevantMemoryAndArchive(t *testing.T) {
	a, project, agent := newMemoryGateTestApp(t)
	now := time.Now().UTC()
	key := projectAgentKey(project.ID, agent.ID)
	a.memories[key] = []AgentMemoryEntry{{
		ID: "cn-decision", ProjectID: project.ID, AgentID: agent.ID, Layer: "decision", State: "active",
		Summary: "登录页按钮颜色", Detail: "决定：登录页的主按钮统一使用品牌蓝 #1A73E8，不再使用绿色。",
		CreatedAt: now, UpdatedAt: now,
	}}
	a.archives[key] = []AgentArchiveMessage{{
		Seq: 7, Role: "assistant", Body: "决定：登录页的主按钮统一使用品牌蓝 #1A73E8，不再使用绿色。", CreatedAt: now,
	}}
	if got := a.relevantMemoriesFor(project.ID, agent.ID, "登录页的按钮用什么颜色", 5); len(got) != 1 || got[0].ID != "cn-decision" {
		t.Fatalf("relevant memories = %#v", got)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(a.searchArchive(project.ID, agent.ID, "记得我们之前定的登录页按钮颜色吗", 5)), &result); err != nil {
		t.Fatal(err)
	}
	if len(result["messages"].([]any)) != 1 {
		t.Fatalf("archive search result = %#v", result)
	}
}

func TestCodexReasoningReplayIsOpaqueOrderedAndTurnLocal(t *testing.T) {
	payload := []byte(`{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"ciphertext","summary":[]}}`)
	item, ok := codexSSEReasoningItem(payload)
	if !ok || item["encrypted_content"] != "ciphertext" {
		t.Fatalf("reasoning item = %#v, %t", item, ok)
	}
	call := map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "repo_read", "arguments": `{"path":"go.mod"}`}
	message := map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "checking"}}}
	secondReasoning := map[string]any{"id": "rs_2", "type": "reasoning", "encrypted_content": "ciphertext-2"}
	wire := newCodexStreamWire("/tmp/project", "current", "", "", nil)
	wire.appendAssistantTurn(residentStepOutput{CompletedItems: []map[string]any{item, message, call, secondReasoning}})
	wire.appendToolCall(codexToolCall{ID: "fc_1", CallID: "call_1", Name: "repo_read", Arguments: `{"path":"go.mod"}`})
	if wire.input[1]["type"] != "reasoning" || wire.input[2]["type"] != "message" || wire.input[3]["type"] != "function_call" || wire.input[4]["type"] != "reasoning" || len(wire.input) != 5 {
		t.Fatalf("same-turn replay order = %#v", wire.input)
	}
	next := newCodexStreamWire("/tmp/project", "next", "", "", nil)
	if len(next.input) != 1 {
		t.Fatalf("reasoning crossed user turn: %#v", next.input)
	}
}

func TestCodexCompletedOutputItemsPreserveSSEOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"one\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"repo_read\",\"arguments\":\"{}\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_2\",\"type\":\"reasoning\",\"encrypted_content\":\"two\"}}\n\n"))
	}))
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := streamCodexResponse(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CompletedItems) != 3 || result.CompletedItems[0]["id"] != "rs_1" || result.CompletedItems[1]["id"] != "fc_1" || result.CompletedItems[2]["id"] != "rs_2" {
		t.Fatalf("completed item order = %#v", result.CompletedItems)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].CallID != "call_1" {
		t.Fatalf("derived tool calls = %#v", result.ToolCalls)
	}
}

func TestProviderWiresUseNativeToolPairsAndTextFallback(t *testing.T) {
	success := true
	items := []AgentTranscriptItem{
		{Role: "assistant", Kind: "tool_call", ToolCallID: "call_1", ToolName: "repo_read", ToolArguments: `{"path":"go.mod"}`},
		{Role: "tool", Kind: "tool_result", ToolCallID: "call_1", ToolResult: "module example", ToolSuccess: &success},
		{Role: "tool", Kind: "tool_result", ToolResult: "legacy result"},
	}
	codex := codexTranscriptInput(items)
	if codex[0]["type"] != "function_call" || codex[1]["type"] != "function_call_output" || codex[2]["type"] != "message" {
		t.Fatalf("Codex history mapping = %#v", codex)
	}
	if _, present := codex[0]["id"]; present {
		t.Fatalf("persisted call_id was incorrectly reused as provider item id: %#v", codex[0])
	}
	claude := claudeTranscriptMessages(items)
	if content := claude[0]["content"].([]map[string]any); content[0]["type"] != "tool_use" {
		t.Fatalf("Claude tool call = %#v", claude[0])
	}
	if content := claude[1]["content"].([]map[string]any); content[0]["type"] != "tool_result" {
		t.Fatalf("Claude tool result = %#v", claude[1])
	} else if len(content) != 2 || !strings.Contains(content[1]["text"].(string), "legacy-unpaired") {
		t.Fatalf("legacy fallback = %#v", claude[1])
	}
}

func TestNativeTranscriptPairingRejectsCollisionsAndSupportsMultiCallGrouping(t *testing.T) {
	success := true
	items := []AgentTranscriptItem{
		{SessionID: "s", RunID: "r", Kind: "tool_call", ToolCallID: "a", ToolName: "repo_read", ToolArguments: `{}`},
		{SessionID: "s", RunID: "r", Kind: "tool_call", ToolCallID: "b", ToolName: "repo_search", ToolArguments: `{}`},
		{SessionID: "s", RunID: "r", Kind: "tool_result", ToolCallID: "a", ToolResult: "A", ToolSuccess: &success},
		{SessionID: "s", RunID: "r", Kind: "tool_result", ToolCallID: "b", ToolResult: "B", ToolSuccess: &success},
	}
	native := codexTranscriptInput(items)
	wantTypes := []string{"function_call", "function_call", "function_call_output", "function_call_output"}
	for i, want := range wantTypes {
		if native[i]["type"] != want {
			t.Fatalf("multi-call chronology[%d] = %#v, want %s", i, native, want)
		}
	}

	fallbackCases := map[string][]AgentTranscriptItem{
		"duplicate id": {
			{SessionID: "s", RunID: "r", Kind: "tool_call", ToolCallID: "x", ToolName: "one"},
			{SessionID: "s", RunID: "r", Kind: "tool_call", ToolCallID: "x", ToolName: "two"},
			{SessionID: "s", RunID: "r", Kind: "tool_result", ToolCallID: "x", ToolResult: "result"},
		},
		"cross run collision": {
			{SessionID: "s", RunID: "r1", Kind: "tool_call", ToolCallID: "x", ToolName: "one"},
			{SessionID: "s", RunID: "r2", Kind: "tool_result", ToolCallID: "x", ToolResult: "result"},
		},
		"orphan call and result": {
			{SessionID: "s", RunID: "r", Kind: "tool_result", ToolCallID: "before", ToolResult: "result"},
			{SessionID: "s", RunID: "r", Kind: "tool_call", ToolCallID: "after", ToolName: "one"},
		},
	}
	for name, transcript := range fallbackCases {
		t.Run(name, func(t *testing.T) {
			for _, item := range codexTranscriptInput(transcript) {
				if item["type"] != "message" {
					t.Fatalf("ambiguous/orphan history became native: %#v", item)
				}
			}
		})
	}
}

func TestNativeTranscriptPayloadUsesContextBounds(t *testing.T) {
	success := true
	items := []AgentTranscriptItem{
		{SessionID: "s", RunID: "r", Kind: "tool_call", ToolCallID: "large", ToolName: "repo_read", ToolArguments: strings.Repeat("a", 9000)},
		{SessionID: "s", RunID: "r", Kind: "tool_result", ToolCallID: "large", ToolResult: strings.Repeat("b", 9000), ToolSuccess: &success},
	}
	input := codexTranscriptInput(items)
	if arguments := input[0]["arguments"].(string); len(arguments) > 1400 || !json.Valid([]byte(arguments)) {
		t.Fatalf("native arguments exceeded context bound or became invalid JSON: %d %q", len(arguments), arguments)
	}
	if result := input[1]["output"].(string); len(result) > 2200 {
		t.Fatalf("native result exceeded context bound: %d", len(result))
	}
}

func TestProviderTranscriptBoundsToolPairsAtomically(t *testing.T) {
	success := true
	items := make([]AgentTranscriptItem, 0, residentTranscriptPromptMaxItems+2)
	items = append(items,
		AgentTranscriptItem{Role: "assistant", Kind: "tool_call", ToolCallID: "edge-call", ToolName: "repo_read", ToolArguments: `{}`},
		AgentTranscriptItem{Role: "tool", Kind: "tool_result", ToolCallID: "edge-call", ToolResult: "edge-result", ToolSuccess: &success},
	)
	for i := 0; i < residentTranscriptPromptMaxItems-1; i++ {
		items = append(items, AgentTranscriptItem{Role: "assistant", Kind: "message", Body: "newer"})
	}
	bounded := boundedProviderTranscript(items, "", "")
	if len(bounded) > residentTranscriptPromptMaxItems {
		t.Fatalf("item bound exceeded: %d", len(bounded))
	}
	for i, item := range bounded {
		if item.ToolCallID != "edge-call" {
			continue
		}
		t.Fatalf("boundary retained orphaned member of native pair at %d: %#v", i, bounded)
	}
}

func TestClaudeHistoryNormalizesRolesAndCurrentUserOnce(t *testing.T) {
	items := []AgentTranscriptItem{
		{Role: "user", Kind: "message", Body: "first"},
		{Role: "user", Kind: "message", Body: "second"},
		{Role: "tool", Kind: "tool_result", Body: "legacy orphan"},
		{Role: "assistant", Kind: "message", Body: "answer"},
	}
	wire := newClaudeStreamWire("/workspace", "current request", "", "", items)
	for i := 1; i < len(wire.messages); i++ {
		if wire.messages[i-1]["role"] == wire.messages[i]["role"] {
			t.Fatalf("adjacent Claude roles were not coalesced: %#v", wire.messages)
		}
	}
	raw, err := json.Marshal(wire.messages)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "current request") != 1 {
		t.Fatalf("current user input duplicated: %s", raw)
	}
}

func TestCurrentUserInputRemovedOnceWithOrWithoutRunID(t *testing.T) {
	tests := []struct {
		name  string
		items []AgentTranscriptItem
		runID string
	}{
		{
			name: "current item has run id",
			items: []AgentTranscriptItem{
				{Role: "user", Kind: "message", Body: "older"},
				{Role: "user", Kind: "message", Body: "repeat", RunID: "run-1"},
			},
			runID: "run-1",
		},
		{
			name: "visible item lacks run id",
			items: []AgentTranscriptItem{
				{Role: "assistant", Kind: "message", Body: "prior"},
				{Role: "user", Kind: "message", Body: "current"},
			},
			runID: "run-2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			history := boundedProviderTranscript(tt.items, tt.runID, tt.items[len(tt.items)-1].Body)
			wire := newCodexStreamWire("/workspace", tt.items[len(tt.items)-1].Body, "", "", history)
			raw, err := json.Marshal(wire.input)
			if err != nil {
				t.Fatal(err)
			}
			if count := strings.Count(string(raw), tt.items[len(tt.items)-1].Body); count != 1 {
				t.Fatalf("current input count = %d, want 1: %s", count, raw)
			}
		})
	}
}

func TestPromptTokenAccountingIncludesNativeTranscriptWithoutSubtractingItFromDynamic(t *testing.T) {
	transcript := []AgentTranscriptItem{{Role: "assistant", Kind: "message", Body: strings.Repeat("history ", 40)}}
	prompt := "stable-prefix\ndynamic suffix"
	total, stable, history, dynamic := residentPromptTokenAccounting(prompt, len("stable-prefix\n"), transcript)
	promptTokens := estimateResidentContextTextTokens(prompt)
	if history == 0 || total != promptTokens+history {
		t.Fatalf("total=%d prompt=%d history=%d", total, promptTokens, history)
	}
	if dynamic != promptTokens-stable {
		t.Fatalf("dynamic=%d, want prompt(%d)-stable(%d)", dynamic, promptTokens, stable)
	}
}

func TestResidentPromptStablePrefixAndSingleToolContract(t *testing.T) {
	a, project, agent := newMemoryGateTestApp(t)
	prefix := func(prompt string) string {
		index := strings.Index(prompt, "## Current chat turn type:")
		if index < 0 {
			t.Fatalf("dynamic boundary missing")
		}
		return prompt[:index]
	}
	ask := a.buildResidentAgentPrompt(project, agent, "plain request", "ask")
	plan := a.buildResidentAgentPrompt(project, agent, "use $some-skill", "plan")
	if prefix(ask) != prefix(plan) {
		t.Fatal("stable prefix changed across turn type or user skill text")
	}
	if strings.Count(ask, "### Provider-neutral tool contract") != 1 || strings.Contains(ask, "allowed_tools:") {
		t.Fatalf("tool contract duplicated schema material:\n%s", ask)
	}
	if strings.Contains(ask, "### Recent structured resident transcript") {
		t.Fatal("provider-native transcript was also duplicated into prompt prose")
	}
	if strings.Count(ask, "plain request") != 1 {
		t.Fatalf("current user input must appear once:\n%s", ask)
	}
}
