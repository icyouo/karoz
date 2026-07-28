package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestResidentTurnBudgetSelectsProfilesAndAppliesOverrides(t *testing.T) {
	ask := residentTurnBudgetFor("ask")
	plan := residentTurnBudgetFor("plan")
	dev := residentTurnBudgetFor("dev")
	if ask.TotalDuration != 2*time.Minute || ask.ToolPhaseDuration != 90*time.Second || ask.FinalResponseReserve != 30*time.Second || ask.MaxModelRounds != 8 || ask.MaxToolRounds != 8 || ask.MaxToolOutputChars != 12000 {
		t.Fatalf("ask compatibility budget = %+v", ask)
	}
	if plan.TotalDuration <= ask.TotalDuration || dev.TotalDuration <= plan.TotalDuration {
		t.Fatalf("expected larger plan/dev budgets: ask=%+v plan=%+v dev=%+v", ask, plan, dev)
	}

	t.Setenv("KAROZ_RESIDENT_DEV_TOTAL_TIMEOUT", "90s")
	t.Setenv("KAROZ_RESIDENT_DEV_TOOL_TIMEOUT", "80s")
	t.Setenv("KAROZ_RESIDENT_DEV_FINAL_RESERVE", "30s")
	t.Setenv("KAROZ_RESIDENT_DEV_MAX_TOOL_ROUNDS", "3")
	configured := residentTurnBudgetFor("dev")
	if configured.TotalDuration != 90*time.Second || configured.ToolPhaseDuration != 60*time.Second || configured.FinalResponseReserve != 30*time.Second || configured.MaxToolRounds != 3 {
		t.Fatalf("configured dev budget = %+v", configured)
	}
}

func TestBashTimeoutClampsToRemainingToolContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer cancel()
	clamped := clampResidentBashTimeout(ctx, 300000)
	if clamped < 1 || clamped > 70 {
		t.Fatalf("clamped bash timeout = %dms, want 1..70ms", clamped)
	}
	result := runResidentBashTool(ctx, t.TempDir(), "sleep 1", clamped, 1024)
	if result.OK || result.Error != "command timed out" || result.DurationMS > 500 {
		t.Fatalf("bash did not obey remaining context: %+v", result)
	}
}

func TestToolBudgetExhaustionLeavesFinalResponseReserve(t *testing.T) {
	budget := ResidentTurnBudget{
		TotalDuration:        180 * time.Millisecond,
		ToolPhaseDuration:    45 * time.Millisecond,
		FinalResponseReserve: 100 * time.Millisecond,
		MaxModelRounds:       2,
		MaxToolRounds:        1,
		MaxToolOutputChars:   1024,
	}
	wire := &budgetTestWire{finalRemaining: make(chan time.Duration, 1)}
	var exhausted map[string]any
	err := invokeResidentToolLoop(context.Background(), wire, nil, AgentStreamCallbacks{
		OnBudgetExhausted: func(payload map[string]any) { exhausted = payload },
	}, budget, func(ctx context.Context, _ codexToolCall) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if exhausted["error"] != "budget_exhausted" || exhausted["phase"] != "tool" {
		t.Fatalf("budget exhaustion payload = %#v", exhausted)
	}
	select {
	case remaining := <-wire.finalRemaining:
		if remaining < 50*time.Millisecond {
			t.Fatalf("final reserve was not preserved: %s remaining", remaining)
		}
	case <-time.After(time.Second):
		t.Fatal("final response was not invoked")
	}
	if !strings.Contains(wire.limitMessage, "budget_exhausted") {
		t.Fatalf("final prompt did not receive typed budget result: %q", wire.limitMessage)
	}
}

type budgetTestWire struct {
	steps          int
	limitMessage   string
	finalRemaining chan time.Duration
}

func (wire *budgetTestWire) step(context.Context, []map[string]any, AgentStreamCallbacks) (residentStepOutput, []AgentInterrupt, error) {
	wire.steps++
	return residentStepOutput{ToolCalls: []codexToolCall{{ID: "tool-1", CallID: "tool-1", Name: "bash"}}}, nil, nil
}

func (*budgetTestWire) appendAssistantTurn(residentStepOutput)                   {}
func (*budgetTestWire) appendInterruptTurn(residentStepOutput, []AgentInterrupt) {}
func (*budgetTestWire) appendToolCall(codexToolCall)                             {}
func (*budgetTestWire) appendToolResult(codexToolCall, string, bool)             {}
func (*budgetTestWire) appendInlineInterrupts([]AgentInterrupt)                  {}
func (*budgetTestWire) flushToolResults()                                        {}
func (wire *budgetTestWire) appendLimitMessage(message string)                   { wire.limitMessage = message }
func (wire *budgetTestWire) finalize(_ context.Context, finalCtx context.Context, _ AgentStreamCallbacks) error {
	deadline, ok := finalCtx.Deadline()
	if !ok {
		return context.DeadlineExceeded
	}
	wire.finalRemaining <- time.Until(deadline)
	return nil
}
