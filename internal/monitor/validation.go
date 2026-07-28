package monitor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

const MaxProbeSourceBytes = 64 * 1024
const MaxActionTemplateBytes = 16 * 1024

func ValidateAction(action Action) error {
	if action.Revision <= 0 || len(action.Template) > MaxActionTemplateBytes {
		return errors.New("invalid action revision")
	}
	switch action.Kind {
	case ActionNotifyAgent:
		if strings.TrimSpace(action.AgentID) == "" ||
			action.TurnType != "ask" && action.TurnType != "plan" ||
			action.Topic != "" {
			return errors.New("invalid notify_agent action")
		}
	case ActionBlackboard:
		if strings.TrimSpace(action.Topic) == "" || action.AgentID != "" || action.TurnType != "" {
			return errors.New("invalid blackboard action")
		}
	default:
		return errors.New("invalid action kind")
	}
	return nil
}

func ValidateMonitor(item Monitor) error {
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.ProjectID) == "" ||
		strings.TrimSpace(item.AgentID) == "" || strings.TrimSpace(item.Name) == "" ||
		item.Revision <= 0 {
		return errors.New("monitor identity is incomplete")
	}
	switch item.State {
	case StateActive, StateDisabled, StateExhausted, StateExpired, StateError:
	default:
		return errors.New("invalid monitor state")
	}
	if item.CooldownMS != 0 && item.CooldownMS < MinimumCooldownMS ||
		item.MaxTriggers < 0 || item.TriggerCount < 0 || item.Sequence < 0 ||
		item.ConsecutiveProbeErrors < 0 {
		return errors.New("invalid monitor guard state")
	}
	if err := ValidateTrigger(item.Trigger); err != nil {
		return err
	}
	if err := ValidateAction(item.Action); err != nil {
		return err
	}
	if len(item.PendingFires) > MaxPendingFires {
		return errors.New("pending fire capacity exceeded")
	}
	for _, pending := range item.PendingFires {
		if err := ValidatePendingFire(pending); err != nil {
			return err
		}
		if pending.MonitorID != item.ID {
			return errors.New("pending fire monitor mismatch")
		}
	}
	if err := ValidateSourceGaps(item.SourceGaps); err != nil {
		return err
	}
	if item.State == StateActive {
		for _, gap := range item.SourceGaps {
			if gap.AcknowledgedGapVersion != gap.GapVersion {
				return errors.New("active monitor has unacknowledged source gap")
			}
		}
	}
	return nil
}

func ValidateSourceGaps(gaps map[string]SourceGapStatus) error {
	if len(gaps) > MaxSourceGaps {
		return errors.New("source gap capacity exceeded")
	}
	for key, gap := range gaps {
		if key != SourceGapKey(gap.AuthorityID, gap.SourceKind) ||
			gap.AuthorityID == "" || gap.SourceKind == "" ||
			gap.GapVersion == 0 || gap.FirstVersion == 0 ||
			gap.LastVersion < gap.FirstVersion || gap.LostCount == 0 {
			return errors.New("invalid source gap")
		}
		if gap.AcknowledgedGapVersion != 0 {
			if gap.AcknowledgedGapVersion != gap.GapVersion ||
				gap.AcknowledgedAt == nil || gap.AcknowledgedBy == "" ||
				gap.ResumeAfterVersion < gap.LastVersion {
				return errors.New("invalid source gap acknowledgement")
			}
		} else if gap.AcknowledgedAt != nil || gap.AcknowledgedBy != "" || gap.ResumeAfterVersion != 0 {
			return errors.New("partial source gap acknowledgement")
		}
	}
	return nil
}

type ProbeAuthorizationCheck struct {
	MutationID       string
	CanonicalWorkdir string
	SnapshotPath     string
	SnapshotDevice   uint64
	SnapshotInode    uint64
	ModePerm         uint32
	Regular          bool
	Symlink          bool
	RuntimeOwned     bool
	Source           []byte
}

func ProbeAuthorized(item Monitor, receipt ProbeApprovalReceipt, check ProbeAuthorizationCheck) error {
	if item.Trigger.Kind != TriggerScriptProbe {
		return errors.New("monitor is not a script probe")
	}
	if err := ValidateTrigger(item.Trigger); err != nil {
		return err
	}
	if receipt.ID == "" || receipt.ID != item.Trigger.ApprovalReceiptID ||
		receipt.ProjectID != item.ProjectID || receipt.AgentID != item.AgentID ||
		receipt.MonitorID != item.ID || receipt.TriggerRevision != item.Trigger.Revision {
		return ErrApprovalSubjectMismatch
	}
	if receipt.ClaimedMutationID == "" || receipt.ClaimedMutationID != check.MutationID ||
		receipt.ClaimedAt == nil {
		return ErrApprovalSubjectMismatch
	}
	if receipt.RevokedAt != nil {
		return ErrApprovalRevoked
	}
	if receipt.Language != item.Trigger.ProbeLanguage ||
		receipt.SnapshotPath != item.Trigger.ProbePath ||
		receipt.SourceSHA256 != item.Trigger.ProbeSHA256 ||
		receipt.IntervalMS != item.Trigger.IntervalMS ||
		receipt.TimeoutMS != item.Trigger.TimeoutMS ||
		receipt.CanonicalWorkdir != check.CanonicalWorkdir {
		return ErrApprovalSubjectMismatch
	}
	if !validApprovalFlow(receipt) || receipt.ApprovedBy == "" || receipt.ApprovedAt.IsZero() {
		return errors.New("invalid probe approval receipt")
	}
	if receipt.ClaimedAt.Before(receipt.ApprovedAt) {
		return errors.New("probe approval claim predates approval")
	}
	if len(receipt.NormalizedSource) == 0 || len(receipt.NormalizedSource) > MaxProbeSourceBytes ||
		!validSHA256(receipt.SourceSHA256) ||
		sha256Hex(receipt.NormalizedSource) != receipt.SourceSHA256 ||
		!bytes.Equal(receipt.NormalizedSource, check.Source) {
		return errors.New("probe source mismatch")
	}
	if check.SnapshotPath != receipt.SnapshotPath ||
		check.SnapshotDevice == 0 || check.SnapshotDevice != receipt.SnapshotDevice ||
		check.SnapshotInode == 0 || check.SnapshotInode != receipt.SnapshotInode ||
		check.ModePerm != 0o600 || !check.Regular || check.Symlink || !check.RuntimeOwned {
		return errors.New("probe snapshot identity mismatch")
	}
	return nil
}

func validApprovalFlow(receipt ProbeApprovalReceipt) bool {
	switch receipt.ApprovalFlow {
	case "agent_choice":
		return receipt.ApprovalRunID != "" && receipt.ChoiceRequestID != "" &&
			receipt.OperatorSessionID == "" && receipt.ChallengeID == ""
	case "ui_challenge":
		return receipt.OperatorSessionID != "" && receipt.ChallengeID != "" &&
			receipt.ApprovalRunID == "" && receipt.ChoiceRequestID == ""
	default:
		return false
	}
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func validRawJSON(value json.RawMessage) bool {
	return len(value) <= MaxEventPayload && len(value) > 0 && json.Valid(value)
}
