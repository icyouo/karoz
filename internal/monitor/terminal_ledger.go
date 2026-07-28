package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

const TerminalReservationCapacity uint16 = 4096
const runtimeProjectKeyDomain = "karoz-project-runtime-v1\x00"

type RuntimeProjectIdentity struct {
	ProjectID            string `json:"project_id"`
	CanonicalProjectPath string `json:"canonical_project_path"`
	CanonicalPathSHA256  string `json:"canonical_path_sha256"`
	SafeProjectKey       string `json:"safe_project_key"`
}

func (identity RuntimeProjectIdentity) Validate() error {
	if identity.ProjectID == "" || identity.CanonicalProjectPath == "" ||
		identity.CanonicalPathSHA256 == "" || identity.SafeProjectKey == "" {
		return errors.New("runtime project identity is incomplete")
	}
	if identity.CanonicalPathSHA256 != CanonicalProjectPathSHA256(identity.CanonicalProjectPath) {
		return errors.New("runtime project canonical path digest mismatch")
	}
	if identity.SafeProjectKey != SafeProjectKey(identity.ProjectID) {
		return errors.New("runtime project safe key mismatch")
	}
	return nil
}

func SafeProjectKey(canonicalProjectID string) string {
	sum := sha256.Sum256([]byte(runtimeProjectKeyDomain + canonicalProjectID))
	return hex.EncodeToString(sum[:])
}

func CanonicalProjectPathSHA256(canonicalProjectPath string) string {
	sum := sha256.Sum256([]byte(canonicalProjectPath))
	return hex.EncodeToString(sum[:])
}

type TerminalReservationState string

const (
	ReservationAllocating             TerminalReservationState = "allocating"
	ReservationActive                 TerminalReservationState = "active"
	ReservationTerminalUnacknowledged TerminalReservationState = "terminal_unacknowledged"
	ReservationReleasing              TerminalReservationState = "releasing"
)

type TerminalReservation struct {
	Slot                  uint16                   `json:"slot"`
	Token                 string                   `json:"token"`
	ProjectID             string                   `json:"project_id"`
	ProjectIdentitySHA256 string                   `json:"project_identity_sha256"`
	OperationID           string                   `json:"operation_id"`
	AuthorityID           string                   `json:"authority_id"`
	EntityID              string                   `json:"entity_id"`
	EventKind             string                   `json:"event_kind"`
	State                 TerminalReservationState `json:"state"`
}

type TerminalReservationLedger struct {
	Project  RuntimeProjectIdentity         `json:"project"`
	Capacity uint16                         `json:"capacity"`
	Slots    map[uint16]TerminalReservation `json:"slots"`
}

type RuntimeMutationOperation struct {
	ID               string                 `json:"id"`
	Project          RuntimeProjectIdentity `json:"project"`
	Kind             string                 `json:"kind"`
	State            string                 `json:"state"`
	ReservationToken string                 `json:"reservation_token,omitempty"`
	AuthorityID      string                 `json:"authority_id"`
	EntityID         string                 `json:"entity_id"`
}

func (operation RuntimeMutationOperation) Validate(project RuntimeProjectIdentity) error {
	if err := project.Validate(); err != nil {
		return err
	}
	if operation.Project != project {
		return errors.New("runtime mutation operation project mismatch")
	}
	if operation.ID == "" || operation.Kind == "" || operation.State == "" ||
		operation.AuthorityID == "" || operation.EntityID == "" {
		return errors.New("runtime mutation operation is incomplete")
	}
	return nil
}

type TerminalAuthorityReservation struct {
	Project     RuntimeProjectIdentity `json:"project"`
	Reservation TerminalReservation    `json:"reservation"`
}

func (authority TerminalAuthorityReservation) Validate(project RuntimeProjectIdentity) error {
	if err := project.Validate(); err != nil {
		return err
	}
	if authority.Project != project {
		return errors.New("terminal authority project mismatch")
	}
	return ValidateTerminalReservation(project, authority.Reservation)
}

func SameTerminalAuthorityReservation(
	project RuntimeProjectIdentity,
	left, right TerminalAuthorityReservation,
) bool {
	return left.Validate(project) == nil && right.Validate(project) == nil && left == right
}

func NewTerminalReservationLedger(project RuntimeProjectIdentity) (TerminalReservationLedger, error) {
	if err := project.Validate(); err != nil {
		return TerminalReservationLedger{}, err
	}
	return TerminalReservationLedger{
		Project: project, Capacity: TerminalReservationCapacity,
		Slots: make(map[uint16]TerminalReservation),
	}, nil
}

func (ledger TerminalReservationLedger) Validate(project RuntimeProjectIdentity) error {
	if err := project.Validate(); err != nil {
		return err
	}
	if ledger.Project != project {
		return errors.New("terminal reservation ledger project mismatch")
	}
	if ledger.Capacity != TerminalReservationCapacity {
		return errors.New("terminal reservation ledger capacity must be 4096")
	}
	if len(ledger.Slots) > int(ledger.Capacity) {
		return errors.New("terminal reservation capacity exceeded")
	}
	tokens := make(map[string]bool, len(ledger.Slots))
	for slot, reservation := range ledger.Slots {
		if reservation.Slot != slot {
			return fmt.Errorf("terminal reservation slot mismatch at %d", slot)
		}
		if err := ValidateTerminalReservation(project, reservation); err != nil {
			return err
		}
		if tokens[reservation.Token] {
			return errors.New("duplicate terminal reservation token in project")
		}
		tokens[reservation.Token] = true
	}
	return nil
}

func ValidateTerminalReservation(project RuntimeProjectIdentity, reservation TerminalReservation) error {
	if err := project.Validate(); err != nil {
		return err
	}
	if reservation.ProjectID != project.ProjectID ||
		reservation.ProjectIdentitySHA256 != project.CanonicalPathSHA256 {
		return errors.New("terminal reservation project mismatch")
	}
	if reservation.Slot >= TerminalReservationCapacity ||
		reservation.Token == "" || reservation.OperationID == "" ||
		reservation.AuthorityID == "" || reservation.EntityID == "" ||
		reservation.EventKind == "" {
		return errors.New("invalid terminal reservation")
	}
	switch reservation.State {
	case ReservationAllocating, ReservationActive, ReservationTerminalUnacknowledged, ReservationReleasing:
		return nil
	default:
		return errors.New("invalid terminal reservation state")
	}
}

// SameTerminalReservation requires both records to belong to the explicitly
// supplied project and then compares every persisted field.
func SameTerminalReservation(project RuntimeProjectIdentity, left, right TerminalReservation) bool {
	return ValidateTerminalReservation(project, left) == nil &&
		ValidateTerminalReservation(project, right) == nil &&
		left == right
}

func (ledger TerminalReservationLedger) Allocate(project RuntimeProjectIdentity, reservation TerminalReservation) (TerminalReservationLedger, error) {
	if err := ledger.Validate(project); err != nil {
		return ledger, err
	}
	if err := ValidateTerminalReservation(project, reservation); err != nil {
		return ledger, err
	}
	if reservation.State != ReservationAllocating {
		return ledger, errors.New("new terminal reservation must be allocating")
	}
	if len(ledger.Slots) >= int(ledger.Capacity) {
		return ledger, errors.New("terminal reservation capacity exceeded")
	}
	if _, exists := ledger.Slots[reservation.Slot]; exists {
		return ledger, errors.New("terminal reservation slot already occupied")
	}
	for _, existing := range ledger.Slots {
		if existing.Token == reservation.Token {
			return ledger, errors.New("duplicate terminal reservation token in project")
		}
	}
	result := cloneTerminalLedger(ledger)
	result.Slots[reservation.Slot] = reservation
	return result, nil
}

func (ledger TerminalReservationLedger) Transition(
	project RuntimeProjectIdentity,
	expected TerminalReservation,
	to TerminalReservationState,
) (TerminalReservationLedger, error) {
	if err := ledger.Validate(project); err != nil {
		return ledger, err
	}
	current, exists := ledger.Slots[expected.Slot]
	if !exists || !SameTerminalReservation(project, current, expected) {
		return ledger, errors.New("terminal reservation comparison failed")
	}
	if !canTransitionTerminalReservation(current.State, to) {
		return ledger, errors.New("invalid terminal reservation transition")
	}
	result := cloneTerminalLedger(ledger)
	current.State = to
	result.Slots[current.Slot] = current
	return result, nil
}

func (ledger TerminalReservationLedger) Release(
	project RuntimeProjectIdentity,
	expected TerminalReservation,
) (TerminalReservationLedger, error) {
	if err := ledger.Validate(project); err != nil {
		return ledger, err
	}
	current, exists := ledger.Slots[expected.Slot]
	if !exists || !SameTerminalReservation(project, current, expected) {
		return ledger, errors.New("terminal reservation comparison failed")
	}
	if current.State != ReservationReleasing {
		return ledger, errors.New("terminal reservation is not releasable")
	}
	result := cloneTerminalLedger(ledger)
	delete(result.Slots, current.Slot)
	return result, nil
}

func canTransitionTerminalReservation(from, to TerminalReservationState) bool {
	if from == to {
		return true
	}
	return from == ReservationAllocating && to == ReservationActive ||
		from == ReservationActive && to == ReservationTerminalUnacknowledged ||
		from == ReservationTerminalUnacknowledged && to == ReservationReleasing
}

func cloneTerminalLedger(ledger TerminalReservationLedger) TerminalReservationLedger {
	result := TerminalReservationLedger{
		Project: ledger.Project, Capacity: ledger.Capacity,
		Slots: make(map[uint16]TerminalReservation, len(ledger.Slots)+1),
	}
	for slot, reservation := range ledger.Slots {
		result.Slots[slot] = reservation
	}
	return result
}
