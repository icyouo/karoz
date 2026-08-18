package monitor

import (
	"encoding/json"
	"errors"
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
) (PendingFire, error) {
	if item.ID == "" || item.Revision <= 0 || item.Sequence <= 0 ||
		event.ProjectID != item.ProjectID || event.Validate() != nil {
		return PendingFire{}, errors.New("invalid pending fire source")
	}
	if err := ValidateAction(item.Action); err != nil {
		return PendingFire{}, err
	}
	if !validRawJSON(eventBriefing) || !validRawJSON(actionPayload) {
		return PendingFire{}, errors.New("pending fire payload is invalid")
	}
	fireID := StableFireID(item.ID, event.ID)
	pending := PendingFire{
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
	if err := ValidatePendingFire(pending); err != nil {
		return PendingFire{}, err
	}
	return pending, nil
}

func ValidatePendingFire(pending PendingFire) error {
	if pending.ID == "" || pending.MonitorID == "" || pending.EventID == "" ||
		pending.DedupKey == "" || pending.Sequence <= 0 || pending.CreatedAt.IsZero() ||
		pending.ID != StableFireID(pending.MonitorID, pending.EventID) ||
		pending.DedupKey != pending.ID ||
		len(pending.Action.RenderedBriefing) > MaxEventPayload ||
		!validRawJSON(pending.EventBriefing) ||
		pending.Action.MonitorRevision <= 0 || pending.Action.ActionRevision <= 0 ||
		!validRawJSON(pending.Action.ActionPayload) {
		return errors.New("invalid pending fire")
	}
	action := Action{
		Revision: pending.Action.ActionRevision,
		Kind:     ActionKind(pending.Action.Kind),
		AgentID:  pending.Action.AgentID,
		TurnType: pending.Action.TurnType,
		Topic:    pending.Action.Topic,
	}
	if err := ValidateAction(action); err != nil {
		return err
	}
	switch pending.Status {
	case "pending", "cancelled":
		if pending.ClaimToken != "" {
			return errors.New("unclaimed pending fire has claim token")
		}
	case "admitting":
		if pending.ClaimToken == "" {
			return errors.New("admitting fire requires claim token")
		}
	default:
		return errors.New("invalid pending fire status")
	}
	return nil
}

func AdmitPendingFire(item Monitor, pending PendingFire) (Monitor, error) {
	if err := ValidatePendingFire(pending); err != nil {
		return item, err
	}
	if pending.MonitorID != item.ID {
		return item, errors.New("pending fire monitor mismatch")
	}
	for _, existing := range item.PendingFires {
		if err := ValidatePendingFire(existing); err != nil || existing.MonitorID != item.ID {
			return item, errors.New("existing pending fire state is invalid")
		}
	}
	if len(item.PendingFires) >= MaxPendingFires {
		item = cloneMonitor(item)
		item.State = StateError
		item.ErrorCode = "pending_fire_capacity"
		item.LastError = "pending fire capacity exceeded"
		return item, errors.New("pending fire capacity exceeded")
	}
	for _, existing := range item.PendingFires {
		if existing.ID == pending.ID || existing.DedupKey == pending.DedupKey {
			return item, errors.New("duplicate pending fire")
		}
	}
	item = cloneMonitor(item)
	pending.EventBriefing = append(json.RawMessage(nil), pending.EventBriefing...)
	pending.Action.ActionPayload = append(json.RawMessage(nil), pending.Action.ActionPayload...)
	item.PendingFires = append(item.PendingFires, pending)
	return item, nil
}
