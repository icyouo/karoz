package monitor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

type ProbeResult struct {
	Matched bool   `json:"matched"`
	Detail  string `json:"detail,omitempty"`
}

type ProbeExecution struct {
	Stdout   []byte
	ExitCode int
	TimedOut bool
	Signaled bool
}

func ParseProbeResult(execution ProbeExecution) (ProbeResult, error) {
	switch {
	case execution.TimedOut:
		return ProbeResult{}, errors.New("probe timed out")
	case execution.Signaled:
		return ProbeResult{}, errors.New("probe terminated by signal")
	case execution.ExitCode != 0:
		return ProbeResult{}, fmt.Errorf("probe exited with status %d", execution.ExitCode)
	}
	decoder := json.NewDecoder(bytes.NewReader(execution.Stdout))
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return ProbeResult{}, errors.New("probe stdout must be one JSON object")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return ProbeResult{}, errors.New("probe stdout must contain exactly one JSON object")
	}
	for key := range raw {
		if key != "matched" && key != "detail" {
			return ProbeResult{}, fmt.Errorf(
				"probe result contains unknown field %q",
				key,
			)
		}
	}
	matchedRaw, ok := raw["matched"]
	if !ok {
		return ProbeResult{}, errors.New("probe result requires matched")
	}
	var matchedValue any
	if err := json.Unmarshal(matchedRaw, &matchedValue); err != nil {
		return ProbeResult{}, errors.New("probe result matched must be boolean")
	}
	matched, ok := matchedValue.(bool)
	if !ok {
		return ProbeResult{}, errors.New("probe result matched must be boolean")
	}
	var detail string
	if detailRaw, ok := raw["detail"]; ok {
		var detailValue any
		if err := json.Unmarshal(detailRaw, &detailValue); err != nil {
			return ProbeResult{}, errors.New("probe result detail must be text")
		}
		var valid bool
		detail, valid = detailValue.(string)
		if !valid {
			return ProbeResult{}, errors.New("probe result detail must be text")
		}
	}
	if len(detail) > MaxProbeDetail {
		return ProbeResult{}, errors.New("probe result detail exceeds 1 KiB")
	}
	return ProbeResult{Matched: matched, Detail: detail}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return errors.New("extra JSON value")
	}
	return err
}

// MatchProbe applies observable check/error state even when the probe does not
// match. Three consecutive execution/result errors move the monitor to error.
func MatchProbe(item Monitor, result ProbeResult, resultErr error, now time.Time) (Monitor, bool, string) {
	item = cloneMonitor(item)
	item.LastCheckedAt = timePointer(now)
	interval := item.Trigger.IntervalMS
	if interval < 10_000 {
		interval = 10_000
	}
	next := now.Add(time.Duration(interval) * time.Millisecond)
	item.NextCheckAt = &next
	item.UpdatedAt = now
	if resultErr != nil {
		item.ConsecutiveProbeErrors++
		item.LastError = boundedPlainText(resultErr.Error(), MaxProbeDetail)
		if item.ConsecutiveProbeErrors >= 3 {
			item.State = StateError
			item.ErrorCode = "probe_error"
		}
		return item, false, ""
	}
	item.ConsecutiveProbeErrors = 0
	item.LastError = ""
	if !result.Matched {
		return item, false, result.Detail
	}
	item.LastMatch = result.Detail
	return item, true, result.Detail
}

func boundedPlainText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
