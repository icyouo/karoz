package main

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"

	monitordomain "github.com/karoz/karoz/internal/monitor"
	processdomain "github.com/karoz/karoz/internal/process"
)

const (
	processRuntimeEventSinkSchemaVersion = 1
	processRuntimeEventSinkFile          = "process-runtime-events.json"
)

type processRuntimeEventSinkSnapshot struct {
	SchemaVersion int                                `json:"schema_version"`
	Projects      map[string]map[string]RuntimeEvent `json:"projects"`
}

type processRuntimeEventSink struct {
	mu       sync.Mutex
	store    *secureRuntimeStore
	snapshot processRuntimeEventSinkSnapshot
	fail     func(processPersistenceFailpoint) error
}

func newProcessRuntimeEventSink(
	dataDir string,
	fail func(processPersistenceFailpoint) error,
) (*processRuntimeEventSink, error) {
	store, err := newSecureRuntimeStore(dataDir)
	if err != nil {
		return nil, err
	}
	snapshot := processRuntimeEventSinkSnapshot{}
	found, err := store.loadJSON(processRuntimeEventSinkFile, &snapshot)
	if err != nil {
		return nil, err
	}
	if !found {
		snapshot = processRuntimeEventSinkSnapshot{
			SchemaVersion: processRuntimeEventSinkSchemaVersion,
			Projects:      map[string]map[string]RuntimeEvent{},
		}
	} else if err := validateProcessRuntimeEventSinkSnapshot(snapshot); err != nil {
		return nil, err
	}
	return &processRuntimeEventSink{
		store: store, snapshot: snapshot, fail: fail,
	}, nil
}

func (sink *processRuntimeEventSink) Accept(
	event RuntimeEvent,
) (bool, error) {
	if sink == nil {
		return false, errors.New("process runtime event sink is unavailable")
	}
	if err := validateProcessRuntimeEvent(event); err != nil {
		return false, err
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if existing, ok := sink.snapshot.Projects[event.ProjectID][event.ID]; ok {
		if !reflect.DeepEqual(existing, event) {
			return false, errors.New("process runtime event identity collision")
		}
		return false, nil
	}
	if len(sink.snapshot.Projects[event.ProjectID]) >=
		int(monitordomain.TerminalReservationCapacity) {
		return false, errors.New("process runtime event sink is full")
	}
	if err := sink.persistenceFail(processPersistBeforeRuntimeEventSink); err != nil {
		return false, err
	}
	candidate := cloneProcessRuntimeEventSinkSnapshot(sink.snapshot)
	if candidate.Projects[event.ProjectID] == nil {
		candidate.Projects[event.ProjectID] = map[string]RuntimeEvent{}
	}
	candidate.Projects[event.ProjectID][event.ID] = cloneRuntimeEvent(event)
	if err := sink.store.saveJSON(processRuntimeEventSinkFile, candidate); err != nil {
		return false, err
	}
	sink.snapshot = candidate
	if err := sink.persistenceFail(processPersistAfterRuntimeEventSink); err != nil {
		return false, err
	}
	return true, nil
}

// Pending returns the durable, stable process events that a future monitor
// consumer can apply idempotently before removing its own receipt.
func (sink *processRuntimeEventSink) Pending(projectID string) []RuntimeEvent {
	if sink == nil {
		return nil
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	events := make([]RuntimeEvent, 0, len(sink.snapshot.Projects[projectID]))
	for _, event := range sink.snapshot.Projects[projectID] {
		events = append(events, cloneRuntimeEvent(event))
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].CreatedAt.Equal(events[j].CreatedAt) {
			return events[i].ID < events[j].ID
		}
		return events[i].CreatedAt.Before(events[j].CreatedAt)
	})
	return events
}

func (sink *processRuntimeEventSink) persistenceFail(
	point processPersistenceFailpoint,
) error {
	if sink.fail == nil {
		return nil
	}
	return sink.fail(point)
}

func validateProcessRuntimeEventSinkSnapshot(
	snapshot processRuntimeEventSinkSnapshot,
) error {
	if snapshot.SchemaVersion != processRuntimeEventSinkSchemaVersion ||
		snapshot.Projects == nil {
		return errors.New("invalid process runtime event sink header")
	}
	for projectID, events := range snapshot.Projects {
		if strings.TrimSpace(projectID) == "" ||
			len(events) > int(monitordomain.TerminalReservationCapacity) {
			return errors.New("invalid process runtime event sink project")
		}
		for eventID, event := range events {
			if event.ProjectID != projectID || event.ID != eventID {
				return errors.New("process runtime event sink identity mismatch")
			}
			if err := validateProcessRuntimeEvent(event); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateProcessRuntimeEvent(event RuntimeEvent) error {
	if event.ID != processTerminalEventID(event.EntityID) ||
		strings.TrimSpace(event.ProjectID) == "" ||
		strings.TrimSpace(event.AgentID) == "" ||
		!safeProcessID(event.EntityID) ||
		event.Kind != processTerminalEventKind ||
		event.Reason != "process_terminal" ||
		!processdomain.State(event.To).Terminal() ||
		event.ExitCode == nil ||
		event.CreatedAt.IsZero() {
		return errors.New("invalid process runtime event")
	}
	return nil
}

func cloneProcessRuntimeEventSinkSnapshot(
	snapshot processRuntimeEventSinkSnapshot,
) processRuntimeEventSinkSnapshot {
	cloned := processRuntimeEventSinkSnapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Projects: make(
			map[string]map[string]RuntimeEvent,
			len(snapshot.Projects),
		),
	}
	for projectID, events := range snapshot.Projects {
		cloned.Projects[projectID] = make(map[string]RuntimeEvent, len(events))
		for eventID, event := range events {
			cloned.Projects[projectID][eventID] = cloneRuntimeEvent(event)
		}
	}
	return cloned
}

func cloneRuntimeEvent(event RuntimeEvent) RuntimeEvent {
	cloned := event
	if event.ExitCode != nil {
		exitCode := *event.ExitCode
		cloned.ExitCode = &exitCode
	}
	return cloned
}
