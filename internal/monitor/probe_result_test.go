package monitor

import (
	"strings"
	"testing"
	"time"
)

func TestParseProbeResult(t *testing.T) {
	result, err := ParseProbeResult(ProbeExecution{Stdout: []byte(`{"matched":true,"detail":"degraded"}`)})
	if err != nil || !result.Matched || result.Detail != "degraded" {
		t.Fatalf("valid result = %+v, %v", result, err)
	}
	result, err = ParseProbeResult(ProbeExecution{Stdout: []byte(`{"matched":false}`)})
	if err != nil || result.Matched {
		t.Fatalf("false result = %+v, %v", result, err)
	}
	invalid := []ProbeExecution{
		{Stdout: []byte(`{}`)},
		{Stdout: []byte(`{"matched":"yes"}`)},
		{Stdout: []byte(`{"matched":null}`)},
		{Stdout: []byte(`{"matched":false,"detail":null}`)},
		{Stdout: []byte(`{"matched":false,"unexpected":"ignored"}`)},
		{Stdout: []byte(`{"matched":true} {"matched":false}`)},
		{Stdout: []byte(`{"matched":true,"detail":"` + strings.Repeat("x", MaxProbeDetail+1) + `"}`)},
		{Stdout: []byte(`{"matched":true}`), ExitCode: 1},
		{Stdout: []byte(`{"matched":true}`), TimedOut: true},
		{Stdout: []byte(`{"matched":true}`), Signaled: true},
	}
	for index, execution := range invalid {
		if _, err := ParseProbeResult(execution); err == nil {
			t.Fatalf("invalid result %d accepted", index)
		}
	}
}

func TestMatchProbeErrorState(t *testing.T) {
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	item := Monitor{
		State: StateActive, Trigger: Trigger{Kind: TriggerScriptProbe, IntervalMS: 60_000},
	}
	for index := 0; index < 3; index++ {
		item, _, _ = MatchProbe(item, ProbeResult{}, errInvalidTrigger, now.Add(time.Duration(index)*time.Minute))
	}
	if item.State != StateError || item.ErrorCode != "probe_error" || item.ConsecutiveProbeErrors != 3 {
		t.Fatalf("probe error state = %+v", item)
	}
	item.State = StateActive
	item, matched, detail := MatchProbe(item, ProbeResult{Matched: true, Detail: "ready"}, nil, now.Add(4*time.Minute))
	if !matched || detail != "ready" || item.ConsecutiveProbeErrors != 0 || item.LastCheckedAt == nil || item.NextCheckAt == nil {
		t.Fatalf("successful probe = %+v, %t, %q", item, matched, detail)
	}
}
