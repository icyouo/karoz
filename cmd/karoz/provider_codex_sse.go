package main

import (
	"encoding/json"
	"strings"
)

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
	var event struct {
		Type string `json:"type"`
		Item struct {
			ID        string          `json:"id"`
			Type      string          `json:"type"`
			CallID    string          `json:"call_id"`
			ToolCall  string          `json:"tool_call_id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Args      json.RawMessage `json:"args"`
		} `json:"item"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return codexToolCall{}, false
	}
	if event.Type != "response.output_item.done" && event.Type != "item.completed" {
		return codexToolCall{}, false
	}
	itemType := strings.TrimSpace(event.Item.Type)
	if itemType != "function_call" && itemType != "tool_call" {
		return codexToolCall{}, false
	}
	args := decodeRawJSONText(event.Item.Arguments)
	if args == "" || args == "null" {
		args = decodeRawJSONText(event.Item.Args)
	}
	return codexToolCall{
		ID:        event.Item.ID,
		CallID:    firstNonEmpty(event.Item.CallID, event.Item.ToolCall, event.Item.ID),
		Name:      event.Item.Name,
		Arguments: args,
	}, strings.TrimSpace(event.Item.Name) != ""
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
