package main

// Gate3 deliberately keeps monitors in the application store.  The monitor
// package owns matching/guard semantics; this adapter only persists a frozen
// PendingFire before handing it to the existing scheduler or blackboard.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	data, err := os.ReadFile(filepath.Join(a.settings.DataDir, "monitors.json"))
	found := err == nil
	legacy := false
	if errors.Is(err, os.ErrNotExist) {
		found = false
	} else if err != nil {
		return err
	}
	if found {
		raw := json.RawMessage(data)
		var envelope struct {
			Monitors json.RawMessage `json:"monitors"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return err
		}
		if len(envelope.Monitors) > 0 {
			var snapshot monitorRegistrySnapshot
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				return err
			}
			if snapshot.SchemaVersion != 2 {
				return fmt.Errorf(
					"unsupported monitor registry schema %d",
					snapshot.SchemaVersion,
				)
			}
			a.monitors = snapshot.Monitors
			a.monitorProbeReservations = snapshot.ProbeReservations
			a.monitorProbeChallenges = snapshot.ProbeChallenges
			a.monitorProbeReceipts = snapshot.ProbeReceipts
			a.monitorProbeSessions = snapshot.ProbeSessions
		} else if err := json.Unmarshal(raw, &a.monitors); err != nil {
			return err
		} else {
			legacy = true
		}
	}
	if a.monitors == nil {
		a.monitors = map[string][]Monitor{}
	}
	if a.monitorProbeReservations == nil {
		a.monitorProbeReservations = map[string]monitorProbeReservation{}
	}
	if a.monitorProbeChallenges == nil {
		a.monitorProbeChallenges = map[string]monitorProbeChallenge{}
	}
	if a.monitorProbeReceipts == nil {
		a.monitorProbeReceipts = map[string]monitordomain.ProbeApprovalReceipt{}
	}
	if a.monitorProbeSessions == nil {
		a.monitorProbeSessions = map[string]monitorProbeOperatorSession{}
	}
	normalized := legacy
	now := time.Now().UTC()
	beforeSecurityRecords := len(a.monitorProbeReservations) +
		len(a.monitorProbeChallenges) +
		len(a.monitorProbeReceipts) +
		len(a.monitorProbeSessions)
	a.pruneMonitorProbeApprovalsLocked(now)
	afterSecurityRecords := len(a.monitorProbeReservations) +
		len(a.monitorProbeChallenges) +
		len(a.monitorProbeReceipts) +
		len(a.monitorProbeSessions)
	normalized = normalized || beforeSecurityRecords != afterSecurityRecords
	for projectID, items := range a.monitors {
		for index := range items {
			item := &items[index]
			if item.ProjectID != projectID || !gate3MonitorTrigger(item.Trigger.Kind) || monitordomain.ValidateMonitor(*item) != nil {
				return fmt.Errorf("invalid monitor %s", item.ID)
			}
			if item.Trigger.Kind != monitordomain.TriggerScriptProbe {
				continue
			}
			item.ProbeRunning = false
			if !scriptProbeSupported {
				if item.State != monitordomain.StateError ||
					item.ErrorCode != "unsupported_platform" {
					item.State = monitordomain.StateError
					item.ErrorCode = "unsupported_platform"
					item.LastError = "script probes are unsupported on this platform"
					item.UpdatedAt = now
					normalized = true
				}
				continue
			}
			if item.State != monitordomain.StateActive {
				continue
			}
			receipt, ok := a.monitorProbeReceipts[item.Trigger.ApprovalReceiptID]
			if !ok {
				item.State = monitordomain.StateError
				item.ErrorCode = "probe_authorization"
				item.LastError = "probe approval receipt is unavailable"
				item.UpdatedAt = now
				normalized = true
				continue
			}
			if _, err := a.authorizedMonitorProbeSnapshot(*item, receipt); err != nil {
				item.State = monitordomain.StateError
				item.ErrorCode = "probe_authorization"
				item.LastError = limitString(err.Error(), 1024)
				item.UpdatedAt = now
				normalized = true
				continue
			}
			next := now.Add(time.Duration(item.Trigger.IntervalMS) * time.Millisecond)
			item.NextCheckAt = &next
			normalized = true
		}
		a.monitors[projectID] = items
	}
	if normalized {
		if err := a.saveMonitorsLocked(); err != nil {
			return err
		}
	}
	return nil
}

func gate3MonitorTrigger(kind monitordomain.TriggerKind) bool {
	return kind == monitordomain.TriggerRuntimeEvent ||
		kind == monitordomain.TriggerProcessExit ||
		kind == monitordomain.TriggerProcessOutput ||
		kind == monitordomain.TriggerScriptProbe
}

func (a *app) saveMonitorsLocked() error {
	return a.saveJSON("monitors.json", monitorRegistrySnapshot{
		SchemaVersion:     2,
		Monitors:          a.monitors,
		ProbeReservations: a.monitorProbeReservations,
		ProbeChallenges:   a.monitorProbeChallenges,
		ProbeReceipts:     a.monitorProbeReceipts,
		ProbeSessions:     a.monitorProbeSessions,
	}, 0600)
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
	a.agentRuntimeLocked().backgroundOwnerMu.Lock()
	defer a.agentRuntimeLocked().backgroundOwnerMu.Unlock()
	if item.Trigger.Kind == monitordomain.TriggerScriptProbe {
		if !scriptProbeSupported {
			return Monitor{}, errScriptProbeUnsupported
		}
		return Monitor{}, errors.New("dev_turn_required: script_probe requires a claimed approval receipt")
	}
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
		output := a.processOutputRuntime
		if output.baselines == nil {
			output.baselines = map[string]uint64{}
		}
		if output.cursors == nil {
			output.cursors = map[string]uint64{}
		}
		key := projectAgentKey(project.ID, item.ID)
		output.baselines[key] = outputBaseline
		output.cursors[key] = outputBaseline
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
	output := a.processOutputRuntime
	output.monitorOnce.Do(func() {
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
				case observation := <-output.monitorCh:
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
	output := a.processOutputRuntime
	drained := make(chan struct{})
	select {
	case <-a.supervisorCtx.Done():
		return a.supervisorCtx.Err()
	case output.monitorCh <- processOutputObservation{Drained: drained}:
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
	output := a.processOutputRuntime
	if line.Sequence == 0 || output.monitorCh == nil {
		return
	}
	item := processOutputObservation{ProjectID: record.ProjectID, ProcessID: record.ID, Line: line}
	select {
	case output.monitorCh <- item:
	default:
		a.recordProcessOutputGap(record.ProjectID, record.ID, line.Sequence)
	}
}

func (a *app) recordProcessOutputGap(projectID, processID string, sequence uint64) {
	if sequence == 0 {
		return
	}
	key := projectAgentKey(projectID, processID)
	output := a.processOutputRuntime
	output.gapMu.Lock()
	delta := output.pendingGaps[key]
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
	output.pendingGaps[key] = delta
	output.gapMu.Unlock()
	select {
	case output.gapWake <- struct{}{}:
	default:
	}
}

func (a *app) setOutputBaseline(projectID, monitorID string, sequence uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	output := a.processOutputRuntime
	if output.baselines == nil {
		output.baselines = map[string]uint64{}
	}
	if output.cursors == nil {
		output.cursors = map[string]uint64{}
	}
	key := projectAgentKey(projectID, monitorID)
	output.baselines[key] = sequence
	output.cursors[key] = sequence
}

func (a *app) evaluateProcessOutput(observation processOutputObservation) {
	a.mu.Lock()
	output := a.processOutputRuntime
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
		if observation.Line.Sequence <= output.cursors[key] {
			continue
		}
		output.cursors[key] = observation.Line.Sequence
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
	a.agentRuntimeLocked().backgroundOwnerMu.Lock()
	defer a.agentRuntimeLocked().backgroundOwnerMu.Unlock()
	a.mu.Lock()
	items := a.monitors[project.ID]
	for i := range items {
		if items[i].ID != id {
			continue
		}
		before := items[i]
		if state == monitordomain.StateActive && items[i].ErrorCode == "owner_deleted" {
			a.mu.Unlock()
			return Monitor{}, errors.New("owner-deleted monitor cannot be resumed")
		}
		if state == monitordomain.StateActive &&
			items[i].ErrorCode == "probe_authorization" {
			a.mu.Unlock()
			return Monitor{}, errors.New(
				"probe authorization error requires a newly approved trigger revision",
			)
		}
		if state == monitordomain.StateActive &&
			items[i].ErrorCode == "source_gap" {
			barriers := make(map[string]uint64, len(items[i].SourceGaps))
			for key, gap := range items[i].SourceGaps {
				barriers[key] = gap.ResumeAfterVersion
			}
			enabled, err := monitordomain.EnableAfterSourceGaps(
				items[i],
				barriers,
				time.Now().UTC(),
			)
			if err != nil {
				a.mu.Unlock()
				return Monitor{}, err
			}
			items[i] = enabled
		}
		if items[i].Trigger.Kind == monitordomain.TriggerScriptProbe &&
			state == monitordomain.StateActive {
			if !scriptProbeSupported {
				a.mu.Unlock()
				return Monitor{}, errScriptProbeUnsupported
			}
			if a.enabledScriptProbeCountLocked(project.ID, id) >= maximumEnabledProbes {
				a.mu.Unlock()
				return Monitor{}, errors.New("enabled script probe capacity exceeded")
			}
			receipt, ok := a.monitorProbeReceipts[items[i].Trigger.ApprovalReceiptID]
			if !ok {
				a.mu.Unlock()
				return Monitor{}, errors.New("probe approval receipt not found")
			}
			if _, err := a.authorizedMonitorProbeSnapshot(items[i], receipt); err != nil {
				a.mu.Unlock()
				return Monitor{}, err
			}
			items[i].ErrorCode = ""
			items[i].LastError = ""
			items[i].ConsecutiveProbeErrors = 0
		}
		items[i].State, items[i].UpdatedAt = state, time.Now().UTC()
		if items[i].Trigger.Kind == monitordomain.TriggerScriptProbe {
			next := time.Now().UTC().Add(
				time.Duration(items[i].Trigger.IntervalMS) * time.Millisecond,
			)
			items[i].NextCheckAt = &next
		}
		a.monitors[project.ID] = items
		if err := a.saveMonitorsLocked(); err != nil {
			items[i] = before
			a.monitors[project.ID] = items
			a.mu.Unlock()
			return Monitor{}, err
		}
		updated := items[i]
		a.mu.Unlock()
		if updated.Trigger.Kind == monitordomain.TriggerScriptProbe {
			if state == monitordomain.StateActive {
				a.armMonitorProbe(updated)
			} else {
				a.cancelMonitorProbe(project.ID, id)
			}
		}
		return updated, nil
	}
	a.mu.Unlock()
	return Monitor{}, errors.New("monitor not found")
}

func (a *app) deleteMonitor(project Project, id string) error {
	a.agentRuntimeLocked().backgroundOwnerMu.Lock()
	defer a.agentRuntimeLocked().backgroundOwnerMu.Unlock()
	a.mu.Lock()
	items := a.monitors[project.ID]
	for i := range items {
		if items[i].ID != id {
			continue
		}
		before := append([]Monitor(nil), items...)
		deleted := items[i]
		beforeReceipt, hadReceipt := a.monitorProbeReceipts[deleted.Trigger.ApprovalReceiptID]
		beforeReservations := make(map[string]monitorProbeReservation)
		for reservationID, reservation := range a.monitorProbeReservations {
			if reservation.ProjectID == project.ID &&
				reservation.MonitorID == id {
				beforeReservations[reservationID] = reservation
				delete(a.monitorProbeReservations, reservationID)
			}
		}
		if hadReceipt {
			receipt := beforeReceipt
			receipt.RevokedAt = timePointer(time.Now().UTC())
			a.monitorProbeReceipts[receipt.ID] = receipt
		}
		a.monitors[project.ID] = append(items[:i:i], items[i+1:]...)
		if err := a.saveMonitorsLocked(); err != nil {
			a.monitors[project.ID] = before
			if hadReceipt {
				a.monitorProbeReceipts[beforeReceipt.ID] = beforeReceipt
			}
			for reservationID, reservation := range beforeReservations {
				a.monitorProbeReservations[reservationID] = reservation
			}
			a.mu.Unlock()
			return err
		}
		a.mu.Unlock()
		if deleted.Trigger.Kind == monitordomain.TriggerScriptProbe {
			a.cancelMonitorProbe(project.ID, id)
		}
		return nil
	}
	a.mu.Unlock()
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
	a.agentRuntimeLocked().backgroundOwnerMu.Lock()
	defer a.agentRuntimeLocked().backgroundOwnerMu.Unlock()
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
		if item.State != monitordomain.StateActive || a.agentRuntimeLocked().backgroundOwnerDeleting[projectAgentKey(projectID, item.AgentID)] {
			return false
		}
		return targetID == "" || !a.agentRuntimeLocked().backgroundOwnerDeleting[projectAgentKey(projectID, targetID)]
	}
	return false
}

func (a *app) admitMonitorBlackboard(project Project, monitorID string, pending monitordomain.PendingFire) monitordomain.Admission {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, entry := range a.collaborationServiceLocked().BlackboardFor(project.ID) {
		if entry.SourceType == "monitor" && entry.SourceID == pending.ID {
			return monitordomain.AdmissionAlreadyPresent
		}
	}
	now := time.Now().UTC()
	entry := AgentBlackboardEntry{ID: randomID(), ProjectID: project.ID, AgentName: "Monitor", ActivityKind: "monitor", Summary: pending.Action.Topic, Detail: pending.Action.RenderedBriefing, SourceType: "monitor", SourceID: pending.ID, CreatedAt: now, UpdatedAt: now, Status: "active", RequiresAction: false}
	previous := a.collaborationServiceLocked().BlackboardFor(project.ID)
	a.collaborationServiceLocked().AppendBlackboard(project.ID, entry)
	if err := a.saveJSON("agent-blackboard.json", a.collaborationServiceLocked().BlackboardSnapshot(), 0644); err != nil {
		a.collaborationServiceLocked().ReplaceProjectBlackboard(project.ID, previous)
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
	if item.Trigger.Kind == monitordomain.TriggerScriptProbe ||
		strings.TrimSpace(toolStringArg(args, "approval_receipt_id", 128)) != "" {
		receiptID := firstNonEmpty(
			toolStringArg(args, "approval_receipt_id", 128),
			item.Trigger.ApprovalReceiptID,
		)
		created, err := a.claimScriptProbeMonitor(
			toolCtx.Project,
			toolCtx.Agent,
			item,
			receiptID,
			toolStringArg(args, "mutation_id", 128),
		)
		if err != nil {
			return toolJSON(map[string]any{"error": "probe_approval_error", "message": err.Error()})
		}
		return toolJSON(map[string]any{"monitor": publicMonitor(created)})
	}
	created, err := a.createMonitor(toolCtx.Project, item)
	if err != nil {
		return toolJSON(map[string]any{"error": "validation_error", "message": err.Error()})
	}
	return toolJSON(map[string]any{"monitor": publicMonitor(created)})
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
	return toolJSON(map[string]any{"monitor": publicMonitor(updated)})
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
