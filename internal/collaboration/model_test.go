package collaboration

import "testing"

func TestHandoffLifecycleContract(t *testing.T) {
	if NormalizeHandoffStatus("pending") != HandoffDelivered || !HandoffOpen("claimed") || HandoffOpen("closed") {
		t.Fatal("handoff normalization/open contract changed")
	}
	valid := [][2]string{{"queued", "delivered"}, {"delivered", "claimed"}, {"claimed", "working"}, {"working", "replied"}, {"failed", "delivered"}, {"replied", "closed"}}
	for _, transition := range valid {
		if !ValidHandoffTransition(transition[0], transition[1]) {
			t.Fatalf("expected valid transition %s -> %s", transition[0], transition[1])
		}
	}
	if ValidHandoffTransition("closed", "working") || ValidHandoffTransition("replied", "working") {
		t.Fatal("terminal handoff transition was accepted")
	}
}
