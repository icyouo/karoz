package monitor

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFreezePendingFireIsStableAndImmutable(t *testing.T) {
	item := Monitor{
		ID: "monitor-1", ProjectID: "project-1", Revision: 4, Sequence: 3,
		Action: Action{
			Revision: 2, Kind: ActionNotifyAgent, AgentID: "agent-1",
			TurnType: "ask", Template: "original",
		},
	}
	event := Event{
		ID: "task/task-1/7", ProjectID: "project-1", AuthorityID: "task-store",
		AuthorityGeneration: 7, Kind: "task_changed", EntityID: "task-1",
		Origin: Origin{Kind: "runtime"},
	}
	eventBriefing := json.RawMessage(`{"state":"done"}`)
	actionPayload := json.RawMessage(`{"key":"value"}`)
	pending, err := FreezePendingFire(item, event, "task completed", eventBriefing, actionPayload, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if pending.ID != "monitor/monitor-1/task/task-1/7" || pending.DedupKey != pending.ID ||
		pending.Action.MonitorRevision != 4 || pending.Action.ActionRevision != 2 ||
		pending.Action.AgentID != "agent-1" || pending.Status != "pending" {
		t.Fatalf("frozen pending fire = %+v", pending)
	}

	item.Action.AgentID = "agent-2"
	item.Revision++
	eventBriefing[2] = 'X'
	actionPayload[2] = 'X'
	if pending.Action.AgentID != "agent-1" || pending.Action.MonitorRevision != 4 ||
		string(pending.EventBriefing) != `{"state":"done"}` ||
		string(pending.Action.ActionPayload) != `{"key":"value"}` {
		t.Fatalf("pending fire changed after source mutation: %+v", pending)
	}
}

func TestPendingFireBoundsAndDeepCopy(t *testing.T) {
	item := Monitor{ID: "monitor-1"}
	fireID := StableFireID(item.ID, "event-1")
	pending := PendingFire{
		ID: fireID, MonitorID: item.ID, EventID: "event-1", Sequence: 1,
		DedupKey: fireID, EventBriefing: json.RawMessage(`{"event":1}`),
		Action: FrozenAction{
			MonitorRevision: 1, ActionRevision: 1, Kind: string(ActionBlackboard),
			Topic: "status", ActionPayload: json.RawMessage(`{"action":1}`),
		},
		CreatedAt: time.Unix(1, 0), Status: "pending",
	}
	updated, err := AdmitPendingFire(item, pending)
	if err != nil {
		t.Fatal(err)
	}
	pending.EventBriefing[2] = 'X'
	pending.Action.ActionPayload[2] = 'X'
	if string(updated.PendingFires[0].EventBriefing) != `{"event":1}` ||
		string(updated.PendingFires[0].Action.ActionPayload) != `{"action":1}` {
		t.Fatal("pending admission retained caller payload aliases")
	}

	full := item
	for index := 0; index < MaxPendingFires; index++ {
		copy := updated.PendingFires[0]
		copy.EventID = fmt.Sprintf("event-%d", index)
		copy.ID = StableFireID(item.ID, copy.EventID)
		copy.DedupKey = copy.ID
		full.PendingFires = append(full.PendingFires, copy)
	}
	overflow, err := AdmitPendingFire(full, updated.PendingFires[0])
	if err == nil {
		t.Fatal("33rd pending fire admitted")
	}
	if overflow.State != StateError || overflow.ErrorCode != "pending_fire_capacity" {
		t.Fatalf("overflow did not fail monitor closed: %+v", overflow)
	}

	bad := updated.PendingFires[0]
	bad.EventBriefing = json.RawMessage(`{`)
	if _, err := AdmitPendingFire(item, bad); err == nil {
		t.Fatal("invalid event JSON admitted")
	}
	bad = updated.PendingFires[0]
	bad.Action.ActionPayload = json.RawMessage(`"` + strings.Repeat("x", MaxEventPayload) + `"`)
	if _, err := AdmitPendingFire(item, bad); err == nil {
		t.Fatal("oversized action payload admitted")
	}
}
