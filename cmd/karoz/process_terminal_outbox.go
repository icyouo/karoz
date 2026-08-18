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
	outbox := a.processTerminalOutbox
	outbox.workerOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(processTerminalOutboxRetryInterval)
			defer ticker.Stop()
			for {
				select {
				case <-a.supervisorCtx.Done():
					return
				case <-outbox.wake:
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
	outbox := a.processTerminalOutbox
	if outbox == nil || outbox.wake == nil {
		return
	}
	select {
	case outbox.wake <- struct{}{}:
	default:
	}
}

func (a *app) drainProcessTerminalOutbox() {
	outbox := a.processTerminalOutbox
	outbox.drainMu.Lock()
	defer outbox.drainMu.Unlock()
	runtime := a.processRuntime
	if runtime == nil {
		return
	}
	a.armProcessOutputMonitor()
	events, faults := runtime.PendingTerminalEvents()
	for _, fault := range faults {
		log.Printf("process terminal outbox skipped invalid event: %v", fault)
	}
	for _, event := range events {
		if err := a.waitForProcessOutputHandoff(); err != nil {
			log.Printf(
				"process terminal outbox output handoff %s failed: %v",
				event.ID,
				err,
			)
			continue
		}
		if err := a.flushProcessOutputGap(
			event.ProjectID,
			event.ProcessID,
		); err != nil {
			log.Printf(
				"process terminal outbox gap flush %s failed: %v",
				event.ID,
				err,
			)
			continue
		}
		runtimeEvent := event.runtimeEvent()
		_, err := a.admitProcessTerminalMessage(runtimeEvent)
		if err != nil {
			log.Printf(
				"process terminal outbox delivery %s failed: %v",
				event.ID,
				err,
			)
			continue
		}
		// A durable message can predate a crash between its save and the source
		// acknowledgement. Re-evaluate the stable terminal event on retry; the
		// monitor fire ID is derived from this event ID.
		a.emitRuntimeStateChanged(runtimeEvent)
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
