package main

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

const (
	residentTranscriptPromptMaxItems  = 50
	residentTranscriptPromptMaxChars  = 24_000
	residentTranscriptMessageMaxChars = 5_000
	residentContextRoleMaxChars       = 64
	residentContextIntentMaxChars     = 256
	residentContextIDMaxChars         = 256
	// The frontend deliberately uses an inexpensive character heuristic rather
	// than provider tokenization. Keep the same heuristic for the model-bound
	// transcript and allow one rounding token when comparing implementations.
	residentContextEstimatorTolerance = 1
)

type agentTranscriptPromptLine struct {
	Role string
	Body string
}

func renderAgentTranscriptDelta(items []AgentTranscriptItem, maxItems, maxChars int) []agentTranscriptPromptLine {
	context := compactTranscriptForModelContext(items, maxItems, maxChars)
	lines := make([]agentTranscriptPromptLine, 0, len(context))
	for _, item := range context {
		lines = append(lines, agentTranscriptPromptLine{Role: firstNonEmpty(item.Role, "system"), Body: item.Body})
	}
	return lines
}

func promptAgentTranscriptBody(item AgentTranscriptItem) string {
	kind := firstNonEmpty(strings.TrimSpace(item.Kind), transcriptKindForMessage(item.Role, item.Intent))
	switch kind {
	case "tool_call":
		name := firstNonEmpty(strings.TrimSpace(item.ToolName), strings.TrimSpace(item.Intent), "unknown")
		callID := firstNonEmpty(strings.TrimSpace(item.ToolCallID), "legacy-unpaired")
		arguments := firstNonEmpty(strings.TrimSpace(item.ToolArguments), strings.TrimSpace(item.Body))
		return "tool_call id=" + callID + " name=" + name + " arguments=" + limitString(arguments, 1400)
	case "tool_result":
		name := firstNonEmpty(strings.TrimSpace(item.ToolName), strings.TrimSpace(item.Intent), "unknown")
		callID := firstNonEmpty(strings.TrimSpace(item.ToolCallID), "legacy-unpaired")
		result := firstNonEmpty(strings.TrimSpace(item.ToolResult), strings.TrimSpace(item.Body))
		success := "unknown"
		if item.ToolSuccess != nil {
			success = fmt.Sprintf("%t", *item.ToolSuccess)
		}
		return "tool_result call_id=" + callID + " name=" + name + " success=" + success + " result=" + compactToolResultForPrompt(result)
	case "interrupt":
		return "interrupt: " + limitString(item.Body, 5000)
	case "status":
		return "status: " + limitString(item.Body, 2400)
	default:
		return promptAgentMessageBody(AgentMessage{Role: item.Role, Body: item.Body})
	}
}

// modelBoundTranscriptCounterText mirrors the bounded projection delivered to
// the Studio context meter. It uses the same transcript-body rendering and
// item/character selection as the resident prompt, without runtime rules or a
// currently typed draft.
func modelBoundTranscriptCounterText(items []AgentTranscriptItem) string {
	return modelContextCounterText(compactTranscriptForContextCounter(items))
}

func modelContextCounterText(items []AgentContextMessage) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if content := contextCounterRecord(item); content != "" {
			parts = append(parts, content)
		}
	}
	return strings.Join(parts, "\n")
}

// compactTranscriptForContextCounter returns the exact bounded transcript
// window the resident prompt can render. In particular, model-only scheduled
// inputs are transformed through promptAgentTranscriptBody before counting, so
// their persisted raw plan JSON cannot make the API or meter exceed the
// model's five-thousand-character message limit.
func compactTranscriptForContextCounter(items []AgentTranscriptItem) []AgentContextMessage {
	return compactTranscriptForModelContext(items, residentTranscriptPromptMaxItems, residentTranscriptPromptMaxChars)
}

// compactTranscriptForModelContext is shared by prompt rendering and the API
// projection. Its record cost is exactly the browser context-record cost, so
// an already-projected response will not lose additional history when the
// client applies its own defensive compaction.
func compactTranscriptForModelContext(items []AgentTranscriptItem, maxItems, maxChars int) []AgentContextMessage {
	if maxItems <= 0 {
		maxItems = residentTranscriptPromptMaxItems
	}
	if maxChars <= 0 {
		maxChars = residentTranscriptPromptMaxChars
	}
	var reversed []AgentContextMessage
	used := 0
	for i := len(items) - 1; i >= 0; i-- {
		item := contextMessageFromTranscript(items[i])
		if item.Body == "" {
			continue
		}
		cost := utf8.RuneCountInString(contextCounterRecord(item))
		if len(reversed) > 0 && used+cost > maxChars {
			break
		}
		reversed = append(reversed, item)
		used += cost
		if len(reversed) >= maxItems {
			break
		}
	}
	out := make([]AgentContextMessage, len(reversed))
	for i := range reversed {
		out[len(reversed)-1-i] = reversed[i]
	}
	return out
}

func boundedProviderTranscript(items []AgentTranscriptItem, currentRunID, userText string) []AgentTranscriptItem {
	filtered := make([]AgentTranscriptItem, 0, len(items))
	for _, item := range items {
		if currentRunID != "" && item.RunID == currentRunID {
			continue
		}
		filtered = append(filtered, item)
	}
	if currentRunID == "" {
		for i := len(filtered) - 1; i >= 0; i-- {
			if strings.EqualFold(filtered[i].Role, "user") && strings.TrimSpace(filtered[i].Body) == strings.TrimSpace(userText) {
				filtered = append(filtered[:i], filtered[i+1:]...)
				break
			}
		}
	}
	var reversed []AgentTranscriptItem
	used := 0
	for i := len(filtered) - 1; i >= 0; i-- {
		item := filtered[i]
		cost := utf8.RuneCountInString(promptAgentTranscriptBody(item))
		if len(reversed) > 0 && used+cost > residentTranscriptPromptMaxChars {
			break
		}
		reversed = append(reversed, item)
		used += cost
		if len(reversed) >= residentTranscriptPromptMaxItems {
			break
		}
	}
	out := make([]AgentTranscriptItem, len(reversed))
	for i := range reversed {
		out[len(reversed)-1-i] = reversed[i]
	}
	return out
}

func contextMessageFromTranscript(item AgentTranscriptItem) AgentContextMessage {
	return AgentContextMessage{
		ID:     limitString(item.ID, residentContextIDMaxChars),
		Seq:    item.Seq,
		Role:   limitString(firstNonEmpty(item.Role, "system"), residentContextRoleMaxChars),
		Intent: limitString(item.Intent, residentContextIntentMaxChars),
		Body:   limitString(promptAgentTranscriptBody(item), residentTranscriptMessageMaxChars),
	}
}

func contextCounterRecord(item AgentContextMessage) string {
	fields := make([]string, 0, 3)
	for _, value := range []string{item.Role, item.Intent, item.Body} {
		if strings.TrimSpace(value) != "" {
			fields = append(fields, value)
		}
	}
	return strings.Join(fields, "\n")
}

func estimateModelBoundTranscriptTokens(items []AgentTranscriptItem) int {
	return estimateModelContextTokens(compactTranscriptForContextCounter(items))
}

func estimateModelContextTokens(items []AgentContextMessage) int {
	content := strings.TrimSpace(modelContextCounterText(items))
	if content == "" {
		return 0
	}
	return estimateResidentContextTextTokens(content)
}

func estimateResidentContextTextTokens(text string) int {
	characters := 0
	cjk := 0
	for _, r := range text {
		characters++
		if r >= 0x3040 && r <= 0x30ff || r >= 0x3400 && r <= 0x9fff || r >= 0xf900 && r <= 0xfaff {
			cjk++
		}
	}
	if characters == 0 {
		return 0
	}
	return int(math.Ceil(float64(characters-cjk)/4 + float64(cjk)*1.5))
}
