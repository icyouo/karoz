package monitor

import (
	"encoding/json"
	"fmt"
	"time"
)

func StableFireID(monitorID, eventID string) string {
	return fmt.Sprintf("monitor/%s/%s", monitorID, eventID)
}

// FreezePendingFire snapshots the action and event data so later monitor edits
// cannot redirect a retry.
func FreezePendingFire(
	item Monitor,
	event Event,
	renderedBriefing string,
	eventBriefing, actionPayload json.RawMessage,
	now time.Time,
) PendingFire {
	fireID := StableFireID(item.ID, event.ID)
	return PendingFire{
		ID:            fireID,
		MonitorID:     item.ID,
		EventID:       event.ID,
		Sequence:      item.Sequence,
		DedupKey:      fireID,
		EventBriefing: append(json.RawMessage(nil), eventBriefing...),
		Action: FrozenAction{
			MonitorRevision:  item.Revision,
			ActionRevision:   item.Action.Revision,
			Kind:             string(item.Action.Kind),
			AgentID:          item.Action.AgentID,
			TurnType:         item.Action.TurnType,
			Topic:            item.Action.Topic,
			RenderedBriefing: renderedBriefing,
			ActionPayload:    append(json.RawMessage(nil), actionPayload...),
		},
		CreatedAt: now,
		Status:    "pending",
	}
}
