package agent

import "testing"

func TestReviewerCapabilityUsesEstablishedReviewerIdentity(t *testing.T) {
	reviewer := Agent{ID: "reviewer", Name: "quality-reviewer", Role: "review code correctness"}
	if !CapabilitiesFor(reviewer).CanReviewArtifacts {
		t.Fatal("reviewer identity should enable artifact review")
	}
	ordinary := Agent{ID: "worker", Name: "implementation-lead", Role: "build code"}
	if CapabilitiesFor(ordinary).CanReviewArtifacts {
		t.Fatal("ordinary builder should not receive artifact review capability")
	}
}
