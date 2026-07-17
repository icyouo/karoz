package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

func (a *app) runResidentAgentTurn(ctx context.Context, project Project, agent Agent, userText, turnType string, callbacks *AgentStreamCallbacks) (string, error) {
	var out strings.Builder
	runID := ""
	if run, active := a.activeAgentRun(project.ID, agent.ID); active {
		runID = run.ID
	}
	toolCtx := ResidentToolContext{
		Project: project, Agent: agent, Workdir: project.Path, RunID: runID,
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
			if _, ok := a.appendAgentMessageForRun(project.ID, agent.ID, runID, "tool_call", call.Name, call.Arguments); !ok {
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
			if _, ok := a.appendAgentMessageForRun(project.ID, agent.ID, runID, "tool_result", call.Name, result); !ok {
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
	prompt := a.buildResidentAgentPrompt(project, agent, userText, turnType)
	if runID != "" {
		if _, ok := a.transitionAgentRun(project.ID, agent.ID, runID, RunStateInvokingModel); !ok {
			return "", fmt.Errorf("resident run %s is no longer active", runID)
		}
	}
	request := CLI2APIRequest{
		Provider: getenv("KAROZ_AGENT_PROVIDER", "auto"),
		Prompt:   prompt,
		Workdir:  project.Path,
		Mode:     chatTurnRuntimeMode(turnType),
	}
	provider := a.residentModelProvider()
	if capabilities := provider.Capabilities(request); !capabilities.SupportsResidentRuntime() {
		return "", fmt.Errorf("resident provider %q does not support the required streaming, tool, and interrupt capabilities", a.resolveResidentProvider(request.Provider))
	}
	err := provider.Stream(ctx, request, toolCtx, cb)
	return strings.TrimSpace(out.String()), err
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
	key := agentMessageKey(projectID, agentID)
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
	key := agentMessageKey(projectID, agentID)
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
