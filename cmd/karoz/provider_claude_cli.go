package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	executiondomain "github.com/karoz/karoz/internal/execution"
)

type claudeBridgeEvent struct {
	Kind    string
	Call    codexToolCall
	Result  string
	Success bool
}

func claudeCLIAuthenticated(ctx context.Context) bool {
	return claudeCLIAuthenticatedWithRunner(ctx, executiondomain.NewHostRunner())
}

func claudeCLIAuthenticatedWithRunner(ctx context.Context, runner executiondomain.Runner) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KAROZ_CLAUDE_CLI_AUTH"))) {
	case "1", "true", "available":
		return true
	case "0", "false", "disabled", "unavailable":
		return false
	}
	if runner == nil {
		runner = executiondomain.NewHostRunner()
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := runner.Run(probeCtx, commandRequest("", "claude", "auth", "status", "--json"))
	if err != nil {
		return false
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	return json.Unmarshal([]byte(result.Output()), &status) == nil && status.LoggedIn
}

func invokeClaudeCLIStreamWithBudgetAndRunner(ctx context.Context, workdir, prompt, model, effort string, tools []map[string]any, callbacks AgentStreamCallbacks, budget ResidentTurnBudget, runner executiondomain.StreamRunner, executeTool residentToolExecutor) error {
	if runner == nil {
		runner = executiondomain.NewHostStreamRunner()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	turnCtx, cancelTurn := context.WithTimeout(ctx, budget.TotalDuration)
	defer cancelTurn()
	toolWindow := budget.ToolPhaseDuration
	if maximum := budget.TotalDuration - budget.FinalResponseReserve; toolWindow > maximum {
		toolWindow = maximum
	}
	toolCtx, cancelTools := context.WithTimeout(turnCtx, toolWindow)
	defer cancelTools()
	currentPrompt := prompt
	for round := 0; round < budget.MaxModelRounds; round++ {
		partial, interrupts, err := streamClaudeCLIOnceWithRunner(turnCtx, toolCtx, started, budget, workdir, currentPrompt, model, effort, tools, callbacks, runner, executeTool)
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

func invokeClaudeCLINoToolsOnceWithRunner(ctx context.Context, workdir, prompt, model, effort string, callbacks AgentStreamCallbacks, budget ResidentTurnBudget, runner executiondomain.StreamRunner) error {
	if runner == nil {
		runner = executiondomain.NewHostStreamRunner()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, interrupts, err := streamClaudeCLIOnceWithRunner(ctx, ctx, time.Now(), budget, workdir, prompt, model, effort, nil, callbacks, runner, func(context.Context, codexToolCall) (string, error) {
		return "", errors.New("tools are disabled for this request")
	})
	if err != nil {
		return err
	}
	if len(interrupts) > 0 {
		return errors.New("interrupts are disabled for no-tools model requests")
	}
	return nil
}

func streamClaudeCLIOnceWithRunner(ctx, toolCtx context.Context, started time.Time, budget ResidentTurnBudget, workdir, prompt, model, effort string, tools []map[string]any, callbacks AgentStreamCallbacks, runner executiondomain.StreamRunner, executeTool residentToolExecutor) (string, []AgentInterrupt, error) {
	if runner == nil {
		runner = executiondomain.NewHostStreamRunner()
	}
	bridge, err := startClaudeToolBridge(tools, callbacks, budget, started, ctx, toolCtx, executeTool)
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
	stream, err := runner.Start(stepCtx, executiondomain.StreamCommandRequest{
		Name: "claude", Args: args, Dir: workdir, Configure: configureResidentCommand,
	})
	if err != nil {
		return "", nil, err
	}
	lines := make(chan string, 32)
	scanErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stream.Stdout)
		scanner.Buffer(make([]byte, 64*1024), 8<<20)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
		scanErr <- scanner.Err()
	}()
	stderrDone := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(stream.Stderr)
		_ = stream.Stderr.Close()
		stderrDone <- string(data)
	}()
	waitErr := make(chan error, 1)
	go func() { waitErr <- stream.Cmd.Wait() }()
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
	stderrText := <-stderrDone
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
		return output.String(), nil, fmt.Errorf("claude CLI failed: %w: %s", commandErr, strings.TrimSpace(stderrText))
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
	Config         string
	AllowedTools   []string
	Events         chan claudeBridgeEvent
	listener       net.Listener
	tempDir        string
	callbacks      AgentStreamCallbacks
	budget         ResidentTurnBudget
	started        time.Time
	turnCtx        context.Context
	toolCtx        context.Context
	mu             sync.Mutex
	toolCalls      int
	reportedBudget bool
}

func startClaudeToolBridge(tools []map[string]any, callbacks AgentStreamCallbacks, budget ResidentTurnBudget, started time.Time, turnCtx, toolCtx context.Context, executeTool residentToolExecutor) (*claudeToolBridge, error) {
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
	bridge := &claudeToolBridge{
		Config: string(config), Events: make(chan claudeBridgeEvent, 128), listener: listener, tempDir: tempDir,
		callbacks: callbacks, budget: budget, started: started, turnCtx: turnCtx, toolCtx: toolCtx,
	}
	for _, spec := range specs {
		if name, _ := spec["name"].(string); name != "" {
			bridge.AllowedTools = append(bridge.AllowedTools, "mcp__karoz__"+name)
		}
	}
	go bridge.serve(token, executeTool)
	return bridge, nil
}

func (bridge *claudeToolBridge) serve(token string, executeTool residentToolExecutor) {
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
			result, success := bridge.executeToolCall(call, executeTool)
			bridge.Events <- claudeBridgeEvent{Kind: "result", Call: call, Result: result, Success: success}
			response := claudeBridgeResponse{Result: result}
			if !success {
				response.Error = "tool returned an error"
			}
			_ = json.NewEncoder(connection).Encode(response)
		}()
	}
}

func (bridge *claudeToolBridge) executeToolCall(call codexToolCall, executeTool residentToolExecutor) (string, bool) {
	bridge.mu.Lock()
	if bridge.toolCalls >= bridge.budget.MaxToolRounds {
		payload := residentRoundBudgetExhaustion("tool_rounds", bridge.started, bridge.budget.MaxToolRounds)
		bridge.reportBudgetLocked(payload)
		bridge.mu.Unlock()
		return toolJSON(payload), false
	}
	bridge.toolCalls++
	bridge.mu.Unlock()

	result, callErr := executeTool(bridge.toolCtx, call)
	success := callErr == nil && toolResultSuccess(result)
	if callErr != nil {
		if errors.Is(callErr, context.DeadlineExceeded) && bridge.turnCtx.Err() == nil && bridge.toolCtx.Err() != nil {
			payload := residentTimeBudgetExhaustion("tool", bridge.started, bridge.toolBudgetWindow())
			bridge.mu.Lock()
			bridge.reportBudgetLocked(payload)
			bridge.mu.Unlock()
			result = toolJSON(payload)
		} else {
			result = `{"error":"tool_failed","message":` + fmt.Sprintf("%q", callErr.Error()) + `}`
		}
	}
	return limitToolResultForBudget(result, bridge.budget.MaxToolOutputChars), success
}

func (bridge *claudeToolBridge) toolBudgetWindow() time.Duration {
	window := bridge.budget.ToolPhaseDuration
	if maximum := bridge.budget.TotalDuration - bridge.budget.FinalResponseReserve; window > maximum {
		return maximum
	}
	return window
}

func (bridge *claudeToolBridge) reportBudgetLocked(payload map[string]any) {
	if bridge.reportedBudget {
		return
	}
	bridge.reportedBudget = true
	reportResidentBudgetExhaustion(bridge.callbacks, payload)
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
