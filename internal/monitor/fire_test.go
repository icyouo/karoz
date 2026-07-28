package monitor

import (
	"testing"
	"time"
)

func TestFireGuardPrecedenceAndState(t *testing.T) {
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Second)
	item := Monitor{
		ID: "m", State: StateDisabled, MaxTriggers: 1, TriggerCount: 1,
		ExpiresAt: &expired, LastFiredAt: timePointer(now),
	}
	decision := Fire(item, "detail", now)
	if decision.Suppressed != "expired" || decision.Monitor.State != StateExpired {
		t.Fatalf("expiry did not win: %+v", decision)
	}

	item = Monitor{ID: "m", State: StateActive, CooldownMS: MinimumCooldownMS}
	first := Fire(item, "first", now)
	if !first.Fire || first.Monitor.TriggerCount != 1 || first.Monitor.Sequence != 1 {
		t.Fatalf("first fire = %+v", first)
	}
	second := Fire(first.Monitor, "second", now.Add(time.Second))
	if second.Fire || second.Suppressed != "cooldown" {
		t.Fatalf("cooldown = %+v", second)
	}
}

func TestFireRateCeilingAndMaxTriggers(t *testing.T) {
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	item := Monitor{ID: "m", State: StateActive, CooldownMS: MinimumCooldownMS}
	for index := 0; index < MaxFiresRateWindow; index++ {
		decision := Fire(item, "match", now.Add(time.Duration(index)*10*time.Second))
		if !decision.Fire {
			t.Fatalf("fire %d suppressed: %+v", index+1, decision)
		}
		item = decision.Monitor
	}
	seventh := Fire(item, "match", now.Add(70*time.Second))
	if seventh.Fire || seventh.Suppressed != "rate_limit" || seventh.Monitor.State != StateError || !seventh.AutoDisabled {
		t.Fatalf("rate ceiling = %+v", seventh)
	}

	limited := Fire(Monitor{ID: "m2", State: StateActive, MaxTriggers: 1}, "once", now)
	if !limited.Fire || limited.Monitor.State != StateExhausted {
		t.Fatalf("max-trigger terminal fire = %+v", limited)
	}
	next := Fire(limited.Monitor, "twice", now.Add(time.Minute))
	if next.Fire || next.Suppressed != string(StateExhausted) {
		t.Fatalf("exhausted monitor fired: %+v", next)
	}
}
