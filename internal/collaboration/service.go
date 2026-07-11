package collaboration

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type HandoffRepository interface {
	Get(projectID, agentID, messageID string) (Handoff, bool, error)
	Save(Handoff) error
}

type HandoffChange struct {
	Handoff Handoff
	From    string
	To      string
	Reason  string
	At      time.Time
}

type HandoffEventSink interface {
	HandoffChanged(HandoffChange)
}

type HandoffService struct {
	Repository HandoffRepository
	Events     HandoffEventSink
	Now        func() time.Time
}

func (service HandoffService) Transition(projectID, agentID, messageID, next, result string) (Handoff, error) {
	if service.Repository == nil {
		return Handoff{}, errors.New("handoff service is not configured")
	}
	handoff, found, err := service.Repository.Get(projectID, agentID, messageID)
	if err != nil {
		return Handoff{}, err
	}
	if !found {
		return Handoff{}, errors.New("handoff not found")
	}
	previous := string(NormalizeHandoffStatus(handoff.Status))
	next = string(NormalizeHandoffStatus(next))
	if !ValidHandoffTransition(previous, next) {
		return Handoff{}, fmt.Errorf("invalid handoff transition %s -> %s", previous, next)
	}
	now := service.now()
	handoff.Status = next
	handoff.UpdatedAt = now
	if strings.TrimSpace(result) != "" {
		handoff.Result = strings.TrimSpace(result)
	}
	switch HandoffStatus(next) {
	case HandoffDelivered:
		handoff.DeliveredAt = &now
		handoff.FailureReason = ""
	case HandoffClaimed:
		handoff.ClaimedAt = &now
	case HandoffWorking:
		handoff.WorkingAt = &now
	case HandoffReplied:
		handoff.RepliedAt = &now
		handoff.ConsumedAt = &now
	case HandoffDeclined:
		handoff.DeclinedAt = &now
		handoff.ConsumedAt = &now
	case HandoffFailed:
		handoff.FailedAt = &now
		handoff.FailureReason = strings.TrimSpace(result)
	case HandoffClosed:
		handoff.ClosedAt = &now
		handoff.ConsumedAt = &now
	}
	if err := service.Repository.Save(handoff); err != nil {
		return Handoff{}, err
	}
	if service.Events != nil {
		service.Events.HandoffChanged(HandoffChange{Handoff: handoff, From: previous, To: next, Reason: "handoff_" + next, At: now})
	}
	return handoff, nil
}

func (service HandoffService) now() time.Time {
	if service.Now != nil {
		return service.Now().UTC()
	}
	return time.Now().UTC()
}
