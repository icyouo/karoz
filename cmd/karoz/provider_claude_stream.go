package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

type claudeStreamResult struct {
	Text             string
	ToolCalls        []codexToolCall
	AssistantContent []map[string]any
}

type claudeToolAccumulator struct {
	ID      string
	Name    string
	Initial any
	Partial strings.Builder
}

func invokeClaudeDirectStream(ctx context.Context, workdir, prompt, model, effort string, tools []map[string]any, callbacks AgentStreamCallbacks, executeTool func(codexToolCall) (string, error)) error {
	return invokeResidentToolLoop(ctx, newClaudeStreamWire(workdir, prompt, model, effort), tools, callbacks, executeTool)
}

// claudeStreamWire adapts the Claude messages SSE protocol to the shared
// resident tool loop. It owns the messages history and the per-round
// tool_result buffer that becomes the next user message.
type claudeStreamWire struct {
	messages []map[string]any
	model    string
	effort   string
	results  []map[string]any
}

func newClaudeStreamWire(workdir, prompt, model, effort string) *claudeStreamWire {
	return &claudeStreamWire{
		messages: []map[string]any{{"role": "user", "content": prompt + "\n\nProject workspace: " + workdir}},
		model:    model,
		effort:   effort,
	}
}

func (w *claudeStreamWire) step(ctx context.Context, tools []map[string]any, callbacks AgentStreamCallbacks) (residentStepOutput, []AgentInterrupt, error) {
	streamed, interrupts, err := streamClaudeStep(ctx, w.model, w.effort, w.messages, tools, callbacks)
	return residentStepOutput{Text: streamed.Text, ToolCalls: streamed.ToolCalls, AssistantContent: streamed.AssistantContent}, interrupts, err
}

func (w *claudeStreamWire) appendAssistantTurn(streamed residentStepOutput) {
	if len(streamed.AssistantContent) > 0 {
		w.messages = append(w.messages, map[string]any{"role": "assistant", "content": streamed.AssistantContent})
	}
}

func (w *claudeStreamWire) appendInterruptTurn(_ residentStepOutput, interrupts []AgentInterrupt) {
	w.messages = append(w.messages, map[string]any{"role": "user", "content": renderAgentInterruptsForModel(interrupts)})
}

func (w *claudeStreamWire) appendToolCall(codexToolCall) {}

func (w *claudeStreamWire) appendToolResult(call codexToolCall, result string, success bool) {
	w.results = append(w.results, map[string]any{"type": "tool_result", "tool_use_id": firstNonEmpty(call.CallID, call.ID), "content": result, "is_error": !success})
}

func (w *claudeStreamWire) appendInlineInterrupts(interrupts []AgentInterrupt) {
	w.results = append(w.results, map[string]any{"type": "text", "text": renderAgentInterruptsForModel(interrupts)})
}

func (w *claudeStreamWire) flushToolResults() {
	w.messages = append(w.messages, map[string]any{"role": "user", "content": w.results})
	w.results = nil
}

func (w *claudeStreamWire) appendLimitMessage(limitReason string) {
	w.messages = append(w.messages, map[string]any{"role": "user", "content": "You have reached the " + limitReason + ". Stop using tools and provide the best concise answer now."})
}

func (w *claudeStreamWire) finalize(parentCtx, finalCtx context.Context, callbacks AgentStreamCallbacks) error {
	_, _, err := streamClaudeStep(finalCtx, w.model, w.effort, w.messages, nil, callbacks)
	if err != nil && parentCtx.Err() != nil {
		return parentCtx.Err()
	}
	return err
}

func streamClaudeStep(ctx context.Context, model, effort string, messages []map[string]any, tools []map[string]any, callbacks AgentStreamCallbacks) (claudeStreamResult, []AgentInterrupt, error) {
	return runResidentStep(ctx, callbacks, func(stepCtx context.Context) (*http.Request, error) {
		return newClaudeRequest(stepCtx, model, effort, messages, tools)
	}, streamClaudeResponse)
}

func newClaudeRequest(ctx context.Context, model, effort string, messages []map[string]any, tools []map[string]any) (*http.Request, error) {
	key := strings.TrimSpace(firstNonEmpty(os.Getenv("KAROZ_ANTHROPIC_API_KEY"), os.Getenv("ANTHROPIC_API_KEY")))
	if key == "" {
		return nil, errors.New("ANTHROPIC_API_KEY is required for the Claude resident provider")
	}
	model = firstNonEmpty(strings.TrimSpace(model), "claude-sonnet-4-6")
	maxTokens := 32768
	if effort == "xhigh" || effort == "max" {
		maxTokens = 65536
	}
	payload := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"stream":     true,
		"system":     "You are Karoz, a project-scoped resident agent. Keep responses concise and actionable.",
		"messages":   messages,
	}
	if converted := claudeToolSpecs(tools); len(converted) > 0 {
		payload["tools"] = converted
	}
	if descriptor, ok := residentModelDescriptor("claude", model); ok && len(descriptor.EffortLevels) > 0 {
		payload["output_config"] = map[string]any{"effort": effort}
		payload["thinking"] = map[string]any{"type": "adaptive"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(getenv("KAROZ_ANTHROPIC_BASE_URL", "https://api.anthropic.com"), "/")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("x-api-key", key)
	request.Header.Set("anthropic-version", "2023-06-01")
	return request, nil
}

func claudeToolSpecs(tools []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, spec := range tools {
		name, _ := spec["name"].(string)
		if name == "" {
			if function, ok := spec["function"].(map[string]any); ok {
				name, _ = function["name"].(string)
				spec = function
			}
		}
		if name == "" {
			continue
		}
		description, _ := spec["description"].(string)
		schema := spec["parameters"]
		if schema == nil {
			schema = spec["input_schema"]
		}
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"name": name, "description": description, "input_schema": schema})
	}
	return out
}

func streamClaudeResponse(request *http.Request, onDelta func(string)) (claudeStreamResult, error) {
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return claudeStreamResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		return claudeStreamResult{}, fmt.Errorf("claude status %d: %s", response.StatusCode, strings.TrimSpace(string(raw)))
	}
	var textOut strings.Builder
	tools := map[int]*claudeToolAccumulator{}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
				Name  string `json:"name"`
				Input any    `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		switch event.Type {
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				tools[event.Index] = &claudeToolAccumulator{ID: event.ContentBlock.ID, Name: event.ContentBlock.Name, Initial: event.ContentBlock.Input}
			}
		case "content_block_delta":
			if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
				textOut.WriteString(event.Delta.Text)
				if onDelta != nil {
					onDelta(event.Delta.Text)
				}
			}
			if event.Delta.Type == "input_json_delta" {
				if tool := tools[event.Index]; tool != nil {
					tool.Partial.WriteString(event.Delta.PartialJSON)
				}
			}
		case "error":
			return claudeStreamResult{}, errors.New(firstNonEmpty(event.Error.Message, "Claude stream failed"))
		}
	}
	if err := scanner.Err(); err != nil {
		return claudeStreamResult{}, err
	}
	result := claudeStreamResult{Text: textOut.String()}
	if result.Text != "" {
		result.AssistantContent = append(result.AssistantContent, map[string]any{"type": "text", "text": result.Text})
	}
	for index := 0; index < len(tools)+64; index++ {
		tool := tools[index]
		if tool == nil {
			continue
		}
		input := tool.Initial
		if raw := strings.TrimSpace(tool.Partial.String()); raw != "" {
			var parsed any
			if json.Unmarshal([]byte(raw), &parsed) == nil {
				input = parsed
			}
		}
		if input == nil {
			input = map[string]any{}
		}
		arguments, _ := json.Marshal(input)
		call := codexToolCall{ID: tool.ID, CallID: tool.ID, Name: tool.Name, Arguments: string(arguments)}
		result.ToolCalls = append(result.ToolCalls, call)
		result.AssistantContent = append(result.AssistantContent, map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": input})
	}
	return result, nil
}
