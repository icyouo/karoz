package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxCodexToolOutputChars  = 12000
	maxResidentToolRounds    = 8
	residentToolPhaseTimeout = 90 * time.Second
	residentFinalTimeout     = 30 * time.Second
)

// residentStepOutput is the protocol-neutral view of one streamed model round.
// AssistantContent is only populated by the Claude wire.
type residentStepOutput struct {
	Text             string
	ToolCalls        []codexToolCall
	AssistantContent []map[string]any
}

// residentStreamWire adapts one resident provider protocol (Codex responses
// SSE, Claude messages SSE) to the shared resident tool loop. Implementations
// own the conversation history and all protocol-specific payload shapes.
type residentStreamWire interface {
	step(ctx context.Context, tools []map[string]any, callbacks AgentStreamCallbacks) (residentStepOutput, []AgentInterrupt, error)
	appendAssistantTurn(streamed residentStepOutput)
	appendInterruptTurn(streamed residentStepOutput, interrupts []AgentInterrupt)
	appendToolCall(call codexToolCall)
	appendToolResult(call codexToolCall, result string, success bool)
	appendInlineInterrupts(interrupts []AgentInterrupt)
	flushToolResults()
	appendLimitMessage(limitReason string)
	finalize(parentCtx, finalCtx context.Context, callbacks AgentStreamCallbacks) error
}

// invokeResidentToolLoop runs the shared resident-agent tool loop: stream a
// model round, dispatch tool calls, fold interrupts into the conversation,
// and stop with a budgeted final answer when the tool phase is exhausted.
func invokeResidentToolLoop(ctx context.Context, wire residentStreamWire, tools []map[string]any, callbacks AgentStreamCallbacks, executeTool func(codexToolCall) (string, error)) error {
	parentCtx := ctx
	toolCtx, cancelTools := context.WithTimeout(parentCtx, residentToolPhaseTimeout)
	defer cancelTools()
	toolRounds := 0
	limitReason := "resident tool loop limit"
toolLoop:
	for modelRound := 0; modelRound < 16 && toolRounds < maxResidentToolRounds; modelRound++ {
		streamed, interrupts, err := wire.step(toolCtx, tools, callbacks)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && parentCtx.Err() == nil {
				limitReason = "resident tool phase time budget"
				break toolLoop
			}
			return err
		}
		wire.appendAssistantTurn(streamed)
		if len(interrupts) > 0 {
			wire.appendInterruptTurn(streamed, interrupts)
			continue
		}
		if len(streamed.ToolCalls) == 0 {
			return nil
		}
		toolRounds++
		reachedLimit := false
		for _, call := range streamed.ToolCalls {
			wire.appendToolCall(call)
			result, success := executeResidentToolCall(call, callbacks, executeTool)
			wire.appendToolResult(call, result, success)
			if callbacks.PollInterrupts != nil {
				if pending := callbacks.PollInterrupts(); len(pending) > 0 {
					if callbacks.OnInterrupt != nil {
						callbacks.OnInterrupt(pending)
					}
					wire.appendInlineInterrupts(pending)
				}
			}
			if toolCtx.Err() != nil && parentCtx.Err() == nil {
				limitReason = "resident tool phase time budget"
				reachedLimit = true
				break
			}
		}
		wire.flushToolResults()
		if reachedLimit {
			break toolLoop
		}
	}
	if err := parentCtx.Err(); err != nil {
		return err
	}
	wire.appendLimitMessage(limitReason)
	finalCtx, cancelFinal := context.WithTimeout(parentCtx, residentFinalTimeout)
	defer cancelFinal()
	return wire.finalize(parentCtx, finalCtx, callbacks)
}

// executeResidentToolCall runs one tool call with the standard lifecycle:
// start callback, error wrapping, output capping, and result callback.
func executeResidentToolCall(call codexToolCall, callbacks AgentStreamCallbacks, executeTool func(codexToolCall) (string, error)) (string, bool) {
	if callbacks.OnToolStart != nil {
		callbacks.OnToolStart(call)
	}
	result, err := executeTool(call)
	success := err == nil && toolResultSuccess(result)
	if err != nil {
		result = `{"error":"tool_failed","message":` + strconv.Quote(err.Error()) + `}`
	}
	result = limitToolResultForModel(result)
	if callbacks.OnToolResult != nil {
		callbacks.OnToolResult(call, result, success)
	}
	return result, success
}

// runResidentStep streams one model round while racing an interrupt poller:
// pending interrupts cancel the in-flight request and replace its
// cancellation error with the drained interrupt list.
func runResidentStep[R any](ctx context.Context, callbacks AgentStreamCallbacks, buildRequest func(context.Context) (*http.Request, error), streamResponse func(*http.Request, func(string)) (R, error)) (R, []AgentInterrupt, error) {
	var zero R
	if callbacks.PollInterrupts != nil {
		if interrupts := callbacks.PollInterrupts(); len(interrupts) > 0 {
			if callbacks.OnInterrupt != nil {
				callbacks.OnInterrupt(interrupts)
			}
			return zero, interrupts, nil
		}
	}
	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpReq, err := buildRequest(stepCtx)
	if err != nil {
		return zero, nil, err
	}
	interruptCh := make(chan []AgentInterrupt, 1)
	stopPolling := make(chan struct{})
	var pollers sync.WaitGroup
	if callbacks.PollInterrupts != nil {
		pollers.Add(1)
		go func() {
			defer pollers.Done()
			ticker := time.NewTicker(40 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stopPolling:
					return
				case <-stepCtx.Done():
					return
				case <-ticker.C:
					interrupts := callbacks.PollInterrupts()
					if len(interrupts) == 0 {
						continue
					}
					interruptCh <- interrupts
					cancel()
					return
				}
			}
		}()
	}
	streamed, streamErr := streamResponse(httpReq, callbacks.OnDelta)
	close(stopPolling)
	pollers.Wait()
	var interrupts []AgentInterrupt
	select {
	case interrupts = <-interruptCh:
	default:
	}
	if len(interrupts) == 0 && callbacks.PollInterrupts != nil {
		interrupts = callbacks.PollInterrupts()
	}
	if len(interrupts) > 0 {
		if callbacks.OnInterrupt != nil {
			callbacks.OnInterrupt(interrupts)
		}
		if errors.Is(streamErr, context.Canceled) {
			streamErr = nil
		}
	}
	return streamed, interrupts, streamErr
}

func limitToolResultForModel(result string) string {
	result = strings.TrimSpace(result)
	if len(result) <= maxCodexToolOutputChars {
		return result
	}
	notice := fmt.Sprintf("\n\n[karoz truncated tool result: original_chars=%d limit_chars=%d; use narrower tool arguments if more detail is needed.]", len(result), maxCodexToolOutputChars)
	keep := maxCodexToolOutputChars - len(notice)
	if keep < 0 {
		keep = 0
	}
	return strings.TrimSpace(result[:keep]) + notice
}

func renderAgentInterruptsForModel(interrupts []AgentInterrupt) string {
	var b strings.Builder
	b.WriteString("User sent the following additional message")
	if len(interrupts) != 1 {
		b.WriteString("s")
	}
	b.WriteString(" while you were working. Treat them as the latest user input and adjust your next steps accordingly:\n")
	for _, item := range interrupts {
		b.WriteString("- ")
		if item.TurnType != "" {
			b.WriteString("[")
			b.WriteString(item.TurnType)
			b.WriteString("] ")
		}
		b.WriteString(limitString(item.Body, 4000))
		b.WriteString("\n")
	}
	return b.String()
}
