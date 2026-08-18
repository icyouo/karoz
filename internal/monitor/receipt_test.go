package monitor

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestProbeApprovalSingleClaimAndIdempotentRetry(t *testing.T) {
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	receipt := ProbeApprovalReceipt{
		ID: "receipt-1", ProjectID: "project-1", AgentID: "agent-1",
		MonitorID: "monitor-1", TriggerRevision: 2,
		NormalizedSource: []byte("echo ok"), ExpiresAt: now.Add(time.Minute),
	}
	claimed, err := ClaimProbeApproval(receipt, "project-1", "agent-1", "monitor-1", 2, "mutation-1", now)
	if err != nil || claimed.ClaimedMutationID != "mutation-1" || claimed.ClaimedAt == nil {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	retry, err := ClaimProbeApproval(claimed, "project-1", "agent-1", "monitor-1", 2, "mutation-1", now.Add(time.Second))
	if err != nil || retry.ClaimedAt == nil || !retry.ClaimedAt.Equal(*claimed.ClaimedAt) {
		t.Fatalf("idempotent retry = %+v, %v", retry, err)
	}
	if _, err := ClaimProbeApproval(claimed, "project-1", "agent-1", "monitor-1", 2, "mutation-2", now); !errors.Is(err, ErrApprovalAlreadyClaimed) {
		t.Fatalf("second mutation error = %v", err)
	}
	for _, subject := range []struct {
		project, agent, monitor string
		revision                int
	}{
		{"other", "agent-1", "monitor-1", 2},
		{"project-1", "other", "monitor-1", 2},
		{"project-1", "agent-1", "other", 2},
		{"project-1", "agent-1", "monitor-1", 3},
	} {
		if _, err := ClaimProbeApproval(receipt, subject.project, subject.agent, subject.monitor, subject.revision, "mutation", now); !errors.Is(err, ErrApprovalSubjectMismatch) {
			t.Fatalf("subject mismatch accepted: %+v, %v", subject, err)
		}
	}
	if !bytes.Equal(receipt.NormalizedSource, []byte("echo ok")) {
		t.Fatal("claim mutated source receipt")
	}
}

func TestProbeApprovalDeterministicJSONRoundTrip(t *testing.T) {
	receipt := ProbeApprovalReceipt{
		ID: "r", ProjectID: "p", AgentID: "a", MonitorID: "m", TriggerRevision: 1,
		NormalizedSource: []byte("source"), ApprovedAt: time.Unix(1, 0).UTC(),
	}
	first, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ProbeApprovalReceipt
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(decoded)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("receipt JSON changed: %s != %s (%v)", first, second, err)
	}
}
