package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCodexAndClaudeReceiveIdenticalAllowedToolContracts(t *testing.T) {
	a, project := newHandlerTestApp(t)
	agent, ok := a.projectAgent(project, "karoz")
	if !ok {
		t.Fatal("default resident agent missing")
	}
	for _, turnType := range []string{"ask", "plan", "dev"} {
		t.Run(turnType, func(t *testing.T) {
			toolCtx := ResidentToolContext{Project: project, Agent: agent, Workdir: project.Path, TurnType: turnType, EnforcePolicy: true}
			codex, err := json.Marshal(a.residentToolContractForProvider(context.Background(), toolCtx, "codex-direct"))
			if err != nil {
				t.Fatal(err)
			}
			claude, err := json.Marshal(a.residentToolContractForProvider(context.Background(), toolCtx, "claude-api"))
			if err != nil {
				t.Fatal(err)
			}
			if string(codex) != string(claude) {
				t.Fatalf("provider tool contracts differ\ncodex=%s\nclaude=%s", codex, claude)
			}
		})
	}
}
