package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const karozIdleReconcileHook = "karoz_idle_reconcile"

func (a *app) emitRuntimeStateChanged(event RuntimeEvent) {
	if event.ProjectID == "" || event.Kind == "" {
		return
	}
	if event.ID == "" {
		event.ID = randomID()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	a.projectRuntimeEventToBlackboard(event)
	a.broadcastRuntimeEvent(event)
	a.maybeTriggerKarozIdleReconcile(event)
}

func (a *app) handleRuntimeEvents(w http.ResponseWriter, r *http.Request, project Project) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming is not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := make(chan RuntimeEvent, 32)
	a.addRuntimeWatcher(project.ID, ch)
	defer a.removeRuntimeWatcher(project.ID, ch)
	writeSSE(w, "snapshot", map[string]any{"agents": a.projectAgents(project), "backlog": a.renderProjectBacklogForKaroz(project.ID)})
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-ch:
			writeSSE(w, "runtime", map[string]any{"event": event, "agents": a.projectAgents(project), "backlog_not_empty": a.projectBacklogNotEmpty(project.ID)})
			flusher.Flush()
		}
	}
}

func (a *app) addRuntimeWatcher(projectID string, ch chan RuntimeEvent) {
	a.mu.Lock()
	if a.runtimeWatchers == nil {
		a.runtimeWatchers = map[string]map[chan RuntimeEvent]bool{}
	}
	if a.runtimeWatchers[projectID] == nil {
		a.runtimeWatchers[projectID] = map[chan RuntimeEvent]bool{}
	}
	a.runtimeWatchers[projectID][ch] = true
	a.mu.Unlock()
}

func (a *app) removeRuntimeWatcher(projectID string, ch chan RuntimeEvent) {
	a.mu.Lock()
	if watchers := a.runtimeWatchers[projectID]; watchers != nil {
		delete(watchers, ch)
		if len(watchers) == 0 {
			delete(a.runtimeWatchers, projectID)
		}
	}
	a.mu.Unlock()
}

func (a *app) broadcastRuntimeEvent(event RuntimeEvent) {
	a.mu.Lock()
	var watchers []chan RuntimeEvent
	for ch := range a.runtimeWatchers[event.ProjectID] {
		watchers = append(watchers, ch)
	}
	a.mu.Unlock()
	for _, ch := range watchers {
		select {
		case ch <- event:
		default:
		}
	}
}

func (a *app) maybeTriggerKarozIdleReconcile(event RuntimeEvent) {
	if strings.EqualFold(os.Getenv("KAROZ_AGENT_AUTO_RESPOND"), "0") || strings.EqualFold(os.Getenv("KAROZ_AGENT_AUTO_RESPOND"), "false") {
		return
	}
	if event.Reason == karozIdleReconcileHook {
		return
	}
	// Handoff creation and lifecycle transitions happen immediately before or
	// during the target agent's scheduled run. Reconciling in that narrow window
	// can race the target scheduler and create duplicate coordination work.
	if event.Kind == "handoff_created" || event.Kind == "handoff_changed" {
		return
	}
	// Active WorkPlans have their own owner and event loop. Karoz observes them
	// but must not race their coordinator by treating plan transitions as idle
	// project backlog.
	if event.Kind == "plan_changed" {
		return
	}
	a.mu.Lock()
	hasKaroz := false
	for _, agent := range a.agents[event.ProjectID] {
		if agent.ID == "karoz" {
			hasKaroz = true
			break
		}
	}
	a.mu.Unlock()
	if !hasKaroz {
		return
	}
	if !a.projectRuntimeQuiescent(event.ProjectID) {
		return
	}
	if !a.projectBacklogNotEmpty(event.ProjectID) {
		return
	}
	if !a.tryBeginRuntimeHook(event.ProjectID, karozIdleReconcileHook) {
		return
	}
	projectID := event.ProjectID
	job, err := newScheduledRun(
		ScheduledRunIdleReconcile,
		AgentRunInput{ProjectID: projectID, AgentID: "karoz", Trigger: RunTriggerSystem, TurnType: "dev", SourceID: karozIdleReconcileHook},
		projectID+"/"+karozIdleReconcileHook,
		IdleReconcileRunPayload{Reason: event.Reason},
		3*time.Minute,
	)
	if err != nil {
		a.endRuntimeHook(projectID, karozIdleReconcileHook)
		return
	}
	_, scheduled := a.scheduleAgentRun(job)
	if !scheduled {
		a.endRuntimeHook(projectID, karozIdleReconcileHook)
	}
}

func (a *app) projectRuntimeIdle(projectID string) bool {
	return a.projectRuntimeQuiescent(projectID)
}

func (a *app) projectRuntimeQuiescent(projectID string) bool {
	return a.projectRuntimeQuiescentIgnoringHook(projectID, "")
}

func (a *app) projectRuntimeIdleIgnoringHook(projectID, ignoredHook string) bool {
	return a.projectRuntimeQuiescentIgnoringHook(projectID, ignoredHook)
}

func (a *app) projectRuntimeQuiescentIgnoringHook(projectID, ignoredHook string) bool {
	return a.projectRuntimeQuiescentIgnoring(projectID, ignoredHook, "")
}

func (a *app) projectRuntimeQuiescentIgnoring(projectID, ignoredHook, ignoredAgentID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	prefix := projectID + "/"
	for key, run := range a.agentRuns {
		if ignoredAgentID != "" && key == agentMessageKey(projectID, ignoredAgentID) {
			continue
		}
		if run.State.Active() && strings.HasPrefix(key, prefix) {
			return false
		}
	}
	for key, active := range a.runtimeHooks {
		if ignoredHook != "" && key == projectID+"/"+ignoredHook {
			continue
		}
		if active && strings.HasPrefix(key, prefix) {
			return false
		}
	}
	for _, task := range a.tasks[projectID] {
		if taskStatusBlocksRuntimeQuiescent(task.Status) {
			return false
		}
	}
	return true
}

func (a *app) hasKarozIdleReconcileWork(projectID string) bool {
	return a.projectBacklogNotEmpty(projectID)
}

func (a *app) projectBacklogNotEmpty(projectID string) bool {
	if len(a.pendingInboxBacklog(projectID, 1)) > 0 {
		return true
	}
	if len(a.taskBacklog(projectID, 1)) > 0 {
		return true
	}
	if len(a.activeMemoriesFor(projectID, "karoz", "pending", 1)) > 0 {
		return true
	}
	if len(a.unhandledBlackboardSignals(projectID, 1)) > 0 {
		return true
	}
	return false
}

func (a *app) tryBeginRuntimeHook(projectID, name string) bool {
	key := projectID + "/" + name
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runtimeHooks == nil {
		a.runtimeHooks = map[string]bool{}
	}
	if a.runtimeHooks[key] {
		return false
	}
	a.runtimeHooks[key] = true
	return true
}

func (a *app) endRuntimeHook(projectID, name string) {
	key := projectID + "/" + name
	a.mu.Lock()
	delete(a.runtimeHooks, key)
	a.mu.Unlock()
}

func taskStatusBlocksRuntimeQuiescent(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "verifying", "deploying", "merging":
		return true
	default:
		return false
	}
}

func taskStatusIsBacklog(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "pending", "failed", "deploy_failed":
		return true
	default:
		return false
	}
}

func (a *app) pendingInboxBacklog(projectID string, limit int) []AgentInboxMessage {
	a.mu.Lock()
	var out []AgentInboxMessage
	for _, items := range a.inbox {
		for _, item := range items {
			if item.ProjectID == projectID && handoffStatusOpen(item.Status) {
				out = append(out, item)
				if limit > 0 && len(out) >= limit {
					a.mu.Unlock()
					return out
				}
			}
		}
	}
	a.mu.Unlock()
	if out == nil {
		return []AgentInboxMessage{}
	}
	return out
}

func (a *app) taskBacklog(projectID string, limit int) []Task {
	a.mu.Lock()
	items := append([]Task{}, a.tasks[projectID]...)
	a.mu.Unlock()
	var out []Task
	for _, task := range items {
		if task.PlanID == "" && taskStatusIsBacklog(task.Status) {
			out = append(out, task)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	if out == nil {
		return []Task{}
	}
	return out
}

func (a *app) unhandledBlackboardSignals(projectID string, limit int) []AgentBlackboardEntry {
	items := a.blackboardFor(projectID, 100)
	var out []AgentBlackboardEntry
	for _, entry := range items {
		if entry.HandledAt != nil {
			continue
		}
		if blackboardEntryActionable(entry) {
			out = append(out, entry)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	if out == nil {
		return []AgentBlackboardEntry{}
	}
	return out
}

func (a *app) renderProjectBacklogForKaroz(projectID string) string {
	var b strings.Builder
	b.WriteString("### Runtime backlog snapshot\n")
	inbox := a.pendingInboxBacklog(projectID, 12)
	b.WriteString(fmt.Sprintf("pending_inbox: %d\n", len(inbox)))
	for _, item := range inbox {
		b.WriteString(fmt.Sprintf("- inbox_id=%s from=%s to=%s intent=%s subject=%s body=%s\n", item.ID, item.SourceAgentID, item.TargetAgentID, item.Intent, limitString(item.Subject, 120), limitString(item.Body, 260)))
	}
	tasks := a.taskBacklog(projectID, 12)
	b.WriteString(fmt.Sprintf("task_backlog: %d\n", len(tasks)))
	for _, task := range tasks {
		b.WriteString(fmt.Sprintf("- task_id=%s status=%s type=%s title=%s result=%s failure=%s\n", task.ID, task.Status, task.Type, limitString(task.Title, 140), limitString(task.Result, 180), limitString(task.FailureSummary, 180)))
	}
	pending := a.activeMemoriesFor(projectID, "karoz", "pending", 8)
	b.WriteString(fmt.Sprintf("karoz_pending_memory: %d\n", len(pending)))
	for _, item := range pending {
		b.WriteString(fmt.Sprintf("- memory_id=%s priority=%d summary=%s detail=%s\n", item.ID, item.Priority, limitString(item.Summary, 160), limitString(item.Detail, 220)))
	}
	signals := a.unhandledBlackboardSignals(projectID, 12)
	b.WriteString(fmt.Sprintf("unhandled_blackboard_signals: %d\n", len(signals)))
	for _, item := range signals {
		b.WriteString(fmt.Sprintf("- activity_id=%s kind=%s by=%s summary=%s detail=%s\n", item.ID, item.ActivityKind, item.AgentID, limitString(item.Summary, 160), limitString(item.Detail, 260)))
	}
	return b.String()
}

func blackboardEntryActionable(entry AgentBlackboardEntry) bool {
	if entry.Derived {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(entry.Status))
	if status == "" {
		status = "active"
	}
	if status == "expired" || status == "done" || status == "ignored" {
		return false
	}
	if entry.HandledAt != nil {
		return false
	}
	if entry.ExpiresAt != nil && time.Now().UTC().After(*entry.ExpiresAt) {
		return true
	}
	if entry.RequiresAction {
		return true
	}
	return blackboardEntryRequiresAction(entry.ActivityKind, entry.Summary, entry.Detail)
}

func blackboardEntryRequiresAction(kind, summary, detail string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "blocker", "handoff", "error", "next_step", "decision_needed":
		return true
	case "done", "focus", "start":
		return false
	}
	text := strings.ToLower(summary + "\n" + detail)
	for _, marker := range []string{"需要", "待处理", "下一步", "协调", "阻塞", "决策", "review", "handoff", "blocker", "next step", "todo"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
