package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// ResidentTurnBudget is the bounded execution contract for one resident-agent
// turn. It is intentionally selected before provider dispatch so Codex and
// Claude receive the same limits for the same turn type.
type ResidentTurnBudget struct {
	TotalDuration        time.Duration
	ToolPhaseDuration    time.Duration
	FinalResponseReserve time.Duration
	MaxModelRounds       int
	MaxToolRounds        int
	MaxToolOutputChars   int
}

func residentTurnBudgetFor(turnType string) ResidentTurnBudget {
	turnType = normalizeChatTurnType(turnType)
	budget := defaultResidentTurnBudget(turnType)
	prefix := "KAROZ_RESIDENT_" + strings.ToUpper(turnType) + "_"
	budget.TotalDuration = residentBudgetDuration(prefix+"TOTAL_TIMEOUT", budget.TotalDuration)
	budget.ToolPhaseDuration = residentBudgetDuration(prefix+"TOOL_TIMEOUT", budget.ToolPhaseDuration)
	budget.FinalResponseReserve = residentBudgetDuration(prefix+"FINAL_RESERVE", budget.FinalResponseReserve)
	budget.MaxModelRounds = residentBudgetInt(prefix+"MAX_MODEL_ROUNDS", budget.MaxModelRounds)
	budget.MaxToolRounds = residentBudgetInt(prefix+"MAX_TOOL_ROUNDS", budget.MaxToolRounds)
	budget.MaxToolOutputChars = residentBudgetInt(prefix+"MAX_TOOL_OUTPUT_CHARS", budget.MaxToolOutputChars)

	// A malformed environment override must never consume the final-response
	// reserve. Fall back to the known-safe profile rather than creating a turn
	// that can spend all of its time in tools.
	if budget.TotalDuration <= budget.FinalResponseReserve || budget.ToolPhaseDuration <= 0 || budget.MaxModelRounds < 1 || budget.MaxToolRounds < 1 || budget.MaxToolOutputChars < 1 {
		return defaultResidentTurnBudget(turnType)
	}
	if availableToolTime := budget.TotalDuration - budget.FinalResponseReserve; budget.ToolPhaseDuration > availableToolTime {
		budget.ToolPhaseDuration = availableToolTime
	}
	return budget
}

func defaultResidentTurnBudget(turnType string) ResidentTurnBudget {
	// ask preserves the historical 90 second tool phase and 30 second final
	// response window. plan and dev are deliberately larger for their broader
	// local-Studio workflows, while remaining bounded without a settings UI.
	switch normalizeChatTurnType(turnType) {
	case "plan":
		return ResidentTurnBudget{
			TotalDuration: 4 * time.Minute, ToolPhaseDuration: 3 * time.Minute, FinalResponseReserve: 45 * time.Second,
			MaxModelRounds: 24, MaxToolRounds: 12, MaxToolOutputChars: 18000,
		}
	case "dev":
		return ResidentTurnBudget{
			TotalDuration: 6 * time.Minute, ToolPhaseDuration: 5 * time.Minute, FinalResponseReserve: time.Minute,
			MaxModelRounds: 32, MaxToolRounds: 16, MaxToolOutputChars: 24000,
		}
	default:
		return ResidentTurnBudget{
			TotalDuration: 2 * time.Minute, ToolPhaseDuration: 90 * time.Second, FinalResponseReserve: 30 * time.Second,
			// The legacy shared loop and Claude CLI restart path both capped ask
			// turns at eight model/tool rounds. Keep that compatibility contract;
			// plan and dev receive the deliberately larger profiles above.
			MaxModelRounds: 8, MaxToolRounds: 8, MaxToolOutputChars: 12000,
		}
	}
}

func residentBudgetDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func residentBudgetInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 1 {
		return fallback
	}
	return parsed
}
