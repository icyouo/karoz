package process

import (
	"fmt"
	"testing"
	"time"
)

func TestSelectRetention(t *testing.T) {
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	record := func(id string, state State, age time.Duration, bytes int64, open bool) RetentionRecord {
		ended := now.Add(-age)
		return RetentionRecord{
			Process:    Process{ID: id, State: state, LogBytes: bytes, EndedAt: &ended, UpdatedAt: ended},
			ReaderOpen: open,
		}
	}
	records := []RetentionRecord{
		record("active", StateRunning, 100*time.Hour, 100, false),
		record("open", StateFailed, 100*time.Hour, 100, true),
		record("old", StateSucceeded, 10*time.Hour, 40, false),
		record("middle", StateKilled, 5*time.Hour, 40, false),
		record("new", StateInterrupted, time.Hour, 40, false),
	}
	selected := SelectRetention(records, RetentionPolicy{
		MaxRecords: 3, MaxAge: 7 * time.Hour, MaxTotalBytes: 150,
	}, now)
	var ids []string
	for _, item := range selected {
		ids = append(ids, item.ID)
	}
	if got := fmt.Sprint(ids); got != "[old middle]" {
		t.Fatalf("selected %s", got)
	}
}
