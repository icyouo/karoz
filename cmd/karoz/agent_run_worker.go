package main

import (
	"context"
	"errors"
	"log"
)

// startAgentRunWorker deliberately roots execution in Background: observers may
// leave, but only explicit cancelAgentRun cancels this context.
func (a *app) startAgentRunWorker(project Project, agent Agent, run AgentRun, userText, turnType string, inputIdentity ...AgentTranscriptItem) {
	ctx, claimed := a.claimAndBindAgentRunWorkerContext(context.Background(), project.ID, agent.ID, run.ID)
	if !claimed {
		// Another worker (or a terminal transition) already owns this immutable
		// Run. Do not publish a synthetic terminal event into its ledger.
		return
	}
	ledger := a.createRunLedger(run.ID)
	ledger.publish("meta", map[string]any{"run_id": run.ID, "type": normalizeChatTurnType(turnType), "agent": a.agentWithRuntimeState(project, agent)})
	go func() {
		currentInput := AgentTranscriptItem{}
		if len(inputIdentity) > 0 {
			currentInput = inputIdentity[0]
		}
		message, err := a.runResidentAgentTurnWithCurrentInput(ctx, project, agent, userText, turnType, currentInput, a.agentRunLedgerCallbacks(project, agent, run.ID))
		if err != nil {
			if ctx.Err() != nil {
				a.finishAgentRunWithLedger(project, agent, run.ID, RunStateCancelled, err, "Agent run cancelled.")
			} else {
				a.appendAgentMessageForRun(project.ID, agent.ID, run.ID, "assistant", "status", "Agent runtime failed: "+err.Error())
				a.finishAgentRunWithLedger(project, agent, run.ID, RunStateFailed, err, err.Error())
			}
			return
		}
		if message == "" {
			message = emptyAgentOutputMessage(agent)
		}
		if hook := a.agentRuntimeLocked().agentRunAfterProviderHook; hook != nil {
			hook()
		}
		if ctx.Err() != nil || !a.commitAgentRunSuccessWithLedger(project, agent, run.ID, message) {
			a.finishAgentRunWithLedger(project, agent, run.ID, RunStateCancelled, context.Canceled, "Agent run cancelled.")
			return
		}
		if hook := a.agentRuntimeLocked().agentRunAfterSuccessHook; hook != nil {
			hook()
		}
	}()
}

// agentRunLedgerCallbacks is the only bridge from provider callbacks to live
// delivery. It records events; HTTP observers are deliberately not involved.
func (a *app) agentRunLedgerCallbacks(project Project, agent Agent, runID string) *AgentStreamCallbacks {
	ledger := a.runLedger(runID)
	if ledger == nil {
		return nil
	}
	return &AgentStreamCallbacks{
		OnDelta: func(delta string) {
			if delta != "" {
				ledger.publish("delta", map[string]any{"delta": delta, "content": delta})
			}
		},
		OnToolStart: func(call codexToolCall) {
			payload := map[string]any{"call_id": firstNonEmpty(call.CallID, call.ID), "tool": call.Name, "arguments": call.Arguments}
			if message, ok := a.latestMatchingAgentMessage(project.ID, agent.ID, "tool_call", call.Name, call.Arguments); ok {
				payload["message_id"] = message.ID
				payload["message_seq"] = message.Seq
			}
			ledger.publish("tool_start", payload)
		},
		OnToolResult: func(call codexToolCall, result string, success bool) {
			displayResult := compactToolResultForDisplay(call.Name, result)
			payload := map[string]any{"call_id": firstNonEmpty(call.CallID, call.ID), "tool": call.Name, "success": success, "result": displayResult}
			if message, ok := a.latestMatchingAgentMessage(project.ID, agent.ID, "tool_result", call.Name, result); ok {
				payload["message_id"] = message.ID
				payload["message_seq"] = message.Seq
			}
			ledger.publish("tool_result", payload)
			if success && (call.Name == "write_workspace_file" || call.Name == "show_preview") {
				if preview := a.previewFromToolResult(project.ID, agent.ID, call, result); preview != nil {
					ledger.publish("preview", preview)
				}
			}
		},
		OnBudgetExhausted: func(payload map[string]any) {
			log.Printf("resident budget exhausted project=%s agent=%s run=%s phase=%v elapsed_ms=%v limit_ms=%v limit_rounds=%v", project.ID, agent.ID, runID, payload["phase"], payload["elapsed_ms"], payload["limit_ms"], payload["limit_rounds"])
			ledger.publish("budget_exhausted", payload)
		},
		OnInterrupt: func(items []AgentInterrupt) {
			ledger.publish("interrupt", map[string]any{"interrupts": items})
		},
	}
}

func (a *app) finishAgentRunWithLedger(project Project, agent Agent, runID string, final RunState, runErr error, message string) {
	if _, finished := a.finishAgentRun(project.ID, agent.ID, runID, final, runErr); !finished {
		return
	}
	ledger := a.runLedger(runID)
	if ledger == nil {
		return
	}
	if errors.Is(runErr, context.Canceled) || final == RunStateCancelled {
		ledger.publish("cancelled", map[string]any{"message": firstNonEmpty(message, "Agent run cancelled.")})
		return
	}
	if runErr != nil || final == RunStateFailed {
		ledger.publish("error", map[string]any{"message": firstNonEmpty(message, runErr.Error())})
		return
	}
	payload := map[string]any{"message": message}
	if agent.ID != "" {
		payload["agent"] = a.agentWithRuntimeState(project, agent)
	}
	ledger.publish("done", payload)
}
