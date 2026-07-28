package monitor

import (
	"testing"
	"time"
)

func TestSourceGapsAreIndependentAndAcknowledgedPerEntry(t *testing.T) {
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	item := Monitor{State: StateActive}
	var err error
	item, err = ApplySourceGap(item, SourceGapStatus{
		AuthorityID: "task-store", SourceKind: "task_changed",
		GapVersion: 1, FirstVersion: 10, LastVersion: 12, LostCount: 3,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	item, err = ApplySourceGap(item, SourceGapStatus{
		AuthorityID: "plan-store", SourceKind: "plan_changed",
		GapVersion: 1, FirstVersion: 7, LastVersion: 7, LostCount: 1,
	}, now)
	if err != nil || len(item.SourceGaps) != 2 || item.State != StateDisabled {
		t.Fatalf("independent gaps = %+v, %v", item.SourceGaps, err)
	}
	item, err = AcknowledgeSourceGap(item, "task-store", "task_changed", 1, 15, "operator", now)
	if err != nil {
		t.Fatal(err)
	}
	if CanEnableAfterSourceGaps(item, map[string]uint64{
		SourceGapKey("task-store", "task_changed"): 15,
	}) == nil {
		t.Fatal("one acknowledgement enabled two gaps")
	}
	item, err = AcknowledgeSourceGap(item, "plan-store", "plan_changed", 1, 9, "operator", now)
	if err != nil {
		t.Fatal(err)
	}
	barriers := map[string]uint64{
		SourceGapKey("task-store", "task_changed"): 15,
		SourceGapKey("plan-store", "plan_changed"): 9,
	}
	item, err = EnableAfterSourceGaps(item, barriers, now)
	if err != nil || item.State != StateActive || item.ErrorCode != "" {
		t.Fatalf("enable = %+v, %v", item, err)
	}
}

func TestSourceGapNewVersionClearsAcknowledgementAndCap(t *testing.T) {
	now := time.Now()
	item := Monitor{State: StateActive}
	var err error
	item, err = ApplySourceGap(item, SourceGapStatus{
		AuthorityID: "a", SourceKind: "k", GapVersion: 1, FirstVersion: 1, LastVersion: 1, LostCount: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	item, err = AcknowledgeSourceGap(item, "a", "k", 1, 2, "operator", now)
	if err != nil {
		t.Fatal(err)
	}
	item, err = ApplySourceGap(item, SourceGapStatus{
		AuthorityID: "a", SourceKind: "k", GapVersion: 2, FirstVersion: 3, LastVersion: 4, LostCount: 2,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	gap := item.SourceGaps[SourceGapKey("a", "k")]
	if gap.AcknowledgedGapVersion != 0 || gap.FirstVersion != 3 || gap.LastVersion != 4 {
		t.Fatalf("new gap did not replace acknowledged range: %+v", gap)
	}

	full := Monitor{State: StateActive, SourceGaps: make(map[string]SourceGapStatus)}
	for index := 0; index < MaxSourceGaps; index++ {
		authority := string(rune('a' + index))
		full.SourceGaps[SourceGapKey(authority, "kind")] = SourceGapStatus{AuthorityID: authority, SourceKind: "kind"}
	}
	if _, err := ApplySourceGap(full, SourceGapStatus{
		AuthorityID: "overflow", SourceKind: "kind", GapVersion: 1, FirstVersion: 1, LastVersion: 1, LostCount: 1,
	}, now); err == nil {
		t.Fatal("17th source gap accepted")
	}
}

func TestSourceGapSameKeyMergeAndStaleVersion(t *testing.T) {
	now := time.Now()
	item, err := ApplySourceGap(Monitor{State: StateActive}, SourceGapStatus{
		AuthorityID: "task-store", SourceKind: "task_changed",
		GapVersion: 3, FirstVersion: 10, LastVersion: 11, LostCount: 2,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	item, err = ApplySourceGap(item, SourceGapStatus{
		AuthorityID: "task-store", SourceKind: "task_changed",
		GapVersion: 4, FirstVersion: 12, LastVersion: 14, LostCount: 3,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	gap := item.SourceGaps[SourceGapKey("task-store", "task_changed")]
	if gap.GapVersion != 4 || gap.FirstVersion != 10 || gap.LastVersion != 14 || gap.LostCount != 5 {
		t.Fatalf("unacknowledged ranges did not merge: %+v", gap)
	}
	if _, err := AcknowledgeSourceGap(item, "task-store", "task_changed", 3, 14, "operator", now); err == nil {
		t.Fatal("stale expected gap version accepted")
	}
}
