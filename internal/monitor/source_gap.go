package monitor

import (
	"errors"
	"fmt"
	"time"
)

type SourceGapStatus struct {
	SourceKind             string     `json:"source_kind"`
	AuthorityID            string     `json:"authority_id"`
	GapVersion             uint64     `json:"gap_version"`
	FirstVersion           uint64     `json:"first_version"`
	LastVersion            uint64     `json:"last_version"`
	LostCount              uint64     `json:"lost_count"`
	AcknowledgedGapVersion uint64     `json:"acknowledged_gap_version,omitempty"`
	AcknowledgedAt         *time.Time `json:"acknowledged_at,omitempty"`
	AcknowledgedBy         string     `json:"acknowledged_by,omitempty"`
	ResumeAfterVersion     uint64     `json:"resume_after_version,omitempty"`
}

func SourceGapKey(authorityID, sourceKind string) string {
	return fmt.Sprintf("%d:%s%d:%s", len(authorityID), authorityID, len(sourceKind), sourceKind)
}

func ApplySourceGap(item Monitor, gap SourceGapStatus, now time.Time) (Monitor, error) {
	if err := ValidateSourceGaps(item.SourceGaps); err != nil {
		return item, err
	}
	if gap.AuthorityID == "" || gap.SourceKind == "" || gap.GapVersion == 0 ||
		gap.FirstVersion == 0 || gap.LastVersion < gap.FirstVersion || gap.LostCount == 0 {
		return item, errors.New("invalid source gap")
	}
	item = cloneMonitor(item)
	if item.SourceGaps == nil {
		item.SourceGaps = make(map[string]SourceGapStatus)
	}
	key := SourceGapKey(gap.AuthorityID, gap.SourceKind)
	current, found := item.SourceGaps[key]
	if !found && len(item.SourceGaps) >= MaxSourceGaps {
		return item, errors.New("source gap capacity exceeded")
	}
	if found {
		if gap.GapVersion <= current.GapVersion {
			return item, errors.New("stale source gap")
		}
		if current.AcknowledgedGapVersion != current.GapVersion {
			if gap.FirstVersion < current.FirstVersion {
				current.FirstVersion = gap.FirstVersion
			}
			if gap.LastVersion > current.LastVersion {
				current.LastVersion = gap.LastVersion
			}
			current.LostCount += gap.LostCount
			current.GapVersion = gap.GapVersion
			gap = current
		}
	}
	gap.AcknowledgedGapVersion = 0
	gap.AcknowledgedAt = nil
	gap.AcknowledgedBy = ""
	gap.ResumeAfterVersion = 0
	item.SourceGaps[key] = gap
	item.State = StateDisabled
	item.ErrorCode = "source_gap"
	item.LastError = "monitor source coverage has a gap"
	item.UpdatedAt = now
	return item, nil
}

func AcknowledgeSourceGap(item Monitor, authorityID, sourceKind string, expectedGapVersion, barrier uint64, principal string, now time.Time) (Monitor, error) {
	if err := ValidateSourceGaps(item.SourceGaps); err != nil {
		return item, err
	}
	item = cloneMonitor(item)
	key := SourceGapKey(authorityID, sourceKind)
	gap, ok := item.SourceGaps[key]
	if !ok {
		return item, errors.New("source gap not found")
	}
	if expectedGapVersion == 0 || gap.GapVersion != expectedGapVersion {
		return item, errors.New("source gap version conflict")
	}
	if barrier < gap.LastVersion || principal == "" {
		return item, errors.New("invalid source gap acknowledgement")
	}
	gap.AcknowledgedGapVersion = gap.GapVersion
	gap.AcknowledgedAt = timePointer(now)
	gap.AcknowledgedBy = principal
	gap.ResumeAfterVersion = barrier
	item.SourceGaps[key] = gap
	item.UpdatedAt = now
	return item, nil
}

func CanEnableAfterSourceGaps(item Monitor, validatedBarriers map[string]uint64) error {
	if err := ValidateSourceGaps(item.SourceGaps); err != nil {
		return err
	}
	if len(item.SourceGaps) == 0 {
		return errors.New("source gap set is empty")
	}
	if item.State != StateDisabled || item.ErrorCode != "source_gap" {
		return errors.New("monitor is not disabled for source gap")
	}
	for key, gap := range item.SourceGaps {
		if gap.AcknowledgedGapVersion != gap.GapVersion || gap.AcknowledgedGapVersion == 0 {
			return fmt.Errorf("source gap %s is unacknowledged", key)
		}
		if validatedBarriers[key] != gap.ResumeAfterVersion {
			return fmt.Errorf("source gap %s barrier is stale", key)
		}
	}
	return nil
}

func EnableAfterSourceGaps(item Monitor, validatedBarriers map[string]uint64, now time.Time) (Monitor, error) {
	if err := CanEnableAfterSourceGaps(item, validatedBarriers); err != nil {
		return item, err
	}
	item = cloneMonitor(item)
	item.State = StateActive
	if item.ErrorCode == "source_gap" {
		item.ErrorCode = ""
		item.LastError = ""
	}
	item.UpdatedAt = now
	return item, nil
}
