package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
)

func (a *app) runResidentAgentTurn(ctx context.Context, project Project, agent Agent, userText, turnType string, callbacks *AgentStreamCallbacks) (string, error) {
	return a.runResidentAgentTurnWithCurrentInput(ctx, project, agent, userText, turnType, AgentTranscriptItem{}, callbacks)
}

func (a *app) runResidentAgentTurnWithCurrentInput(ctx context.Context, project Project, agent Agent, userText, turnType string, currentInput AgentTranscriptItem, callbacks *AgentStreamCallbacks) (string, error) {
	var out strings.Builder
	runID := ""
	effectiveAgent := normalizeAgentModelConfig(agent)
	if run, active := a.activeAgentRun(project.ID, agent.ID); active {
		runID = run.ID
		effectiveAgent.Provider, effectiveAgent.Model = run.Provider, run.Model
		effectiveAgent.ThinkingEffort, effectiveAgent.ModelConfigVersion = run.ThinkingEffort, run.ModelConfigVersion
	}
	toolCtx := ResidentToolContext{
		Project: project, Agent: effectiveAgent, Workdir: project.Path, RunID: runID,
		TurnType: normalizeChatTurnType(turnType), EnforceRunScope: runID != "", EnforcePolicy: true,
	}
	cb := AgentStreamCallbacks{}
	if callbacks != nil {
		cb = *callbacks
	}
	outerDelta := cb.OnDelta
	cb.OnDelta = func(delta string) {
		if runID != "" {
			if _, ok := a.transitionAgentRun(project.ID, agent.ID, runID, RunStateInvokingModel); !ok {
				return
			}
		}
		out.WriteString(delta)
		if outerDelta != nil {
			outerDelta(delta)
		}
	}
	outerToolStart := cb.OnToolStart
	cb.OnToolStart = func(call codexToolCall) {
		if runID != "" {
			if _, ok := a.transitionAgentRun(project.ID, agent.ID, runID, RunStateExecutingTool); !ok {
				return
			}
			if _, ok := a.appendAgentToolCallForRun(project.ID, agent.ID, runID, call); !ok {
				return
			}
		}
		if outerToolStart != nil {
			outerToolStart(call)
		}
	}
	outerToolResult := cb.OnToolResult
	cb.OnToolResult = func(call codexToolCall, result string, success bool) {
		if runID != "" {
			if _, ok := a.appendAgentToolResultForRun(project.ID, agent.ID, runID, call, result, success); !ok {
				return
			}
			if _, ok := a.transitionAgentRun(project.ID, agent.ID, runID, RunStateWaitingModel); !ok {
				return
			}
		}
		if outerToolResult != nil {
			outerToolResult(call, result, success)
		}
	}
	cb.PollInterrupts = func() []AgentInterrupt {
		if runID == "" {
			return []AgentInterrupt{}
		}
		return a.drainAgentInterrupts(project.ID, agent.ID, runID)
	}
	memoryQuery := a.memoryRetrievalQueryFor(ctx, effectiveAgent, userText)
	prompt := a.buildResidentAgentPromptWithMemoryQuery(project, effectiveAgent, userText, turnType, memoryQuery)
	request := CLI2APIRequest{
		Provider:       effectiveAgent.Provider,
		Model:          effectiveAgent.Model,
		ThinkingEffort: effectiveAgent.ThinkingEffort,
		Prompt:         prompt,
		Workdir:        project.Path,
		Mode:           chatTurnRuntimeMode(turnType),
	}
	provider := a.residentModelProvider()
	if capabilities := provider.Capabilities(request); !capabilities.SupportsResidentRuntime() {
		return "", fmt.Errorf("resident provider %q does not support the required streaming, tool, and interrupt capabilities", a.resolveResidentProvider(request.Provider))
	}
	if strings.TrimSpace(currentInput.ID) == "" || currentInput.Seq <= 0 {
		var err error
		currentInput, err = a.currentResidentInputIdentity(project.ID, agent.ID, runID)
		if err != nil {
			return "", err
		}
	}
	history, err := boundedProviderTranscript(a.agentTranscriptDeltaForModel(project.ID, agent.ID), runID, currentInput.ID, currentInput.Seq)
	if err != nil {
		return "", fmt.Errorf("build resident model context: %w", err)
	}
	stablePrefixChars := strings.Index(prompt, "## Current chat turn type:")
	totalTokens, stableTokens, transcriptTokens, dynamicTokens := residentPromptTokenAccounting(prompt, stablePrefixChars, history)
	log.Printf("resident model context project=%s agent=%s turn=%s total_estimated_tokens=%d stable_prefix_tokens=%d transcript_tokens=%d dynamic_tokens=%d", project.ID, agent.ID, turnType, totalTokens, stableTokens, transcriptTokens, dynamicTokens)
	if runID != "" {
		if _, ok := a.transitionAgentRun(project.ID, agent.ID, runID, RunStateInvokingModel); !ok {
			return "", fmt.Errorf("resident run %s is no longer active", runID)
		}
	}
	request.Transcript = history
	err = provider.Stream(ctx, request, toolCtx, cb)
	return strings.TrimSpace(out.String()), err
}

func (a *app) currentResidentInputIdentity(projectID, agentID, runID string) (AgentTranscriptItem, error) {
	if strings.TrimSpace(runID) == "" {
		return AgentTranscriptItem{}, fmt.Errorf("resident current Run identity is required")
	}
	run, active := a.activeAgentRun(projectID, agentID)
	if !active || run.ID != runID {
		return AgentTranscriptItem{}, fmt.Errorf("resident current Run %s is not active", runID)
	}
	items := a.agentTranscriptDeltaForModel(projectID, agentID)
	if strings.TrimSpace(run.MessageID) != "" {
		for _, item := range items {
			if item.ID == run.MessageID || item.MessageID == run.MessageID {
				return item, nil
			}
		}
	}
	var found *AgentTranscriptItem
	for i := range items {
		item := items[i]
		if item.RunID != runID || item.Role != "user" || item.Intent == "interrupt" {
			continue
		}
		if found != nil {
			return AgentTranscriptItem{}, fmt.Errorf("resident current input identity is ambiguous for Run %s", runID)
		}
		copy := item
		found = &copy
	}
	if found == nil {
		return AgentTranscriptItem{}, fmt.Errorf("resident current input identity was not found for Run %s", runID)
	}
	return *found, nil
}

func (a *app) agentRouteAllowed(projectID, fromAgentID, toAgentID, intent string) bool {
	if fromAgentID == "karoz" && toAgentID != "karoz" {
		return true
	}
	if toAgentID == "karoz" && fromAgentID != "karoz" {
		return true
	}
	routes := a.routesForProject(projectID)
	if len(routes) == 0 {
		return true
	}
	for _, route := range routes {
		if !route.Enabled {
			continue
		}
		if route.FromAgentID != fromAgentID || route.ToAgentID != toAgentID {
			continue
		}
		// A route is the peer relationship/acceptance boundary. Intent describes
		// the message, not a second authorization dimension. Treating request and
		// handoff as different permissions made valid team edges impossible for an
		// agent to use without knowing an internal route encoding.
		return true
	}
	return false
}

func (a *app) activeMemoriesFor(projectID, agentID, layer string, limit int) []AgentMemoryEntry {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	items := append([]AgentMemoryEntry{}, a.memories[key]...)
	a.mu.Unlock()
	var out []AgentMemoryEntry
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.State != "active" || item.ArchivedAt != nil {
			continue
		}
		if layer != "" && item.Layer != layer {
			continue
		}
		out = append(out, item)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	if out == nil {
		return []AgentMemoryEntry{}
	}
	return out
}

func (a *app) blackboardFor(projectID string, limit int) []AgentBlackboardEntry {
	a.mu.Lock()
	items := append([]AgentBlackboardEntry{}, a.blackboard[projectID]...)
	a.mu.Unlock()
	sort.SliceStable(items, func(i, j int) bool {
		left := items[i].UpdatedAt
		if left.IsZero() {
			left = items[i].CreatedAt
		}
		right := items[j].UpdatedAt
		if right.IsZero() {
			right = items[j].CreatedAt
		}
		return left.After(right)
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	if items == nil {
		return []AgentBlackboardEntry{}
	}
	return items
}

func (a *app) pendingInboxFor(projectID, agentID string, limit int) []AgentInboxMessage {
	items := a.inboxFor(projectID, agentID, 0)
	var out []AgentInboxMessage
	for _, item := range items {
		if !handoffStatusOpen(item.Status) {
			continue
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority == out[j].Priority {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Priority > out[j].Priority
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	if out == nil {
		return []AgentInboxMessage{}
	}
	return out
}

func (a *app) inboxFor(projectID, agentID string, limit int) []AgentInboxMessage {
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	items := append([]AgentInboxMessage{}, a.inbox[key]...)
	a.mu.Unlock()
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	if items == nil {
		return []AgentInboxMessage{}
	}
	return items
}
