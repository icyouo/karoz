package collaboration

import "testing"

func TestHandoffLifecycleContract(t *testing.T) {
	if NormalizeHandoffStatus("pending") != HandoffDelivered || !HandoffOpen("claimed") || HandoffOpen("closed") {
		t.Fatal("handoff normalization/open contract changed")
	}
	valid := [][2]string{{"queued", "delivered"}, {"queued", "canceled"}, {"delivered", "claimed"}, {"claimed", "working"}, {"working", "replied"}, {"failed", "delivered"}, {"replied", "closed"}, {"delivered", "consumed"}, {"delivered", "canceled"}}
	for _, transition := range valid {
		if !ValidHandoffTransition(transition[0], transition[1]) && !ValidMessageTransition(transition[0], transition[1]) {
			t.Fatalf("expected valid transition %s -> %s", transition[0], transition[1])
		}
	}
	if ValidHandoffTransition("closed", "working") || ValidHandoffTransition("replied", "working") || ValidMessageTransition("consumed", "queued") || ValidMessageTransition("canceled", "delivered") {
		t.Fatal("terminal handoff transition was accepted")
	}
	if CanonicalMessageStatus("canceled") != HandoffDeclined || PersistableMessageStatus(HandoffClosed) != "consumed" {
		t.Fatal("commercial message status mapping changed")
	}
}

func TestCommercialMessageStatusRejectsIllegalJumps(t *testing.T) {
	if ValidMessageTransition("consumed", "queued") || ValidMessageTransition("consumed", "delivered") || ValidMessageTransition("canceled", "queued") {
		t.Fatal("terminal commercial message status was allowed to reopen")
	}
	if !ValidMessageTransition("queued", "delivered") || !ValidMessageTransition("queued", "failed") || !ValidMessageTransition("queued", "canceled") || !ValidMessageTransition("delivered", "consumed") {
		t.Fatal("expected commercial dispatch/close transitions to remain legal")
	}
}
