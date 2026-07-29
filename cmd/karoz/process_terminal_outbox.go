package main

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	processdomain "github.com/karoz/karoz/internal/process"
)

const processTerminalOutboxRetryInterval = time.Second

type processTerminalOutboxEvent struct {
	ID         string
	ProjectID  string
	AgentID    string
	ProcessID  string
	RunID      string
	State      processdomain.State
	ExitCode   int
	OccurredAt time.Time
}

func (event processTerminalOutboxEvent) validate() error {
	if event.ID != processTerminalEventID(event.ProcessID) ||
		strings.TrimSpace(event.ProjectID) == "" ||
		strings.TrimSpace(event.AgentID) == "" ||
		!safeProcessID(event.ProcessID) ||
		!event.State.Terminal() ||
		event.OccurredAt.IsZero() {
		return errors.New("invalid process terminal outbox event")
	}
	return nil
}

func (event processTerminalOutboxEvent) runtimeEvent() RuntimeEvent {
	exitCode := event.ExitCode
	return RuntimeEvent{
		ID: event.ID, ProjectID: event.ProjectID,
		Kind: processTerminalEventKind, EntityID: event.ProcessID,
		AgentID: event.AgentID, RunID: event.RunID,
		To: string(event.State), ExitCode: &exitCode,
		Reason: "process_terminal", CreatedAt: event.OccurredAt,
	}
}

// PendingTerminalEvents returns only validated events from ready project
// partitions. A corrupt or disabled project is reported and skipped so another
// project's terminal slot can still be delivered and released.
func (runtime *processRuntimePersistence) PendingTerminalEvents() (
	[]processTerminalOutboxEvent,
	[]error,
) {
	runtime.registryMu.RLock()
	projects := make([]*processProjectRuntime, 0, len(runtime.projects))
	for _, project := range runtime.projects {
		projects = append(projects, project)
	}
	runtime.registryMu.RUnlock()
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].identity.SafeProjectKey <
			projects[j].identity.SafeProjectKey
	})

	events := make([]processTerminalOutboxEvent, 0)
	var faults []error
	for _, project := range projects {
		if runtime.projectDisabled(project.identity.SafeProjectKey) {
			continue
		}
		projectEvents := make([]processTerminalOutboxEvent, 0)
		var projectFault error
		project.lane.Lock()
		runtime.authorityMu.Lock()
		partition, exists := runtime.authority.Projects[project.identity.SafeProjectKey]
		if !exists || partition.Project != project.identity {
			projectFault = fmt.Errorf(
				"project %s terminal outbox authority is unavailable",
				project.identity.ProjectID,
			)
		}
		for processID, record := range partition.Records {
			if projectFault != nil {
				break
			}
			if record.Event == nil {
				continue
			}
			if err := validateProcessTerminalEvent(
				project.identity,
				record,
			); err != nil {
				projectFault = fmt.Errorf(
					"project %s process %s terminal outbox: %w",
					project.identity.ProjectID,
					processID,
					err,
				)
				break
			}
			event := processTerminalOutboxEvent{
				ID:         record.Event.ID,
				ProjectID:  project.identity.ProjectID,
				AgentID:    record.Process.AgentID,
				ProcessID:  processID,
				RunID:      record.Process.RunID,
				State:      record.Event.State,
				ExitCode:   record.Event.ExitCode,
				OccurredAt: record.Event.OccurredAt,
			}
			if err := event.validate(); err != nil {
				projectFault = fmt.Errorf(
					"project %s process %s terminal outbox: %w",
					project.identity.ProjectID,
					processID,
					err,
				)
				break
			}
			projectEvents = append(projectEvents, event)
		}
		runtime.authorityMu.Unlock()
		project.lane.Unlock()
		if projectFault != nil {
			runtime.disableProject(project.identity, projectFault)
			faults = append(faults, projectFault)
			continue
		}
		events = append(events, projectEvents...)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].OccurredAt.Equal(events[j].OccurredAt) {
			return events[i].ID < events[j].ID
		}
		return events[i].OccurredAt.Before(events[j].OccurredAt)
	})
	return events, faults
}

func (a *app) startProcessTerminalOutbox() {
	a.processTerminalWorkerOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(processTerminalOutboxRetryInterval)
			defer ticker.Stop()
			for {
				select {
				case <-a.supervisorCtx.Done():
					return
				case <-a.processTerminalWake:
					a.drainProcessTerminalOutbox()
				case <-ticker.C:
					a.drainProcessTerminalOutbox()
				}
			}
		}()
	})
	// Startup recovery is attempted synchronously. A failed sink leaves the
	// durable event and terminal reservation for the bounded retry worker.
	a.drainProcessTerminalOutbox()
}

func (a *app) wakeProcessTerminalOutbox() {
	if a.processTerminalWake == nil {
		return
	}
	select {
	case a.processTerminalWake <- struct{}{}:
	default:
	}
}

func (a *app) drainProcessTerminalOutbox() {
	a.processTerminalDrainMu.Lock()
	defer a.processTerminalDrainMu.Unlock()
	runtime := a.processRuntime
	if runtime == nil {
		return
	}
	events, faults := runtime.PendingTerminalEvents()
	for _, fault := range faults {
		log.Printf("process terminal outbox skipped invalid event: %v", fault)
	}
	for _, event := range events {
		runtimeEvent := event.runtimeEvent()
		accepted, err := a.acceptProcessTerminalEvent(runtimeEvent)
		if err != nil {
			log.Printf(
				"process terminal outbox delivery %s failed: %v",
				event.ID,
				err,
			)
			continue
		}
		if accepted {
			a.emitRuntimeStateChanged(runtimeEvent)
		}
		if err := runtime.AcknowledgeTerminal(
			event.ProjectID,
			event.ProcessID,
			event.ID,
		); err != nil {
			log.Printf(
				"process terminal outbox acknowledgement %s failed: %v",
				event.ID,
				err,
			)
			continue
		}
	}
}

func (a *app) acceptProcessTerminalEvent(
	event RuntimeEvent,
) (bool, error) {
	if a.processEventSink == nil {
		return false, errors.New("process runtime event sink is unavailable")
	}
	return a.processEventSink.Accept(event)
}
