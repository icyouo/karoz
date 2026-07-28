package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
	processdomain "github.com/karoz/karoz/internal/process"
)

func (runtime *processRuntimePersistence) recoverProjectLocked(
	project *processProjectRuntime,
) error {
	operationIDs := make([]string, 0, len(project.journal.Operations))
	for id, operation := range project.journal.Operations {
		if operation.Kind == "process_terminal_release" {
			operationIDs = append(operationIDs, id)
		}
	}
	for id, operation := range project.journal.Operations {
		if operation.Kind != "process_terminal_release" {
			operationIDs = append(operationIDs, id)
		}
	}
	for _, id := range operationIDs {
		operation, exists := project.journal.Operations[id]
		if !exists {
			continue
		}
		switch operation.Kind {
		case "process_admission":
			if err := runtime.recoverAdmissionLocked(project, operation); err != nil {
				return err
			}
		case "process_terminal_release":
			if err := runtime.recoverReleaseLocked(project, operation); err != nil {
				return err
			}
		default:
			return errors.New("unknown runtime mutation operation kind")
		}
	}
	return runtime.validateProjectBijection(project)
}

func (runtime *processRuntimePersistence) recoverAdmissionLocked(
	project *processProjectRuntime,
	operation monitordomain.RuntimeMutationOperation,
) error {
	project.ledgerMu.Lock()
	ledgerReservation, ledgerExists := reservationByOperation(project.ledger, operation.ID)
	project.ledgerMu.Unlock()
	runtime.authorityMu.Lock()
	partition := runtime.authority.Projects[project.identity.SafeProjectKey]
	authority, authorityExists := partition.Records[operation.EntityID]
	runtime.authorityMu.Unlock()

	if !authorityExists {
		if !ledgerExists {
			return runtime.deleteOperation(project, operation.ID)
		}
		if ledgerReservation.State != monitordomain.ReservationAllocating {
			return errors.New("active ledger reservation has no process authority record")
		}
		project.ledgerMu.Lock()
		next, err := project.ledger.RollbackAllocation(project.identity, ledgerReservation)
		if err == nil {
			project.ledger = next
			err = runtime.saveLedgerLocked(project)
		}
		project.ledgerMu.Unlock()
		if err != nil {
			return err
		}
		return runtime.deleteOperation(project, operation.ID)
	}
	if authority.Reservation == nil || !ledgerExists ||
		authority.Reservation.Token != ledgerReservation.Token ||
		authority.Reservation.OperationID != operation.ID {
		return errors.New("process admission ledger/authority mismatch")
	}
	if ledgerReservation.State == monitordomain.ReservationAllocating {
		active, err := runtime.transitionLedger(project, ledgerReservation, monitordomain.ReservationActive)
		if err != nil {
			return err
		}
		ledgerReservation = active
	}
	targetState := monitordomain.ReservationActive
	if authority.Process.State.Terminal() {
		targetState = authority.Reservation.State
		if targetState != monitordomain.ReservationTerminalUnacknowledged &&
			targetState != monitordomain.ReservationReleasing {
			return errors.New("terminal process has invalid reservation state")
		}
	}
	if ledgerReservation.State == monitordomain.ReservationActive &&
		(targetState == monitordomain.ReservationTerminalUnacknowledged ||
			targetState == monitordomain.ReservationReleasing) {
		next, err := runtime.transitionLedger(
			project, ledgerReservation, monitordomain.ReservationTerminalUnacknowledged,
		)
		if err != nil {
			return err
		}
		ledgerReservation = next
	}
	if ledgerReservation.State != targetState {
		next, err := runtime.transitionLedger(project, ledgerReservation, targetState)
		if err != nil {
			return err
		}
		ledgerReservation = next
	}
	if authority.Reservation.State != ledgerReservation.State {
		if err := runtime.updateAuthorityReservation(
			project.identity, operation.EntityID, ledgerReservation,
		); err != nil {
			return err
		}
	}
	if operation.State != "committed" {
		operation.State = "committed"
		operation.ReservationToken = ledgerReservation.Token
		if err := runtime.putOperation(project, operation); err != nil {
			return err
		}
	}
	return nil
}

func (runtime *processRuntimePersistence) validateProjectBijection(
	project *processProjectRuntime,
) error {
	runtime.authorityMu.Lock()
	partition := runtime.authority.Projects[project.identity.SafeProjectKey]
	records := make(map[string]durableProcessRecord, len(partition.Records))
	for id, record := range partition.Records {
		records[id] = record
	}
	runtime.authorityMu.Unlock()
	project.ledgerMu.Lock()
	ledger := project.ledger
	project.ledgerMu.Unlock()

	seenTokens := make(map[string]bool, len(records))
	for id, record := range records {
		if record.Reservation == nil {
			if !record.Process.State.Terminal() || record.Event != nil {
				return fmt.Errorf("process %s has invalid detached reservation", id)
			}
			continue
		}
		reservation := *record.Reservation
		ledgerReservation, exists := ledger.Slots[reservation.Slot]
		if !exists || !monitordomain.SameTerminalReservation(
			project.identity, reservation, ledgerReservation,
		) {
			return fmt.Errorf("process %s reservation is not bijective", id)
		}
		seenTokens[reservation.Token] = true
	}
	for _, reservation := range ledger.Slots {
		if reservation.AuthorityID != processAuthorityID || !seenTokens[reservation.Token] {
			return errors.New("terminal ledger contains orphan/non-process reservation")
		}
	}
	return nil
}

func (runtime *processRuntimePersistence) recoverInterruptedProcesses() error {
	now := runtime.now()
	type transition struct {
		projectID   string
		reservation monitordomain.TerminalReservation
	}
	transitions := make([]transition, 0)
	runtime.authorityMu.Lock()
	changed := false
	for key, partition := range runtime.authority.Projects {
		for id, record := range partition.Records {
			if record.Process.State != processdomain.StateStarting &&
				record.Process.State != processdomain.StateRunning {
				continue
			}
			if record.Reservation == nil ||
				record.Reservation.State != monitordomain.ReservationActive {
				runtime.authorityMu.Unlock()
				return errors.New("recovered active process lacks active reservation")
			}
			normalized := processdomain.Normalize(record.Process, now)
			terminal := *record.Reservation
			terminal.State = monitordomain.ReservationTerminalUnacknowledged
			record.Process = normalized
			record.Reservation = &terminal
			record.Event = &processTerminalEvent{
				ID: processTerminalEventID(id), Kind: processTerminalEventKind,
				Project: partition.Project, ProcessID: id, State: normalized.State,
				ExitCode: normalized.ExitCode, OccurredAt: now, Reservation: terminal,
			}
			partition.Records[id] = record
			transitions = append(transitions, transition{
				projectID: partition.Project.ProjectID, reservation: *record.Reservation,
			})
			changed = true
		}
		runtime.authority.Projects[key] = partition
	}
	if changed {
		runtime.authority.Generation++
		if err := runtime.saveAuthorityLocked(); err != nil {
			runtime.authorityMu.Unlock()
			return err
		}
	}
	runtime.authorityMu.Unlock()
	for _, item := range transitions {
		project := runtime.projects[item.projectID]
		project.lane.Lock()
		_, err := runtime.transitionLedger(
			project, item.reservation, monitordomain.ReservationTerminalUnacknowledged,
		)
		project.lane.Unlock()
		if err != nil {
			return err
		}
	}
	for _, project := range runtime.projects {
		if err := runtime.validateProjectBijection(project); err != nil {
			return err
		}
	}
	return nil
}

func (runtime *processRuntimePersistence) AcknowledgeTerminal(
	projectID, processID, eventID string,
) error {
	project := runtime.projects[projectID]
	if project == nil {
		return errors.New("process project runtime is unavailable")
	}
	project.lane.Lock()
	defer project.lane.Unlock()
	return runtime.releaseTerminalLocked(project, processID, eventID)
}

func (runtime *processRuntimePersistence) releaseTerminalLocked(
	project *processProjectRuntime,
	processID, eventID string,
) error {
	if eventID != processTerminalEventID(processID) {
		return errors.New("terminal event acknowledgement identity mismatch")
	}
	alreadyReleased, err := runtime.validateTerminalAcknowledgement(
		project.identity, processID, eventID,
	)
	if err != nil || alreadyReleased {
		return err
	}
	operationID := "process/" + processID + "/release"
	operation, exists := project.journal.Operations[operationID]
	if !exists {
		operation = monitordomain.RuntimeMutationOperation{
			ID: operationID, Project: project.identity, Kind: "process_terminal_release",
			State: "release_intent", AuthorityID: processAuthorityID, EntityID: processID,
		}
		if err := runtime.putOperation(project, operation); err != nil {
			return err
		}
		if err := runtime.persistenceFail(processPersistAfterReleaseIntent); err != nil {
			return err
		}
	}
	if err := runtime.markAuthorityAcknowledged(project.identity, processID, eventID); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterAck); err != nil {
		return err
	}
	operation.State = "release_marked"
	if err := runtime.putOperation(project, operation); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterReleaseOperation); err != nil {
		return err
	}
	reservation, err := runtime.authorityReservation(project.identity, processID)
	if err != nil {
		return err
	}
	releasing, err := runtime.transitionLedger(
		project, reservation, monitordomain.ReservationReleasing,
	)
	if err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterReleaseMark); err != nil {
		return err
	}
	if err := runtime.updateAuthorityReservation(project.identity, processID, releasing); err != nil {
		return err
	}
	if err := runtime.detachAuthorityReservation(project.identity, processID); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterAuthorityDetach); err != nil {
		return err
	}
	operation.State = "authority_detached"
	if err := runtime.putOperation(project, operation); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterDetachedOperation); err != nil {
		return err
	}
	if err := runtime.releaseLedger(project, releasing); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterLedgerRelease); err != nil {
		return err
	}
	if err := runtime.deleteOperation(project, releasing.OperationID); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterAdmissionDelete); err != nil {
		return err
	}
	if err := runtime.deleteOperation(project, operationID); err != nil {
		return err
	}
	return runtime.applyRetentionLocked(project, runtime.retention, runtime.now())
}

func (runtime *processRuntimePersistence) validateTerminalAcknowledgement(
	identity monitordomain.RuntimeProjectIdentity,
	processID, eventID string,
) (bool, error) {
	runtime.authorityMu.Lock()
	defer runtime.authorityMu.Unlock()
	record, exists := runtime.authority.Projects[identity.SafeProjectKey].Records[processID]
	if !exists || !record.Process.State.Terminal() {
		return false, errors.New("terminal process record is missing")
	}
	if record.Event != nil {
		if record.Event.ID != eventID || record.Reservation == nil ||
			record.Reservation.State != monitordomain.ReservationTerminalUnacknowledged {
			return false, errors.New("terminal event acknowledgement comparison failed")
		}
		return false, nil
	}
	if record.AcknowledgedEventID != eventID {
		return false, errors.New("terminal acknowledgement marker mismatch")
	}
	return record.Reservation == nil, nil
}

func (runtime *processRuntimePersistence) recoverReleaseLocked(
	project *processProjectRuntime,
	operation monitordomain.RuntimeMutationOperation,
) error {
	runtime.authorityMu.Lock()
	partition := runtime.authority.Projects[project.identity.SafeProjectKey]
	record, recordExists := partition.Records[operation.EntityID]
	runtime.authorityMu.Unlock()
	if !recordExists {
		return errors.New("terminal release operation lost its process record")
	}
	if record.Event != nil {
		if err := runtime.markAuthorityAcknowledged(
			project.identity, operation.EntityID, record.Event.ID,
		); err != nil {
			return err
		}
	}
	if record.Reservation != nil {
		reservation := *record.Reservation
		if reservation.State == monitordomain.ReservationTerminalUnacknowledged {
			next, err := runtime.transitionLedger(
				project, reservation, monitordomain.ReservationReleasing,
			)
			if err != nil {
				return err
			}
			reservation = next
			if err := runtime.updateAuthorityReservation(
				project.identity, operation.EntityID, reservation,
			); err != nil {
				return err
			}
		}
		if err := runtime.detachAuthorityReservation(
			project.identity, operation.EntityID,
		); err != nil {
			return err
		}
		if err := runtime.releaseLedger(project, reservation); err != nil {
			return err
		}
	} else {
		project.ledgerMu.Lock()
		reservation, exists := reservationByEntity(project.ledger, processAuthorityID, operation.EntityID)
		project.ledgerMu.Unlock()
		if exists {
			if reservation.State == monitordomain.ReservationTerminalUnacknowledged {
				next, err := runtime.transitionLedger(
					project, reservation, monitordomain.ReservationReleasing,
				)
				if err != nil {
					return err
				}
				reservation = next
			}
			if err := runtime.releaseLedger(project, reservation); err != nil {
				return err
			}
		}
	}
	admissionID := "process/" + operation.EntityID + "/admit"
	if _, exists := project.journal.Operations[admissionID]; exists {
		if err := runtime.deleteOperation(project, admissionID); err != nil {
			return err
		}
	}
	return runtime.deleteOperation(project, operation.ID)
}

func (runtime *processRuntimePersistence) markAuthorityAcknowledged(
	identity monitordomain.RuntimeProjectIdentity,
	processID, eventID string,
) error {
	runtime.authorityMu.Lock()
	defer runtime.authorityMu.Unlock()
	partition := runtime.authority.Projects[identity.SafeProjectKey]
	record, exists := partition.Records[processID]
	if !exists || !record.Process.State.Terminal() {
		return errors.New("terminal process record is missing")
	}
	if record.Event == nil {
		if record.AcknowledgedEventID == eventID {
			return nil
		}
		return errors.New("terminal acknowledgement marker mismatch")
	}
	if record.Event.ID != eventID || record.Reservation == nil {
		return errors.New("terminal event acknowledgement comparison failed")
	}
	record.Event = nil
	record.AcknowledgedEventID = eventID
	partition.Records[processID] = record
	runtime.authority.Projects[identity.SafeProjectKey] = partition
	runtime.authority.Generation++
	return runtime.saveAuthorityLocked()
}

func (runtime *processRuntimePersistence) authorityReservation(
	identity monitordomain.RuntimeProjectIdentity,
	processID string,
) (monitordomain.TerminalReservation, error) {
	runtime.authorityMu.Lock()
	defer runtime.authorityMu.Unlock()
	record, exists := runtime.authority.Projects[identity.SafeProjectKey].Records[processID]
	if !exists || record.Reservation == nil {
		return monitordomain.TerminalReservation{}, errors.New("process authority reservation is missing")
	}
	return *record.Reservation, nil
}

func (runtime *processRuntimePersistence) detachAuthorityReservation(
	identity monitordomain.RuntimeProjectIdentity,
	processID string,
) error {
	runtime.authorityMu.Lock()
	defer runtime.authorityMu.Unlock()
	partition := runtime.authority.Projects[identity.SafeProjectKey]
	record, exists := partition.Records[processID]
	if !exists {
		return errors.New("process authority record is missing")
	}
	if record.Reservation == nil {
		return nil
	}
	if record.Reservation.State != monitordomain.ReservationReleasing || record.Event != nil {
		return errors.New("process authority reservation is not detachable")
	}
	record.Reservation = nil
	partition.Records[processID] = record
	runtime.authority.Projects[identity.SafeProjectKey] = partition
	runtime.authority.Generation++
	return runtime.saveAuthorityLocked()
}

func (runtime *processRuntimePersistence) releaseLedger(
	project *processProjectRuntime,
	reservation monitordomain.TerminalReservation,
) error {
	project.ledgerMu.Lock()
	defer project.ledgerMu.Unlock()
	current, exists := project.ledger.Slots[reservation.Slot]
	if !exists {
		return nil
	}
	if current.State != monitordomain.ReservationReleasing ||
		current.Token != reservation.Token {
		return errors.New("terminal reservation is not releasable")
	}
	next, err := project.ledger.Release(project.identity, current)
	if err != nil {
		return err
	}
	project.ledger = next
	return runtime.saveLedgerLocked(project)
}

func reservationByEntity(
	ledger monitordomain.TerminalReservationLedger,
	authorityID, entityID string,
) (monitordomain.TerminalReservation, bool) {
	for _, reservation := range ledger.Slots {
		if reservation.AuthorityID == authorityID && reservation.EntityID == entityID {
			return reservation, true
		}
	}
	return monitordomain.TerminalReservation{}, false
}

func (runtime *processRuntimePersistence) ApplyRetention(
	projectID string,
	policy processdomain.RetentionPolicy,
	now time.Time,
) error {
	project := runtime.projects[projectID]
	if project == nil {
		return errors.New("process project runtime is unavailable")
	}
	project.lane.Lock()
	defer project.lane.Unlock()
	return runtime.applyRetentionLocked(project, policy, now)
}

func (runtime *processRuntimePersistence) applyRetentionLocked(
	project *processProjectRuntime,
	policy processdomain.RetentionPolicy,
	now time.Time,
) error {
	runtime.authorityMu.Lock()
	partition := runtime.authority.Projects[project.identity.SafeProjectKey]
	candidates := make([]processdomain.RetentionRecord, 0, len(partition.Records))
	for _, record := range partition.Records {
		if record.Event != nil || record.Reservation != nil {
			continue
		}
		candidates = append(candidates, processdomain.RetentionRecord{Process: record.Process})
	}
	selected := processdomain.SelectRetention(candidates, policy, now)
	for _, item := range selected {
		partition.Tombstones[item.ID] = processTombstone{
			ProcessID: item.ID, LogPath: item.LogPath, CreatedAt: now,
		}
	}
	runtime.authority.Projects[project.identity.SafeProjectKey] = partition
	if len(selected) > 0 {
		runtime.authority.Generation++
		if err := runtime.saveAuthorityLocked(); err != nil {
			runtime.authorityMu.Unlock()
			return err
		}
	}
	runtime.authorityMu.Unlock()
	for _, item := range selected {
		if err := runtime.store.remove(item.LogPath); err != nil {
			return err
		}
	}
	runtime.authorityMu.Lock()
	partition = runtime.authority.Projects[project.identity.SafeProjectKey]
	for _, item := range selected {
		delete(partition.Records, item.ID)
		delete(partition.Tombstones, item.ID)
	}
	runtime.authority.Projects[project.identity.SafeProjectKey] = partition
	if len(selected) > 0 {
		runtime.authority.Generation++
	}
	err := runtime.saveAuthorityLocked()
	runtime.authorityMu.Unlock()
	return err
}

func (runtime *processRuntimePersistence) recoverTombstones() error {
	type tombstoneRef struct {
		projectKey string
		value      processTombstone
	}
	runtime.authorityMu.Lock()
	refs := make([]tombstoneRef, 0)
	for key, partition := range runtime.authority.Projects {
		for _, tombstone := range partition.Tombstones {
			refs = append(refs, tombstoneRef{projectKey: key, value: tombstone})
		}
	}
	runtime.authorityMu.Unlock()
	for _, ref := range refs {
		if err := runtime.store.remove(ref.value.LogPath); err != nil {
			return err
		}
	}
	if len(refs) == 0 {
		return nil
	}
	runtime.authorityMu.Lock()
	for _, ref := range refs {
		partition := runtime.authority.Projects[ref.projectKey]
		delete(partition.Records, ref.value.ProcessID)
		delete(partition.Tombstones, ref.value.ProcessID)
		runtime.authority.Projects[ref.projectKey] = partition
	}
	runtime.authority.Generation++
	err := runtime.saveAuthorityLocked()
	runtime.authorityMu.Unlock()
	return err
}

func processLogRelativePath(
	identity monitordomain.RuntimeProjectIdentity,
	processID string,
) string {
	return filepath.Join("process-logs", identity.SafeProjectKey, processID+".log")
}
