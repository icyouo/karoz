package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
	"unicode"
)

// memoryAnalysisTimeout bounds the side-channel classifier call. It is a var
// so tests can shrink it when exercising the timeout fallback.
var memoryAnalysisTimeout = 15 * time.Second

// memoryGateSystemPrompt is the fixed compact classifier prompt for the
// side-channel memory gate. The model must answer with strict JSON only.
const memoryGateSystemPrompt = `You decide whether answering a user message would benefit from the project's long-term memory (remembered facts, decisions, completed work, preferences, history).
Respond with strict JSON only: {"retrieve": boolean, "queries": [string]}.
Set retrieve=true when the message builds on earlier conversations, project history, prior decisions, or stored preferences. Set retrieve=false for self-contained questions, new tasks, and small talk.
queries holds 0-4 short topical search phrases that would best retrieve the relevant memories; keep the user's language. No markdown, no explanation.`

// memoryGateResult is the parsed side-channel verdict for one user message.
type memoryGateResult struct {
	Retrieve bool     `json:"retrieve"`
	Queries  []string `json:"queries"`
}

// memoryAnalyzerFunc analyzes one raw user message via a side-channel model
// call and returns the retrieval verdict. It is injectable on app so tests can
// stub it without a real provider.
type memoryAnalyzerFunc func(ctx context.Context, agent Agent, userText string) (memoryGateResult, error)

// memoryRetrievalQueryFor resolves which query text the resident prompt should
// use for long-term memory retrieval, or "" to skip retrieval for this turn.
// It never fails the turn: every analyzer problem falls back to the keyword
// baseline (retrieve with the raw user text).
func (a *app) memoryRetrievalQueryFor(ctx context.Context, agent Agent, userText string) string {
	text := strings.TrimSpace(userText)
	if text == "" {
		return ""
	}
	if memoryAnalysisDisabled() {
		return userText
	}
	// Memory cues force retrieval without spending a model call. They are
	// checked before the skip rules so a short "记得..." message still
	// retrieves.
	if memoryMessageHasCue(text) {
		return userText
	}
	if memoryMessageSkipsAnalysis(text) {
		return ""
	}
	analyzer := a.memoryAnalyzer
	if analyzer == nil {
		analyzer = a.analyzeMemoryNeed
	}
	analyzeCtx, cancel := context.WithTimeout(ctx, memoryAnalysisTimeout)
	defer cancel()
	result, err := analyzer(analyzeCtx, agent, text)
	if err != nil {
		log.Printf("memory analysis unavailable, falling back to keyword retrieval: %v", err)
		return userText
	}
	if !result.Retrieve {
		return ""
	}
	return memoryUnionQuery(userText, result.Queries)
}

// analyzeMemoryNeed is the production analyzer: exactly one non-tool model
// step through the same resident provider machinery the current run uses. Any
// failure (stub provider, transport error, timeout, unparseable JSON) is
// returned as an error so the caller falls back to the keyword baseline.
func (a *app) analyzeMemoryNeed(ctx context.Context, agent Agent, userText string) (memoryGateResult, error) {
	// The side channel reuses the single-step resident provider machinery.
	// When the run's provider port is replaced (stub/fake), that machinery is
	// not what serves the run, so analysis counts as unavailable.
	if a.modelProvider != nil {
		if _, ok := a.modelProvider.(cliModelProviderAdapter); !ok {
			return memoryGateResult{}, errors.New("memory analysis is unavailable for the configured provider")
		}
	}
	provider := a.resolveResidentProvider(agent.Provider)
	effort := memoryGateEffort(agent)
	callbacks := AgentStreamCallbacks{}
	switch provider {
	case "codex-direct", "codex-oauth", "codex-api":
		input := []map[string]any{
			codexMessage("system", memoryGateSystemPrompt),
			codexMessage("user", userText),
		}
		result, _, err := streamCodexStep(ctx, input, agent.Model, effort, nil, callbacks)
		if err != nil {
			return memoryGateResult{}, err
		}
		return parseMemoryGateResult(result.Text)
	case "claude-api":
		// newClaudeRequest owns the wire-level system field, so the classifier
		// prompt travels inside the single user message.
		messages := []map[string]any{{
			"role":    "user",
			"content": memoryGateSystemPrompt + "\n\nUser message to classify:\n" + userText,
		}}
		result, _, err := streamClaudeStep(ctx, agent.Model, effort, messages, nil, callbacks)
		if err != nil {
			return memoryGateResult{}, err
		}
		return parseMemoryGateResult(result.Text)
	default:
		return memoryGateResult{}, fmt.Errorf("memory analysis is unavailable for provider %q", provider)
	}
}

// parseMemoryGateResult extracts the strict JSON verdict from the classifier
// output, tolerating code fences and surrounding prose.
func parseMemoryGateResult(text string) (memoryGateResult, error) {
	trimmed := strings.TrimSpace(text)
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end <= start {
		return memoryGateResult{}, errors.New("memory analysis returned no JSON object")
	}
	var parsed memoryGateResult
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &parsed); err != nil {
		return memoryGateResult{}, fmt.Errorf("memory analysis returned invalid JSON: %w", err)
	}
	queries := make([]string, 0, 4)
	for _, query := range parsed.Queries {
		query = strings.TrimSpace(query)
		if query == "" {
			continue
		}
		if runes := []rune(query); len(runes) > 200 {
			query = string(runes[:200])
		}
		queries = append(queries, query)
		if len(queries) >= 4 {
			break
		}
	}
	parsed.Queries = queries
	return parsed, nil
}

// memoryUnionQuery builds the retrieval query as the union of the raw user
// text and the classifier-generated search phrases.
func memoryUnionQuery(userText string, queries []string) string {
	parts := make([]string, 0, len(queries)+1)
	parts = append(parts, strings.TrimSpace(userText))
	parts = append(parts, queries...)
	return strings.Join(parts, " ")
}

// memoryAnalysisDisabled reports the KAROZ_MEMORY_ANALYSIS kill switch: 0, off,
// or false disables the side channel and keeps the keyword baseline always.
func memoryAnalysisDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KAROZ_MEMORY_ANALYSIS"))) {
	case "0", "off", "false":
		return true
	default:
		return false
	}
}

// memoryCueTerms are the phrases that prove the user is referring to stored
// memory; Chinese terms are case-free, English terms match case-insensitively.
var memoryCueTerms = []string{"记住", "记得", "上次", "之前", "以前", "remember", "recall", "previously", "last time"}

func memoryMessageHasCue(text string) bool {
	lower := strings.ToLower(text)
	for _, cue := range memoryCueTerms {
		if strings.Contains(lower, cue) {
			return true
		}
	}
	return false
}

// memoryMessageSkipsAnalysis applies the cheap rule pre-filter: slash
// commands, choice-answer submissions (the UI submits them as "Selected: ..."),
// and trivially short messages never reach the analyzer and skip retrieval.
func memoryMessageSkipsAnalysis(text string) bool {
	if strings.HasPrefix(text, "/") {
		return true
	}
	if strings.HasPrefix(strings.ToLower(text), "selected: ") {
		return true
	}
	return memoryWordCount(text) < 3
}

// memoryWordCount counts whitespace-delimited words, treating each CJK rune as
// one word so Chinese messages are not misread as trivially short.
func memoryWordCount(text string) int {
	count := 0
	inWord := false
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			inWord = false
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r):
			count++
			inWord = false
		default:
			if !inWord {
				count++
				inWord = true
			}
		}
	}
	return count
}

// memoryGateEffort picks the minimal thinking effort for the agent's
// configured model, or "" when the model does not support effort selection.
// Catalogs list effort levels from lowest to highest.
func memoryGateEffort(agent Agent) string {
	descriptor, ok := residentModelDescriptor(agent.Provider, agent.Model)
	if !ok || len(descriptor.EffortLevels) == 0 {
		return ""
	}
	return descriptor.EffortLevels[0]
}
