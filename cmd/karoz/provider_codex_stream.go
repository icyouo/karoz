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
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxCodexToolOutputChars  = 12000
	maxResidentToolRounds    = 8
	residentToolPhaseTimeout = 90 * time.Second
	residentFinalTimeout     = 30 * time.Second
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
	parentCtx := ctx
	toolCtx, cancelTools := context.WithTimeout(parentCtx, residentToolPhaseTimeout)
	defer cancelTools()
	input := []map[string]any{codexMessage("user", prompt+"\n\nProject workspace: "+workdir)}
	toolRounds := 0
	limitReason := "resident tool loop limit"
toolLoop:
	for modelRound := 0; modelRound < 16 && toolRounds < maxResidentToolRounds; modelRound++ {
		streamed, interrupts, err := streamCodexStep(toolCtx, input, model, thinkingEffort, tools, callbacks)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && parentCtx.Err() == nil {
				limitReason = "resident tool phase time budget"
				break toolLoop
			}
			return err
		}
		if len(interrupts) > 0 {
			if strings.TrimSpace(streamed.Text) != "" {
				input = append(input, codexMessage("assistant", streamed.Text))
			}
			input = append(input, codexMessage("user", renderAgentInterruptsForModel(interrupts)))
			continue
		}
		if len(streamed.ToolCalls) == 0 {
			return nil
		}
		toolRounds++
		for _, call := range streamed.ToolCalls {
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
					if callbacks.OnInterrupt != nil {
						callbacks.OnInterrupt(interrupts)
					}
					input = append(input, codexMessage("user", renderAgentInterruptsForModel(interrupts)))
				}
			}
			if toolCtx.Err() != nil && parentCtx.Err() == nil {
				limitReason = "resident tool phase time budget"
				break toolLoop
			}
		}
	}
	if err := parentCtx.Err(); err != nil {
		return err
	}
	input = append(input, codexMessage("user", "You have reached the "+limitReason+". Stop using tools and provide the best concise answer now. Directly answer the latest user message using the evidence already collected, state any uncertainty, and name the next concrete step."))
	finalCtx, cancelFinal := context.WithTimeout(parentCtx, residentFinalTimeout)
	defer cancelFinal()
	httpReq, err := newCodexDirectRequestWithInput(finalCtx, compactCodexInputForFinal(input, 90000), model, thinkingEffort, nil)
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
	if callbacks.PollInterrupts != nil {
		if interrupts := callbacks.PollInterrupts(); len(interrupts) > 0 {
			if callbacks.OnInterrupt != nil {
				callbacks.OnInterrupt(interrupts)
			}
			return codexStreamResult{}, interrupts, nil
		}
	}
	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpReq, err := newCodexDirectRequestWithInput(stepCtx, input, model, thinkingEffort, tools)
	if err != nil {
		return codexStreamResult{}, nil, err
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
					interrupts := callbacks.PollInterrupts()
					if len(interrupts) == 0 {
						continue
					}
					interruptCh <- interrupts
					cancel()
					return
				}
			}
		}()
	}
	streamed, streamErr := streamCodexResponse(httpReq, callbacks.OnDelta)
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
