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

// residentStepOutput is the protocol-neutral view of one streamed model round.
// AssistantContent is only populated by the Claude wire.
type residentStepOutput struct {
	Text             string
	ToolCalls        []codexToolCall
	AssistantContent []map[string]any
	CodexOutputItems []codexResponseOutputItem
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

type residentToolExecutor func(context.Context, codexToolCall) (string, error)

// invokeResidentToolLoop runs the shared resident-agent tool loop: stream a
// model round, dispatch tool calls, fold interrupts into the conversation,
// and stop with a budgeted final answer when the tool phase is exhausted.
func invokeResidentToolLoop(ctx context.Context, wire residentStreamWire, tools []map[string]any, callbacks AgentStreamCallbacks, budget ResidentTurnBudget, executeTool residentToolExecutor) error {
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
	toolRounds := 0
	var exhausted map[string]any
toolLoop:
	for modelRound := 0; modelRound < budget.MaxModelRounds && toolRounds < budget.MaxToolRounds; modelRound++ {
		streamed, interrupts, err := wire.step(toolCtx, tools, callbacks)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil && turnCtx.Err() == nil && toolCtx.Err() != nil {
				exhausted = residentTimeBudgetExhaustion("tool", started, toolWindow)
				break toolLoop
			}
			return err
		}
		if len(interrupts) > 0 {
			wire.appendInterruptTurn(streamed, interrupts)
			continue
		}
		wire.appendAssistantTurn(streamed)
		if len(streamed.ToolCalls) == 0 {
			return nil
		}
		toolRounds++
		reachedLimit := false
		for _, call := range streamed.ToolCalls {
			wire.appendToolCall(call)
			result, success, toolBudgetExhausted := executeResidentToolCall(ctx, turnCtx, toolCtx, call, callbacks, budget, started, toolWindow, executeTool)
			wire.appendToolResult(call, result, success)
			if toolBudgetExhausted != nil {
				exhausted = toolBudgetExhausted
				reachedLimit = true
				break
			}
			if callbacks.PollInterrupts != nil {
				if pending := callbacks.PollInterrupts(); len(pending) > 0 {
					if callbacks.OnInterrupt != nil {
						callbacks.OnInterrupt(pending)
					}
					wire.appendInlineInterrupts(pending)
				}
			}
			if toolCtx.Err() != nil && ctx.Err() == nil && turnCtx.Err() == nil {
				exhausted = residentTimeBudgetExhaustion("tool", started, toolWindow)
				reachedLimit = true
				break
			}
		}
		wire.flushToolResults()
		if reachedLimit {
			break toolLoop
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := turnCtx.Err(); err != nil {
		return err
	}
	if exhausted == nil {
		if toolRounds >= budget.MaxToolRounds {
			exhausted = residentRoundBudgetExhaustion("tool_rounds", started, budget.MaxToolRounds)
		} else {
			exhausted = residentRoundBudgetExhaustion("model_rounds", started, budget.MaxModelRounds)
		}
	}
	reportResidentBudgetExhaustion(callbacks, exhausted)
	wire.appendLimitMessage(residentBudgetLimitMessage(exhausted))
	finalCtx, cancelFinal := context.WithTimeout(turnCtx, budget.FinalResponseReserve)
	defer cancelFinal()
	return wire.finalize(turnCtx, finalCtx, callbacks)
}

// executeResidentToolCall runs one tool call with the standard lifecycle:
// start callback, error wrapping, output capping, and result callback.
func executeResidentToolCall(callerCtx, turnCtx, toolCtx context.Context, call codexToolCall, callbacks AgentStreamCallbacks, budget ResidentTurnBudget, started time.Time, toolWindow time.Duration, executeTool residentToolExecutor) (string, bool, map[string]any) {
	if callbacks.OnToolStart != nil {
		callbacks.OnToolStart(call)
	}
	result, err := executeTool(toolCtx, call)
	success := err == nil && toolResultSuccess(result)
	var exhausted map[string]any
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && callerCtx.Err() == nil && turnCtx.Err() == nil && toolCtx.Err() != nil {
			exhausted = residentTimeBudgetExhaustion("tool", started, toolWindow)
			result = toolJSON(exhausted)
		} else {
			result = `{"error":"tool_failed","message":` + strconv.Quote(err.Error()) + `}`
		}
	}
	result = limitToolResultForBudget(result, budget.MaxToolOutputChars)
	if callbacks.OnToolResult != nil {
		callbacks.OnToolResult(call, result, success)
	}
	return result, success, exhausted
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

func residentTimeBudgetExhaustion(phase string, started time.Time, limit time.Duration) map[string]any {
	return map[string]any{
		"error":      "budget_exhausted",
		"phase":      phase,
		"elapsed_ms": time.Since(started).Milliseconds(),
		"limit_ms":   limit.Milliseconds(),
	}
}

func residentRoundBudgetExhaustion(phase string, started time.Time, limit int) map[string]any {
	return map[string]any{
		"error":        "budget_exhausted",
		"phase":        phase,
		"elapsed_ms":   time.Since(started).Milliseconds(),
		"limit_rounds": limit,
	}
}

func reportResidentBudgetExhaustion(callbacks AgentStreamCallbacks, payload map[string]any) {
	if callbacks.OnBudgetExhausted != nil && payload != nil {
		callbacks.OnBudgetExhausted(payload)
	}
}

func residentBudgetLimitMessage(payload map[string]any) string {
	if payload == nil {
		return "resident turn budget"
	}
	phase, _ := payload["phase"].(string)
	return "resident " + firstNonEmpty(phase, "turn") + " budget (" + toolJSON(payload) + ")"
}

// limitToolResultForModel remains a compatibility helper for direct callers
// and tests. Production paths use the selected turn budget below.
func limitToolResultForModel(result string) string {
	return limitToolResultForBudget(result, residentTurnBudgetFor("ask").MaxToolOutputChars)
}

func limitToolResultForBudget(result string, maxChars int) string {
	result = strings.TrimSpace(result)
	if maxChars < 1 {
		maxChars = defaultResidentTurnBudget("ask").MaxToolOutputChars
	}
	if len(result) <= maxChars {
		return result
	}
	notice := fmt.Sprintf("\n\n[karoz truncated tool result: original_chars=%d limit_chars=%d; use narrower tool arguments if more detail is needed.]", len(result), maxChars)
	keep := maxChars - len(notice)
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
