package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"
)

func runResidentBashTool(workdir, rawArgs string) BashToolResult {
	start := time.Now()
	var args struct {
		Command   string `json:"command"`
		TimeoutMS int    `json:"timeout_ms"`
		MaxOutput int    `json:"max_output"`
	}
	if strings.TrimSpace(rawArgs) == "" {
		rawArgs = "{}"
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return BashToolResult{OK: false, Workspace: workdir, Error: "parse bash arguments: " + err.Error(), DurationMS: time.Since(start).Milliseconds()}
	}
	command := strings.TrimSpace(args.Command)
	if command == "" {
		return BashToolResult{OK: false, Workspace: workdir, Error: "command is required", DurationMS: time.Since(start).Milliseconds()}
	}
	timeout := time.Duration(args.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if timeout > 5*time.Minute {
		timeout = 5 * time.Minute
	}
	maxOutput := args.MaxOutput
	if maxOutput <= 0 {
		maxOutput = 20000
	}
	if maxOutput > 200000 {
		maxOutput = 200000
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = workdir
	out, err := cmd.CombinedOutput()
	text, truncated := truncateString(string(out), maxOutput)
	result := BashToolResult{
		OK:         err == nil,
		Workspace:  workdir,
		Command:    command,
		Code:       0,
		DurationMS: time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
	if cmd.ProcessState != nil {
		result.Code = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() == context.DeadlineExceeded {
		result.OK = false
		result.Error = "command timed out"
		result.Code = -1
	}
	if err != nil && result.Error == "" {
		result.Error = err.Error()
	}
	if result.OK {
		result.Stdout = text
	} else {
		result.Stderr = text
	}
	return result
}

func decodeRawJSONText(raw json.RawMessage) string {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return ""
	}
	if strings.HasPrefix(text, `"`) {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err == nil {
			return strings.TrimSpace(decoded)
		}
	}
	return text
}
