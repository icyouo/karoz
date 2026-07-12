package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxCodexToolOutputChars = 900000

func invokeCodexDirect(ctx context.Context, workdir, prompt string) (CLI2APIResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	httpReq, err := newCodexDirectRequest(ctx, workdir, prompt)
	if err != nil {
		return CLI2APIResponse{}, err
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return CLI2APIResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return CLI2APIResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CLI2APIResponse{}, fmt.Errorf("codex direct status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	output := parseCodexSSEText(raw)
	if output == "" {
		output = strings.TrimSpace(string(raw))
	}
	return CLI2APIResponse{Provider: "codex-direct", Output: output}, nil
}

func invokeCodexDirectStream(ctx context.Context, workdir, prompt string, tools []map[string]any, callbacks AgentStreamCallbacks, executeTool func(codexToolCall) (string, error)) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	input := []map[string]any{codexMessage("user", prompt+"\n\nProject workspace: "+workdir)}
	for step := 0; step < 12; step++ {
		httpReq, err := newCodexDirectRequestWithInput(ctx, input, tools)
		if err != nil {
			return err
		}
		toolCalls, err := streamCodexResponse(httpReq, callbacks.OnDelta)
		if err != nil {
			return err
		}
		if len(toolCalls) == 0 {
			return nil
		}
		for _, call := range toolCalls {
			input = append(input, codexFunctionCallItem(call))
			if callbacks.OnToolStart != nil {
				callbacks.OnToolStart(call)
			}
			result, err := executeTool(call)
			success := err == nil && toolResultSuccess(result)
			if err != nil {
				result = `{"error":"tool_failed","message":` + strconv.Quote(err.Error()) + `}`
			}
			result = limitToolResultForModel(result)
			if callbacks.OnToolResult != nil {
				callbacks.OnToolResult(call, result, success)
			}
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": firstNonEmpty(call.CallID, call.ID),
				"output":  result,
			})
			if callbacks.PollInterrupts != nil {
				if interrupts := callbacks.PollInterrupts(); len(interrupts) > 0 {
					input = append(input, codexMessage("user", renderAgentInterruptsForModel(interrupts)))
				}
			}
		}
	}
	input = append(input, codexMessage("user", "You have reached the resident tool loop limit. Stop using tools and provide the best concise answer now, summarizing what happened, any completed actions, and the next concrete step."))
	httpReq, err := newCodexDirectRequestWithInput(ctx, input, nil)
	if err != nil {
		return err
	}
	if _, err := streamCodexResponse(httpReq, callbacks.OnDelta); err != nil {
		return err
	}
	return nil
}

func limitToolResultForModel(result string) string {
	result = strings.TrimSpace(result)
	if len(result) <= maxCodexToolOutputChars {
		return result
	}
	notice := fmt.Sprintf("\n\n[karoz truncated tool result: original_chars=%d limit_chars=%d; use narrower tool arguments if more detail is needed.]", len(result), maxCodexToolOutputChars)
	keep := maxCodexToolOutputChars - len(notice)
	if keep < 0 {
		keep = 0
	}
	return strings.TrimSpace(result[:keep]) + notice
}

func renderAgentInterruptsForModel(interrupts []AgentInterrupt) string {
	var b strings.Builder
	b.WriteString("User sent the following additional message")
	if len(interrupts) != 1 {
		b.WriteString("s")
	}
	b.WriteString(" while you were working. Treat them as the latest user input and adjust your next steps accordingly:\n")
	for _, item := range interrupts {
		b.WriteString("- ")
		if item.TurnType != "" {
			b.WriteString("[")
			b.WriteString(item.TurnType)
			b.WriteString("] ")
		}
		b.WriteString(limitString(item.Body, 4000))
		b.WriteString("\n")
	}
	return b.String()
}

func newCodexDirectRequest(ctx context.Context, workdir, prompt string) (*http.Request, error) {
	return newCodexDirectRequestWithInput(ctx, []map[string]any{codexMessage("user", prompt+"\n\nProject workspace: "+workdir)}, nil)
}

func newCodexDirectRequestWithInput(ctx context.Context, input []map[string]any, tools []map[string]any) (*http.Request, error) {
	credential, err := resolveCodexCredential(ctx)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"model":               getenv("KAROZ_CODEX_MODEL", "gpt-5.6-luna"),
		"instructions":        "You are Karoz, a project-scoped resident agent. Keep responses concise and actionable.",
		"stream":              true,
		"store":               false,
		"parallel_tool_calls": true,
		"include":             []string{"reasoning.encrypted_content"},
		"reasoning":           map[string]any{"effort": "medium", "summary": "auto"},
		"input":               input,
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(getenv("KAROZ_CODEX_BASE_URL", "https://chatgpt.com/backend-api/codex"), "/")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Connection", "Keep-Alive")
	httpReq.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	httpReq.Header.Set("User-Agent", "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)")
	httpReq.Header.Set("Originator", "codex-tui")
	httpReq.Header.Set("Session_id", randomID())
	if credential.AccountID != "" {
		httpReq.Header.Set("Chatgpt-Account-Id", credential.AccountID)
	}
	return httpReq, nil
}

func codexMessage(role, text string) map[string]any {
	apiRole := role
	contentType := "input_text"
	if role == "system" {
		apiRole = "developer"
	}
	if role == "assistant" {
		contentType = "output_text"
	}
	return map[string]any{
		"type": "message",
		"role": apiRole,
		"content": []map[string]string{{
			"type": contentType,
			"text": text,
		}},
	}
}

func codexFunctionCallItem(call codexToolCall) map[string]any {
	item := map[string]any{
		"type":      "function_call",
		"call_id":   firstNonEmpty(call.CallID, call.ID),
		"name":      call.Name,
		"arguments": call.Arguments,
	}
	if strings.TrimSpace(call.ID) != "" {
		item["id"] = call.ID
	}
	return item
}

func streamCodexResponse(httpReq *http.Request, onDelta func(string)) ([]codexToolCall, error) {
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return nil, fmt.Errorf("codex direct status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var toolCalls []codexToolCall
	var streamed strings.Builder
	var finalText string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		delta := codexSSEDelta([]byte(payload))
		if delta != "" && onDelta != nil {
			streamed.WriteString(delta)
			onDelta(delta)
		}
		if streamed.Len() == 0 {
			if text := codexSSETextSnapshot([]byte(payload)); text != "" {
				finalText = text
			}
		}
		if call, ok := codexSSEToolCall([]byte(payload)); ok {
			toolCalls = append(toolCalls, call)
		}
	}
	if err := scanner.Err(); err != nil {
		return toolCalls, err
	}
	if streamed.Len() == 0 && strings.TrimSpace(finalText) != "" && onDelta != nil {
		onDelta(finalText)
	}
	return toolCalls, nil
}
