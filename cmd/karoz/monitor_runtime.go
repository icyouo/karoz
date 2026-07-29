package main

// Gate3 deliberately keeps monitors in the application store.  The monitor
// package owns matching/guard semantics; this adapter only persists a frozen
// PendingFire before handing it to the existing scheduler or blackboard.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
	processdomain "github.com/karoz/karoz/internal/process"
)

type MonitorRunPayload struct {
	MonitorID string `json:"monitor_id"`
	FireID    string `json:"fire_id"`
	Briefing  string `json:"briefing"`
}

func (a *app) loadMonitors() error {
	_, err := a.loadJSON("monitors.json", &a.monitors)
	if err != nil {
		return err
	}
	if a.monitors == nil {
		a.monitors = map[string][]Monitor{}
	}
	for projectID, items := range a.monitors {
		for _, item := range items {
			if item.ProjectID != projectID || !gate3MonitorTrigger(item.Trigger.Kind) || monitordomain.ValidateMonitor(item) != nil {
				return fmt.Errorf("invalid monitor %s", item.ID)
			}
		}
	}
	return nil
}

func gate3MonitorTrigger(kind monitordomain.TriggerKind) bool {
	return kind == monitordomain.TriggerRuntimeEvent || kind == monitordomain.TriggerProcessExit || kind == monitordomain.TriggerProcessOutput
}

func (a *app) saveMonitorsLocked() error {
	return a.saveJSON("monitors.json", a.monitors, 0644)
}

func (a *app) saveMonitors() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveMonitorsLocked()
}

func (a *app) monitorsForProject(projectID string) []Monitor {
	a.mu.Lock()
	defer a.mu.Unlock()
	items := append([]Monitor(nil), a.monitors[projectID]...)
	if items == nil {
		return []Monitor{}
	}
	return items
}

func (a *app) monitorsForOwner(projectID, agentID string) []Monitor {
	items := a.monitorsForProject(projectID)
	out := make([]Monitor, 0, len(items))
	for _, item := range items {
		if item.AgentID == agentID {
			out = append(out, item)
		}
	}
	return out
}

func cloneMonitorList(items []Monitor) []Monitor {
	if len(items) == 0 {
		return nil
	}
	raw, _ := json.Marshal(items)
	var copied []Monitor
	_ = json.Unmarshal(raw, &copied)
	return copied
}

func (a *app) createMonitor(project Project, item Monitor) (Monitor, error) {
	a.backgroundOwnerMu.Lock()
	defer a.backgroundOwnerMu.Unlock()
	item.ProjectID = project.ID
	item.ID = strings.TrimSpace(item.ID)
	if item.ID == "" {
		item.ID = randomID()
	}
	item.Name = strings.TrimSpace(item.Name)
	if item.Revision == 0 {
		item.Revision = 1
	}
	if item.Trigger.Revision == 0 {
		item.Trigger.Revision = 1
	}
	if item.Action.Revision == 0 {
		item.Action.Revision = 1
	}
	if item.State == "" {
		item.State = monitordomain.StateActive
	}
	if !gate3MonitorTrigger(item.Trigger.Kind) {
		return Monitor{}, errors.New("trigger kind is not available in Gate3")
	}
	now := time.Now().UTC()
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	if err := a.requireMonitorOwner(project, item.AgentID); err != nil {
		return Monitor{}, err
	}
	if item.Action.Kind == monitordomain.ActionNotifyAgent {
		if _, ok := a.projectAgent(project, item.Action.AgentID); !ok {
			return Monitor{}, errors.New("monitor notify target agent not found")
		}
	}
	var outputBaseline uint64
	if item.Trigger.Kind == monitordomain.TriggerProcessOutput {
		record, err := a.processRecord(project.ID, item.Trigger.ProcessID)
		if err != nil || record.State.Terminal() {
			return Monitor{}, errors.New("monitor process target is retired or unavailable")
		}
		outputBaseline = record.OutputSeq
	}
	if err := monitordomain.ValidateMonitor(item); err != nil {
		return Monitor{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, current := range a.monitors[project.ID] {
		if current.ID == item.ID {
			return Monitor{}, errors.New("monitor already exists")
		}
	}
	a.monitors[project.ID] = append(a.monitors[project.ID], item)
	if err := a.saveMonitorsLocked(); err != nil {
		a.monitors[project.ID] = a.monitors[project.ID][:len(a.monitors[project.ID])-1]
		return Monitor{}, err
	}
	if item.Trigger.Kind == monitordomain.TriggerProcessOutput {
		if a.processOutputBaselines == nil {
			a.processOutputBaselines = map[string]uint64{}
		}
		if a.processOutputCursors == nil {
			a.processOutputCursors = map[string]uint64{}
		}
		key := projectAgentKey(project.ID, item.ID)
		a.processOutputBaselines[key] = outputBaseline
		a.processOutputCursors[key] = outputBaseline
	}
	return item, nil
}

type processOutputObservation struct {
	ProjectID string
	ProcessID string
	Line      processdomain.OutputLine
	Drained   chan struct{}
}

func (a *app) armProcessOutputMonitor() {
	a.processOutputMonitorOnce.Do(func() {
		// A new server deliberately begins at the current tail sequence:
		// output before this process lifetime is coverage, not an event replay
		// source. The arm baseline is immutable; live consumption advances a
		// separate runtime-only cursor.
		for _, item := range a.monitorsForAllProjects() {
			if item.Trigger.Kind != monitordomain.TriggerProcessOutput {
				continue
			}
			if record, err := a.processRecord(
				item.ProjectID,
				item.Trigger.ProcessID,
			); err == nil {
				a.setOutputBaseline(item.ProjectID, item.ID, record.OutputSeq)
			}
		}
		go func() {
			for {
				select {
				case <-a.supervisorCtx.Done():
					return
				case observation := <-a.processOutputMonitorCh:
					if observation.Drained != nil {
						close(observation.Drained)
						continue
					}
					a.evaluateProcessOutput(observation)
				}
			}
		}()
	})
	a.armProcessOutputGapWorker()
}

func (a *app) waitForProcessOutputHandoff() error {
	drained := make(chan struct{})
	select {
	case <-a.supervisorCtx.Done():
		return a.supervisorCtx.Err()
	case a.processOutputMonitorCh <- processOutputObservation{Drained: drained}:
	}
	select {
	case <-a.supervisorCtx.Done():
		return a.supervisorCtx.Err()
	case <-drained:
		return nil
	}
}

func (a *app) monitorsForAllProjects() []Monitor {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []Monitor
	for _, items := range a.monitors {
		out = append(out, items...)
	}
	return out
}

func (a *app) enqueueProcessOutputObservation(record processdomain.Process, line processdomain.OutputLine) {
	if line.Sequence == 0 || a.processOutputMonitorCh == nil {
		return
	}
	item := processOutputObservation{ProjectID: record.ProjectID, ProcessID: record.ID, Line: line}
	select {
	case a.processOutputMonitorCh <- item:
	default:
		a.recordProcessOutputGap(record.ProjectID, record.ID, line.Sequence)
	}
}

func (a *app) recordProcessOutputGap(projectID, processID string, sequence uint64) {
	if sequence == 0 {
		return
	}
	key := projectAgentKey(projectID, processID)
	a.processOutputGapMu.Lock()
	delta := a.processOutputPendingGaps[key]
	delta.ProjectID = projectID
	delta.ProcessID = processID
	delta.LostLines++
	if delta.OldestSeq == 0 || sequence < delta.OldestSeq {
		delta.OldestSeq = sequence
	}
	if sequence > delta.NewestSeq {
		delta.NewestSeq = sequence
	}
	if len(delta.Recent) > 0 && sequence == delta.Recent[len(delta.Recent)-1].End+1 {
		delta.Recent[len(delta.Recent)-1].End = sequence
	} else {
		delta.GapCount++
		if len(delta.Recent) == 32 {
			delta.Recent = delta.Recent[1:]
		}
		delta.Recent = append(delta.Recent, processdomain.SeqRange{Start: sequence, End: sequence})
	}
	a.processOutputPendingGaps[key] = delta
	a.processOutputGapMu.Unlock()
	select {
	case a.processOutputGapWake <- struct{}{}:
	default:
	}
}

func (a *app) setOutputBaseline(projectID, monitorID string, sequence uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.processOutputBaselines == nil {
		a.processOutputBaselines = map[string]uint64{}
	}
	if a.processOutputCursors == nil {
		a.processOutputCursors = map[string]uint64{}
	}
	key := projectAgentKey(projectID, monitorID)
	a.processOutputBaselines[key] = sequence
	a.processOutputCursors[key] = sequence
}

func (a *app) evaluateProcessOutput(observation processOutputObservation) {
	a.mu.Lock()
	items := a.monitors[observation.ProjectID]
	before := cloneMonitorList(items)
	var fires []monitorFireRef
	changed := false
	coverageFailed := false
	now := time.Now().UTC()
	text := redactSensitiveProcessText(observation.Line.Text)
	for i := range items {
		item := items[i]
		if item.State != monitordomain.StateActive ||
			item.Trigger.Kind != monitordomain.TriggerProcessOutput ||
			item.Trigger.ProcessID != observation.ProcessID {
			continue
		}
		key := projectAgentKey(observation.ProjectID, item.ID)
		if observation.Line.Sequence <= a.processOutputCursors[key] {
			continue
		}
		a.processOutputCursors[key] = observation.Line.Sequence
		matched, detail := monitordomain.MatchProcessOutput(item, monitordomain.ProcessOutput{ProcessID: observation.ProcessID, Sequence: observation.Line.Sequence, Line: text, Origin: monitordomain.Origin{Kind: "runtime"}})
		if !matched {
			continue
		}
		decision := monitordomain.Fire(item, detail, now)
		items[i] = decision.Monitor
		changed = true
		if !decision.Fire {
			continue
		}
		payload, _ := json.Marshal(map[string]any{"process_id": observation.ProcessID, "seq": observation.Line.Sequence, "stream": observation.Line.Stream, "line": text})
		event := monitordomain.Event{ID: fmt.Sprintf("process/%s/output/%d", observation.ProcessID, observation.Line.Sequence), ProjectID: observation.ProjectID, AuthorityID: "runtime", AuthorityGeneration: 1, Kind: "process_output", EntityID: observation.ProcessID, Origin: monitordomain.Origin{Kind: "runtime"}, At: now, Payload: payload}
		briefing := strings.TrimSpace(items[i].Action.Template)
		if briefing == "" {
			briefing = detail
		}
		pending, err := monitordomain.FreezePendingFire(items[i], event, briefing, payload, json.RawMessage(`{}`), now)
		if err != nil {
			coverageFailed = true
			continue
		}
		updated, err := monitordomain.AdmitPendingFire(items[i], pending)
		if err != nil {
			coverageFailed = true
			continue
		}
		items[i] = updated
		fires = append(fires, monitorFireRef{observation.ProjectID, item.ID, pending.ID})
	}
	if !changed {
		a.mu.Unlock()
		return
	}
	a.monitors[observation.ProjectID] = items
	if err := a.saveMonitorsLocked(); err != nil {
		a.monitors[observation.ProjectID] = before
		a.mu.Unlock()
		a.recordProcessOutputGap(
			observation.ProjectID,
			observation.ProcessID,
			observation.Line.Sequence,
		)
		return
	}
	a.mu.Unlock()
	if coverageFailed {
		a.recordProcessOutputGap(
			observation.ProjectID,
			observation.ProcessID,
			observation.Line.Sequence,
		)
	}
	for _, fire := range fires {
		a.dispatchMonitorPending(fire)
	}
}

func (a *app) setMonitorState(project Project, id string, state monitordomain.State) (Monitor, error) {
	if state != monitordomain.StateActive && state != monitordomain.StateDisabled {
		return Monitor{}, errors.New("invalid monitor state")
	}
	a.backgroundOwnerMu.Lock()
	defer a.backgroundOwnerMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	items := a.monitors[project.ID]
	for i := range items {
		if items[i].ID != id {
			continue
		}
		if state == monitordomain.StateActive && items[i].ErrorCode == "owner_deleted" {
			return Monitor{}, errors.New("owner-deleted monitor cannot be resumed")
		}
		before := items[i]
		items[i].State, items[i].UpdatedAt = state, time.Now().UTC()
		a.monitors[project.ID] = items
		if err := a.saveMonitorsLocked(); err != nil {
			items[i] = before
			a.monitors[project.ID] = items
			return Monitor{}, err
		}
		return items[i], nil
	}
	return Monitor{}, errors.New("monitor not found")
}

func (a *app) deleteMonitor(project Project, id string) error {
	a.backgroundOwnerMu.Lock()
	defer a.backgroundOwnerMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	items := a.monitors[project.ID]
	for i := range items {
		if items[i].ID != id {
			continue
		}
		before := append([]Monitor(nil), items...)
		a.monitors[project.ID] = append(items[:i:i], items[i+1:]...)
		if err := a.saveMonitorsLocked(); err != nil {
			a.monitors[project.ID] = before
			return err
		}
		return nil
	}
	return errors.New("monitor not found")
}

func (a *app) requireMonitorOwner(project Project, agentID string) error {
	if _, ok := a.projectAgent(project, agentID); !ok {
		return errors.New("monitor owner agent not found")
	}
	return nil
}

func (a *app) evaluateRuntimeMonitorEvent(event RuntimeEvent) {
	monitorEvent, ok := monitorEventFromRuntime(event)
	if !ok {
		return
	}
	processExit := monitorProcessExit(event)
	var fires []monitorFireRef
	a.mu.Lock()
	items := a.monitors[event.ProjectID]
	before := cloneMonitorList(items)
	changed := false
	for i := range items {
		item := items[i]
		matched, detail := monitordomain.MatchEvent(item, monitorEvent)
		if !matched && processExit != nil {
			matched, detail = monitordomain.MatchProcessExit(item, *processExit)
		}
		if !matched {
			continue
		}
		decision := monitordomain.Fire(item, detail, event.CreatedAt)
		items[i] = decision.Monitor
		changed = changed || decision.Changed
		if !decision.Fire {
			continue
		}
		briefing, _ := json.Marshal(map[string]any{"event_id": event.ID, "kind": event.Kind, "entity_id": event.EntityID, "from": event.From, "to": event.To, "reason": event.Reason})
		renderedBriefing := strings.TrimSpace(items[i].Action.Template)
		if renderedBriefing == "" {
			renderedBriefing = detail
		}
		pending, err := monitordomain.FreezePendingFire(items[i], monitorEvent, renderedBriefing, briefing, json.RawMessage(`{}`), event.CreatedAt)
		if err != nil {
			items[i].State = monitordomain.StateError
			items[i].ErrorCode = "pending_freeze"
			items[i].LastError = err.Error()
			changed = true
			continue
		}
		updated, err := monitordomain.AdmitPendingFire(items[i], pending)
		if err != nil {
			items[i].State = monitordomain.StateError
			items[i].ErrorCode = "pending_fire_capacity"
			items[i].LastError = err.Error()
			changed = true
			continue
		}
		items[i] = updated
		changed = true
		fires = append(fires, monitorFireRef{ProjectID: event.ProjectID, MonitorID: item.ID, FireID: pending.ID})
	}
	if changed {
		a.monitors[event.ProjectID] = items
		if err := a.saveMonitorsLocked(); err != nil {
			a.monitors[event.ProjectID] = before
			a.mu.Unlock()
			return
		}
	}
	a.mu.Unlock()
	for _, fire := range fires {
		a.dispatchMonitorPending(fire)
	}
}

type monitorFireRef struct{ ProjectID, MonitorID, FireID string }

func monitorEventFromRuntime(event RuntimeEvent) (monitordomain.Event, bool) {
	if event.ID == "" || event.ProjectID == "" || event.Kind == "" || event.EntityID == "" {
		return monitordomain.Event{}, false
	}
	origin := event.Origin
	if origin.Kind == "" {
		origin.Kind = "runtime"
	}
	payload, _ := json.Marshal(map[string]any{"from": event.From, "to": event.To, "exit_code": event.ExitCode, "reason": event.Reason})
	return monitordomain.Event{ID: event.ID, ProjectID: event.ProjectID, AuthorityID: "runtime", AuthorityGeneration: 1, Kind: event.Kind, EntityID: event.EntityID, Origin: origin, At: event.CreatedAt, Payload: payload}, true
}

func monitorProcessExit(event RuntimeEvent) *monitordomain.ProcessExit {
	if event.Reason != "process_terminal" || event.ExitCode == nil {
		return nil
	}
	origin := event.Origin
	if origin.Kind == "" {
		origin.Kind = "runtime"
	}
	return &monitordomain.ProcessExit{ProcessID: event.EntityID, State: event.To, ExitCode: *event.ExitCode, Detail: event.Reason, Origin: origin}
}

func (a *app) dispatchMonitorPending(ref monitorFireRef) {
	a.mu.Lock()
	items := a.monitors[ref.ProjectID]
	before := cloneMonitorList(items)
	var pending monitordomain.PendingFire
	found := false
	for i := range items {
		for j := range items[i].PendingFires {
			if items[i].ID == ref.MonitorID && items[i].PendingFires[j].ID == ref.FireID && items[i].PendingFires[j].Status == "pending" {
				pending = items[i].PendingFires[j]
				items[i].PendingFires[j].Status = "admitting"
				items[i].PendingFires[j].ClaimToken = randomID()
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		a.mu.Unlock()
		return
	}
	a.monitors[ref.ProjectID] = items
	if err := a.saveMonitorsLocked(); err != nil {
		a.monitors[ref.ProjectID] = before
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	admission := a.admitMonitorPending(ref.ProjectID, ref.MonitorID, pending)
	a.mu.Lock()
	items = a.monitors[ref.ProjectID]
	before = cloneMonitorList(items)
	for i := range items {
		for j := range items[i].PendingFires {
			p := &items[i].PendingFires[j]
			if items[i].ID != ref.MonitorID || p.ID != ref.FireID {
				continue
			}
			if admission == monitordomain.AdmissionFailed {
				p.Status = "pending"
				p.ClaimToken = ""
				p.Attempts++
				break
			}
			items[i].PendingFires = append(items[i].PendingFires[:j:j], items[i].PendingFires[j+1:]...)
			break
		}
	}
	a.monitors[ref.ProjectID] = items
	if err := a.saveMonitorsLocked(); err != nil {
		a.monitors[ref.ProjectID] = before
		a.mu.Unlock()
		return
	}
	retry := admission == monitordomain.AdmissionFailed && pending.Attempts < 2
	confirmed := admission != monitordomain.AdmissionFailed
	a.mu.Unlock()
	if confirmed {
		queue := a.ensureSchedulerQueue()
		queue.ConfirmMonitorFirePendingRemoval(pending.DedupKey)
		a.saveOrLog("scheduled monitor fire receipt", a.saveScheduledRuns())
	}
	if retry {
		time.AfterFunc(time.Second, func() { a.dispatchMonitorPending(ref) })
	}
}

func (a *app) admitMonitorPending(projectID, monitorID string, pending monitordomain.PendingFire) monitordomain.Admission {
	// Agent deletion shares this short fence with terminal delivery. It prevents
	// a frozen notification from being admitted after its owner/target has been
	// removed and before deletion disables the monitor.
	a.backgroundOwnerMu.Lock()
	defer a.backgroundOwnerMu.Unlock()
	if !a.monitorAdmissionAvailable(projectID, monitorID, pending.Action.AgentID) {
		return monitordomain.AdmissionFailed
	}
	project, err := a.projectByID(projectID)
	if err != nil {
		return monitordomain.AdmissionFailed
	}
	if pending.Action.Kind == string(monitordomain.ActionBlackboard) {
		return a.admitMonitorBlackboard(project, monitorID, pending)
	}
	agent, ok := a.projectAgent(project, pending.Action.AgentID)
	if !ok {
		return monitordomain.AdmissionFailed
	}
	job, err := newScheduledRun(ScheduledRunMonitorEvent, AgentRunInput{RunID: pending.ID, ProjectID: projectID, AgentID: agent.ID, Trigger: RunTriggerMonitor, TurnType: pending.Action.TurnType, SourceID: pending.ID, Origin: monitordomain.Origin{Kind: "monitor", MonitorID: monitorID, FireID: pending.ID}}, pending.DedupKey, MonitorRunPayload{MonitorID: monitorID, FireID: pending.ID, Briefing: pending.Action.RenderedBriefing}, scheduledRunExecutionTimeout(pending.Action.TurnType))
	if err != nil {
		return monitordomain.AdmissionFailed
	}
	_, accepted := a.scheduleAgentRun(job)
	if accepted {
		return monitordomain.AdmissionAdmitted
	}
	for _, existing := range a.ensureSchedulerQueue().Jobs() {
		if existing.DedupKey == pending.DedupKey {
			return monitordomain.AdmissionAlreadyPresent
		}
	}
	if a.ensureSchedulerQueue().HasCompletedMonitorFire(pending.DedupKey) {
		return monitordomain.AdmissionAlreadyPresent
	}
	return monitordomain.AdmissionFailed
}

func (a *app) monitorAdmissionAvailable(projectID, monitorID, targetID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, item := range a.monitors[projectID] {
		if item.ID != monitorID {
			continue
		}
		if item.State != monitordomain.StateActive || a.backgroundOwnerDeleting[projectAgentKey(projectID, item.AgentID)] {
			return false
		}
		return targetID == "" || !a.backgroundOwnerDeleting[projectAgentKey(projectID, targetID)]
	}
	return false
}

func (a *app) admitMonitorBlackboard(project Project, monitorID string, pending monitordomain.PendingFire) monitordomain.Admission {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, entry := range a.blackboard[project.ID] {
		if entry.SourceType == "monitor" && entry.SourceID == pending.ID {
			return monitordomain.AdmissionAlreadyPresent
		}
	}
	now := time.Now().UTC()
	entry := AgentBlackboardEntry{ID: randomID(), ProjectID: project.ID, AgentName: "Monitor", ActivityKind: "monitor", Summary: pending.Action.Topic, Detail: pending.Action.RenderedBriefing, SourceType: "monitor", SourceID: pending.ID, CreatedAt: now, UpdatedAt: now, Status: "active", RequiresAction: false}
	a.blackboard[project.ID] = append(a.blackboard[project.ID], entry)
	if err := a.saveJSON("agent-blackboard.json", a.blackboard, 0644); err != nil {
		a.blackboard[project.ID] = a.blackboard[project.ID][:len(a.blackboard[project.ID])-1]
		return monitordomain.AdmissionFailed
	}
	return monitordomain.AdmissionAdmitted
}

func (a *app) resumeMonitorPending() {
	a.mu.Lock()
	var refs []monitorFireRef
	for projectID, items := range a.monitors {
		for _, item := range items {
			for _, pending := range item.PendingFires {
				if pending.Status == "pending" || pending.Status == "admitting" {
					refs = append(refs, monitorFireRef{projectID, item.ID, pending.ID})
				}
			}
		}
	}
	a.mu.Unlock()
	for _, ref := range refs {
		a.resetAndDispatchMonitorPending(ref)
	}
}

func (a *app) resetAndDispatchMonitorPending(ref monitorFireRef) {
	a.mu.Lock()
	for i := range a.monitors[ref.ProjectID] {
		for j := range a.monitors[ref.ProjectID][i].PendingFires {
			p := &a.monitors[ref.ProjectID][i].PendingFires[j]
			if a.monitors[ref.ProjectID][i].ID == ref.MonitorID && p.ID == ref.FireID {
				p.Status = "pending"
				p.ClaimToken = ""
			}
		}
	}
	_ = a.saveMonitorsLocked()
	a.mu.Unlock()
	a.dispatchMonitorPending(ref)
}

func (a *app) executeMonitorScheduledRun(ctx context.Context, job ScheduledRun) error {
	var payload MonitorRunPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return err
	}
	project, err := a.projectByID(job.ProjectID)
	if err != nil {
		return err
	}
	agent, ok := a.projectAgent(project, job.AgentID)
	if !ok {
		return errors.New("monitor target agent not found")
	}
	prompt := "[monitor event] " + strings.TrimSpace(payload.Briefing) + "\n\nA durable monitor matched runtime state. Assess it and take the next appropriate action."
	out, err := a.runScheduledResidentAgentTurn(ctx, job, project, agent, prompt, firstNonEmpty(job.TurnType, "ask"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return a.commitScheduledRunResult(project, agent, job.ID, "monitor_result", out)
	}
	return nil
}

func (a *app) createMonitorFromTool(toolCtx ResidentToolContext, args map[string]any) string {
	raw, ok := args["monitor"]
	if !ok {
		return toolJSON(map[string]any{"error": "validation_error", "message": "monitor is required"})
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return toolJSON(map[string]any{"error": "validation_error", "message": err.Error()})
	}
	var item Monitor
	if err := json.Unmarshal(encoded, &item); err != nil {
		return toolJSON(map[string]any{"error": "validation_error", "message": err.Error()})
	}
	item.AgentID = toolCtx.Agent.ID
	created, err := a.createMonitor(toolCtx.Project, item)
	if err != nil {
		return toolJSON(map[string]any{"error": "validation_error", "message": err.Error()})
	}
	return toolJSON(map[string]any{"monitor": created})
}

func (a *app) setMonitorFromTool(toolCtx ResidentToolContext, args map[string]any, active bool) string {
	id := toolStringArg(args, "monitor_id", 128)
	item, err := a.monitorOwnedBy(toolCtx.Project.ID, toolCtx.Agent.ID, id)
	if err != nil {
		return toolJSON(map[string]any{"error": "not_found", "message": err.Error()})
	}
	state := monitordomain.StateDisabled
	if active {
		state = monitordomain.StateActive
	}
	updated, err := a.setMonitorState(toolCtx.Project, item.ID, state)
	if err != nil {
		return toolJSON(map[string]any{"error": "monitor_error", "message": err.Error()})
	}
	return toolJSON(map[string]any{"monitor": updated})
}

func (a *app) deleteMonitorFromTool(toolCtx ResidentToolContext, args map[string]any) string {
	item, err := a.monitorOwnedBy(toolCtx.Project.ID, toolCtx.Agent.ID, toolStringArg(args, "monitor_id", 128))
	if err != nil {
		return toolJSON(map[string]any{"error": "not_found", "message": err.Error()})
	}
	if err := a.deleteMonitor(toolCtx.Project, item.ID); err != nil {
		return toolJSON(map[string]any{"error": "monitor_error", "message": err.Error()})
	}
	return toolJSON(map[string]any{"deleted": item.ID})
}

func (a *app) monitorOwnedBy(projectID, agentID, id string) (Monitor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, item := range a.monitors[projectID] {
		if item.ID == id && item.AgentID == agentID {
			return item, nil
		}
	}
	return Monitor{}, errors.New("monitor not found")
}
