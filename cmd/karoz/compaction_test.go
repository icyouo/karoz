package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestUTF8SafeTruncationHelpersInOSS(t *testing.T) {
	t.Parallel()

	sample := strings.Repeat("中文测试🙂", 60)
	for capBytes := 1; capBytes <= 40; capBytes++ {
		limited := limitString(sample, capBytes)
		if !utf8.ValidString(limited) {
			t.Fatalf("limitString(cap=%d) invalid UTF-8: %q", capBytes, limited)
		}
		truncated, did := truncateString(sample, capBytes)
		if !did || !utf8.ValidString(truncated) {
			t.Fatalf("truncateString(cap=%d) invalid UTF-8: %q", capBytes, truncated)
		}
	}

	longCJK := strings.Repeat("日志信息：", 200)
	capped := limitToolResultForBudget(longCJK, 240)
	if len(capped) > 240 || !utf8.ValidString(capped) {
		t.Fatalf("limitToolResultForBudget invalid len=%d valid=%v", len(capped), utf8.ValidString(capped))
	}
}

func TestScrubToolArgumentsForPromptPreservesPathAndBoundedTranscript(t *testing.T) {
	t.Parallel()

	raw, _ := json.Marshal(map[string]any{
		"path":    "internal/app.go",
		"content": strings.Repeat("package main\nfunc f() {}\n", 400),
	})
	scrubbed := scrubToolArgumentsForPrompt(string(raw), 1400)
	if !json.Valid([]byte(scrubbed)) {
		t.Fatalf("scrubbed arguments not valid JSON: %s", scrubbed)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(scrubbed), &decoded); err != nil {
		t.Fatalf("decode scrubbed args: %v", err)
	}
	if decoded["path"] != "internal/app.go" {
		t.Fatalf("lost path key in scrubbed args: %#v", decoded)
	}

	bounded := boundedTranscriptToolArguments(AgentTranscriptItem{
		Kind:          "tool_call",
		ToolName:      "write_workspace_file",
		ToolCallID:    "call-1",
		ToolArguments: string(raw),
	})
	if len([]rune(bounded)) > 1400 || !strings.Contains(bounded, "internal/app.go") {
		t.Fatalf("boundedTranscriptToolArguments failed: %s", bounded)
	}
}

func TestLiveToolResultCompactionOnCodexAndClaudeWires(t *testing.T) {
	t.Parallel()

	var b64 strings.Builder
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for i := 0; i < 1600; i++ {
		b64.WriteByte(alphabet[i%len(alphabet)])
	}
	raw, _ := json.Marshal(map[string]any{
		"status": "ok",
		"image":  "data:image/png;base64," + b64.String(),
	})

	call := codexToolCall{CallID: "c-img", Name: "device_screenshot"}

	codexWire := newCodexStreamWire("/tmp/ws", "take screenshot", "", "", nil)
	codexWire.appendToolResult(call, string(raw), true)
	last := codexWire.input[len(codexWire.input)-1].(map[string]any)
	out := last["output"].(string)
	if strings.Contains(out, b64.String()[:120]) || !strings.Contains(out, "omitted") {
		t.Fatalf("codex wire leaked raw image data URL: %s", out)
	}

	claudeWire := newClaudeStreamWire("/tmp/ws", "take screenshot", "", "", nil)
	claudeWire.appendToolResult(call, string(raw), true)
	if len(claudeWire.results) != 1 {
		t.Fatalf("expected 1 claude result, got %d", len(claudeWire.results))
	}
	cOut := claudeWire.results[0]["content"].(string)
	if strings.Contains(cOut, b64.String()[:120]) || !strings.Contains(cOut, "omitted") {
		t.Fatalf("claude wire leaked raw image data URL: %s", cOut)
	}
}

func TestCompactClaudeMessagesForFinalKeepsAtomicToolPairs(t *testing.T) {
	t.Parallel()

	messages := []map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "initial task prompt"}}},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "u1", "name": "read", "input": map[string]any{}}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "u1", "content": strings.Repeat("old evidence ", 300)}}},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "u2", "name": "test", "input": map[string]any{}}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "u2", "content": "recent test passed"}}},
		{"role": "user", "content": "You have reached the budget. Stop using tools."},
	}

	compacted := compactClaudeMessagesForFinal(messages, 900, 0)
	if len(compacted) >= len(messages) {
		t.Fatalf("expected claude messages to compact, got len=%d", len(compacted))
	}
	raw, _ := json.Marshal(compacted)
	s := string(raw)
	if !strings.Contains(s, "initial task prompt") || !strings.Contains(s, "recent test passed") || !strings.Contains(s, "Stop using tools") {
		t.Fatalf("compacted claude messages dropped required anchor/recent evidence: %s", s)
	}
	if strings.Contains(s, `"u1"`) && !strings.Contains(s, `"tool_result"`) {
		t.Fatalf("compacted claude messages orphaned tool_use u1: %s", s)
	}
	// Verify u2 assistant tool_use and user tool_result both survived atomically without u1.
	if !strings.Contains(s, `"id":"u2"`) || !strings.Contains(s, `"tool_use_id":"u2"`) {
		t.Fatalf("compacted claude messages failed to preserve atomic u2 pair: %s", s)
	}
}
