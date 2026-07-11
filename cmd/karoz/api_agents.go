package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (a *app) handleAgents(w http.ResponseWriter, r *http.Request, project Project, parts []string) {
	if len(parts) == 0 {
		if r.Method == http.MethodGet {
			writeJSON(w, a.projectAgents(project))
			return
		}
		if r.Method == http.MethodPost {
			var req AgentCreateRequest
			if err := readJSON(r, &req); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			agent, err := a.createProjectAgent(project, req)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, agent)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	agentID := parts[0]
	agent, ok := a.projectAgent(project, agentID)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("agent %s not found", agentID))
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPatch {
		var req AgentUpdateRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		updated, err := a.updateProjectAgent(project, agent.ID, req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, updated)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := a.deleteProjectAgent(project, agent.ID); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"deleted": true, "agent_id": agent.ID})
		return
	}
	if len(parts) == 2 && parts[1] == "messages" && r.Method == http.MethodGet {
		if r.URL.Query().Has("limit") || r.URL.Query().Has("before_seq") {
			limit := 80
			if raw := r.URL.Query().Get("limit"); raw != "" {
				if parsed, err := strconv.Atoi(raw); err == nil {
					limit = parsed
				}
			}
			var beforeSeq int64
			if raw := r.URL.Query().Get("before_seq"); raw != "" {
				beforeSeq, _ = strconv.ParseInt(raw, 10, 64)
			}
			writeJSON(w, a.agentMessagesPageForDisplay(project.ID, agent.ID, beforeSeq, limit))
			return
		}
		writeJSON(w, a.agentMessagesForDisplay(project.ID, agent.ID))
		return
	}
	if len(parts) == 2 && parts[1] == "inbox" && r.Method == http.MethodGet {
		if r.URL.Query().Get("include_closed") == "1" || strings.EqualFold(r.URL.Query().Get("include_closed"), "true") {
			writeJSON(w, a.inboxFor(project.ID, agent.ID, 200))
			return
		}
		writeJSON(w, a.pendingInboxFor(project.ID, agent.ID, 50))
		return
	}
	if len(parts) == 2 && parts[1] == "memory" && r.Method == http.MethodGet {
		writeJSON(w, a.activeMemoriesFor(project.ID, agent.ID, "", 100))
		return
	}
	if len(parts) == 2 && parts[1] == "archive" && r.Method == http.MethodGet {
		a.mu.Lock()
		items := append([]AgentArchiveMessage{}, a.archives[agentMessageKey(project.ID, agent.ID)]...)
		a.mu.Unlock()
		if items == nil {
			items = []AgentArchiveMessage{}
		}
		writeJSON(w, items)
		return
	}
	if len(parts) == 2 && parts[1] == "run" && r.Method == http.MethodGet {
		run, active := a.activeAgentRun(project.ID, agent.ID)
		writeJSON(w, map[string]any{"active": active, "run": run, "queued": a.scheduledAgentRunCount(project.ID, agent.ID)})
		return
	}
	if len(parts) == 3 && parts[1] == "run" && parts[2] == "cancel" && r.Method == http.MethodPost {
		run, cancelled := a.cancelAgentRun(project.ID, agent.ID)
		if !cancelled {
			writeError(w, http.StatusConflict, errors.New("agent has no active run"))
			return
		}
		writeJSON(w, map[string]any{"cancelled": true, "run": run})
		return
	}
	if len(parts) >= 2 && parts[1] == "workspace" {
		a.handleAgentWorkspace(w, r, project, agent, parts[2:])
		return
	}
	if len(parts) == 2 && parts[1] == "messages" && r.Method == http.MethodPost {
		req, attachments, err := a.readAgentMessageRequest(r, project, agent)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		userText := strings.TrimSpace(req.Message)
		if userText == "" && len(attachments) == 0 {
			writeError(w, http.StatusBadRequest, errors.New("message or attachment is required"))
			return
		}
		userText = messageTextWithAttachments(userText, attachments)
		turnType := normalizeChatTurnType(req.Type)
		run, started := a.beginAgentRun(AgentRunInput{ProjectID: project.ID, AgentID: agent.ID, Trigger: RunTriggerUserDirect, TurnType: turnType})
		messageStored := false
		if !started {
			msg := a.appendAgentMessage(project.ID, agent.ID, "user", "interrupt", userText)
			messageStored = true
			item, queued := a.enqueueAgentInterrupt(project.ID, agent.ID, msg, turnType)
			if queued {
				if r.URL.Query().Get("stream") == "1" || strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Cache-Control", "no-cache")
					writeSSE(w, "queued", map[string]any{"interrupt": item, "message": "Queued for the running agent turn."})
					writeSSE(w, "done", map[string]any{"queued": true, "message": "Queued for the running agent turn.", "agent": a.agentWithRuntimeState(project, agent)})
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				writeJSON(w, map[string]any{"queued": true, "interrupt": item, "message": "Queued for the running agent turn.", "agent": a.agentWithRuntimeState(project, agent)})
				return
			}
			// The previous Run finished between the initial submit and interrupt
			// enqueue. Start a fresh direct Run instead of claiming the message
			// was queued when no active Run accepted it.
			run, started = a.beginAgentRun(AgentRunInput{ProjectID: project.ID, AgentID: agent.ID, Trigger: RunTriggerUserDirect, TurnType: turnType, MessageID: msg.ID})
			if !started {
				writeError(w, http.StatusConflict, errors.New("agent run changed while submitting message; retry"))
				return
			}
		}
		defer a.finishAgentRun(project.ID, agent.ID, run.ID, RunStateDone, nil)
		runCtx, _ := a.bindAgentRunContext(r.Context(), project.ID, agent.ID)
		if !messageStored {
			a.appendAgentMessage(project.ID, agent.ID, "user", turnType, userText)
		}
		if r.URL.Query().Get("stream") == "1" || strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			a.streamAgentMessage(w, r.Clone(runCtx), project, agent, run.ID, userText, turnType)
			return
		}
		prompt := a.buildResidentAgentPrompt(project, agent, userText, turnType)
		a.transitionAgentRun(project.ID, agent.ID, RunStateInvokingModel)
		cli, err := a.invokeCLI2API(runCtx, CLI2APIRequest{
			Provider: getenv("KAROZ_AGENT_PROVIDER", "auto"),
			Prompt:   prompt,
			Workdir:  project.Path,
			Mode:     chatTurnRuntimeMode(turnType),
		})
		message := emptyAgentOutputMessage(agent)
		if err != nil {
			a.finishAgentRun(project.ID, agent.ID, run.ID, RunStateFailed, err)
			message += " Agent runtime failed: " + err.Error()
			a.appendAgentMessage(project.ID, agent.ID, "assistant", "status", message)
			writeJSON(w, AgentMessageResponse{Agent: a.agentWithRuntimeState(project, agent), Message: message})
			return
		}
		if strings.TrimSpace(cli.Output) != "" {
			message = cli.Output
		} else {
			log.Printf("agent runtime returned empty output project=%s agent=%s", project.ID, agent.ID)
		}
		a.appendAgentMessage(project.ID, agent.ID, "assistant", "result", message)
		writeJSON(w, AgentMessageResponse{Agent: a.agentWithRuntimeState(project, agent), Message: message, CLI: &cli})
		return
	}
	http.NotFound(w, r)
}

func (a *app) readAgentMessageRequest(r *http.Request, project Project, agent Agent) (AgentMessageRequest, []AgentAttachment, error) {
	contentType := r.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if strings.HasPrefix(mediaType, "multipart/") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			return AgentMessageRequest{}, nil, err
		}
		req := AgentMessageRequest{
			Message: r.FormValue("message"),
			Type:    r.FormValue("type"),
		}
		attachments, err := a.saveAgentMultipartAttachments(project, agent, r)
		return req, attachments, err
	}
	var req AgentMessageRequest
	if err := readJSON(r, &req); err != nil {
		return AgentMessageRequest{}, nil, err
	}
	return req, nil, nil
}

func (a *app) saveAgentMultipartAttachments(project Project, agent Agent, r *http.Request) ([]AgentAttachment, error) {
	if r.MultipartForm == nil || r.MultipartForm.File == nil {
		return nil, nil
	}
	files := append([]*multipart.FileHeader{}, r.MultipartForm.File["files"]...)
	files = append(files, r.MultipartForm.File["attachments"]...)
	if len(files) == 0 {
		return nil, nil
	}
	if len(files) > 12 {
		return nil, errors.New("too many attachments; maximum is 12")
	}
	baseDir := filepath.Join(a.settings.DataDir, "attachments", project.ID, agent.ID, time.Now().UTC().Format("20060102-150405")+"-"+randomID())
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, err
	}
	out := make([]AgentAttachment, 0, len(files))
	for _, header := range files {
		if header == nil {
			continue
		}
		if header.Size > 32<<20 {
			return nil, fmt.Errorf("attachment %s is too large; maximum is 32 MB", header.Filename)
		}
		src, err := header.Open()
		if err != nil {
			return nil, err
		}
		filename := safeAttachmentFilename(header.Filename)
		if filename == "" {
			filename = "attachment"
		}
		id := randomID()
		dstPath := filepath.Join(baseDir, id+"-"+filename)
		dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			_ = src.Close()
			return nil, err
		}
		written, copyErr := io.Copy(dst, io.LimitReader(src, 32<<20+1))
		closeErr := dst.Close()
		_ = src.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if written > 32<<20 {
			return nil, fmt.Errorf("attachment %s is too large; maximum is 32 MB", header.Filename)
		}
		mimeType := header.Header.Get("Content-Type")
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		absPath, err := filepath.Abs(dstPath)
		if err != nil {
			absPath = dstPath
		}
		out = append(out, AgentAttachment{
			ID:           id,
			Filename:     filename,
			MimeType:     mimeType,
			SizeBytes:    written,
			Path:         absPath,
			OriginalName: header.Filename,
			CreatedAt:    time.Now().UTC(),
		})
	}
	return out, nil
}

func safeAttachmentFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == 0 {
			return '-'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if len(name) > 160 {
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		if len(ext) > 20 {
			ext = ""
		}
		if len(base) > 140 {
			base = base[:140]
		}
		name = base + ext
	}
	return name
}

func messageTextWithAttachments(message string, attachments []AgentAttachment) string {
	message = strings.TrimSpace(message)
	if len(attachments) == 0 {
		return message
	}
	if message == "" {
		message = "Please review the attached files."
	}
	var b strings.Builder
	b.WriteString(message)
	b.WriteString("\n\nAttachments:\n")
	for _, item := range attachments {
		b.WriteString("- ")
		b.WriteString(item.Filename)
		b.WriteString(" (")
		b.WriteString(item.MimeType)
		b.WriteString(", ")
		b.WriteString(formatBytesForPrompt(item.SizeBytes))
		b.WriteString("): ")
		b.WriteString(item.Path)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func formatBytesForPrompt(value int64) string {
	if value < 1024 {
		return fmt.Sprintf("%d B", value)
	}
	if value < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(value)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(value)/(1024*1024))
}
