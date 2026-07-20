package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type claudeBridgeEvent struct {
	Kind    string
	Call    codexToolCall
	Result  string
	Success bool
}

func claudeCLIAuthenticated(ctx context.Context) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KAROZ_CLAUDE_CLI_AUTH"))) {
	case "1", "true", "available":
		return true
	case "0", "false", "disabled", "unavailable":
		return false
	}
	if _, err := exec.LookPath("claude"); err != nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, "claude", "auth", "status", "--json").Output()
	if err != nil {
		return false
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	return json.Unmarshal(output, &status) == nil && status.LoggedIn
}

func invokeClaudeCLIStream(ctx context.Context, workdir, prompt, model, effort string, tools []map[string]any, callbacks AgentStreamCallbacks, executeTool func(codexToolCall) (string, error)) error {
	currentPrompt := prompt
	for round := 0; round < 8; round++ {
		partial, interrupts, err := streamClaudeCLIOnce(ctx, workdir, currentPrompt, model, effort, tools, callbacks, executeTool)
		if err != nil {
			return err
		}
		if len(interrupts) == 0 {
			return nil
		}
		currentPrompt = prompt + "\n\nA previous response was interrupted after this partial output:\n" + limitString(partial, 12000) + "\n\n" + renderAgentInterruptsForModel(interrupts)
	}
	return errors.New("Claude interrupt restart limit reached")
}

func streamClaudeCLIOnce(ctx context.Context, workdir, prompt, model, effort string, tools []map[string]any, callbacks AgentStreamCallbacks, executeTool func(codexToolCall) (string, error)) (string, []AgentInterrupt, error) {
	bridge, err := startClaudeToolBridge(tools, executeTool)
	if err != nil {
		return "", nil, err
	}
	defer bridge.Close()
	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	model = firstNonEmpty(strings.TrimSpace(model), "sonnet")
	effort = firstNonEmpty(strings.ToLower(strings.TrimSpace(effort)), "medium")
	args := []string{"-p", prompt, "--verbose", "--output-format", "stream-json", "--include-partial-messages", "--model", model, "--effort", effort, "--tools", "", "--strict-mcp-config", "--mcp-config", bridge.Config, "--disable-slash-commands", "--no-session-persistence", "--permission-mode", "dontAsk"}
	if len(bridge.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(bridge.AllowedTools, ","))
	}
	command := exec.CommandContext(stepCtx, "claude", args...)
	command.Dir = workdir
	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return "", nil, err
	}
	lines := make(chan string, 32)
	scanErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 8<<20)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
		scanErr <- scanner.Err()
	}()
	waitErr := make(chan error, 1)
	go func() { waitErr <- command.Wait() }()
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	var output strings.Builder
	var interrupts []AgentInterrupt
	var resultError string
	lineChannel := lines
	for lineChannel != nil {
		select {
		case line, ok := <-lineChannel:
			if !ok {
				lineChannel = nil
				continue
			}
			delta, cliErr := claudeCLILineDelta(line)
			if cliErr != "" {
				resultError = cliErr
			}
			if delta != "" {
				output.WriteString(delta)
				if callbacks.OnDelta != nil {
					callbacks.OnDelta(delta)
				}
			}
		case event := <-bridge.Events:
			if event.Kind == "start" && callbacks.OnToolStart != nil {
				callbacks.OnToolStart(event.Call)
			}
			if event.Kind == "result" && callbacks.OnToolResult != nil {
				callbacks.OnToolResult(event.Call, event.Result, event.Success)
			}
		case <-ticker.C:
			if callbacks.PollInterrupts != nil {
				interrupts = callbacks.PollInterrupts()
			}
			if len(interrupts) > 0 {
				if callbacks.OnInterrupt != nil {
					callbacks.OnInterrupt(interrupts)
				}
				cancel()
			}
		case <-ctx.Done():
			cancel()
		}
	}
	if err := <-scanErr; err != nil && len(interrupts) == 0 && ctx.Err() == nil {
		return output.String(), nil, err
	}
	commandErr := <-waitErr
	for {
		select {
		case event := <-bridge.Events:
			if event.Kind == "start" && callbacks.OnToolStart != nil {
				callbacks.OnToolStart(event.Call)
			}
			if event.Kind == "result" && callbacks.OnToolResult != nil {
				callbacks.OnToolResult(event.Call, event.Result, event.Success)
			}
		default:
			goto drained
		}
	}
drained:
	if len(interrupts) > 0 {
		return output.String(), interrupts, nil
	}
	if ctx.Err() != nil {
		return output.String(), nil, ctx.Err()
	}
	if resultError != "" {
		return output.String(), nil, errors.New(resultError)
	}
	if commandErr != nil {
		return output.String(), nil, fmt.Errorf("claude CLI failed: %w: %s", commandErr, strings.TrimSpace(stderr.String()))
	}
	return output.String(), nil, nil
}

func claudeCLILineDelta(line string) (string, string) {
	var envelope struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
		Event   struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		} `json:"event"`
	}
	if json.Unmarshal([]byte(line), &envelope) != nil {
		return "", ""
	}
	if envelope.Type == "stream_event" && envelope.Event.Type == "content_block_delta" && envelope.Event.Delta.Type == "text_delta" {
		return envelope.Event.Delta.Text, ""
	}
	if envelope.Type == "result" && envelope.IsError {
		return "", firstNonEmpty(envelope.Result, "Claude CLI returned an error")
	}
	return "", ""
}

type claudeToolBridge struct {
	Config       string
	AllowedTools []string
	Events       chan claudeBridgeEvent
	listener     net.Listener
	tempDir      string
}

func startClaudeToolBridge(tools []map[string]any, executeTool func(codexToolCall) (string, error)) (*claudeToolBridge, error) {
	tempDir, err := os.MkdirTemp("", "karoz-claude-bridge-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	specs := claudeToolSpecs(tools)
	raw, err := json.Marshal(specs)
	if err != nil {
		cleanup()
		return nil, err
	}
	specsPath := filepath.Join(tempDir, "tools.json")
	if err := os.WriteFile(specsPath, raw, 0600); err != nil {
		cleanup()
		return nil, err
	}
	socketPath := filepath.Join(tempDir, "bridge.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		cleanup()
		return nil, err
	}
	token := randomID() + randomID()
	executable, err := os.Executable()
	if err != nil {
		listener.Close()
		cleanup()
		return nil, err
	}
	config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"karoz": map[string]any{"type": "stdio", "command": executable, "args": []string{"mcp-bridge", "--socket", socketPath, "--token", token, "--specs", specsPath}}}})
	bridge := &claudeToolBridge{Config: string(config), Events: make(chan claudeBridgeEvent, 128), listener: listener, tempDir: tempDir}
	for _, spec := range specs {
		if name, _ := spec["name"].(string); name != "" {
			bridge.AllowedTools = append(bridge.AllowedTools, "mcp__karoz__"+name)
		}
	}
	go bridge.serve(token, executeTool)
	return bridge, nil
}

func (bridge *claudeToolBridge) serve(token string, executeTool func(codexToolCall) (string, error)) {
	for {
		connection, err := bridge.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer connection.Close()
			var request claudeBridgeRequest
			if json.NewDecoder(connection).Decode(&request) != nil || request.Token != token {
				_ = json.NewEncoder(connection).Encode(claudeBridgeResponse{Error: "unauthorized bridge request"})
				return
			}
			call := codexToolCall{ID: randomID(), CallID: randomID(), Name: request.Name, Arguments: request.Arguments}
			bridge.Events <- claudeBridgeEvent{Kind: "start", Call: call}
			result, callErr := executeTool(call)
			success := callErr == nil && toolResultSuccess(result)
			if callErr != nil {
				result = `{"error":"tool_failed","message":` + fmt.Sprintf("%q", callErr.Error()) + `}`
			}
			result = limitToolResultForModel(result)
			bridge.Events <- claudeBridgeEvent{Kind: "result", Call: call, Result: result, Success: success}
			response := claudeBridgeResponse{Result: result}
			if !success {
				response.Error = firstNonEmpty(errorString(callErr), "tool returned an error")
			}
			_ = json.NewEncoder(connection).Encode(response)
		}()
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (bridge *claudeToolBridge) Close() {
	if bridge == nil {
		return
	}
	if bridge.listener != nil {
		_ = bridge.listener.Close()
	}
	if bridge.tempDir != "" {
		_ = os.RemoveAll(bridge.tempDir)
	}
}
