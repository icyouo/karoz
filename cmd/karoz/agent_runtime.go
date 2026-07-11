package main

import (
	"context"
	"sort"
	"strings"
)

func (a *app) runResidentAgentTurn(ctx context.Context, project Project, agent Agent, userText, turnType string, callbacks *AgentStreamCallbacks) (string, error) {
	var out strings.Builder
	runID := ""
	if run, active := a.activeAgentRun(project.ID, agent.ID); active {
		runID = run.ID
	}
	toolCtx := ResidentToolContext{Project: project, Agent: agent, Workdir: project.Path, RunID: runID}
	cb := AgentStreamCallbacks{}
	if callbacks != nil {
		cb = *callbacks
	}
	outerDelta := cb.OnDelta
	cb.OnDelta = func(delta string) {
		a.transitionAgentRun(project.ID, agent.ID, RunStateInvokingModel)
		out.WriteString(delta)
		if outerDelta != nil {
			outerDelta(delta)
		}
	}
	outerToolStart := cb.OnToolStart
	cb.OnToolStart = func(call codexToolCall) {
		a.transitionAgentRun(project.ID, agent.ID, RunStateExecutingTool)
		a.appendAgentMessage(project.ID, agent.ID, "tool_call", call.Name, call.Arguments)
		if outerToolStart != nil {
			outerToolStart(call)
		}
	}
	outerToolResult := cb.OnToolResult
	cb.OnToolResult = func(call codexToolCall, result string, success bool) {
		a.appendAgentMessage(project.ID, agent.ID, "tool_result", call.Name, result)
		a.transitionAgentRun(project.ID, agent.ID, RunStateWaitingModel)
		if outerToolResult != nil {
			outerToolResult(call, result, success)
		}
	}
	outerInterrupt := cb.OnInterrupt
	cb.PollInterrupts = func() []AgentInterrupt {
		items := a.drainAgentInterrupts(project.ID, agent.ID)
		if len(items) > 0 && outerInterrupt != nil {
			outerInterrupt(items)
		}
		return items
	}
	prompt := a.buildResidentAgentPrompt(project, agent, userText, turnType)
	a.transitionAgentRun(project.ID, agent.ID, RunStateInvokingModel)
	err := a.residentModelProvider().Stream(ctx, CLI2APIRequest{
		Provider: getenv("KAROZ_AGENT_PROVIDER", "auto"),
		Prompt:   prompt,
		Workdir:  project.Path,
		Mode:     chatTurnRuntimeMode(turnType),
	}, toolCtx, cb)
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
		return route.Intent == intent || route.Intent == "request" && intent == ""
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
