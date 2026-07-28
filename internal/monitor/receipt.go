package monitor

import (
	"errors"
	"time"
)

var (
	ErrApprovalAlreadyClaimed  = errors.New("approval already claimed")
	ErrApprovalSubjectMismatch = errors.New("approval subject mismatch")
	ErrApprovalExpired         = errors.New("approval expired")
	ErrApprovalRevoked         = errors.New("approval revoked")
)

// ClaimProbeApproval is a pure value transition. Repeating the exact mutation
// is idempotent; every other claim against an already-claimed receipt fails.
func ClaimProbeApproval(
	receipt ProbeApprovalReceipt,
	projectID, agentID, monitorID string,
	triggerRevision int,
	mutationID string,
	now time.Time,
) (ProbeApprovalReceipt, error) {
	if receipt.ProjectID != projectID || receipt.AgentID != agentID ||
		receipt.MonitorID != monitorID || receipt.TriggerRevision != triggerRevision ||
		mutationID == "" {
		return receipt, ErrApprovalSubjectMismatch
	}
	if receipt.RevokedAt != nil {
		return receipt, ErrApprovalRevoked
	}
	if receipt.ClaimedMutationID != "" {
		if receipt.ClaimedMutationID == mutationID {
			return receipt, nil
		}
		return receipt, ErrApprovalAlreadyClaimed
	}
	if !receipt.ExpiresAt.IsZero() && !now.Before(receipt.ExpiresAt) {
		return receipt, ErrApprovalExpired
	}
	receipt.NormalizedSource = append([]byte(nil), receipt.NormalizedSource...)
	receipt.ClaimedMutationID = mutationID
	receipt.ClaimedAt = timePointer(now)
	return receipt, nil
}
