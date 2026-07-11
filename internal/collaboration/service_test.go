package collaboration

import (
	"testing"
	"time"
)

type memoryHandoffRepository struct{ handoff Handoff }

func (repository *memoryHandoffRepository) Get(projectID, agentID, messageID string) (Handoff, bool, error) {
	item := repository.handoff
	return item, item.ProjectID == projectID && item.TargetAgentID == agentID && item.ID == messageID, nil
}

func (repository *memoryHandoffRepository) Save(handoff Handoff) error {
	repository.handoff = handoff
	return nil
}

type memoryHandoffEvents struct{ changes []HandoffChange }

func (events *memoryHandoffEvents) HandoffChanged(change HandoffChange) {
	events.changes = append(events.changes, change)
}

func TestHandoffServiceTransitionsAndTimestamps(t *testing.T) {
	now := time.Date(2026, 7, 11, 1, 0, 0, 0, time.UTC)
	repository := &memoryHandoffRepository{handoff: Handoff{ID: "h1", ProjectID: "p1", TargetAgentID: "designer", Status: "delivered"}}
	events := &memoryHandoffEvents{}
	service := HandoffService{Repository: repository, Events: events, Now: func() time.Time { return now }}
	claimed, err := service.Transition("p1", "designer", "h1", "claimed", "")
	if err != nil || claimed.ClaimedAt == nil {
		t.Fatalf("claimed handoff = %+v err=%v", claimed, err)
	}
	working, err := service.Transition("p1", "designer", "h1", "working", "")
	if err != nil || working.WorkingAt == nil {
		t.Fatalf("working handoff = %+v err=%v", working, err)
	}
	replied, err := service.Transition("p1", "designer", "h1", "replied", "mockup ready")
	if err != nil || replied.RepliedAt == nil || replied.Result != "mockup ready" || len(events.changes) != 3 {
		t.Fatalf("replied handoff = %+v changes=%+v err=%v", replied, events.changes, err)
	}
	if _, err := service.Transition("p1", "designer", "h1", "working", ""); err == nil {
		t.Fatal("terminal handoff returned to working")
	}
}
