package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

func (a *app) handleAgentWorkspace(w http.ResponseWriter, r *http.Request, project Project, agent Agent, parts []string) {
	if len(parts) == 1 && parts[0] == "files" && r.Method == http.MethodGet {
		files, err := a.listWorkspaceFiles(project.ID, agent.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, map[string]any{"data": files, "total": len(files)})
		return
	}
	if len(parts) == 1 && parts[0] == "file" && r.Method == http.MethodGet {
		preview, err := a.getWorkspaceFilePreview(project.ID, agent.ID, r.URL.Query().Get("path"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, preview)
		return
	}
	http.NotFound(w, r)
}

func (a *app) streamAgentMessage(w http.ResponseWriter, r *http.Request, project Project, agent Agent, runID, userText, turnType string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming is not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	meta := map[string]any{"agent": a.agentWithRuntimeState(project, agent), "type": normalizeChatTurnType(turnType)}
	if run, active := a.activeAgentRun(project.ID, agent.ID); active && run.ID == runID {
		meta["run"] = map[string]any{"id": run.ID, "provider": run.Provider, "model": run.Model, "thinking_effort": run.ThinkingEffort, "model_config_version": run.ModelConfigVersion}
	}
	writeSSE(w, "meta", meta)
	flusher.Flush()

	message, err := a.runResidentAgentTurn(r.Context(), project, agent, userText, turnType, &AgentStreamCallbacks{
		OnDelta: func(delta string) {
			if delta == "" {
				return
			}
			writeSSE(w, "delta", map[string]string{"delta": delta, "content": delta})
			flusher.Flush()
		},
		OnToolStart: func(call codexToolCall) {
			writeSSE(w, "tool_start", map[string]string{"call_id": firstNonEmpty(call.CallID, call.ID), "tool": call.Name, "arguments": call.Arguments})
			flusher.Flush()
		},
		OnToolResult: func(call codexToolCall, result string, success bool) {
			callID := firstNonEmpty(call.CallID, call.ID)
			displayResult := compactToolResultForDisplay(call.Name, result)
			writeSSE(w, "tool_result", map[string]any{"call_id": callID, "tool": call.Name, "success": success, "result": displayResult})
			if success && (call.Name == "write_workspace_file" || call.Name == "show_preview") {
				if preview := a.previewFromToolResult(project.ID, agent.ID, call, result); preview != nil {
					writeSSE(w, "preview", preview)
				}
			}
			flusher.Flush()
		},
		OnInterrupt: func(items []AgentInterrupt) {
			writeSSE(w, "interrupt", map[string]any{"interrupts": items})
			flusher.Flush()
		},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			a.finishAgentRun(project.ID, agent.ID, runID, RunStateCancelled, err)
			writeSSE(w, "cancelled", map[string]string{"message": "Agent run cancelled."})
			flusher.Flush()
			return
		}
		message := "Agent runtime failed: " + err.Error()
		a.appendAgentMessageForRun(project.ID, agent.ID, runID, "assistant", "status", message)
		a.finishAgentRun(project.ID, agent.ID, runID, RunStateFailed, err)
		writeSSE(w, "error", map[string]string{"message": message})
		flusher.Flush()
		return
	}
	if message == "" {
		message = emptyAgentOutputMessage(agent)
		log.Printf("agent runtime returned empty output project=%s agent=%s", project.ID, agent.ID)
	}
	if _, appended := a.appendAgentMessageForRun(project.ID, agent.ID, runID, "assistant", "result", message); !appended {
		writeSSE(w, "cancelled", map[string]string{"message": "Agent run was replaced before its result could be committed."})
		flusher.Flush()
		return
	}
	a.transitionAgentRun(project.ID, agent.ID, runID, RunStateCompleting)
	a.finishAgentRun(project.ID, agent.ID, runID, RunStateDone, nil)
	writeSSE(w, "done", map[string]any{"message": message, "agent": a.agentWithRuntimeState(project, agent)})
	flusher.Flush()
}

func writeSSE(w io.Writer, event string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte(`{"error":"marshal_failed"}`)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

func normalizeChatTurnType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "ask", "plan", "dev":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "ask"
	}
}

func chatTurnRuntimeMode(turnType string) string {
	if normalizeChatTurnType(turnType) == "dev" {
		return "edit"
	}
	return "plan"
}

func toolResultSuccess(result string) bool {
	var payload map[string]any
	if json.Unmarshal([]byte(result), &payload) != nil {
		return true
	}
	if _, ok := payload["error"]; ok {
		return false
	}
	return true
}

func (a *app) previewFromToolResult(projectID, agentID string, call codexToolCall, result string) map[string]any {
	var payload map[string]any
	if json.Unmarshal([]byte(result), &payload) != nil {
		return nil
	}
	if preview, ok := payload["preview"].(map[string]any); ok {
		return map[string]any{
			"filename": firstNonEmpty(fmt.Sprint(preview["filename"]), fmt.Sprint(preview["path"])),
			"path":     fmt.Sprint(preview["path"]),
			"mimeType": fmt.Sprint(preview["mime_type"]),
			"content":  fmt.Sprint(preview["content"]),
			"encoding": fmt.Sprint(preview["encoding"]),
		}
	}
	if filePayload, ok := payload["file"].(map[string]any); ok {
		path := fmt.Sprint(filePayload["path"])
		preview, err := a.getWorkspaceFilePreview(projectID, agentID, path)
		if err != nil {
			return nil
		}
		return map[string]any{"filename": preview.Filename, "path": preview.Path, "mimeType": preview.MimeType, "content": preview.Content, "encoding": preview.Encoding}
	}
	return nil
}
