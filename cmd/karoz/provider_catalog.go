package main

import (
	"context"
	"net/http"
	"os"
	"strings"
)

func normalizeResidentProvider(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude", "claude-api", "anthropic", "anthropic-api":
		return "claude"
	case "codex", "codex-direct", "codex-oauth", "codex-api", "auto", "":
		return "codex"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func residentModelCatalog() []ResidentProviderDescriptor {
	codexAvailable := fileExists(expandHome(getenv("KAROZ_CODEX_AUTH_PATH", "~/.codex/auth.json")))
	claudeCLIAvailable := claudeCLIAuthenticated(context.Background())
	claudeAPIAvailable := strings.TrimSpace(firstNonEmpty(os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("KAROZ_ANTHROPIC_API_KEY"))) != ""
	claudeAvailable := claudeCLIAvailable || claudeAPIAvailable
	claudeTransport := "anthropic-api"
	if claudeCLIAvailable {
		claudeTransport = "claude-cli-oauth"
	}
	codexDefault := getenv("KAROZ_CODEX_MODEL", "gpt-5.6-luna")
	codexModels := []ResidentModelDescriptor{
		{Provider: "codex", ID: codexDefault, DisplayName: codexDefault, EffortLevels: []string{"low", "medium", "high", "xhigh", "max"}},
		{Provider: "codex", ID: "gpt-5.6-sol", DisplayName: "GPT-5.6 Sol", EffortLevels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
		{Provider: "codex", ID: "gpt-5.6-terra", DisplayName: "GPT-5.6 Terra", EffortLevels: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
		{Provider: "codex", ID: "gpt-5.6-luna", DisplayName: "GPT-5.6 Luna", EffortLevels: []string{"low", "medium", "high", "xhigh", "max"}},
		{Provider: "codex", ID: "gpt-5.3-codex", DisplayName: "GPT-5.3 Codex", EffortLevels: []string{"low", "medium", "high", "xhigh"}},
		{Provider: "codex", ID: "gpt-5.2", DisplayName: "GPT-5.2", EffortLevels: []string{"low", "medium", "high", "xhigh"}},
	}
	codexModels = uniqueResidentModels(codexModels)
	return []ResidentProviderDescriptor{
		{ID: "codex", DisplayName: "Codex", Transport: "codex-oauth", Available: codexAvailable, Reason: unavailableReason(codexAvailable, "Codex OAuth credentials were not found"), Models: codexModels},
		{ID: "claude", DisplayName: "Claude", Transport: claudeTransport, Available: claudeAvailable, Reason: unavailableReason(claudeAvailable, "Claude CLI is not logged in and ANTHROPIC_API_KEY is not configured"), Models: []ResidentModelDescriptor{
			{Provider: "claude", ID: "claude-opus-4-8", DisplayName: "Claude Opus 4.8", EffortLevels: []string{"low", "medium", "high", "xhigh", "max"}},
			{Provider: "claude", ID: "claude-sonnet-5", DisplayName: "Claude Sonnet 5", EffortLevels: []string{"low", "medium", "high", "xhigh", "max"}},
			{Provider: "claude", ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6", EffortLevels: []string{"low", "medium", "high", "max"}},
			{Provider: "claude", ID: "claude-haiku-4-5", DisplayName: "Claude Haiku 4.5", EffortLevels: []string{}},
		}},
	}
}

func uniqueResidentModels(items []ResidentModelDescriptor) []ResidentModelDescriptor {
	seen := map[string]bool{}
	out := make([]ResidentModelDescriptor, 0, len(items))
	for _, item := range items {
		key := item.Provider + "\x00" + item.ID
		if item.ID == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	return out
}

func unavailableReason(available bool, reason string) string {
	if available {
		return ""
	}
	return reason
}

func residentModelDescriptor(provider, model string) (ResidentModelDescriptor, bool) {
	provider = normalizeResidentProvider(provider)
	for _, item := range residentModelCatalog() {
		if item.ID != provider {
			continue
		}
		for _, candidate := range item.Models {
			if candidate.ID == strings.TrimSpace(model) {
				return candidate, true
			}
		}
	}
	return ResidentModelDescriptor{}, false
}

func residentProviderDescriptor(provider string) (ResidentProviderDescriptor, bool) {
	provider = normalizeResidentProvider(provider)
	for _, item := range residentModelCatalog() {
		if item.ID == provider {
			return item, true
		}
	}
	return ResidentProviderDescriptor{}, false
}

func validateResidentModelConfig(provider, model, effort string) error {
	descriptor, ok := residentModelDescriptor(provider, model)
	if !ok {
		return &modelConfigError{"model is not available for the selected provider"}
	}
	providerDescriptor, _ := residentProviderDescriptor(provider)
	if !providerDescriptor.Available {
		return &modelConfigError{providerDescriptor.Reason}
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	if len(descriptor.EffortLevels) == 0 {
		if effort != "" {
			return &modelConfigError{"the selected model does not support thinking effort"}
		}
		return nil
	}
	for _, allowed := range descriptor.EffortLevels {
		if effort == allowed {
			return nil
		}
	}
	return &modelConfigError{"thinking effort is not supported by the selected model"}
}

type modelConfigError struct{ message string }

func (err *modelConfigError) Error() string { return err.message }

func (a *app) handleRuntimeProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{"providers": residentModelCatalog()})
}
