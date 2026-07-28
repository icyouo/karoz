package monitor

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFreezePendingFireIsStableAndImmutable(t *testing.T) {
	item := Monitor{
		ID: "monitor-1", Revision: 4, Sequence: 3,
		Action: Action{
			Revision: 2, Kind: ActionNotifyAgent, AgentID: "agent-1",
			TurnType: "ask", Template: "original",
		},
	}
	event := Event{ID: "task/task-1/7"}
	eventBriefing := json.RawMessage(`{"state":"done"}`)
	actionPayload := json.RawMessage(`{"key":"value"}`)
	pending := FreezePendingFire(item, event, "task completed", eventBriefing, actionPayload, time.Unix(1, 0))
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
