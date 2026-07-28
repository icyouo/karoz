package monitor

import (
	"encoding/json"
	"testing"
)

func TestTriggerMatching(t *testing.T) {
	runtimeMonitor := Monitor{
		ID: "monitor-1", ProjectID: "project-1",
		Trigger: Trigger{
			Kind: TriggerRuntimeEvent, EventKinds: []string{"task_changed"},
			EntityID: "task-1", FromState: "running", ToState: "done",
		},
	}
	event := Event{
		ID: "task/task-1/1", ProjectID: "project-1",
		AuthorityID: "task-store", AuthorityGeneration: 1,
		Kind: "task_changed", EntityID: "task-1",
		Origin: Origin{Kind: "runtime"}, Payload: json.RawMessage(`{"from":"running","to":"done"}`),
	}
	if matched, _ := MatchEvent(runtimeMonitor, event); !matched {
		t.Fatal("runtime event did not match")
	}
	runtimeMonitor.Trigger.EventKinds = nil
	if matched, _ := MatchEvent(runtimeMonitor, event); matched {
		t.Fatal("empty event kinds acted as wildcard")
	}

	outputMonitor := Monitor{
		ID: "monitor-1", Trigger: Trigger{Kind: TriggerProcessOutput, ProcessID: "process-1", Pattern: `panic|fatal`},
	}
	if matched, _ := MatchProcessOutput(outputMonitor, ProcessOutput{
		ProcessID: "process-1", Line: "fatal: unavailable", Origin: Origin{Kind: "runtime"},
	}); !matched {
		t.Fatal("process output did not match")
	}
	outputMonitor.Trigger.Pattern = "["
	if matched, _ := MatchProcessOutput(outputMonitor, ProcessOutput{
		ProcessID: "process-1", Line: "fatal", Origin: Origin{Kind: "runtime"},
	}); matched {
		t.Fatal("invalid regex matched")
	}

	exitMonitor := Monitor{Trigger: Trigger{Kind: TriggerProcessExit, ProcessID: "process-1", FailureOnly: true}}
	if matched, _ := MatchProcessExit(exitMonitor, ProcessExit{
		ProcessID: "process-1", State: "succeeded", Origin: Origin{Kind: "runtime"},
	}); matched {
		t.Fatal("successful exit matched failure-only trigger")
	}
	if matched, _ := MatchProcessExit(exitMonitor, ProcessExit{
		ProcessID: "process-1", State: "failed", ExitCode: 2, Origin: Origin{Kind: "runtime"},
	}); !matched {
		t.Fatal("failed exit did not match")
	}
}

func TestMonitorOriginGuards(t *testing.T) {
	item := Monitor{
		ID: "monitor-1", ProjectID: "project-1",
		Trigger: Trigger{Kind: TriggerRuntimeEvent, EventKinds: []string{"task_changed"}},
	}
	event := Event{
		ID: "task/task-1/1", ProjectID: "project-1",
		AuthorityID: "task-store", AuthorityGeneration: 1,
		Kind: "task_changed", EntityID: "task-1",
		Origin: Origin{Kind: "monitor", MonitorID: "monitor-2", FireID: "fire-2"},
	}
	if matched, _ := MatchEvent(item, event); matched {
		t.Fatal("monitor-origin event matched without opt-in")
	}
	item.Trigger.IncludeMonitorEvents = true
	if matched, _ := MatchEvent(item, event); !matched {
		t.Fatal("opted-in monitor-origin event did not match")
	}
	event.Origin.MonitorID = item.ID
	if matched, _ := MatchEvent(item, event); matched {
		t.Fatal("monitor matched its own event")
	}
}
