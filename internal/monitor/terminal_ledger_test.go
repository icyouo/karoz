package monitor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func testProjectIdentity(id string) RuntimeProjectIdentity {
	return RuntimeProjectIdentity{
		ProjectID: id, CanonicalProjectPath: "/projects/" + id,
		CanonicalPathSHA256: "path-sha-" + id, SafeProjectKey: "safe-key-" + id,
	}
}

func testReservation(project RuntimeProjectIdentity, slot uint16, suffix string) TerminalReservation {
	return TerminalReservation{
		Slot: slot, Token: "token-" + suffix,
		ProjectID: project.ProjectID, ProjectIdentitySHA256: project.CanonicalPathSHA256,
		OperationID: "operation-" + suffix, AuthorityID: "task-store",
		EntityID: "task-" + suffix, EventKind: "task_changed",
		State: ReservationAllocating,
	}
}

func TestTerminalReservationProjectScopedLifecycle(t *testing.T) {
	project := testProjectIdentity("project-a")
	ledger, err := NewTerminalReservationLedger(project)
	if err != nil {
		t.Fatal(err)
	}
	current := testReservation(project, 3, "shared-id")
	ledger, err = ledger.Allocate(project, current)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []TerminalReservationState{
		ReservationActive, ReservationTerminalUnacknowledged, ReservationReleasing,
	} {
		ledger, err = ledger.Transition(project, current, state)
		if err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
		current.State = state
	}
	ledger, err = ledger.Release(project, current)
	if err != nil || len(ledger.Slots) != 0 {
		t.Fatalf("release = %+v, %v", ledger, err)
	}
}

func TestTerminalReservationRejectsProjectMismatch(t *testing.T) {
	projectA := testProjectIdentity("project-a")
	projectB := testProjectIdentity("project-b")
	ledgerA, err := NewTerminalReservationLedger(projectA)
	if err != nil {
		t.Fatal(err)
	}
	reservationA := testReservation(projectA, 0, "same")
	ledgerA, err = ledgerA.Allocate(projectA, reservationA)
	if err != nil {
		t.Fatal(err)
	}
	reservationB := testReservation(projectB, 0, "same")
	if _, err := ledgerA.Allocate(projectB, reservationB); err == nil {
		t.Fatal("project B mutated project A ledger")
	}
	if SameTerminalReservation(projectA, reservationA, reservationB) {
		t.Fatal("same IDs compared equal across projects")
	}
	badIdentity := reservationA
	badIdentity.ProjectIdentitySHA256 = projectB.CanonicalPathSHA256
	if ValidateTerminalReservation(projectA, badIdentity) == nil {
		t.Fatal("mismatched project identity digest accepted")
	}
}

func TestTerminalReservationCapacityIsIndependentPerProject(t *testing.T) {
	projectA := testProjectIdentity("project-a")
	projectB := testProjectIdentity("project-b")
	ledgerA, _ := NewTerminalReservationLedger(projectA)
	ledgerB, _ := NewTerminalReservationLedger(projectB)
	for slot := 0; slot < int(TerminalReservationCapacity); slot++ {
		suffix := fmt.Sprintf("%d", slot)
		ledgerA.Slots[uint16(slot)] = testReservation(projectA, uint16(slot), suffix)
		ledgerB.Slots[uint16(slot)] = testReservation(projectB, uint16(slot), suffix)
	}
	if err := ledgerA.Validate(projectA); err != nil {
		t.Fatalf("full project A ledger: %v", err)
	}
	if err := ledgerB.Validate(projectB); err != nil {
		t.Fatalf("full project B ledger: %v", err)
	}
	if len(ledgerA.Slots) != 4096 || len(ledgerB.Slots) != 4096 {
		t.Fatalf("capacity is not per project: %d, %d", len(ledgerA.Slots), len(ledgerB.Slots))
	}
	if _, err := ledgerA.Allocate(projectA, testReservation(projectA, 0, "overflow")); err == nil {
		t.Fatal("project A accepted a 4097th reservation")
	}
	if _, err := ledgerB.Allocate(projectB, testReservation(projectB, 0, "overflow")); err == nil {
		t.Fatal("project B accepted a 4097th reservation")
	}
}

func TestTerminalReservationDeterministicJSONAndCopy(t *testing.T) {
	project := testProjectIdentity("project-a")
	ledger, _ := NewTerminalReservationLedger(project)
	reservation := testReservation(project, 7, "one")
	updated, err := ledger.Allocate(project, reservation)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Slots) != 0 || len(updated.Slots) != 1 {
		t.Fatal("allocate mutated the source ledger")
	}
	first, err := json.Marshal(updated)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TerminalReservationLedger
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(project); err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(decoded)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("ledger JSON changed: %s != %s (%v)", first, second, err)
	}
}
