package main

import (
	"encoding/json"
	"errors"
	"strings"
)

var errCodexCompletedItemInvalid = errors.New("codex_completed_output_item_invalid")

type codexResponseOutputItem struct {
	Raw      json.RawMessage
	ToolCall *codexToolCall
}

func parseCodexSSEText(raw []byte) string {
	var out strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Item  struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"item"`
			Response struct {
				Output []struct {
					Type    string `json:"type"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		switch event.Type {
		case "response.output_text.delta":
			out.WriteString(event.Delta)
		case "response.output_item.done":
			if out.Len() == 0 && event.Item.Type == "message" {
				for _, part := range event.Item.Content {
					if part.Type == "output_text" {
						out.WriteString(part.Text)
					}
				}
			}
		case "response.completed":
			if out.Len() == 0 {
				for _, item := range event.Response.Output {
					if item.Type != "message" {
						continue
					}
					for _, part := range item.Content {
						if part.Type == "output_text" {
							out.WriteString(part.Text)
						}
					}
				}
			}
		}
	}
	return strings.TrimSpace(out.String())
}

func codexSSEDelta(payload []byte) string {
	var event struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
		Item  struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return ""
	}
	switch event.Type {
	case "response.output_text.delta":
		return event.Delta
	}
	return ""
}

func codexSSETextSnapshot(payload []byte) string {
	var event struct {
		Type string `json:"type"`
		Item struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
		Response struct {
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return ""
	}
	var out strings.Builder
	switch event.Type {
	case "response.output_item.done":
		if event.Item.Type != "message" {
			return ""
		}
		for _, part := range event.Item.Content {
			if part.Type == "output_text" {
				out.WriteString(part.Text)
			}
		}
	case "response.completed":
		for _, item := range event.Response.Output {
			if item.Type != "message" {
				continue
			}
			for _, part := range item.Content {
				if part.Type == "output_text" {
					out.WriteString(part.Text)
				}
			}
		}
	}
	return strings.TrimSpace(out.String())
}

func codexSSEToolCall(payload []byte) (codexToolCall, bool) {
	item, ok, err := codexSSECompletedOutputItem(payload)
	if err != nil || !ok || item.ToolCall == nil {
		return codexToolCall{}, false
	}
	return *item.ToolCall, true
}

func codexToolCallFromCompletedItem(item map[string]any) (codexToolCall, bool) {
	raw, err := json.Marshal(item)
	if err != nil {
		return codexToolCall{}, false
	}
	return codexToolCallFromRawCompletedItem(raw)
}

func codexToolCallFromRawCompletedItem(raw json.RawMessage) (codexToolCall, bool) {
	var parsed struct {
		ID        string          `json:"id"`
		Type      string          `json:"type"`
		CallID    string          `json:"call_id"`
		ToolCall  string          `json:"tool_call_id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Args      json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return codexToolCall{}, false
	}
	itemType := strings.TrimSpace(parsed.Type)
	if itemType != "function_call" && itemType != "tool_call" {
		return codexToolCall{}, false
	}
	args := decodeRawJSONText(parsed.Arguments)
	if args == "" || args == "null" {
		args = decodeRawJSONText(parsed.Args)
	}
	return codexToolCall{
		ID:        parsed.ID,
		CallID:    firstNonEmpty(parsed.CallID, parsed.ToolCall, parsed.ID),
		Name:      parsed.Name,
		Arguments: args,
	}, strings.TrimSpace(parsed.Name) != ""
}

func codexSSEReasoningItem(payload []byte) (map[string]any, bool) {
	item, ok, err := codexSSECompletedOutputItem(payload)
	if err != nil || !ok {
		return nil, false
	}
	var outer struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(item.Raw, &outer) != nil || outer.Type != "reasoning" {
		return nil, false
	}
	var decoded map[string]any
	if json.Unmarshal(item.Raw, &decoded) != nil {
		return nil, false
	}
	return decoded, true
}

func codexSSECompletedOutputItem(payload []byte) (codexResponseOutputItem, bool, error) {
	var event struct {
		Type string          `json:"type"`
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return codexResponseOutputItem{}, false, nil
	}
	if event.Type != "response.output_item.done" && event.Type != "item.completed" {
		return codexResponseOutputItem{}, false, nil
	}
	raw := append(json.RawMessage(nil), event.Item...)
	if len(raw) == 0 || !json.Valid(raw) {
		return codexResponseOutputItem{}, true, errCodexCompletedItemInvalid
	}
	var outer struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil || strings.TrimSpace(outer.Type) == "" {
		return codexResponseOutputItem{}, true, errCodexCompletedItemInvalid
	}
	item := codexResponseOutputItem{Raw: raw}
	switch outer.Type {
	case "reasoning", "message":
		return item, true, nil
	case "function_call", "tool_call":
		call, ok := codexToolCallFromRawCompletedItem(raw)
		if !ok || strings.TrimSpace(call.CallID) == "" {
			return codexResponseOutputItem{}, true, errCodexCompletedItemInvalid
		}
		item.ToolCall = &call
		return item, true, nil
	default:
		return codexResponseOutputItem{}, true, errCodexCompletedItemInvalid
	}
}

func decodeRawJSONText(raw json.RawMessage) string {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return ""
	}
	if strings.HasPrefix(text, `"`) {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err == nil {
			return strings.TrimSpace(decoded)
		}
	}
	return text
}
