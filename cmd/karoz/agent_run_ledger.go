package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const agentRunLedgerLimit = 512
const agentRunLedgerByteLimit = 1 << 20
const agentRunTerminalLedgerLimit = 32
const agentRunSubscriberBuffer = 32

type agentRunLedgerEvent struct {
	Seq   int64  `json:"seq"`
	Type  string `json:"type"`
	Data  any    `json:"data"`
	bytes int
}
type agentRunLedger struct {
	mu          sync.Mutex
	next, floor int64
	events      []agentRunLedgerEvent
	bytes       int
	terminal    bool
	terminalAt  time.Time
	subs        map[chan agentRunLedgerEvent]bool
}

func newAgentRunLedger() *agentRunLedger {
	return &agentRunLedger{subs: map[chan agentRunLedgerEvent]bool{}}
}
func (l *agentRunLedger) publish(kind string, data any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	e := newAgentRunLedgerEvent(l.next, kind, data)
	l.events = append(l.events, e)
	l.bytes += e.bytes
	for (len(l.events) > agentRunLedgerLimit || l.bytes > agentRunLedgerByteLimit) && len(l.events) > 1 {
		discarded := l.events[0]
		l.events = l.events[1:]
		l.bytes -= discarded.bytes
		// floor is the last discarded sequence, so a client can safely resume
		// at floor and still receive the first retained event (floor + 1).
		l.floor = discarded.Seq
	}
	if kind == "done" || kind == "error" || kind == "cancelled" {
		l.terminal = true
		l.terminalAt = time.Now().UTC()
	}
	for ch := range l.subs {
		select {
		case ch <- e:
		default:
			// A lagging observer must never silently lose ordering. Detach it;
			// its reconnect replays from its last observed sequence.
			delete(l.subs, ch)
			close(ch)
			continue
		}
		if l.terminal {
			delete(l.subs, ch)
			close(ch)
		}
	}
}

func newAgentRunLedgerEvent(seq int64, kind string, data any) agentRunLedgerEvent {
	e := agentRunLedgerEvent{Seq: seq, Type: kind, Data: data}
	e.bytes = encodedLedgerEventBytes(e)
	if e.bytes <= agentRunLedgerByteLimit {
		return e
	}
	// A single pathological provider payload cannot make the in-memory ledger
	// unbounded. Keep an explicit, inspectable marker rather than retaining it.
	e.Data = map[string]any{"truncated": true, "original_bytes": e.bytes}
	e.bytes = encodedLedgerEventBytes(e)
	return e
}

func encodedLedgerEventBytes(event agentRunLedgerEvent) int {
	encoded, err := json.Marshal(struct {
		Seq  int64  `json:"seq"`
		Type string `json:"type"`
		Data any    `json:"data"`
	}{event.Seq, event.Type, event.Data})
	if err != nil {
		return 128
	}
	return len(encoded)
}
func (l *agentRunLedger) subscribe(after int64) ([]agentRunLedgerEvent, bool, int64, chan agentRunLedgerEvent, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	reset := l.floor > 0 && after < l.floor
	replay := append([]agentRunLedgerEvent{}, l.events...)
	ch := make(chan agentRunLedgerEvent, agentRunSubscriberBuffer)
	if !l.terminal {
		l.subs[ch] = true
	} else {
		close(ch)
	}
	return replay, reset, l.floor, ch, func() {
		l.mu.Lock()
		if l.subs[ch] {
			delete(l.subs, ch)
			close(ch)
		}
		l.mu.Unlock()
	}
}
func (a *app) createRunLedger(runID string) *agentRunLedger {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agentRunLedgers == nil {
		a.agentRunLedgers = map[string]*agentRunLedger{}
	}
	for terminalLedgerCount(a.agentRunLedgers) >= agentRunTerminalLedgerLimit {
		oldestID := oldestTerminalLedgerID(a.agentRunLedgers)
		if oldestID == "" {
			break
		}
		delete(a.agentRunLedgers, oldestID)
	}
	l := newAgentRunLedger()
	a.agentRunLedgers[runID] = l
	return l
}

func terminalLedgerCount(ledgers map[string]*agentRunLedger) int {
	count := 0
	for _, ledger := range ledgers {
		ledger.mu.Lock()
		terminal := ledger.terminal
		ledger.mu.Unlock()
		if terminal {
			count++
		}
	}
	return count
}

func oldestTerminalLedgerID(ledgers map[string]*agentRunLedger) string {
	var oldestID string
	var oldestAt time.Time
	for id, ledger := range ledgers {
		ledger.mu.Lock()
		terminal, terminalAt := ledger.terminal, ledger.terminalAt
		ledger.mu.Unlock()
		if terminal && (oldestID == "" || terminalAt.Before(oldestAt)) {
			oldestID, oldestAt = id, terminalAt
		}
	}
	return oldestID
}
func (a *app) runLedger(runID string) *agentRunLedger {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.agentRunLedgers[runID]
}

func (a *app) streamRunLedger(w http.ResponseWriter, r *http.Request, runID string, after int64) {
	l := a.runLedger(runID)
	if l == nil {
		http.NotFound(w, r)
		return
	}
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	replay, reset, floor, ch, unsubscribe := l.subscribe(after)
	defer unsubscribe()
	if reset {
		writeSSE(w, "reset", map[string]any{"floor": floor, "run_id": runID})
		f.Flush()
	}
	write := func(e agentRunLedgerEvent) {
		if e.Seq > after {
			writeSSE(w, e.Type, map[string]any{"seq": e.Seq, "run_id": runID, "data": e.Data})
			f.Flush()
		}
	}
	for _, e := range replay {
		write(e)
	}
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return
			}
			write(e)
		case <-r.Context().Done():
			return
		}
	}
}
