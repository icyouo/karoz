package monitor

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

type ProcessExit struct {
	ProcessID string
	State     string
	ExitCode  int
	Detail    string
	Origin    Origin
}

type ProcessOutput struct {
	ProcessID string
	Sequence  uint64
	Line      string
	Origin    Origin
}

func MatchEvent(item Monitor, event Event) (bool, string) {
	if item.Trigger.Kind != TriggerRuntimeEvent || len(item.Trigger.EventKinds) == 0 {
		return false, ""
	}
	if event.Validate() != nil || event.ProjectID != item.ProjectID || !matchOrigin(item, event.Origin) {
		return false, ""
	}
	if item.Trigger.EntityID != "" && item.Trigger.EntityID != event.EntityID {
		return false, ""
	}
	foundKind := false
	for _, kind := range item.Trigger.EventKinds {
		if kind == event.Kind {
			foundKind = true
			break
		}
	}
	if !foundKind {
		return false, ""
	}
	var state struct {
		From      string `json:"from"`
		To        string `json:"to"`
		FromState string `json:"from_state"`
		ToState   string `json:"to_state"`
	}
	if len(bytes.TrimSpace(event.Payload)) > 0 && json.Unmarshal(event.Payload, &state) != nil {
		return false, ""
	}
	from := state.FromState
	if from == "" {
		from = state.From
	}
	to := state.ToState
	if to == "" {
		to = state.To
	}
	if item.Trigger.FromState != "" && item.Trigger.FromState != from {
		return false, ""
	}
	if item.Trigger.ToState != "" && item.Trigger.ToState != to {
		return false, ""
	}
	return true, event.Kind
}

func MatchProcessExit(item Monitor, event ProcessExit) (bool, string) {
	if item.Trigger.Kind != TriggerProcessExit || !matchOrigin(item, event.Origin) {
		return false, ""
	}
	if item.Trigger.ProcessID != "" && item.Trigger.ProcessID != event.ProcessID {
		return false, ""
	}
	if item.Trigger.FailureOnly && (event.State == "succeeded" || event.State == "exited" || event.ExitCode == 0 && event.State == "") {
		return false, ""
	}
	return true, event.Detail
}

func MatchProcessOutput(item Monitor, event ProcessOutput) (bool, string) {
	if item.Trigger.Kind != TriggerProcessOutput || !matchOrigin(item, event.Origin) {
		return false, ""
	}
	if item.Trigger.ProcessID != "" && item.Trigger.ProcessID != event.ProcessID {
		return false, ""
	}
	pattern, err := regexp.Compile(item.Trigger.Pattern)
	if err != nil || item.Trigger.Pattern == "" || !pattern.MatchString(event.Line) {
		return false, ""
	}
	return true, event.Line
}

func matchOrigin(item Monitor, origin Origin) bool {
	if origin.Validate() != nil {
		return false
	}
	if origin.Kind != "monitor" {
		return true
	}
	if origin.MonitorID == item.ID {
		return false
	}
	return item.Trigger.IncludeMonitorEvents
}

func ValidateTrigger(trigger Trigger) error {
	if trigger.Revision <= 0 {
		return errInvalidTrigger
	}
	switch trigger.Kind {
	case TriggerRuntimeEvent:
		if len(trigger.EventKinds) == 0 || len(trigger.EventKinds) > MaxEventKinds {
			return errInvalidTrigger
		}
		if trigger.ProcessID != "" || trigger.FailureOnly || trigger.Pattern != "" || hasProbeFields(trigger) {
			return errInvalidTrigger
		}
		seen := make(map[string]bool)
		for _, kind := range trigger.EventKinds {
			kind = strings.TrimSpace(kind)
			if kind == "" || seen[kind] {
				return errInvalidTrigger
			}
			seen[kind] = true
		}
	case TriggerProcessExit:
		if strings.TrimSpace(trigger.ProcessID) == "" || trigger.Pattern != "" ||
			hasRuntimeFields(trigger) || hasProbeFields(trigger) {
			return errInvalidTrigger
		}
	case TriggerProcessOutput:
		if strings.TrimSpace(trigger.ProcessID) == "" || trigger.Pattern == "" ||
			trigger.FailureOnly || hasRuntimeFields(trigger) || hasProbeFields(trigger) {
			return errInvalidTrigger
		}
		if _, err := regexp.Compile(trigger.Pattern); err != nil {
			return errInvalidTrigger
		}
	case TriggerScriptProbe:
		if trigger.ProbeLanguage != "shell" && trigger.ProbeLanguage != "javascript" ||
			strings.TrimSpace(trigger.ProbePath) == "" ||
			!validSHA256(trigger.ProbeSHA256) ||
			trigger.IntervalMS < 10_000 ||
			trigger.TimeoutMS <= 0 || trigger.TimeoutMS > 30_000 ||
			strings.TrimSpace(trigger.ApprovalReceiptID) == "" ||
			hasRuntimeFields(trigger) || trigger.ProcessID != "" ||
			trigger.FailureOnly || trigger.Pattern != "" {
			return errInvalidTrigger
		}
	default:
		return errInvalidTrigger
	}
	return nil
}

func hasRuntimeFields(trigger Trigger) bool {
	return len(trigger.EventKinds) != 0 || trigger.EntityID != "" ||
		trigger.FromState != "" || trigger.ToState != "" || trigger.IncludeMonitorEvents
}

func hasProbeFields(trigger Trigger) bool {
	return trigger.ProbeLanguage != "" || trigger.ProbePath != "" ||
		trigger.ProbeSHA256 != "" || trigger.IntervalMS != 0 ||
		trigger.TimeoutMS != 0 || trigger.ApprovalReceiptID != ""
}

var errInvalidTrigger = &validationError{"invalid trigger"}

type validationError struct{ message string }

func (err *validationError) Error() string { return err.message }
