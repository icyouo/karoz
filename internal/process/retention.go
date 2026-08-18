package process

import (
	"sort"
	"time"
)

type RetentionPolicy struct {
	MaxRecords    int
	MaxAge        time.Duration
	MaxTotalBytes int64
}

type RetentionRecord struct {
	Process
	ReaderOpen bool
}

// SelectRetention returns terminal records that may be removed, oldest first.
// Active records and records with open readers never satisfy pressure.
func SelectRetention(records []RetentionRecord, policy RetentionPolicy, now time.Time) []Process {
	eligible := make([]RetentionRecord, 0, len(records))
	terminalCount := 0
	var terminalBytes int64
	for _, record := range records {
		if !record.State.Terminal() {
			continue
		}
		terminalCount++
		if record.LogBytes > 0 {
			terminalBytes += record.LogBytes
		}
		if !record.ReaderOpen {
			eligible = append(eligible, record)
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		return retentionTime(eligible[i].Process).Before(retentionTime(eligible[j].Process))
	})

	selected := make([]Process, 0)
	selectedIDs := make(map[string]bool)
	remove := func(record RetentionRecord) {
		if selectedIDs[record.ID] {
			return
		}
		selectedIDs[record.ID] = true
		selected = append(selected, record.Process)
		terminalCount--
		if record.LogBytes > 0 {
			terminalBytes -= record.LogBytes
		}
	}
	if policy.MaxAge > 0 {
		cutoff := now.Add(-policy.MaxAge)
		for _, record := range eligible {
			if retentionTime(record.Process).Before(cutoff) {
				remove(record)
			}
		}
	}
	for _, record := range eligible {
		overRecords := policy.MaxRecords >= 0 && terminalCount > policy.MaxRecords
		overBytes := policy.MaxTotalBytes >= 0 && terminalBytes > policy.MaxTotalBytes
		if !overRecords && !overBytes {
			break
		}
		remove(record)
	}
	return selected
}

func retentionTime(item Process) time.Time {
	if item.EndedAt != nil {
		return *item.EndedAt
	}
	return item.UpdatedAt
}
