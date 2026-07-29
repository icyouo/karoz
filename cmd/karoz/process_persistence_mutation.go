package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
	processdomain "github.com/karoz/karoz/internal/process"
)

func (runtime *processRuntimePersistence) PrepareRecord(
	record processdomain.Process,
) (processdomain.Process, error) {
	record = cloneDurableProcess(record)
	project := runtime.projectRuntime(record.ProjectID)
	if project == nil {
		return record, errors.New("process project runtime is unavailable")
	}
	if !safeProcessID(record.ID) {
		return record, errors.New("process id is not a safe runtime component")
	}
	record.LogPath = filepath.Join(
		"process-logs", project.identity.SafeProjectKey, record.ID+".log",
	)
	return record, nil
}

func safeProcessID(id string) bool {
	if id == "" || len(id) > 160 {
		return false
	}
	for _, value := range id {
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
			value >= '0' && value <= '9' || value == '-' || value == '_' {
			continue
		}
		return false
	}
	return true
}

func (runtime *processRuntimePersistence) Reserve(record processdomain.Process) error {
	project := runtime.projectRuntime(record.ProjectID)
	if project == nil {
		return errors.New("process project runtime is unavailable")
	}
	project.lane.Lock()
	defer project.lane.Unlock()
	return runtime.admitProcessLocked(project, record)
}

func (runtime *processRuntimePersistence) admitProcessLocked(
	project *processProjectRuntime,
	record processdomain.Process,
) error {
	if record.State != processdomain.StateStarting || record.ID == "" ||
		record.ProjectID != project.identity.ProjectID || record.LogPath == "" {
		return errors.New("invalid durable starting process")
	}
	operationID := "process/" + record.ID + "/admit"
	operation, exists := project.journal.Operations[operationID]
	if exists {
		if operation.EntityID != record.ID || operation.Kind != "process_admission" {
			return errors.New("process admission operation collision")
		}
		if operation.State == "committed" {
			return runtime.validateCommittedAdmission(project, record, operation)
		}
		if err := runtime.recoverAdmissionLocked(project, operation); err != nil {
			return err
		}
		operation, exists = project.journal.Operations[operationID]
		if exists && operation.State == "committed" {
			return runtime.validateCommittedAdmission(project, record, operation)
		}
	}
	if !exists {
		operation = monitordomain.RuntimeMutationOperation{
			ID: operationID, Project: project.identity, Kind: "process_admission",
			State: "intent", AuthorityID: processAuthorityID, EntityID: record.ID,
		}
		if err := runtime.putOperation(project, operation); err != nil {
			return err
		}
		if err := runtime.persistenceFail(processPersistAfterIntent); err != nil {
			return err
		}
	}
	token, err := randomRuntimeID("ptr_")
	if err != nil {
		return err
	}
	operation.ReservationToken = token
	operation.State = "token_selected"
	if err := runtime.putOperation(project, operation); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterTokenSelection); err != nil {
		return err
	}
	slot, err := runtime.nextFreeSlot(project)
	if err != nil {
		return err
	}
	reservation := monitordomain.TerminalReservation{
		Slot: slot, Token: token, ProjectID: project.identity.ProjectID,
		ProjectIdentitySHA256: project.identity.CanonicalPathSHA256,
		OperationID:           operation.ID, AuthorityID: processAuthorityID,
		EntityID: record.ID, EventKind: processTerminalEventKind,
		State: monitordomain.ReservationAllocating,
	}
	if err := runtime.allocateLedger(project, reservation); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterLedgerAllocate); err != nil {
		return err
	}
	operation.State = "allocated"
	if err := runtime.putOperation(project, operation); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterAllocatedOperation); err != nil {
		return err
	}
	if err := runtime.createAuthorityRecord(project.identity, record, reservation); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterAuthorityCreate); err != nil {
		return err
	}
	operation.State = "authority_saved"
	if err := runtime.putOperation(project, operation); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterAuthorityOperation); err != nil {
		return err
	}
	active, err := runtime.transitionLedger(project, reservation, monitordomain.ReservationActive)
	if err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterLedgerActivate); err != nil {
		return err
	}
	operation.State = "active"
	if err := runtime.putOperation(project, operation); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterActiveOperation); err != nil {
		return err
	}
	if err := runtime.updateAuthorityReservation(project.identity, record.ID, active); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterAuthorityActive); err != nil {
		return err
	}
	operation.State = "committed"
	if err := runtime.putOperation(project, operation); err != nil {
		return err
	}
	return runtime.persistenceFail(processPersistAfterCommit)
}

func (runtime *processRuntimePersistence) CreateStarting(record processdomain.Process) error {
	project := runtime.projectRuntime(record.ProjectID)
	if project == nil {
		return errors.New("process project runtime is unavailable")
	}
	runtime.authorityMu.Lock()
	defer runtime.authorityMu.Unlock()
	partition := runtime.authority.Projects[project.identity.SafeProjectKey]
	durable, exists := partition.Records[record.ID]
	if !exists || !sameDurableProcess(durable.Process, record) || durable.Reservation == nil ||
		durable.Reservation.State != monitordomain.ReservationActive {
		return errors.New("durable process admission was not committed")
	}
	return nil
}

func (runtime *processRuntimePersistence) MarkRunning(record processdomain.Process) error {
	if record.State != processdomain.StateRunning {
		return errors.New("durable running process has invalid state")
	}
	return runtime.updateProcessRecord(record, false)
}

func (runtime *processRuntimePersistence) MarkTerminal(record processdomain.Process) error {
	if !record.State.Terminal() {
		return errors.New("durable terminal process has invalid state")
	}
	project := runtime.projectRuntime(record.ProjectID)
	if project == nil {
		return errors.New("process project runtime is unavailable")
	}
	project.lane.Lock()
	defer project.lane.Unlock()
	reservation, err := runtime.persistTerminalAuthority(project.identity, record)
	if err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterTerminal); err != nil {
		return err
	}
	terminal, err := runtime.transitionLedger(
		project, reservation, monitordomain.ReservationTerminalUnacknowledged,
	)
	if err != nil {
		return err
	}
	if err := runtime.updateAuthorityReservation(project.identity, record.ID, terminal); err != nil {
		return err
	}
	return runtime.persistenceFail(processPersistAfterLedgerTerminal)
}

// Abort is intentionally an internal admission boundary. Once MarkTerminal
// has installed a terminal event, acknowledgement owns token release.
func (runtime *processRuntimePersistence) Abort(record processdomain.Process) error {
	project := runtime.projectRuntime(record.ProjectID)
	if project == nil {
		return errors.New("process project runtime is unavailable")
	}
	runtime.authorityMu.Lock()
	partition := runtime.authority.Projects[project.identity.SafeProjectKey]
	durable, exists := partition.Records[record.ID]
	runtime.authorityMu.Unlock()
	if exists && durable.Event != nil {
		return nil
	}
	if exists &&
		durable.Process.State.Terminal() &&
		durable.Reservation == nil &&
		durable.AcknowledgedEventID == processTerminalEventID(record.ID) {
		return nil
	}
	if exists {
		return errors.New("cannot abort an admitted nonterminal process")
	}
	return nil
}

func (runtime *processRuntimePersistence) OpenLog(
	record processdomain.Process,
) (io.WriteCloser, error) {
	if record.LogPath == "" {
		return nil, errors.New("process log path is empty")
	}
	return runtime.store.openLog(record.LogPath)
}

func (runtime *processRuntimePersistence) updateProcessRecord(
	record processdomain.Process,
	allowTerminal bool,
) error {
	project := runtime.projectRuntime(record.ProjectID)
	if project == nil {
		return errors.New("process project runtime is unavailable")
	}
	runtime.authorityMu.Lock()
	defer runtime.authorityMu.Unlock()
	partition := runtime.authority.Projects[project.identity.SafeProjectKey]
	current, exists := partition.Records[record.ID]
	if !exists || current.Reservation == nil {
		return errors.New("durable process record is missing")
	}
	if !processdomain.CanTransition(current.Process.State, record.State) ||
		(record.State.Terminal() && !allowTerminal) {
		return errors.New("invalid durable process transition")
	}
	if record.LogPath != current.Process.LogPath || record.ProjectID != current.Process.ProjectID {
		return errors.New("durable process immutable fields changed")
	}
	current.Process = cloneDurableProcess(record)
	partition.Records[record.ID] = current
	runtime.authority.Projects[project.identity.SafeProjectKey] = partition
	runtime.authority.Generation++
	return runtime.saveAuthorityLocked()
}

func (runtime *processRuntimePersistence) persistTerminalAuthority(
	identity monitordomain.RuntimeProjectIdentity,
	record processdomain.Process,
) (monitordomain.TerminalReservation, error) {
	runtime.authorityMu.Lock()
	defer runtime.authorityMu.Unlock()
	partition := runtime.authority.Projects[identity.SafeProjectKey]
	current, exists := partition.Records[record.ID]
	if !exists || current.Reservation == nil {
		return monitordomain.TerminalReservation{}, errors.New("durable process reservation is missing")
	}
	if current.Event != nil {
		if current.Process.State == record.State && current.Process.ExitCode == record.ExitCode {
			return *current.Reservation, nil
		}
		return monitordomain.TerminalReservation{}, errors.New("terminal process event conflicts with existing event")
	}
	if !processdomain.CanTransition(current.Process.State, record.State) {
		return monitordomain.TerminalReservation{}, errors.New("invalid durable terminal transition")
	}
	reservation := *current.Reservation
	if reservation.State == monitordomain.ReservationAllocating {
		return reservation, errors.New("process reservation was not activated")
	}
	terminalReservation := reservation
	terminalReservation.State = monitordomain.ReservationTerminalUnacknowledged
	current.Process = cloneDurableProcess(record)
	current.Reservation = &terminalReservation
	current.Event = &processTerminalEvent{
		ID: processTerminalEventID(record.ID), Kind: processTerminalEventKind,
		Project: identity, ProcessID: record.ID, State: record.State,
		ExitCode: record.ExitCode, OccurredAt: record.UpdatedAt,
		Reservation: terminalReservation,
	}
	partition.Records[record.ID] = current
	runtime.authority.Projects[identity.SafeProjectKey] = partition
	runtime.authority.Generation++
	if err := runtime.saveAuthorityLocked(); err != nil {
		return reservation, err
	}
	return reservation, nil
}

func (runtime *processRuntimePersistence) createAuthorityRecord(
	identity monitordomain.RuntimeProjectIdentity,
	record processdomain.Process,
	reservation monitordomain.TerminalReservation,
) error {
	runtime.authorityMu.Lock()
	defer runtime.authorityMu.Unlock()
	partition := runtime.authority.Projects[identity.SafeProjectKey]
	if existing, exists := partition.Records[record.ID]; exists {
		if sameDurableProcess(existing.Process, record) && existing.Reservation != nil &&
			monitordomain.SameTerminalReservation(identity, *existing.Reservation, reservation) {
			return nil
		}
		return errors.New("process authority record already exists")
	}
	copyReservation := reservation
	partition.Records[record.ID] = durableProcessRecord{
		Process: cloneDurableProcess(record), Reservation: &copyReservation,
	}
	runtime.authority.Projects[identity.SafeProjectKey] = partition
	runtime.authority.Generation++
	return runtime.saveAuthorityLocked()
}

func (runtime *processRuntimePersistence) updateAuthorityReservation(
	identity monitordomain.RuntimeProjectIdentity,
	processID string,
	reservation monitordomain.TerminalReservation,
) error {
	runtime.authorityMu.Lock()
	defer runtime.authorityMu.Unlock()
	partition := runtime.authority.Projects[identity.SafeProjectKey]
	record, exists := partition.Records[processID]
	if !exists || record.Reservation == nil ||
		record.Reservation.Token != reservation.Token ||
		record.Reservation.OperationID != reservation.OperationID {
		return errors.New("process authority reservation comparison failed")
	}
	copyReservation := reservation
	record.Reservation = &copyReservation
	if record.Event != nil {
		record.Event.Reservation = reservation
	}
	partition.Records[processID] = record
	runtime.authority.Projects[identity.SafeProjectKey] = partition
	runtime.authority.Generation++
	return runtime.saveAuthorityLocked()
}

func (runtime *processRuntimePersistence) saveAuthorityLocked() error {
	if err := validateProcessAuthorityHeader(runtime.authority); err != nil {
		return err
	}
	for key, partition := range runtime.authority.Projects {
		if runtime.projectDisabled(key) {
			continue
		}
		if err := validateProcessAuthorityProject(key, partition); err != nil {
			return err
		}
	}
	return runtime.store.saveJSON("processes.json", runtime.authority)
}

func (runtime *processRuntimePersistence) putOperation(
	project *processProjectRuntime,
	operation monitordomain.RuntimeMutationOperation,
) error {
	project.journalMu.Lock()
	defer project.journalMu.Unlock()
	if err := operation.Validate(project.identity); err != nil {
		return err
	}
	project.journal.Operations[operation.ID] = operation
	project.journal.Generation++
	if err := validateRuntimeMutationSnapshot(project.journal, project.identity); err != nil {
		return err
	}
	return runtime.store.saveJSON(
		filepath.Join("project-runtime", project.identity.SafeProjectKey, "runtime-mutations.json"),
		project.journal,
	)
}

func (runtime *processRuntimePersistence) deleteOperation(
	project *processProjectRuntime,
	operationID string,
) error {
	project.journalMu.Lock()
	defer project.journalMu.Unlock()
	delete(project.journal.Operations, operationID)
	project.journal.Generation++
	return runtime.store.saveJSON(
		filepath.Join("project-runtime", project.identity.SafeProjectKey, "runtime-mutations.json"),
		project.journal,
	)
}

func (runtime *processRuntimePersistence) nextFreeSlot(
	project *processProjectRuntime,
) (uint16, error) {
	project.ledgerMu.Lock()
	defer project.ledgerMu.Unlock()
	for slot := uint16(0); slot < monitordomain.TerminalReservationCapacity; slot++ {
		if _, exists := project.ledger.Slots[slot]; !exists {
			return slot, nil
		}
	}
	return 0, errors.New("terminal reservation capacity exceeded")
}

func (runtime *processRuntimePersistence) allocateLedger(
	project *processProjectRuntime,
	reservation monitordomain.TerminalReservation,
) error {
	project.ledgerMu.Lock()
	defer project.ledgerMu.Unlock()
	next, err := project.ledger.Allocate(project.identity, reservation)
	if err != nil {
		return err
	}
	project.ledger = next
	return runtime.saveLedgerLocked(project)
}

func (runtime *processRuntimePersistence) transitionLedger(
	project *processProjectRuntime,
	expected monitordomain.TerminalReservation,
	state monitordomain.TerminalReservationState,
) (monitordomain.TerminalReservation, error) {
	project.ledgerMu.Lock()
	defer project.ledgerMu.Unlock()
	current, exists := project.ledger.Slots[expected.Slot]
	if !exists {
		return monitordomain.TerminalReservation{}, errors.New("terminal reservation is missing")
	}
	if current.Token != expected.Token || current.OperationID != expected.OperationID ||
		current.AuthorityID != expected.AuthorityID || current.EntityID != expected.EntityID {
		return monitordomain.TerminalReservation{}, errors.New("terminal reservation comparison failed")
	}
	if current.State == state {
		return current, nil
	}
	next, err := project.ledger.Transition(project.identity, current, state)
	if err != nil {
		return monitordomain.TerminalReservation{}, err
	}
	project.ledger = next
	if err := runtime.saveLedgerLocked(project); err != nil {
		return monitordomain.TerminalReservation{}, err
	}
	return project.ledger.Slots[expected.Slot], nil
}

func (runtime *processRuntimePersistence) saveLedgerLocked(project *processProjectRuntime) error {
	if err := project.ledger.Validate(project.identity); err != nil {
		return err
	}
	return runtime.store.saveJSON(
		filepath.Join("project-runtime", project.identity.SafeProjectKey, "terminal-reservations.json"),
		project.ledger,
	)
}

func (runtime *processRuntimePersistence) validateCommittedAdmission(
	project *processProjectRuntime,
	record processdomain.Process,
	operation monitordomain.RuntimeMutationOperation,
) error {
	runtime.authorityMu.Lock()
	partition := runtime.authority.Projects[project.identity.SafeProjectKey]
	durable, exists := partition.Records[record.ID]
	runtime.authorityMu.Unlock()
	if !exists || durable.Reservation == nil || !sameDurableProcess(durable.Process, record) ||
		durable.Reservation.Token != operation.ReservationToken ||
		durable.Reservation.State != monitordomain.ReservationActive {
		return errors.New("committed process admission is inconsistent")
	}
	project.ledgerMu.Lock()
	reservation, exists := reservationByOperation(project.ledger, operation.ID)
	project.ledgerMu.Unlock()
	if !exists || !monitordomain.SameTerminalReservation(
		project.identity, reservation, *durable.Reservation,
	) {
		return errors.New("committed process ledger/authority mismatch")
	}
	return nil
}

func reservationByOperation(
	ledger monitordomain.TerminalReservationLedger,
	operationID string,
) (monitordomain.TerminalReservation, bool) {
	for _, reservation := range ledger.Slots {
		if reservation.OperationID == operationID {
			return reservation, true
		}
	}
	return monitordomain.TerminalReservation{}, false
}

func (runtime *processRuntimePersistence) List(projectID string) []processdomain.Process {
	project := runtime.projectRuntime(projectID)
	if project == nil {
		return nil
	}
	runtime.authorityMu.Lock()
	partition := runtime.authority.Projects[project.identity.SafeProjectKey]
	items := make([]processdomain.Process, 0, len(partition.Records))
	for _, record := range partition.Records {
		items = append(items, cloneDurableProcess(record.Process))
	}
	runtime.authorityMu.Unlock()
	sort.Slice(items, func(i, j int) bool {
		if items[i].StartedAt.Equal(items[j].StartedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].StartedAt.Before(items[j].StartedAt)
	})
	return items
}

func ensureProcessRecordIdentity(record processdomain.Process, identity monitordomain.RuntimeProjectIdentity) error {
	if record.ProjectID != identity.ProjectID || record.ID == "" ||
		!strings.HasPrefix(record.LogPath, filepath.Join("process-logs", identity.SafeProjectKey)+string(filepath.Separator)) {
		return fmt.Errorf("process %s identity/log path mismatch", record.ID)
	}
	return nil
}

func terminalRecordTime(record durableProcessRecord) time.Time {
	if record.Process.EndedAt != nil {
		return *record.Process.EndedAt
	}
	return record.Process.UpdatedAt
}

func cloneDurableProcess(record processdomain.Process) processdomain.Process {
	record.OutputGaps = append([]processdomain.SeqRange(nil), record.OutputGaps...)
	if record.EndedAt != nil {
		endedAt := *record.EndedAt
		record.EndedAt = &endedAt
	}
	return record
}
