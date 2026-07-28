package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	return invokeCodexDirectStreamWithBudget(ctx, workdir, prompt, model, thinkingEffort, nil, tools, callbacks, residentTurnBudgetFor("ask"), func(_ context.Context, call codexToolCall) (string, error) {
		return executeTool(call)
	})
}

func invokeCodexDirectStreamWithBudget(ctx context.Context, workdir, prompt, model, thinkingEffort string, transcript []AgentTranscriptItem, tools []map[string]any, callbacks AgentStreamCallbacks, budget ResidentTurnBudget, executeTool residentToolExecutor) error {
	return invokeResidentToolLoop(ctx, newCodexStreamWire(workdir, prompt, model, thinkingEffort, transcript), tools, callbacks, budget, executeTool)
}

// codexStreamWire adapts the Codex responses SSE protocol to the shared
// resident tool loop. It owns the responses API input items.
type codexStreamWire struct {
	input          []map[string]any
	model          string
	thinkingEffort string
	replayedCalls  map[string]int
}

func newCodexStreamWire(workdir, prompt, model, thinkingEffort string, transcript []AgentTranscriptItem) *codexStreamWire {
	input := codexTranscriptInput(transcript)
	input = append(input, codexMessage("user", prompt+"\n\nProject workspace: "+workdir))
	return &codexStreamWire{
		input:          input,
		model:          model,
		thinkingEffort: thinkingEffort,
		replayedCalls:  map[string]int{},
	}
}

func codexTranscriptInput(items []AgentTranscriptItem) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, unit := range projectResidentHistoryUnits(items) {
		if unit.Native {
			call, result := unit.Items[0], unit.Items[1]
			out = append(out, codexFunctionCallItem(codexToolCall{
				CallID: call.ToolCallID, Name: call.ToolName, Arguments: boundedTranscriptToolArguments(call),
			}))
			out = append(out, map[string]any{"type": "function_call_output", "call_id": call.ToolCallID, "output": boundedTranscriptToolResult(result)})
			continue
		}
		item := unit.Items[0]
		out = append(out, codexMessage(transcriptTextRole(item), boundedTranscriptText(item)))
	}
	return out
}

type residentHistoryUnit struct {
	Items  []AgentTranscriptItem
	Native bool
}

func projectResidentHistoryUnits(items []AgentTranscriptItem) []residentHistoryUnit {
	occurrences := map[string]int{}
	for _, item := range items {
		kind := firstNonEmpty(item.Kind, transcriptKindForMessage(item.Role, item.Intent))
		if kind != "tool_call" && kind != "tool_result" || strings.TrimSpace(item.ToolCallID) == "" {
			continue
		}
		if strings.TrimSpace(item.SessionID) == "" || strings.TrimSpace(item.RunID) == "" {
			continue
		}
		occurrences[residentHistoryPairKey(item)]++
	}
	units := make([]residentHistoryUnit, 0, len(items))
	for i := 0; i < len(items); i++ {
		call := items[i]
		if i+1 < len(items) && residentHistoryPairValid(call, items[i+1], occurrences) {
			units = append(units, residentHistoryUnit{Items: []AgentTranscriptItem{call, items[i+1]}, Native: true})
			i++
		} else {
			units = append(units, residentHistoryUnit{Items: []AgentTranscriptItem{call}})
		}
	}
	return units
}

func residentHistoryPairKey(item AgentTranscriptItem) string {
	return item.SessionID + "\x00" + item.RunID + "\x00" + item.ToolCallID
}

func residentHistoryPairValid(call, result AgentTranscriptItem, occurrences map[string]int) bool {
	return firstNonEmpty(call.Kind, transcriptKindForMessage(call.Role, call.Intent)) == "tool_call" &&
		firstNonEmpty(result.Kind, transcriptKindForMessage(result.Role, result.Intent)) == "tool_result" &&
		strings.TrimSpace(call.SessionID) != "" && call.SessionID == result.SessionID &&
		strings.TrimSpace(call.RunID) != "" && call.RunID == result.RunID &&
		strings.TrimSpace(call.ToolCallID) != "" && call.ToolCallID == result.ToolCallID &&
		result.Seq == call.Seq+1 &&
		strings.TrimSpace(call.ToolName) != "" &&
		occurrences[residentHistoryPairKey(call)] == 2
}

func boundedTranscriptToolArguments(item AgentTranscriptItem) string {
	arguments := firstNonEmpty(strings.TrimSpace(item.ToolArguments), strings.TrimSpace(item.Body))
	if len([]rune(arguments)) <= 1400 && json.Valid([]byte(arguments)) {
		return arguments
	}
	key := "_legacy_text"
	if json.Valid([]byte(arguments)) {
		key = "_truncated_json"
	}
	fallback, _ := json.Marshal(map[string]any{
		key:              limitString(arguments, 1200),
		"original_chars": len([]rune(arguments)),
	})
	return string(fallback)
}

func boundedTranscriptToolResult(item AgentTranscriptItem) string {
	return compactToolResultForPrompt(firstNonEmpty(strings.TrimSpace(item.ToolResult), strings.TrimSpace(item.Body)))
}

func boundedTranscriptText(item AgentTranscriptItem) string {
	return limitString(promptAgentTranscriptBody(item), residentTranscriptMessageMaxChars)
}

func transcriptTextRole(item AgentTranscriptItem) string {
	switch strings.ToLower(strings.TrimSpace(item.Role)) {
	case "assistant":
		return "assistant"
	case "user":
		return "user"
	default:
		return "system"
	}
}

func (w *codexStreamWire) step(ctx context.Context, tools []map[string]any, callbacks AgentStreamCallbacks) (residentStepOutput, []AgentInterrupt, error) {
	streamed, interrupts, err := streamCodexStep(ctx, w.input, w.model, w.thinkingEffort, tools, callbacks)
	return residentStepOutput{Text: streamed.Text, ToolCalls: codexToolCallsFromCompletedItems(streamed.CompletedItems), CompletedItems: streamed.CompletedItems}, interrupts, err
}

func (w *codexStreamWire) appendAssistantTurn(streamed residentStepOutput) {
	for _, item := range streamed.CompletedItems {
		itemType, _ := item["type"].(string)
		if itemType != "reasoning" && itemType != "function_call" && itemType != "tool_call" && itemType != "message" {
			continue
		}
		w.input = append(w.input, item)
		if itemType == "function_call" || itemType == "tool_call" {
			callID, _ := item["call_id"].(string)
			if strings.TrimSpace(callID) == "" {
				callID, _ = item["tool_call_id"].(string)
			}
			if strings.TrimSpace(callID) == "" {
				callID, _ = item["id"].(string)
			}
			w.replayedCalls[callID]++
		}
	}
}

func (w *codexStreamWire) appendInterruptTurn(streamed residentStepOutput, interrupts []AgentInterrupt) {
	if strings.TrimSpace(streamed.Text) != "" {
		w.input = append(w.input, codexMessage("assistant", streamed.Text))
	}
	w.input = append(w.input, codexMessage("user", renderAgentInterruptsForModel(interrupts)))
}

func (w *codexStreamWire) appendToolCall(call codexToolCall) {
	callID := firstNonEmpty(call.CallID, call.ID)
	if w.replayedCalls[callID] > 0 {
		w.replayedCalls[callID]--
		return
	}
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
			callbacks.OnDelta("The tool budget was reached and the concise final summary did not finish in time. Tool results have been preserved; retry the latest message to continue from the current project state.")
		}
		return nil
	}
	return nil
}

func compactCodexInputForFinal(input []map[string]any, maxChars int) []map[string]any {
	if len(input) <= 2 || maxChars <= 0 {
		return input
	}
	parent := make([]int, len(input))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(index int) int {
		if parent[index] != index {
			parent[index] = find(parent[index])
		}
		return parent[index]
	}
	union := func(left, right int) {
		left, right = find(left), find(right)
		if left != right {
			parent[right] = left
		}
	}
	callIndexes := map[string][]int{}
	outputIndexes := map[string][]int{}
	for i := 1; i < len(input); i++ {
		itemType, _ := input[i]["type"].(string)
		callID, _ := input[i]["call_id"].(string)
		switch itemType {
		case "function_call", "tool_call":
			callIndexes[callID] = append(callIndexes[callID], i)
		case "function_call_output":
			outputIndexes[callID] = append(outputIndexes[callID], i)
		}
	}
	invalid := map[int]bool{}
	for callID, calls := range callIndexes {
		outputs := outputIndexes[callID]
		if strings.TrimSpace(callID) == "" || len(calls) != 1 || len(outputs) != 1 || calls[0] >= outputs[0] {
			for _, index := range append(append([]int{}, calls...), outputs...) {
				invalid[index] = true
			}
			continue
		}
		union(calls[0], outputs[0])
	}
	for callID, outputs := range outputIndexes {
		if _, ok := callIndexes[callID]; !ok {
			for _, index := range outputs {
				invalid[index] = true
			}
		}
	}
	// A completed response batch may contain reasoning, assistant text, and
	// multiple calls before their outputs. Keep the whole batch connected so
	// tail compaction cannot retain a result while dropping its predecessor.
	for i := 1; i < len(input); {
		itemType, _ := input[i]["type"].(string)
		if itemType != "reasoning" && itemType != "function_call" && itemType != "tool_call" {
			i++
			continue
		}
		group := []int{i}
		j := i + 1
		seenOutput := false
		for ; j < len(input); j++ {
			nextType, _ := input[j]["type"].(string)
			role, _ := input[j]["role"].(string)
			if nextType == "reasoning" || nextType == "message" && (role == "user" || role == "developer") {
				break
			}
			if seenOutput && (nextType == "function_call" || nextType == "tool_call") {
				break
			}
			if nextType == "message" || nextType == "function_call" || nextType == "tool_call" || nextType == "function_call_output" {
				group = append(group, j)
			}
			if nextType == "function_call_output" {
				seenOutput = true
			}
		}
		for _, index := range group[1:] {
			union(group[0], index)
		}
		i = j
	}
	type compactGroup struct {
		indexes []int
		cost    int
		max     int
		invalid bool
	}
	groupsByRoot := map[int]*compactGroup{}
	for i := 1; i < len(input); i++ {
		root := find(i)
		group := groupsByRoot[root]
		if group == nil {
			group = &compactGroup{max: i}
			groupsByRoot[root] = group
		}
		raw, _ := json.Marshal(input[i])
		group.indexes = append(group.indexes, i)
		group.cost += len(raw)
		group.max = i
		group.invalid = group.invalid || invalid[i]
	}
	groups := make([]*compactGroup, 0, len(groupsByRoot))
	for _, group := range groupsByRoot {
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].max > groups[j].max })
	selected := make([]bool, len(input))
	used := 0
	omitted := false
	for _, group := range groups {
		if group.invalid {
			omitted = true
			continue
		}
		if used > 0 && used+group.cost > maxChars {
			omitted = true
			break
		}
		if group.cost > maxChars {
			omitted = true
			break
		}
		for _, index := range group.indexes {
			selected[index] = true
		}
		used += group.cost
	}
	out := make([]map[string]any, 0, len(input))
	out = append(out, input[0])
	if omitted {
		out = append(out, codexMessage("user", "[Earlier provider reasoning and tool evidence was omitted atomically to fit the final response context.]"))
	}
	for i := 1; i < len(input); i++ {
		if selected[i] {
			out = append(out, input[i])
		}
	}
	return out
}

type codexStreamResult struct {
	CompletedItems []map[string]any
	Text           string
}

func codexToolCallsFromCompletedItems(items []map[string]any) []codexToolCall {
	calls := make([]codexToolCall, 0)
	for _, item := range items {
		if call, ok := codexToolCallFromCompletedItem(item); ok {
			calls = append(calls, call)
		}
	}
	return calls
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
	var completedItems []map[string]any
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
		if item, ok := codexSSECompletedItem([]byte(payload)); ok {
			completedItems = append(completedItems, item)
		}
	}
	if err := scanner.Err(); err != nil {
		return codexStreamResult{CompletedItems: completedItems, Text: streamed.String()}, err
	}
	if streamed.Len() == 0 && strings.TrimSpace(finalText) != "" && onDelta != nil {
		onDelta(finalText)
	}
	text := streamed.String()
	if strings.TrimSpace(text) == "" {
		text = finalText
	}
	return codexStreamResult{CompletedItems: completedItems, Text: text}, nil
}
