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
	"strconv"
	"strings"
	"sync"
	"time"
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
	parentCtx := ctx
	toolCtx, cancelTools := context.WithTimeout(parentCtx, residentToolPhaseTimeout)
	defer cancelTools()
	messages := []map[string]any{{"role": "user", "content": prompt + "\n\nProject workspace: " + workdir}}
	toolRounds := 0
	limitReason := "resident tool loop limit"
toolLoop:
	for modelRound := 0; modelRound < 16 && toolRounds < maxResidentToolRounds; modelRound++ {
		streamed, interrupts, err := streamClaudeStep(toolCtx, model, effort, messages, tools, callbacks)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && parentCtx.Err() == nil {
				limitReason = "resident tool phase time budget"
				break toolLoop
			}
			return err
		}
		if len(streamed.AssistantContent) > 0 {
			messages = append(messages, map[string]any{"role": "assistant", "content": streamed.AssistantContent})
		}
		if len(interrupts) > 0 {
			messages = append(messages, map[string]any{"role": "user", "content": renderAgentInterruptsForModel(interrupts)})
			continue
		}
		if len(streamed.ToolCalls) == 0 {
			return nil
		}
		toolRounds++
		results := make([]map[string]any, 0, len(streamed.ToolCalls))
		reachedLimit := false
		for _, call := range streamed.ToolCalls {
			if callbacks.OnToolStart != nil {
				callbacks.OnToolStart(call)
			}
			result, callErr := executeTool(call)
			success := callErr == nil && toolResultSuccess(result)
			if callErr != nil {
				result = `{"error":"tool_failed","message":` + strconv.Quote(callErr.Error()) + `}`
			}
			result = limitToolResultForModel(result)
			if callbacks.OnToolResult != nil {
				callbacks.OnToolResult(call, result, success)
			}
			results = append(results, map[string]any{"type": "tool_result", "tool_use_id": firstNonEmpty(call.CallID, call.ID), "content": result, "is_error": !success})
			if callbacks.PollInterrupts != nil {
				if pending := callbacks.PollInterrupts(); len(pending) > 0 {
					if callbacks.OnInterrupt != nil {
						callbacks.OnInterrupt(pending)
					}
					results = append(results, map[string]any{"type": "text", "text": renderAgentInterruptsForModel(pending)})
				}
			}
			if toolCtx.Err() != nil && parentCtx.Err() == nil {
				limitReason = "resident tool phase time budget"
				reachedLimit = true
				break
			}
		}
		messages = append(messages, map[string]any{"role": "user", "content": results})
		if reachedLimit {
			break toolLoop
		}
	}
	if err := parentCtx.Err(); err != nil {
		return err
	}
	messages = append(messages, map[string]any{"role": "user", "content": "You have reached the " + limitReason + ". Stop using tools and provide the best concise answer now."})
	finalCtx, cancelFinal := context.WithTimeout(parentCtx, residentFinalTimeout)
	defer cancelFinal()
	_, _, err := streamClaudeStep(finalCtx, model, effort, messages, nil, callbacks)
	if err != nil && parentCtx.Err() != nil {
		return parentCtx.Err()
	}
	return err
}

func streamClaudeStep(ctx context.Context, model, effort string, messages []map[string]any, tools []map[string]any, callbacks AgentStreamCallbacks) (claudeStreamResult, []AgentInterrupt, error) {
	if callbacks.PollInterrupts != nil {
		if pending := callbacks.PollInterrupts(); len(pending) > 0 {
			if callbacks.OnInterrupt != nil {
				callbacks.OnInterrupt(pending)
			}
			return claudeStreamResult{}, pending, nil
		}
	}
	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	request, err := newClaudeRequest(stepCtx, model, effort, messages, tools)
	if err != nil {
		return claudeStreamResult{}, nil, err
	}
	interruptCh := make(chan []AgentInterrupt, 1)
	stopPolling := make(chan struct{})
	var pollers sync.WaitGroup
	if callbacks.PollInterrupts != nil {
		pollers.Add(1)
		go func() {
			defer pollers.Done()
			ticker := time.NewTicker(40 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stopPolling:
					return
				case <-stepCtx.Done():
					return
				case <-ticker.C:
					if pending := callbacks.PollInterrupts(); len(pending) > 0 {
						interruptCh <- pending
						cancel()
						return
					}
				}
			}
		}()
	}
	streamed, streamErr := streamClaudeResponse(request, callbacks.OnDelta)
	close(stopPolling)
	pollers.Wait()
	var interrupts []AgentInterrupt
	select {
	case interrupts = <-interruptCh:
	default:
	}
	if len(interrupts) == 0 && callbacks.PollInterrupts != nil {
		interrupts = callbacks.PollInterrupts()
	}
	if len(interrupts) > 0 {
		if callbacks.OnInterrupt != nil {
			callbacks.OnInterrupt(interrupts)
		}
		if errors.Is(streamErr, context.Canceled) {
			streamErr = nil
		}
	}
	return streamed, interrupts, streamErr
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
