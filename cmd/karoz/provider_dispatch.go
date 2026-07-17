package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

func (a *app) invokeCLI2API(ctx context.Context, req CLI2APIRequest) (CLI2APIResponse, error) {
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" || provider == "auto" {
		if fileExists(expandHome(getenv("KAROZ_CODEX_AUTH_PATH", "~/.codex/auth.json"))) {
			provider = "codex-direct"
		} else if _, err := exec.LookPath("claude"); err == nil {
			provider = "claude"
		} else {
			provider = "stub"
		}
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return CLI2APIResponse{}, errors.New("prompt is required")
	}
	workdir := strings.TrimSpace(req.Workdir)
	if workdir == "" {
		workdir = a.settings.ProjectsRoot
	}
	switch provider {
	case "cliproxy", "cli2api", "external":
		return invokeCLIProxyAPI(ctx, req)
	case "codex-direct", "codex-oauth", "codex-api":
		return invokeCodexDirect(ctx, workdir, prompt)
	case "stub":
		return CLI2APIResponse{
			Provider: provider,
			Output:   "Karoz received the request. cli2api is running in stub mode; set KAROZ_AGENT_PROVIDER=codex-direct to reuse Codex CLI OAuth and call the upstream API directly.",
			Stub:     true,
		}, nil
	case "claude":
		return invokeClaude(ctx, workdir, prompt, req.Mode)
	case "codex":
		return invokeCodex(ctx, workdir, prompt, req.Mode)
	default:
		return CLI2APIResponse{}, fmt.Errorf("unsupported provider %q", provider)
	}
}

func (a *app) invokeCLI2APIStream(ctx context.Context, req CLI2APIRequest, toolCtx ResidentToolContext, callbacks AgentStreamCallbacks) error {
	provider := a.resolveResidentProvider(req.Provider)
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return errors.New("prompt is required")
	}
	workdir := strings.TrimSpace(req.Workdir)
	if workdir == "" {
		workdir = a.settings.ProjectsRoot
	}
	if provider == "codex-direct" || provider == "codex-oauth" || provider == "codex-api" {
		toolCtx.Workdir = workdir
		tools := a.residentToolSpecsForContext(ctx, toolCtx)
		return invokeCodexDirectStream(ctx, workdir, prompt, tools, callbacks, func(call codexToolCall) (string, error) {
			return a.executeResidentTool(ctx, toolCtx, call)
		})
	}
	cli, err := a.invokeCLI2API(ctx, req)
	if err != nil {
		return err
	}
	if callbacks.OnDelta != nil {
		callbacks.OnDelta(cli.Output)
	}
	return nil
}

func (a *app) resolveResidentProvider(raw string) string {
	provider := strings.ToLower(strings.TrimSpace(raw))
	if provider != "" && provider != "auto" {
		return provider
	}
	if fileExists(expandHome(getenv("KAROZ_CODEX_AUTH_PATH", "~/.codex/auth.json"))) {
		return "codex-direct"
	}
	return "unavailable"
}

func (a *app) residentProviderCapabilities(raw string) runtimedomain.ProviderCapabilities {
	switch a.resolveResidentProvider(raw) {
	case "codex-direct", "codex-oauth", "codex-api":
		return runtimedomain.ProviderCapabilities{Streaming: true, Tools: true, Interrupts: true}
	default:
		return runtimedomain.ProviderCapabilities{}
	}
}

func invokeCLIProxyAPI(ctx context.Context, req CLI2APIRequest) (CLI2APIResponse, error) {
	baseURL := strings.TrimRight(getenv("KAROZ_CLI2API_BASE_URL", ""), "/")
	if baseURL == "" {
		return CLI2APIResponse{}, errors.New("KAROZ_CLI2API_BASE_URL is required for cli2api provider")
	}
	model := getenv("KAROZ_CLI2API_MODEL", "claude")
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": "You are Karoz, a project-scoped resident agent."},
			{"role": "user", "content": req.Prompt},
		},
		"stream": false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return CLI2APIResponse{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return CLI2APIResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(os.Getenv("KAROZ_CLI2API_API_KEY")); key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return CLI2APIResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return CLI2APIResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CLI2APIResponse{}, fmt.Errorf("cli2api status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	output := parseOpenAIChatContent(raw)
	if output == "" {
		output = strings.TrimSpace(string(raw))
	}
	return CLI2APIResponse{Provider: "cli2api", Output: output}, nil
}

func parseOpenAIChatContent(raw []byte) string {
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ""
	}
	if len(decoded.Choices) == 0 {
		return ""
	}
	return strings.TrimSpace(decoded.Choices[0].Message.Content)
}
