package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
)

func (a *app) claimScriptProbeMonitor(
	project Project,
	owner Agent,
	item Monitor,
	receiptID, mutationID string,
) (Monitor, error) {
	if !scriptProbeSupported {
		return Monitor{}, errScriptProbeUnsupported
	}
	receiptID = strings.TrimSpace(receiptID)
	mutationID = strings.TrimSpace(mutationID)
	if receiptID == "" || mutationID == "" {
		return Monitor{}, errors.New("script probe requires approval_receipt_id and mutation_id")
	}
	a.backgroundOwnerMu.Lock()
	defer a.backgroundOwnerMu.Unlock()
	if err := a.requireMonitorOwner(project, owner.ID); err != nil {
		return Monitor{}, err
	}
	if item.Action.Kind == monitordomain.ActionNotifyAgent {
		if _, ok := a.projectAgent(project, item.Action.AgentID); !ok {
			return Monitor{}, errors.New("monitor notify target agent not found")
		}
	}

	a.mu.Lock()
	receipt, ok := a.monitorProbeReceipts[receiptID]
	if !ok {
		a.mu.Unlock()
		return Monitor{}, errors.New("probe approval receipt not found")
	}
	interpreter := "bash"
	if receipt.Language == "javascript" {
		interpreter = "node"
	}
	if _, err := exec.LookPath(interpreter); err != nil {
		a.mu.Unlock()
		return Monitor{}, errors.New("probe interpreter is unavailable")
	}
	claimed, err := monitordomain.ClaimProbeApproval(
		receipt,
		project.ID,
		owner.ID,
		receipt.MonitorID,
		receipt.TriggerRevision,
		mutationID,
		time.Now().UTC(),
	)
	if err != nil {
		a.mu.Unlock()
		return Monitor{}, err
	}
	item.ProjectID = project.ID
	item.AgentID = owner.ID
	if strings.TrimSpace(item.ID) == "" {
		item.ID = receipt.MonitorID
	}
	if item.ID != receipt.MonitorID {
		a.mu.Unlock()
		return Monitor{}, monitordomain.ErrApprovalSubjectMismatch
	}
	existingIndex := -1
	var existing Monitor
	for index, current := range a.monitors[project.ID] {
		if current.ID == item.ID {
			existingIndex = index
			existing = current
			break
		}
	}
	if receipt.ClaimedMutationID == mutationID &&
		existingIndex >= 0 &&
		existing.Trigger.Kind == monitordomain.TriggerScriptProbe &&
		existing.Trigger.ApprovalReceiptID == receipt.ID &&
		existing.Trigger.Revision == receipt.TriggerRevision {
		a.mu.Unlock()
		return existing, nil
	}
	expectedRevision := 1
	if existingIndex >= 0 {
		if existing.AgentID != owner.ID {
			a.mu.Unlock()
			return Monitor{}, errors.New("probe monitor owner mismatch")
		}
		if item.Revision != 0 && item.Revision != existing.Revision {
			a.mu.Unlock()
			return Monitor{}, errors.New("monitor revision conflict")
		}
		expectedRevision = existing.Trigger.Revision + 1
	}
	if receipt.TriggerRevision != expectedRevision {
		a.mu.Unlock()
		return Monitor{}, errors.New("probe trigger revision is stale")
	}
	if existingIndex < 0 && len(a.monitors[project.ID]) >= 100 {
		a.mu.Unlock()
		return Monitor{}, errors.New("monitor definition capacity exceeded")
	}
	item.Trigger = monitordomain.Trigger{
		Kind:              monitordomain.TriggerScriptProbe,
		Revision:          receipt.TriggerRevision,
		ProbeLanguage:     receipt.Language,
		ProbePath:         receipt.SnapshotPath,
		ProbeSHA256:       receipt.SourceSHA256,
		IntervalMS:        receipt.IntervalMS,
		TimeoutMS:         receipt.TimeoutMS,
		ApprovalReceiptID: receipt.ID,
	}
	now := time.Now().UTC()
	if item.Name = strings.TrimSpace(item.Name); item.Name == "" {
		item.Name = "Script probe"
	}
	if item.Revision == 0 {
		item.Revision = 1
	}
	if item.Action.Revision == 0 {
		item.Action.Revision = 1
	}
	if item.State == "" {
		item.State = monitordomain.StateActive
	}
	if item.State != monitordomain.StateActive &&
		item.State != monitordomain.StateDisabled {
		a.mu.Unlock()
		return Monitor{}, errors.New("new script probe state must be active or disabled")
	}
	if existingIndex >= 0 {
		item.Revision = existing.Revision + 1
		item.CreatedAt = existing.CreatedAt
		item.TriggerCount = existing.TriggerCount
		item.Sequence = existing.Sequence
		item.LastFiredAt = existing.LastFiredAt
		item.RecentFires = append([]time.Time(nil), existing.RecentFires...)
		item.PendingFires = append(
			[]monitordomain.PendingFire(nil),
			existing.PendingFires...,
		)
	} else {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	next := now.Add(time.Duration(receipt.IntervalMS) * time.Millisecond)
	item.NextCheckAt = &next
	if item.State == monitordomain.StateActive &&
		a.enabledScriptProbeCountLocked(project.ID, item.ID) >= maximumEnabledProbes {
		a.mu.Unlock()
		return Monitor{}, errors.New("enabled script probe capacity exceeded")
	}

	store, err := newSecureRuntimeStore(a.settings.DataDir)
	if err != nil {
		a.mu.Unlock()
		return Monitor{}, err
	}
	snapshot, err := readMonitorProbeSnapshot(store, receipt.SnapshotPath)
	if err != nil {
		a.mu.Unlock()
		return Monitor{}, err
	}
	check := monitordomain.ProbeAuthorizationCheck{
		MutationID:       mutationID,
		CanonicalWorkdir: receipt.CanonicalWorkdir,
		SnapshotPath:     receipt.SnapshotPath,
		SnapshotDevice:   snapshot.Device,
		SnapshotInode:    snapshot.Inode,
		ModePerm:         uint32(snapshot.Mode.Perm()),
		Regular:          true,
		RuntimeOwned:     snapshot.OwnerUID == uint32(os.Geteuid()),
		Source:           snapshot.Source,
	}
	if err := monitordomain.ProbeAuthorized(item, claimed, check); err != nil {
		a.mu.Unlock()
		return Monitor{}, err
	}
	if err := monitordomain.ValidateMonitor(item); err != nil {
		a.mu.Unlock()
		return Monitor{}, err
	}

	beforeMonitors := cloneMonitorList(a.monitors[project.ID])
	beforeReceipts := make(
		map[string]monitordomain.ProbeApprovalReceipt,
		len(a.monitorProbeReceipts),
	)
	for id, current := range a.monitorProbeReceipts {
		beforeReceipts[id] = current
	}
	beforeReservations := make(
		map[string]monitorProbeReservation,
		len(a.monitorProbeReservations),
	)
	for id, current := range a.monitorProbeReservations {
		beforeReservations[id] = current
	}
	a.monitorProbeReceipts[receiptID] = claimed
	if existingIndex >= 0 {
		if oldReceipt, ok := a.monitorProbeReceipts[existing.Trigger.ApprovalReceiptID]; ok &&
			oldReceipt.ID != claimed.ID {
			oldReceipt.RevokedAt = timePointer(now)
			a.monitorProbeReceipts[oldReceipt.ID] = oldReceipt
		}
		a.monitors[project.ID][existingIndex] = item
	} else {
		a.monitors[project.ID] = append(a.monitors[project.ID], item)
	}
	for id, reservation := range a.monitorProbeReservations {
		if reservation.ProjectID == project.ID &&
			reservation.MonitorID == item.ID &&
			reservation.TriggerRevision == item.Trigger.Revision {
			delete(a.monitorProbeReservations, id)
		}
	}
	if err := a.saveMonitorsLocked(); err != nil {
		a.monitorProbeReceipts = beforeReceipts
		a.monitorProbeReservations = beforeReservations
		a.monitors[project.ID] = beforeMonitors
		a.mu.Unlock()
		return Monitor{}, err
	}
	a.mu.Unlock()
	if item.State == monitordomain.StateActive {
		a.armMonitorProbe(item)
	}
	return item, nil
}

func (a *app) enabledScriptProbeCountLocked(
	projectID, exceptID string,
) int {
	count := 0
	for _, item := range a.monitors[projectID] {
		if item.ID != exceptID &&
			item.Trigger.Kind == monitordomain.TriggerScriptProbe &&
			item.State == monitordomain.StateActive {
			count++
		}
	}
	return count
}
