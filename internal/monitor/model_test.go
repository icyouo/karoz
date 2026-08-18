package monitor

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestOriginValidationAndJSON(t *testing.T) {
	valid := []Origin{
		{Kind: "user"}, {Kind: "runtime"},
		{Kind: "monitor", MonitorID: "monitor-1", FireID: "fire-1"},
	}
	for _, origin := range valid {
		if err := origin.Validate(); err != nil {
			t.Fatalf("valid origin %+v: %v", origin, err)
		}
		body, err := json.Marshal(origin)
		if err != nil {
			t.Fatal(err)
		}
		var roundTrip Origin
		if err := json.Unmarshal(body, &roundTrip); err != nil || roundTrip != origin {
			t.Fatalf("origin round trip: %s, %+v, %v", body, roundTrip, err)
		}
	}
	for _, origin := range []Origin{
		{}, {Kind: "monitor", MonitorID: "monitor-1"}, {Kind: "user", FireID: "fire-1"},
	} {
		if origin.Validate() == nil {
			t.Fatalf("invalid origin accepted: %+v", origin)
		}
	}
}

func TestEventIdentityValidationAndDeterministicJSON(t *testing.T) {
	event := Event{
		ID: "task/task-1/7", ProjectID: "project-1",
		AuthorityID: "task-store", AuthorityGeneration: 7,
		Kind: "task_changed", EntityID: "task-1", Origin: Origin{Kind: "runtime"},
		At:      time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC),
		Payload: json.RawMessage(`{"from":"running","to":"done"}`),
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	first, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Event
	if err := json.Unmarshal(first, &roundTrip); err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(roundTrip)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("event JSON changed: %s != %s (%v)", first, second, err)
	}
	event.ID = ""
	if event.Validate() == nil {
		t.Fatal("incomplete stable event identity accepted")
	}
}
