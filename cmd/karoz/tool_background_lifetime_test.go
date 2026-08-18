package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBackgroundLifetimeArgumentExposesAIControlledUnlimited(t *testing.T) {
	encoded, err := json.Marshal(residentToolSpecs())
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(encoded)
	if !strings.Contains(catalog, `"lifetime_ms"`) ||
		!strings.Contains(strings.ToLower(catalog), "use 0 for unlimited") {
		t.Fatalf("run_background catalog does not expose unlimited lifetime to the AI: %s", catalog)
	}

	lifetime, unlimited, err := backgroundLifetimeArg(map[string]any{"lifetime_ms": float64(0)}, 0)
	if err != nil || lifetime != 0 || !unlimited {
		t.Fatalf("unlimited argument = lifetime=%s unlimited=%t err=%v", lifetime, unlimited, err)
	}

	lifetime, unlimited, err = backgroundLifetimeArg(map[string]any{"lifetime_ms": float64((72 * time.Hour).Milliseconds())}, 0)
	if err != nil || lifetime != 72*time.Hour || unlimited {
		t.Fatalf("AI-selected finite argument = lifetime=%s unlimited=%t err=%v", lifetime, unlimited, err)
	}

	_, _, err = backgroundLifetimeArg(map[string]any{"lifetime_ms": float64(0)}, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "configured maximum") {
		t.Fatalf("administrator ceiling did not reject unlimited lifetime: %v", err)
	}
}
