package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

func parseToolArgs(raw string) (map[string]any, error) {
	var args map[string]any
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, err
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

func requestChoiceTool(args map[string]any) string {
	question := toolStringArg(args, "question", 2000)
	mode := strings.ToLower(toolStringArg(args, "mode", 64))
	if question == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "question is required"})
	}
	if mode == "" {
		mode = "single"
	}
	if mode != "yes_no" && mode != "single" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "mode must be yes_no or single"})
	}
	choices := normalizeChoiceOptions(args["choices"])
	if mode == "yes_no" && len(choices) == 0 {
		choices = []map[string]string{
			{"id": "yes", "label": "Yes"},
			{"id": "no", "label": "No"},
		}
	}
	if len(choices) == 0 {
		return toolJSON(map[string]any{"error": "validation_error", "message": "choices are required"})
	}
	if len(choices) > 12 {
		choices = choices[:12]
	}
	return toolJSON(map[string]any{
		"kind":     "choice_request",
		"status":   "pending_user_choice",
		"question": question,
		"mode":     mode,
		"choices":  choices,
	})
}

func normalizeChoiceOptions(raw any) []map[string]string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]string, 0, len(items))
	for i, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		label := strings.TrimSpace(fmt.Sprint(obj["label"]))
		if label == "" || label == "<nil>" {
			continue
		}
		id := strings.TrimSpace(fmt.Sprint(obj["id"]))
		if id == "" || id == "<nil>" {
			id = fmt.Sprintf("%d", i+1)
		}
		description := strings.TrimSpace(fmt.Sprint(obj["description"]))
		if description == "<nil>" {
			description = ""
		}
		out = append(out, map[string]string{
			"id":          limitString(id, 80),
			"label":       limitString(label, 240),
			"description": limitString(description, 800),
		})
	}
	return out
}
