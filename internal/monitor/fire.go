package monitor

import (
	"fmt"
	"time"
)

type Decision struct {
	Monitor      Monitor
	Fire         bool
	Changed      bool
	Suppressed   string
	AutoDisabled bool
	DedupKey     string
	Detail       string
}

func Fire(item Monitor, detail string, now time.Time) Decision {
	item = cloneMonitor(item)
	decision := Decision{Monitor: item, Detail: detail}
	if item.ExpiresAt != nil && !now.Before(*item.ExpiresAt) {
		decision.Monitor.State = StateExpired
		decision.Monitor.UpdatedAt = now
		decision.Changed = true
		decision.AutoDisabled = true
		decision.Suppressed = "expired"
		return decision
	}
	if item.State != StateActive {
		decision.Suppressed = string(item.State)
		return decision
	}
	if item.MaxTriggers > 0 && item.TriggerCount >= item.MaxTriggers {
		decision.Monitor.State = StateExhausted
		decision.Monitor.UpdatedAt = now
		decision.Changed = true
		decision.AutoDisabled = true
		decision.Suppressed = "exhausted"
		return decision
	}
	cooldown := item.CooldownMS
	if cooldown == 0 {
		cooldown = DefaultCooldownMS
	}
	if cooldown < MinimumCooldownMS {
		cooldown = MinimumCooldownMS
	}
	if item.LastFiredAt != nil && now.Sub(*item.LastFiredAt) < time.Duration(cooldown)*time.Millisecond {
		decision.Suppressed = "cooldown"
		return decision
	}
	cutoff := now.Add(-RateWindow)
	recent := item.RecentFires[:0]
	for _, firedAt := range item.RecentFires {
		if !firedAt.Before(cutoff) {
			recent = append(recent, firedAt)
		}
	}
	decision.Monitor.RecentFires = recent
	if len(recent) >= MaxFiresRateWindow {
		decision.Monitor.State = StateError
		decision.Monitor.ErrorCode = "rate_limit"
		decision.Monitor.LastError = "monitor exceeded 6 fires in 5 minutes"
		decision.Monitor.UpdatedAt = now
		decision.Changed = true
		decision.AutoDisabled = true
		decision.Suppressed = "rate_limit"
		return decision
	}
	decision.Monitor.TriggerCount++
	decision.Monitor.Sequence++
	decision.Monitor.LastMatch = detail
	decision.Monitor.LastFiredAt = timePointer(now)
	decision.Monitor.RecentFires = append(decision.Monitor.RecentFires, now)
	decision.Monitor.UpdatedAt = now
	decision.Fire = true
	decision.Changed = true
	decision.DedupKey = fmt.Sprintf("monitor/%s/fire/%d", item.ID, decision.Monitor.Sequence)
	if item.MaxTriggers > 0 && decision.Monitor.TriggerCount >= item.MaxTriggers {
		decision.Monitor.State = StateExhausted
		decision.AutoDisabled = true
	}
	return decision
}

func cloneMonitor(item Monitor) Monitor {
	item.RecentFires = append([]time.Time(nil), item.RecentFires...)
	item.PendingFires = append([]PendingFire(nil), item.PendingFires...)
	if item.SourceGaps != nil {
		sourceGaps := item.SourceGaps
		item.SourceGaps = make(map[string]SourceGapStatus, len(sourceGaps))
		for key, gap := range sourceGaps {
			item.SourceGaps[key] = gap
		}
	}
	return item
}

func timePointer(value time.Time) *time.Time {
	copy := value
	return &copy
}
