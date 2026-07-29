package main

import (
	"fmt"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
	processdomain "github.com/karoz/karoz/internal/process"
)

const processOutputGapRetryDelay = time.Second

func (a *app) armProcessOutputGapWorker() {
	a.processOutputGapWorkerOnce.Do(func() {
		go func() {
			for {
				select {
				case <-a.supervisorCtx.Done():
					return
				case <-a.processOutputGapWake:
					a.drainProcessOutputGaps()
				}
			}
		}()
	})
}

func (a *app) drainProcessOutputGaps() {
	pending := a.takeProcessOutputGapDeltas()
	retry := false
	for _, delta := range pending {
		if err := a.applyProcessOutputGap(delta); err != nil {
			a.returnProcessOutputGapDelta(delta)
			retry = true
		}
	}
	if retry {
		a.scheduleProcessOutputGapRetry()
	}
}

func (a *app) takeProcessOutputGapDeltas() map[string]processOutputGapDelta {
	a.processOutputGapMu.Lock()
	defer a.processOutputGapMu.Unlock()
	pending := a.processOutputPendingGaps
	a.processOutputPendingGaps = make(map[string]processOutputGapDelta)
	return pending
}

func (a *app) returnProcessOutputGapDelta(delta processOutputGapDelta) {
	key := projectAgentKey(delta.ProjectID, delta.ProcessID)
	a.processOutputGapMu.Lock()
	current := a.processOutputPendingGaps[key]
	a.processOutputPendingGaps[key] = mergeProcessOutputGapDeltas(delta, current)
	a.processOutputGapMu.Unlock()
}

func (a *app) scheduleProcessOutputGapRetry() {
	time.AfterFunc(processOutputGapRetryDelay, func() {
		select {
		case <-a.supervisorCtx.Done():
			return
		default:
		}
		select {
		case a.processOutputGapWake <- struct{}{}:
		default:
		}
	})
}

func (a *app) applyProcessOutputGap(delta processOutputGapDelta) error {
	if delta.ProjectID == "" || delta.ProcessID == "" ||
		delta.LostLines == 0 || delta.GapCount == 0 ||
		delta.OldestSeq == 0 || delta.NewestSeq < delta.OldestSeq {
		return fmt.Errorf("invalid process output gap delta")
	}
	if err := a.saveProcessOutputGapDiagnostics(delta); err != nil {
		return err
	}
	if a.processRuntime == nil {
		return fmt.Errorf("process runtime is unavailable")
	}
	return a.processRuntime.ApplyOutputGapDelta(delta)
}

func (a *app) saveProcessOutputGapDiagnostics(delta processOutputGapDelta) error {
	a.mu.Lock()
	items := a.monitors[delta.ProjectID]
	before := cloneMonitorList(items)
	changed := false
	now := time.Now().UTC()
	for index := range items {
		item := &items[index]
		if item.State != monitordomain.StateActive ||
			item.Trigger.Kind != monitordomain.TriggerProcessOutput ||
			item.Trigger.ProcessID != delta.ProcessID {
			continue
		}
		baseline := a.processOutputBaselines[projectAgentKey(delta.ProjectID, item.ID)]
		if baseline >= delta.NewestSeq {
			continue
		}
		item.ErrorCode = "output_gap"
		item.LastError = limitString(
			fmt.Sprintf(
				"process output coverage degraded: lost %d line(s), seq %d..%d",
				delta.LostLines,
				delta.OldestSeq,
				delta.NewestSeq,
			),
			500,
		)
		item.UpdatedAt = now
		changed = true
	}
	if !changed {
		a.mu.Unlock()
		return nil
	}
	a.monitors[delta.ProjectID] = items
	if err := a.saveMonitorsLocked(); err != nil {
		a.monitors[delta.ProjectID] = before
		a.mu.Unlock()
		return err
	}
	a.mu.Unlock()
	a.broadcastRuntimeEvent(RuntimeEvent{
		ID:        randomID(),
		ProjectID: delta.ProjectID,
		Kind:      "monitor_changed",
		EntityID:  delta.ProcessID,
		Reason:    "output_gap",
		CreatedAt: now,
	})
	return nil
}

func mergeProcessOutputGapDeltas(
	older processOutputGapDelta,
	newer processOutputGapDelta,
) processOutputGapDelta {
	if older.LostLines == 0 {
		return cloneProcessOutputGapDelta(newer)
	}
	if newer.LostLines == 0 {
		return cloneProcessOutputGapDelta(older)
	}
	merged := processOutputGapDelta{
		ProjectID: older.ProjectID,
		ProcessID: older.ProcessID,
		LostLines: older.LostLines + newer.LostLines,
		GapCount:  older.GapCount + newer.GapCount,
		OldestSeq: minNonZero(older.OldestSeq, newer.OldestSeq),
		NewestSeq: maxUint64(older.NewestSeq, newer.NewestSeq),
	}
	ranges := append(
		append([]processdomain.SeqRange(nil), older.Recent...),
		newer.Recent...,
	)
	for _, candidate := range ranges {
		if len(merged.Recent) > 0 &&
			candidate.Start == merged.Recent[len(merged.Recent)-1].End+1 {
			merged.Recent[len(merged.Recent)-1].End = candidate.End
			if merged.GapCount > 0 {
				merged.GapCount--
			}
			continue
		}
		merged.Recent = append(merged.Recent, candidate)
		if len(merged.Recent) > 32 {
			merged.Recent = merged.Recent[1:]
		}
	}
	return merged
}

func cloneProcessOutputGapDelta(delta processOutputGapDelta) processOutputGapDelta {
	delta.Recent = append([]processdomain.SeqRange(nil), delta.Recent...)
	return delta
}

func minNonZero(left, right uint64) uint64 {
	if left == 0 {
		return right
	}
	if right == 0 || left < right {
		return left
	}
	return right
}

func maxUint64(left, right uint64) uint64 {
	if left > right {
		return left
	}
	return right
}

func preserveProcessOutputCoverage(
	record processdomain.Process,
	coverage processdomain.Process,
) processdomain.Process {
	record.OutputSeq = maxUint64(record.OutputSeq, coverage.OutputSeq)
	record.OutputGaps = append(
		[]processdomain.SeqRange(nil),
		coverage.OutputGaps...,
	)
	record.OutputGapCount = coverage.OutputGapCount
	record.OutputLostLines = coverage.OutputLostLines
	record.OutputGapOldestSeq = coverage.OutputGapOldestSeq
	record.OutputGapNewestSeq = coverage.OutputGapNewestSeq
	return record
}
