package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentRunLedgerReplaySubscribeAndReset(t *testing.T) {
	l := newAgentRunLedger()
	l.publish("delta", map[string]any{"delta": "one"})
	l.publish("tool_start", map[string]any{"tool": "x"})
	replay, reset, _, ch, unsubscribe := l.subscribe(1)
	defer unsubscribe()
	if reset || len(replay) != 2 || replay[1].Type != "tool_start" {
		t.Fatalf("replay=%+v reset=%t", replay, reset)
	}
	l.publish("tool_result", map[string]any{"tool": "x"})
	if event := <-ch; event.Seq != 3 || event.Type != "tool_result" {
		t.Fatalf("live=%+v", event)
	}
	for i := 0; i < agentRunLedgerLimit+2; i++ {
		l.publish("delta", i)
	}
	replay, reset, floor, _, stop := l.subscribe(1)
	defer stop()
	if !reset {
		t.Fatal("expected deterministic reset below retention floor")
	}
	if floor <= 0 || len(replay) == 0 || replay[0].Seq != floor+1 {
		t.Fatalf("reset floor must be last discarded event: floor=%d replay=%+v", floor, replay[:min(len(replay), 1)])
	}
}

func TestAgentRunLedgerBoundsEncodedBytesAndOversizedPayloads(t *testing.T) {
	l := newAgentRunLedger()
	for i := 0; i < 64; i++ {
		l.publish("delta", map[string]any{"delta": strings.Repeat("d", agentRunLedgerByteLimit/32)})
		l.publish("tool_result", map[string]any{"result": strings.Repeat("r", agentRunLedgerByteLimit/32)})
	}
	l.mu.Lock()
	bytes, count := l.bytes, len(l.events)
	l.mu.Unlock()
	if bytes > agentRunLedgerByteLimit || count > agentRunLedgerLimit {
		t.Fatalf("ledger bounds bytes=%d/%d events=%d/%d", bytes, agentRunLedgerByteLimit, count, agentRunLedgerLimit)
	}
	l.publish("delta", map[string]any{"delta": strings.Repeat("x", agentRunLedgerByteLimit*2)})
	l.mu.Lock()
	last := l.events[len(l.events)-1]
	bytes = l.bytes
	l.mu.Unlock()
	if bytes > agentRunLedgerByteLimit || last.Data.(map[string]any)["truncated"] != true {
		t.Fatalf("oversized event was not bounded: bytes=%d event=%+v", bytes, last)
	}
}

func TestAgentRunLedgerLaggingObserverClosesAndReplaysWithoutGap(t *testing.T) {
	l := newAgentRunLedger()
	_, _, _, slow, stop := l.subscribe(0)
	defer stop()
	for i := 1; i <= agentRunSubscriberBuffer+2; i++ {
		l.publish("delta", i)
	}
	var delivered []agentRunLedgerEvent
	for event := range slow {
		delivered = append(delivered, event)
	}
	if len(delivered) != agentRunSubscriberBuffer || delivered[0].Seq != 1 || delivered[len(delivered)-1].Seq != agentRunSubscriberBuffer {
		t.Fatalf("lagging observer delivery=%+v", delivered)
	}
	replay, reset, _, _, replayStop := l.subscribe(delivered[len(delivered)-1].Seq)
	defer replayStop()
	if reset {
		t.Fatal("lagging reconnect unexpectedly reset within retention")
	}
	var resumed []int64
	for _, event := range replay {
		if event.Seq > delivered[len(delivered)-1].Seq {
			resumed = append(resumed, event.Seq)
		}
	}
	if fmt.Sprint(resumed) != fmt.Sprint([]int64{33, 34}) {
		t.Fatalf("replay after slow observer gap=%v", resumed)
	}
}

func TestRunLedgerResetEmitsFirstRetainedSequenceExactlyOnce(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	ledger := a.createRunLedger("floor-boundary")
	for i := 0; i < agentRunLedgerLimit+2; i++ {
		ledger.publish("delta", map[string]any{"delta": i})
	}
	ledger.publish("done", map[string]any{"message": "complete"})
	recorder := httptest.NewRecorder()
	a.streamRunLedger(recorder, httptest.NewRequest(http.MethodGet, "/events?after=0", nil), "floor-boundary", 0)
	var floor, first int64
	for _, block := range strings.Split(recorder.Body.String(), "\n\n") {
		if strings.Contains(block, "event: reset") {
			var payload struct {
				Floor int64 `json:"floor"`
			}
			_ = json.Unmarshal([]byte(strings.TrimPrefix(strings.Split(block, "data: ")[1], "")), &payload)
			floor = payload.Floor
			continue
		}
		if first != 0 || !strings.Contains(block, "event: delta") {
			continue
		}
		var payload struct {
			Seq int64 `json:"seq"`
		}
		_ = json.Unmarshal([]byte(strings.TrimPrefix(strings.Split(block, "data: ")[1], "")), &payload)
		first = payload.Seq
	}
	if recorder.Code != http.StatusOK || floor == 0 || first != floor+1 {
		t.Fatalf("reset boundary status=%d floor=%d first=%d body=%s", recorder.Code, floor, first, recorder.Body.String())
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestAgentRunLedgerTwoObserversReceiveOneOrderedCopyEach(t *testing.T) {
	l := newAgentRunLedger()
	_, _, _, first, stopFirst := l.subscribe(0)
	defer stopFirst()
	_, _, _, second, stopSecond := l.subscribe(0)
	defer stopSecond()
	l.publish("tool_start", "one")
	l.publish("tool_result", "two")
	for _, ch := range []chan agentRunLedgerEvent{first, second} {
		if one := <-ch; one.Seq != 1 || one.Type != "tool_start" {
			t.Fatalf("first event=%+v", one)
		}
		if two := <-ch; two.Seq != 2 || two.Type != "tool_result" {
			t.Fatalf("second event=%+v", two)
		}
	}
}

func TestAgentRunLedgerTerminalRetentionIsBounded(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	for i := 0; i < agentRunTerminalLedgerLimit+3; i++ {
		ledger := a.createRunLedger(fmt.Sprintf("run-%d", i))
		ledger.publish("done", nil)
	}
	a.mu.Lock()
	count := terminalLedgerCount(a.agentRuntimeLocked().ledgers)
	_, oldestRetained := a.agentRuntimeLocked().ledgers["run-0"]
	a.mu.Unlock()
	if count != agentRunTerminalLedgerLimit {
		t.Fatalf("terminal ledgers=%d, want %d", count, agentRunTerminalLedgerLimit)
	}
	if oldestRetained {
		t.Fatal("oldest terminal ledger was not evicted")
	}
}
