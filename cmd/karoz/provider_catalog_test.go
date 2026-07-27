package main

import "testing"

func TestResidentModelCatalogIncludesContextWindows(t *testing.T) {
	models := map[string]int64{}
	for _, provider := range residentModelCatalog() {
		for _, model := range provider.Models {
			models[model.ID] = model.ContextWindow
		}
	}
	for model, want := range map[string]int64{
		"gpt-5.6-sol":       1_000_000,
		"gpt-5.6-terra":     1_000_000,
		"gpt-5.6-luna":      1_000_000,
		"gpt-5.3-codex":     400_000,
		"gpt-5.2":           400_000,
		"claude-opus-4-8":   1_000_000,
		"claude-sonnet-5":   1_000_000,
		"claude-sonnet-4-6": 1_000_000,
		"claude-haiku-4-5":  200_000,
	} {
		if got := models[model]; got != want {
			t.Errorf("%s context window = %d, want %d", model, got, want)
		}
	}
}
