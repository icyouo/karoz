package main

import (
	"bytes"
	"encoding/json"
	"errors"
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

func TestLexicalTermsPreserveNonCJKUnicodeLettersAndNumbers(t *testing.T) {
	query := newMemoryLexicalQuery("café déjà Москва مرحبا ۱۲۳")
	want := []string{"café", "déjà", "москва", "مرحبا", "۱۲۳"}
	if len(query.asciiTerms) != len(want) {
		t.Fatalf("Unicode terms = %#v, want %#v", query.asciiTerms, want)
	}
	for i := range want {
		if query.asciiTerms[i] != want[i] {
			t.Fatalf("Unicode term[%d] = %q, want %q", i, query.asciiTerms[i], want[i])
		}
	}
	for _, tt := range []struct {
		query string
		text  string
	}{
		{"café déjà", "Le choix du café est déjà enregistré."},
		{"Москва проект", "Решение для проекта Москва сохранено."},
		{"مرحبا مشروع", "تم حفظ قرار مشروع مرحبا."},
		{"نسخة ۱۲۳", "النسخة المعتمدة هي ۱۲۳."},
	} {
		if score := memoryMatchScore(newMemoryLexicalQuery(tt.query), tt.text); score == 0 {
			t.Fatalf("non-CJK Unicode query %q did not match %q", tt.query, tt.text)
		}
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
	call := codexToolCall{ID: "fc_1", CallID: "call_1", Name: "repo_read", Arguments: `{"path":"go.mod"}`}
	outputItems := []codexResponseOutputItem{
		{Raw: json.RawMessage(`{"id":"rs_1","type":"reasoning","encrypted_content":"ciphertext","summary":[]}`)},
		{Raw: json.RawMessage(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]}`)},
		{Raw: json.RawMessage(`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"repo_read","arguments":"{\"path\":\"go.mod\"}"}`), ToolCall: &call},
		{Raw: json.RawMessage(`{"id":"rs_2","type":"reasoning","encrypted_content":"ciphertext-2"}`)},
	}
	wire := newCodexStreamWire("/tmp/project", "current", "", "", nil)
	wire.appendAssistantTurn(residentStepOutput{CodexOutputItems: outputItems})
	wire.appendToolCall(call)
	wire.appendToolResult(call, "result", true)
	if codexInputObject(wire.input[1])["type"] != "reasoning" || codexInputObject(wire.input[2])["type"] != "message" || codexInputObject(wire.input[3])["type"] != "function_call" || codexInputObject(wire.input[4])["type"] != "reasoning" || codexInputObject(wire.input[5])["type"] != "function_call_output" || len(wire.input) != 6 {
		t.Fatalf("same-turn replay order = %#v", wire.input)
	}
	if raw, ok := wire.input[1].(json.RawMessage); !ok || !bytes.Equal(raw, outputItems[0].Raw) {
		t.Fatalf("reasoning raw bytes changed: got=%q want=%q", raw, outputItems[0].Raw)
	}
	next := newCodexStreamWire("/tmp/project", "next", "", "", nil)
	if len(next.input) != 1 {
		t.Fatalf("reasoning crossed user turn: %#v", next.input)
	}
}

func TestCodexCompletedOutputItemsPreserveSSEOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"opaque+/=密文\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"repo_read\",\"arguments\":\"{}\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_2\",\"type\":\"function_call\",\"call_id\":\"call_2\",\"name\":\"repo_search\",\"arguments\":\"{}\"}}\n\n"))
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
	if len(result.OutputItems) != 3 || codexInputObject(result.OutputItems[0].Raw)["id"] != "rs_1" || codexInputObject(result.OutputItems[1].Raw)["id"] != "fc_1" || codexInputObject(result.OutputItems[2].Raw)["id"] != "fc_2" {
		t.Fatalf("completed item order = %#v", result.OutputItems)
	}
	if !bytes.Contains(result.OutputItems[0].Raw, []byte(`"encrypted_content":"opaque+/=密文"`)) {
		t.Fatalf("encrypted_content raw bytes changed: %s", result.OutputItems[0].Raw)
	}
	calls := codexToolCallsFromOutputItems(result.OutputItems)
	if len(calls) != 2 || calls[0].CallID != "call_1" || calls[1].CallID != "call_2" {
		t.Fatalf("derived tool calls = %#v", calls)
	}
	wire := newCodexStreamWire("/tmp/project", "current", "", "", nil)
	wire.appendAssistantTurn(residentStepOutput{CodexOutputItems: result.OutputItems})
	for _, call := range calls {
		wire.appendToolCall(call)
		wire.appendToolResult(call, "output-"+call.CallID, true)
	}
	if len(wire.input) != 6 ||
		codexInputMetadataForCompaction(wire.input[1]).Type != "reasoning" ||
		codexInputMetadataForCompaction(wire.input[2]).CallID != "call_1" ||
		codexInputMetadataForCompaction(wire.input[3]).CallID != "call_2" ||
		codexInputMetadataForCompaction(wire.input[4]).CallID != "call_1" ||
		codexInputMetadataForCompaction(wire.input[5]).CallID != "call_2" {
		t.Fatalf("two-call provider group/output ordering = %#v", wire.input)
	}
}

func TestCodexUnparseableCompletedItemIsExplicitAndNeverDispatched(t *testing.T) {
	const secret = "encrypted-secret-must-not-leak"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"would-dispatch\",\"name\":\"repo_read\",\"arguments\":\"{}\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"unknown_item\",\"encrypted_content\":\"" + secret + "\"}}\n\n"))
	}))
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL, nil)
	result, err := streamCodexResponse(request, nil)
	if err == nil || !errors.Is(err, errCodexCompletedItemInvalid) {
		t.Fatalf("malformed completed item error = %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "encrypted_content") {
		t.Fatalf("payload leaked through error: %v", err)
	}
	if len(result.OutputItems) != 0 || len(codexToolCallsFromOutputItems(result.OutputItems)) != 0 {
		t.Fatalf("tool dispatch survived malformed round: %#v", result)
	}
}

func TestCompactCodexFinalInputKeepsReasoningCallOutputAtomicAtBoundary(t *testing.T) {
	input := []any{
		codexMessage("user", "initial"),
		map[string]any{"type": "reasoning", "id": "reasoning-boundary", "encrypted_content": strings.Repeat("r", 1200)},
		map[string]any{"type": "function_call", "id": "provider-call", "call_id": "boundary-call", "name": "repo_read", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "boundary-call", "output": strings.Repeat("o", 1200)},
		codexMessage("user", "finalize now"),
	}
	compacted := compactCodexInputForFinal(input, 800)
	raw, err := json.Marshal(compacted)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"reasoning-boundary", "boundary-call", "provider-call"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("final compaction orphaned part of an oversized native group: %s", raw)
		}
	}
	if !strings.Contains(string(raw), "omitted atomically") || !strings.Contains(string(raw), "finalize now") {
		t.Fatalf("safe summary or final instruction missing: %s", raw)
	}

	kept := compactCodexInputForFinal(input, 6000)
	keptRaw, err := json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"reasoning-boundary", "boundary-call", "provider-call", "finalize now"} {
		if !strings.Contains(string(keptRaw), required) {
			t.Fatalf("complete native group was not retained with sufficient budget: %s", keptRaw)
		}
	}
}

func TestProviderWiresUseNativeToolPairsAndTextFallback(t *testing.T) {
	success := true
	items := []AgentTranscriptItem{
		{SessionID: "s", RunID: "r", Seq: 1, Role: "assistant", Kind: "tool_call", ToolCallID: "call_1", ToolName: "repo_read", ToolArguments: `{"path":"go.mod"}`},
		{SessionID: "s", RunID: "r", Seq: 2, Role: "tool", Kind: "tool_result", ToolCallID: "call_1", ToolResult: "module example", ToolSuccess: &success},
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
		{SessionID: "s", RunID: "r", Seq: 0, Role: "user", Kind: "message", Body: "before"},
		{SessionID: "s", RunID: "r", Seq: 1, Kind: "tool_call", ToolCallID: "a", ToolName: "repo_read", ToolArguments: `{}`},
		{SessionID: "s", RunID: "r", Seq: 2, Kind: "tool_result", ToolCallID: "a", ToolResult: "A", ToolSuccess: &success},
		{SessionID: "s", RunID: "r", Seq: 2, Role: "assistant", Kind: "message", Body: "between"},
		{SessionID: "s", RunID: "r", Seq: 3, Kind: "tool_call", ToolCallID: "b", ToolName: "repo_search", ToolArguments: `{}`},
		{SessionID: "s", RunID: "r", Seq: 4, Kind: "tool_result", ToolCallID: "b", ToolResult: "B", ToolSuccess: &success},
		{SessionID: "s", RunID: "r", Seq: 5, Role: "assistant", Kind: "message", Body: "after"},
	}
	native := codexTranscriptInput(items)
	wantTypes := []string{"message", "function_call", "function_call_output", "message", "function_call", "function_call_output", "message"}
	for i, want := range wantTypes {
		if native[i]["type"] != want {
			t.Fatalf("multi-call chronology[%d] = %#v, want %s", i, native, want)
		}
	}

	fallbackCases := map[string][]AgentTranscriptItem{
		"duplicate id": {
			{SessionID: "s", RunID: "r", Seq: 1, Kind: "tool_call", ToolCallID: "x", ToolName: "one", ToolArguments: `{}`},
			{SessionID: "s", RunID: "r", Seq: 2, Kind: "tool_result", ToolCallID: "x", ToolResult: "first"},
			{SessionID: "s", RunID: "r", Seq: 3, Kind: "tool_call", ToolCallID: "x", ToolName: "two", ToolArguments: `{}`},
			{SessionID: "s", RunID: "r", Seq: 4, Kind: "tool_result", ToolCallID: "x", ToolResult: "second"},
		},
		"cross run collision": {
			{SessionID: "s", RunID: "r1", Seq: 1, Kind: "tool_call", ToolCallID: "x", ToolName: "one", ToolArguments: `{}`},
			{SessionID: "s", RunID: "r2", Seq: 2, Kind: "tool_result", ToolCallID: "x", ToolResult: "result"},
		},
		"cross session collision": {
			{SessionID: "s1", RunID: "r", Seq: 1, Kind: "tool_call", ToolCallID: "x", ToolName: "one", ToolArguments: `{}`},
			{SessionID: "s2", RunID: "r", Seq: 2, Kind: "tool_result", ToolCallID: "x", ToolResult: "result"},
		},
		"orphan call and result": {
			{SessionID: "s", RunID: "r", Seq: 1, Kind: "tool_result", ToolCallID: "before", ToolResult: "result"},
			{SessionID: "s", RunID: "r", Seq: 2, Kind: "tool_call", ToolCallID: "after", ToolName: "one", ToolArguments: `{}`},
		},
		"malformed arguments": {
			{SessionID: "s", RunID: "r", Seq: 1, Kind: "tool_call", ToolCallID: "bad-args", ToolName: "one", ToolArguments: `{"broken":`},
			{SessionID: "s", RunID: "r", Seq: 2, Kind: "tool_result", ToolCallID: "bad-args", ToolResult: "result"},
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

func TestMalformedNativeArgumentsFallbackForCodexAndClaude(t *testing.T) {
	for name, arguments := range map[string]string{
		"empty":     "",
		"scalar":    `"not-an-object"`,
		"array":     `["not","an","object"]`,
		"malformed": `{"broken":`,
	} {
		t.Run(name, func(t *testing.T) {
			items := []AgentTranscriptItem{
				{SessionID: "s", RunID: "r", Seq: 1, Role: "assistant", Kind: "tool_call", ToolCallID: "bad", ToolName: "repo_read", ToolArguments: arguments},
				{SessionID: "s", RunID: "r", Seq: 2, Role: "tool", Kind: "tool_result", ToolCallID: "bad", ToolResult: "result"},
			}
			for _, item := range codexTranscriptInput(items) {
				if item["type"] != "message" {
					t.Fatalf("Codex malformed arguments became native: %#v", item)
				}
			}
			for _, message := range claudeTranscriptMessages(items) {
				for _, content := range message["content"].([]map[string]any) {
					if content["type"] == "tool_use" || content["type"] == "tool_result" {
						t.Fatalf("Claude malformed arguments became native: %#v", message)
					}
				}
			}
		})
	}
}

func TestOversizedNativeArgumentsDegradeAtomicallyForBothProviders(t *testing.T) {
	success := true
	oversizedArguments := `{"data":"` + strings.Repeat("a", 9000) + `"}`
	oversizedResult := strings.Repeat("b", 9000)
	items := []AgentTranscriptItem{
		{SessionID: "s", RunID: "r", Seq: 1, Kind: "tool_call", ToolCallID: "large", ToolName: "repo_read", ToolArguments: oversizedArguments},
		{SessionID: "s", RunID: "r", Seq: 2, Kind: "tool_result", ToolCallID: "large", ToolResult: oversizedResult, ToolSuccess: &success},
	}
	codex := codexTranscriptInput(items)
	for _, input := range codex {
		if input["type"] != "message" {
			t.Fatalf("oversized Codex arguments remained native: %#v", input)
		}
	}
	claude := claudeTranscriptMessages(items)
	for _, message := range claude {
		for _, content := range message["content"].([]map[string]any) {
			if content["type"] == "tool_use" || content["type"] == "tool_result" {
				t.Fatalf("oversized Claude arguments remained native: %#v", message)
			}
		}
	}
	for provider, payload := range map[string]any{"codex": codex, "claude": claude} {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) >= len(oversizedArguments)+len(oversizedResult) ||
			bytes.Contains(raw, []byte(strings.Repeat("a", 2000))) ||
			bytes.Contains(raw, []byte(strings.Repeat("b", 5000))) {
			t.Fatalf("%s fallback payload remained unbounded: bytes=%d", provider, len(raw))
		}
	}
	withCurrent := append(append([]AgentTranscriptItem{}, items...), AgentTranscriptItem{
		ID: "current", SessionID: "s", RunID: "current-run", Seq: 3, Role: "user", Kind: "message", Body: "current",
	})
	history, err := boundedProviderTranscript(withCurrent, "current-run", "current", 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := estimateModelBoundTranscriptTokens(history); got >= estimateResidentContextTextTokens(oversizedArguments+oversizedResult) {
		t.Fatalf("context meter counted unbounded native payload: bounded=%d raw=%d", got, estimateResidentContextTextTokens(oversizedArguments+oversizedResult))
	}
}

func TestProviderTranscriptBoundsToolPairsAtomically(t *testing.T) {
	success := true
	items := make([]AgentTranscriptItem, 0, residentTranscriptPromptMaxItems+2)
	items = append(items,
		AgentTranscriptItem{SessionID: "s", RunID: "r", Seq: 1, Role: "assistant", Kind: "tool_call", ToolCallID: "edge-call", ToolName: "repo_read", ToolArguments: `{}`},
		AgentTranscriptItem{SessionID: "s", RunID: "r", Seq: 2, Role: "tool", Kind: "tool_result", ToolCallID: "edge-call", ToolResult: "edge-result", ToolSuccess: &success},
	)
	for i := 0; i < residentTranscriptPromptMaxItems-1; i++ {
		items = append(items, AgentTranscriptItem{Role: "assistant", Kind: "message", Body: "newer"})
	}
	items = append(items, AgentTranscriptItem{ID: "current", Seq: 1000, RunID: "current-run", Role: "user", Kind: "message", Body: "current"})
	bounded, err := boundedProviderTranscript(items, "current-run", "current", 1000)
	if err != nil {
		t.Fatal(err)
	}
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

func TestDuplicateIDOutsideProviderCutoffInvalidatesRetainedOccurrence(t *testing.T) {
	success := true
	items := []AgentTranscriptItem{
		{SessionID: "s", RunID: "r", Seq: 1, Kind: "tool_call", ToolCallID: "duplicate", ToolName: "repo_read", ToolArguments: `{}`},
		{SessionID: "s", RunID: "r", Seq: 2, Kind: "tool_result", ToolCallID: "duplicate", ToolResult: "old", ToolSuccess: &success},
	}
	for i := 0; i < residentTranscriptPromptMaxItems-2; i++ {
		items = append(items, AgentTranscriptItem{SessionID: "s", RunID: "r", Seq: int64(i + 3), Role: "assistant", Kind: "message", Body: "middle"})
	}
	items = append(items,
		AgentTranscriptItem{SessionID: "s", RunID: "r", Seq: 100, Kind: "tool_call", ToolCallID: "duplicate", ToolName: "repo_read", ToolArguments: `{}`},
		AgentTranscriptItem{SessionID: "s", RunID: "r", Seq: 101, Kind: "tool_result", ToolCallID: "duplicate", ToolResult: "new", ToolSuccess: &success},
		AgentTranscriptItem{ID: "current", SessionID: "s", RunID: "current-run", Seq: 102, Role: "user", Kind: "message", Body: "current"},
	)
	bounded, err := boundedProviderTranscript(items, "current-run", "current", 102)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != residentTranscriptPromptMaxItems {
		t.Fatalf("bounded item count = %d", len(bounded))
	}
	for _, input := range codexTranscriptInput(bounded) {
		if input["type"] == "function_call" || input["type"] == "function_call_output" {
			t.Fatalf("duplicate outside cutoff allowed retained native pair: %#v", input)
		}
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
			name: "scheduled current item has run id",
			items: []AgentTranscriptItem{
				{ID: "older", Seq: 1, Role: "user", Kind: "message", Body: "older"},
				{ID: "scheduled", Seq: 2, Role: "user", Kind: "message", Intent: "scheduled_plan_event_input", Body: "repeat", RunID: "run-1"},
			},
			runID: "run-1",
		},
		{
			name: "direct visible item lacks run id",
			items: []AgentTranscriptItem{
				{ID: "prior", Seq: 1, Role: "assistant", Kind: "message", Body: "prior"},
				{ID: "direct", Seq: 2, Role: "user", Kind: "message", Body: "current"},
			},
			runID: "run-2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := tt.items[len(tt.items)-1]
			history, err := boundedProviderTranscript(tt.items, tt.runID, current.ID, current.Seq)
			if err != nil {
				t.Fatal(err)
			}
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
	if !strings.Contains(prefix(ask), "### Resident identity and durable role") || !strings.Contains(prefix(ask), agent.Role) {
		t.Fatalf("stable prefix omitted durable identity/role:\n%s", prefix(ask))
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
