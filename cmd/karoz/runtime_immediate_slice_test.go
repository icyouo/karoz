package main

import (
	"encoding/json"
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
	wire := newCodexStreamWire("/tmp/project", "current", "", "", nil)
	wire.appendAssistantTurn(residentStepOutput{ReasoningItems: []map[string]any{item}})
	wire.appendToolCall(codexToolCall{ID: "fc_1", CallID: "call_1", Name: "repo_read", Arguments: `{"path":"go.mod"}`})
	if wire.input[1]["type"] != "reasoning" || wire.input[2]["type"] != "function_call" {
		t.Fatalf("same-turn replay order = %#v", wire.input)
	}
	next := newCodexStreamWire("/tmp/project", "next", "", "", nil)
	if len(next.input) != 1 {
		t.Fatalf("reasoning crossed user turn: %#v", next.input)
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
	claude := claudeTranscriptMessages(items)
	if content := claude[0]["content"].([]map[string]any); content[0]["type"] != "tool_use" {
		t.Fatalf("Claude tool call = %#v", claude[0])
	}
	if content := claude[1]["content"].([]map[string]any); content[0]["type"] != "tool_result" {
		t.Fatalf("Claude tool result = %#v", claude[1])
	}
	if !strings.Contains(claude[2]["content"].(string), "legacy-unpaired") {
		t.Fatalf("legacy fallback = %#v", claude[2])
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
}
