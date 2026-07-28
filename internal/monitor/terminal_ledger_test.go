package monitor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func testProjectIdentity(id string) RuntimeProjectIdentity {
	path := "/projects/" + id
	return RuntimeProjectIdentity{
		ProjectID: id, CanonicalProjectPath: path,
		CanonicalPathSHA256: CanonicalProjectPathSHA256(path), SafeProjectKey: SafeProjectKey(id),
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
	badSafeKey := projectA
	badSafeKey.SafeProjectKey = projectB.SafeProjectKey
	if badSafeKey.Validate() == nil {
		t.Fatal("mismatched safe project key accepted")
	}
	badPath := projectA
	badPath.CanonicalProjectPath = projectB.CanonicalProjectPath
	if badPath.Validate() == nil {
		t.Fatal("mismatched canonical project path accepted")
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

func TestRuntimeProjectSafeKeyContract(t *testing.T) {
	const want = "3689b1e1169ca1c14a9ed48a53a174a986917426624c5c33ad63b6e044572862"
	if got := SafeProjectKey("project-a"); got != want {
		t.Fatalf("safe key = %q want %q", got, want)
	}
	if len(SafeProjectKey("project-a")) != 64 || SafeProjectKey("Project-A") == SafeProjectKey("project-a") {
		t.Fatal("safe key is not full lowercase SHA-256 over exact canonical ID")
	}
}

func TestOperationAndAuthorityRequireExactProjectIdentity(t *testing.T) {
	projectA := testProjectIdentity("project-a")
	projectB := testProjectIdentity("project-b")
	reservation := testReservation(projectA, 1, "one")
	operation := RuntimeMutationOperation{
		ID: "operation-one", Project: projectA, Kind: "admit", State: "intent",
		ReservationToken: reservation.Token, AuthorityID: reservation.AuthorityID,
		EntityID: reservation.EntityID,
	}
	if err := operation.Validate(projectA); err != nil {
		t.Fatal(err)
	}
	if err := operation.Validate(projectB); err == nil {
		t.Fatal("operation validated across projects")
	}
	authority := TerminalAuthorityReservation{Project: projectA, Reservation: reservation}
	if err := authority.Validate(projectA); err != nil {
		t.Fatal(err)
	}
	if err := authority.Validate(projectB); err == nil {
		t.Fatal("authority reservation validated across projects")
	}
	forged := authority
	forged.Project = projectB
	if SameTerminalAuthorityReservation(projectA, authority, forged) {
		t.Fatal("authority records compared equal across project identities")
	}
}
