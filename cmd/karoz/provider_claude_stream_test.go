package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudeResidentProviderStreamsAndExecutesKarozTools(t *testing.T) {
	var requests atomic.Int32
	var firstPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if requests.Add(1) == 1 {
			firstPayload = payload
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"echo\",\"input\":{}}}\n\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"text\\\":\\\"hello\\\"}\"}}\n\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()
	t.Setenv("KAROZ_ANTHROPIC_BASE_URL", server.URL)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	var output strings.Builder
	var called bool
	err := invokeClaudeDirectStream(context.Background(), t.TempDir(), "use the tool", "claude-sonnet-4-6", "medium", []map[string]any{{"type": "function", "name": "echo", "description": "echo", "parameters": map[string]any{"type": "object"}}}, AgentStreamCallbacks{OnDelta: func(delta string) { output.WriteString(delta) }}, func(call codexToolCall) (string, error) {
		called = call.Name == "echo" && strings.Contains(call.Arguments, "hello")
		return `{"ok":true}`, nil
	})
	if err != nil || !called || output.String() != "done" || requests.Load() != 2 {
		t.Fatalf("err=%v called=%v output=%q requests=%d", err, called, output.String(), requests.Load())
	}
	if firstPayload["model"] != "claude-sonnet-4-6" || firstPayload["output_config"] == nil || firstPayload["tools"] == nil {
		t.Fatalf("first payload = %+v", firstPayload)
	}
}

func TestClaudeResidentProviderInterruptsAnActiveStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	t.Setenv("KAROZ_ANTHROPIC_BASE_URL", server.URL)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	start := time.Now()
	var polls atomic.Int32
	interrupt := AgentInterrupt{ID: "interrupt-1", Body: "change direction"}
	_, interrupts, err := streamClaudeStep(context.Background(), "claude-sonnet-4-6", "medium", []map[string]any{{"role": "user", "content": "wait"}}, nil, AgentStreamCallbacks{PollInterrupts: func() []AgentInterrupt {
		if polls.Add(1) > 2 {
			return []AgentInterrupt{interrupt}
		}
		return nil
	}})
	if err != nil || len(interrupts) != 1 || interrupts[0].ID != interrupt.ID || time.Since(start) > time.Second {
		t.Fatalf("err=%v interrupts=%+v elapsed=%s", err, interrupts, time.Since(start))
	}
}
