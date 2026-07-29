package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
)

type monitorProbeCheckResult struct {
	Matched bool   `json:"matched"`
	Detail  string `json:"detail,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (a *app) armMonitorProbes() {
	for _, item := range a.monitorsForAllProjects() {
		if item.Trigger.Kind == monitordomain.TriggerScriptProbe &&
			item.State == monitordomain.StateActive {
			a.armMonitorProbe(item)
		}
	}
}

func (a *app) armMonitorProbe(item Monitor) {
	if !scriptProbeSupported ||
		item.Trigger.Kind != monitordomain.TriggerScriptProbe ||
		item.State != monitordomain.StateActive {
		return
	}
	key := projectAgentKey(item.ProjectID, item.ID)
	a.mu.Lock()
	if a.monitorProbeStopping {
		a.mu.Unlock()
		return
	}
	if cancel := a.monitorProbeCancels[key]; cancel != nil {
		cancel()
	}
	ctx, cancel := context.WithCancel(a.monitorCtx)
	a.monitorProbeCancels[key] = cancel
	a.monitorProbeWG.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.monitorProbeWG.Done()
		a.monitorProbeLoop(ctx, item.ProjectID, item.ID)
	}()
}

func (a *app) monitorProbeLoop(
	ctx context.Context,
	projectID, monitorID string,
) {
	for {
		item, ok := a.monitorByID(projectID, monitorID)
		if !ok || item.Trigger.Kind != monitordomain.TriggerScriptProbe ||
			item.State != monitordomain.StateActive {
			return
		}
		interval := time.Duration(item.Trigger.IntervalMS) * time.Millisecond
		if interval < time.Duration(minimumMonitorProbeInterval)*time.Millisecond {
			interval = time.Duration(defaultMonitorProbeInterval) * time.Millisecond
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case scheduled := <-timer.C:
			_, _ = a.runMonitorProbe(ctx, projectID, monitorID, scheduled, false)
		}
	}
}

func (a *app) runMonitorProbe(
	ctx context.Context,
	projectID, monitorID string,
	scheduled time.Time,
	dryRun bool,
) (monitorProbeCheckResult, error) {
	if !scriptProbeSupported {
		return monitorProbeCheckResult{}, errScriptProbeUnsupported
	}
	item, ok := a.monitorByID(projectID, monitorID)
	if !ok || item.Trigger.Kind != monitordomain.TriggerScriptProbe {
		return monitorProbeCheckResult{}, errors.New("script probe monitor not found")
	}
	if !dryRun && item.State != monitordomain.StateActive {
		return monitorProbeCheckResult{}, errors.New("script probe is not active")
	}
	slot := a.monitorProbeProjectSlot(projectID)
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	default:
		if !dryRun {
			a.recordMonitorProbeResourceSuppressed(projectID, monitorID)
		}
		return monitorProbeCheckResult{}, errors.New("probe concurrency capacity reached")
	}
	a.mu.Lock()
	runningIndex := -1
	for index := range a.monitors[projectID] {
		candidate := a.monitors[projectID][index]
		if candidate.ID != monitorID ||
			candidate.Trigger.Kind != monitordomain.TriggerScriptProbe {
			continue
		}
		if candidate.ProbeRunning {
			a.mu.Unlock()
			return monitorProbeCheckResult{}, errors.New("probe check already running")
		}
		runningIndex = index
		item = candidate
		break
	}
	if runningIndex < 0 {
		a.mu.Unlock()
		return monitorProbeCheckResult{}, errors.New("script probe monitor not found")
	}
	a.monitors[projectID][runningIndex].ProbeRunning = true
	receipt, ok := a.monitorProbeReceipts[item.Trigger.ApprovalReceiptID]
	if !ok {
		a.monitors[projectID][runningIndex].ProbeRunning = false
		a.mu.Unlock()
		err := errors.New("probe approval receipt is unavailable")
		if !dryRun {
			a.failMonitorProbeAuthorization(projectID, monitorID, err)
		}
		return monitorProbeCheckResult{}, err
	}
	a.mu.Unlock()
	defer a.clearMonitorProbeRunning(projectID, monitorID)
	snapshot, err := a.authorizedMonitorProbeSnapshot(item, receipt)
	if err != nil {
		if !dryRun {
			a.failMonitorProbeAuthorization(projectID, monitorID, err)
		}
		return monitorProbeCheckResult{Error: err.Error()}, err
	}
	execution, _, runErr := executeMonitorProbe(
		ctx,
		item.Trigger.ProbeLanguage,
		receipt.CanonicalWorkdir,
		snapshot.Source,
		time.Duration(item.Trigger.TimeoutMS)*time.Millisecond,
	)
	result, resultErr := monitordomain.ParseProbeResult(execution)
	if runErr != nil {
		resultErr = runErr
	}
	out := monitorProbeCheckResult{
		Matched: result.Matched,
		Detail:  result.Detail,
	}
	if resultErr != nil {
		out.Error = resultErr.Error()
	}
	if dryRun {
		return out, resultErr
	}
	if err := a.commitMonitorProbeResult(
		projectID,
		monitorID,
		item.Trigger.Revision,
		scheduled,
		result,
		resultErr,
	); err != nil {
		return out, err
	}
	return out, resultErr
}

func (a *app) authorizedMonitorProbeSnapshot(
	item Monitor,
	receipt monitordomain.ProbeApprovalReceipt,
) (monitorProbeSnapshot, error) {
	store, err := newSecureRuntimeStore(a.settings.DataDir)
	if err != nil {
		return monitorProbeSnapshot{}, err
	}
	snapshot, err := readMonitorProbeSnapshot(store, receipt.SnapshotPath)
	if err != nil {
		return monitorProbeSnapshot{}, err
	}
	workdir, err := canonicalResidentWorkdir(receipt.CanonicalWorkdir)
	if err != nil || workdir != receipt.CanonicalWorkdir {
		return monitorProbeSnapshot{}, errors.New("probe canonical workdir changed")
	}
	check := monitordomain.ProbeAuthorizationCheck{
		MutationID:       receipt.ClaimedMutationID,
		CanonicalWorkdir: workdir,
		SnapshotPath:     receipt.SnapshotPath,
		SnapshotDevice:   snapshot.Device,
		SnapshotInode:    snapshot.Inode,
		ModePerm:         uint32(snapshot.Mode.Perm()),
		Regular:          true,
		RuntimeOwned:     snapshot.OwnerUID == uint32(os.Geteuid()),
		Source:           snapshot.Source,
	}
	if err := monitordomain.ProbeAuthorized(item, receipt, check); err != nil {
		return monitorProbeSnapshot{}, err
	}
	return snapshot, nil
}

func (a *app) commitMonitorProbeResult(
	projectID, monitorID string,
	triggerRevision int,
	scheduled time.Time,
	result monitordomain.ProbeResult,
	resultErr error,
) error {
	var fire *monitorFireRef
	a.mu.Lock()
	items := a.monitors[projectID]
	before := cloneMonitorList(items)
	for index := range items {
		item := items[index]
		if item.ID != monitorID {
			continue
		}
		if item.Trigger.Kind != monitordomain.TriggerScriptProbe ||
			item.Trigger.Revision != triggerRevision ||
			item.State != monitordomain.StateActive {
			a.mu.Unlock()
			return nil
		}
		updated, matched, detail := monitordomain.MatchProbe(
			item,
			result,
			resultErr,
			time.Now().UTC(),
		)
		if resultErr == nil && updated.ErrorCode == "resource_suppressed" {
			updated.ErrorCode = ""
		}
		items[index] = updated
		if matched {
			decision := monitordomain.Fire(updated, detail, time.Now().UTC())
			items[index] = decision.Monitor
			if decision.Fire {
				payload, _ := json.Marshal(map[string]any{
					"matched": true,
					"detail":  detail,
				})
				event := monitordomain.Event{
					ID: fmt.Sprintf(
						"monitor/%s/probe/%d",
						monitorID,
						scheduled.UnixNano(),
					),
					ProjectID:           projectID,
					AuthorityID:         "monitor_probe",
					AuthorityGeneration: uint64(triggerRevision),
					Kind:                "script_probe",
					EntityID:            monitorID,
					Origin:              monitordomain.Origin{Kind: "runtime"},
					At:                  scheduled,
					Payload:             payload,
				}
				briefing := strings.TrimSpace(items[index].Action.Template)
				if briefing == "" {
					briefing = detail
				}
				pending, err := monitordomain.FreezePendingFire(
					items[index],
					event,
					briefing,
					payload,
					json.RawMessage(`{}`),
					time.Now().UTC(),
				)
				if err != nil {
					items[index].LastError = limitString(err.Error(), 1024)
				} else if admitted, err := monitordomain.AdmitPendingFire(
					items[index],
					pending,
				); err == nil {
					items[index] = admitted
					ref := monitorFireRef{projectID, monitorID, pending.ID}
					fire = &ref
				} else {
					items[index].LastError = limitString(err.Error(), 1024)
				}
			}
		}
		a.monitors[projectID] = items
		if err := a.saveMonitorsLocked(); err != nil {
			a.monitors[projectID] = before
			a.mu.Unlock()
			return err
		}
		a.mu.Unlock()
		if fire != nil {
			a.dispatchMonitorPending(*fire)
		}
		return nil
	}
	a.mu.Unlock()
	return nil
}

func (a *app) failMonitorProbeAuthorization(
	projectID, monitorID string,
	cause error,
) {
	a.mu.Lock()
	defer a.mu.Unlock()
	items := a.monitors[projectID]
	before := cloneMonitorList(items)
	for index := range items {
		if items[index].ID != monitorID {
			continue
		}
		items[index].State = monitordomain.StateError
		items[index].ErrorCode = "probe_authorization"
		items[index].LastError = limitString(cause.Error(), 1024)
		items[index].UpdatedAt = time.Now().UTC()
		a.monitors[projectID] = items
		if err := a.saveMonitorsLocked(); err != nil {
			a.monitors[projectID] = before
		}
		return
	}
}

func (a *app) recordMonitorProbeResourceSuppressed(
	projectID, monitorID string,
) {
	a.mu.Lock()
	defer a.mu.Unlock()
	items := a.monitors[projectID]
	before := cloneMonitorList(items)
	for index := range items {
		if items[index].ID != monitorID {
			continue
		}
		items[index].LastError = "probe concurrency capacity reached"
		items[index].ErrorCode = "resource_suppressed"
		items[index].UpdatedAt = time.Now().UTC()
		a.monitors[projectID] = items
		if err := a.saveMonitorsLocked(); err != nil {
			a.monitors[projectID] = before
		}
		return
	}
}

func (a *app) monitorProbeProjectSlot(projectID string) chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	slot := a.monitorProbeProjectSlots[projectID]
	if slot == nil {
		slot = make(chan struct{}, maximumProbeConcurrency)
		a.monitorProbeProjectSlots[projectID] = slot
	}
	return slot
}

func (a *app) clearMonitorProbeRunning(projectID, monitorID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for index := range a.monitors[projectID] {
		if a.monitors[projectID][index].ID == monitorID {
			a.monitors[projectID][index].ProbeRunning = false
			return
		}
	}
}

func (a *app) monitorByID(projectID, monitorID string) (Monitor, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, item := range a.monitors[projectID] {
		if item.ID == monitorID {
			return item, true
		}
	}
	return Monitor{}, false
}

func (a *app) cancelMonitorProbe(projectID, monitorID string) {
	key := projectAgentKey(projectID, monitorID)
	a.mu.Lock()
	cancel := a.monitorProbeCancels[key]
	delete(a.monitorProbeCancels, key)
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *app) shutdownMonitorProbes() {
	a.monitorCancel()
	a.mu.Lock()
	a.monitorProbeStopping = true
	cancels := make([]context.CancelFunc, 0, len(a.monitorProbeCancels))
	for _, cancel := range a.monitorProbeCancels {
		cancels = append(cancels, cancel)
	}
	a.monitorProbeCancels = map[string]context.CancelFunc{}
	a.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	a.monitorProbeWG.Wait()
}
