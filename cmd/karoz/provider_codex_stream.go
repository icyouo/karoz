package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

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

func invokeCodexDirectStream(ctx context.Context, workdir, prompt, model, thinkingEffort string, tools []map[string]any, callbacks AgentStreamCallbacks, executeTool func(codexToolCall) (string, error)) error {
	return invokeResidentToolLoop(ctx, newCodexStreamWire(workdir, prompt, model, thinkingEffort), tools, callbacks, executeTool)
}

// codexStreamWire adapts the Codex responses SSE protocol to the shared
// resident tool loop. It owns the responses API input items.
type codexStreamWire struct {
	input          []map[string]any
	model          string
	thinkingEffort string
}

func newCodexStreamWire(workdir, prompt, model, thinkingEffort string) *codexStreamWire {
	return &codexStreamWire{
		input:          []map[string]any{codexMessage("user", prompt+"\n\nProject workspace: "+workdir)},
		model:          model,
		thinkingEffort: thinkingEffort,
	}
}

func (w *codexStreamWire) step(ctx context.Context, tools []map[string]any, callbacks AgentStreamCallbacks) (residentStepOutput, []AgentInterrupt, error) {
	streamed, interrupts, err := streamCodexStep(ctx, w.input, w.model, w.thinkingEffort, tools, callbacks)
	return residentStepOutput{Text: streamed.Text, ToolCalls: streamed.ToolCalls}, interrupts, err
}

func (w *codexStreamWire) appendAssistantTurn(residentStepOutput) {}

func (w *codexStreamWire) appendInterruptTurn(streamed residentStepOutput, interrupts []AgentInterrupt) {
	if strings.TrimSpace(streamed.Text) != "" {
		w.input = append(w.input, codexMessage("assistant", streamed.Text))
	}
	w.input = append(w.input, codexMessage("user", renderAgentInterruptsForModel(interrupts)))
}

func (w *codexStreamWire) appendToolCall(call codexToolCall) {
	w.input = append(w.input, codexFunctionCallItem(call))
}

func (w *codexStreamWire) appendToolResult(call codexToolCall, result string, _ bool) {
	w.input = append(w.input, map[string]any{
		"type":    "function_call_output",
		"call_id": firstNonEmpty(call.CallID, call.ID),
		"output":  result,
	})
}

func (w *codexStreamWire) appendInlineInterrupts(interrupts []AgentInterrupt) {
	w.input = append(w.input, codexMessage("user", renderAgentInterruptsForModel(interrupts)))
}

func (w *codexStreamWire) flushToolResults() {}

func (w *codexStreamWire) appendLimitMessage(limitReason string) {
	w.input = append(w.input, codexMessage("user", "You have reached the "+limitReason+". Stop using tools and provide the best concise answer now. Directly answer the latest user message using the evidence already collected, state any uncertainty, and name the next concrete step."))
}

func (w *codexStreamWire) finalize(parentCtx, finalCtx context.Context, callbacks AgentStreamCallbacks) error {
	httpReq, err := newCodexDirectRequestWithInput(finalCtx, compactCodexInputForFinal(w.input, 90000), w.model, w.thinkingEffort, nil)
	if err != nil {
		return err
	}
	if _, err := streamCodexResponse(httpReq, callbacks.OnDelta); err != nil {
		if parentCtx.Err() != nil {
			return parentCtx.Err()
		}
		if callbacks.OnDelta != nil {
			callbacks.OnDelta("本轮工具检索已达到运行预算，最终总结请求未能及时完成。已停止继续检索，现有操作结果均已保留；请重试最后一条消息，Agent 将优先使用项目 Task 与 WorkPlan 状态直接回答。")
		}
		return nil
	}
	return nil
}

func compactCodexInputForFinal(input []map[string]any, maxChars int) []map[string]any {
	if len(input) <= 2 || maxChars <= 0 {
		return input
	}
	keptReversed := make([]map[string]any, 0, len(input))
	used := 0
	for i := len(input) - 1; i >= 1; i-- {
		raw, _ := json.Marshal(input[i])
		if len(keptReversed) > 0 && used+len(raw) > maxChars {
			break
		}
		keptReversed = append(keptReversed, input[i])
		used += len(raw)
	}
	out := make([]map[string]any, 0, len(keptReversed)+1)
	out = append(out, input[0])
	for i := len(keptReversed) - 1; i >= 0; i-- {
		out = append(out, keptReversed[i])
	}
	return out
}

type codexStreamResult struct {
	ToolCalls []codexToolCall
	Text      string
}

func streamCodexStep(ctx context.Context, input []map[string]any, model, thinkingEffort string, tools []map[string]any, callbacks AgentStreamCallbacks) (codexStreamResult, []AgentInterrupt, error) {
	return runResidentStep(ctx, callbacks, func(stepCtx context.Context) (*http.Request, error) {
		return newCodexDirectRequestWithInput(stepCtx, input, model, thinkingEffort, tools)
	}, streamCodexResponse)
}

func newCodexDirectRequest(ctx context.Context, workdir, prompt string) (*http.Request, error) {
	return newCodexDirectRequestWithInput(ctx, []map[string]any{codexMessage("user", prompt+"\n\nProject workspace: "+workdir)}, "", "", nil)
}

func newCodexDirectRequestWithInput(ctx context.Context, input []map[string]any, model, thinkingEffort string, tools []map[string]any) (*http.Request, error) {
	credential, err := resolveCodexCredential(ctx)
	if err != nil {
		return nil, err
	}
	model = firstNonEmpty(strings.TrimSpace(model), getenv("KAROZ_CODEX_MODEL", "gpt-5.6-luna"))
	thinkingEffort = firstNonEmpty(strings.ToLower(strings.TrimSpace(thinkingEffort)), "medium")
	payload := map[string]any{
		"model":               model,
		"instructions":        "You are Karoz, a project-scoped resident agent. Keep responses concise and actionable.",
		"stream":              true,
		"store":               false,
		"parallel_tool_calls": true,
		"include":             []string{"reasoning.encrypted_content"},
		"reasoning":           map[string]any{"effort": thinkingEffort, "summary": "auto"},
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

func streamCodexResponse(httpReq *http.Request, onDelta func(string)) (codexStreamResult, error) {
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return codexStreamResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return codexStreamResult{}, fmt.Errorf("codex direct status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
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
		if delta != "" {
			streamed.WriteString(delta)
			if onDelta != nil {
				onDelta(delta)
			}
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
		return codexStreamResult{ToolCalls: toolCalls, Text: streamed.String()}, err
	}
	if streamed.Len() == 0 && strings.TrimSpace(finalText) != "" && onDelta != nil {
		onDelta(finalText)
	}
	text := streamed.String()
	if strings.TrimSpace(text) == "" {
		text = finalText
	}
	return codexStreamResult{ToolCalls: toolCalls, Text: text}, nil
}
