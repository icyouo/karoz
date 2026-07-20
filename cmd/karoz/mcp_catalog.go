package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type mcpTool struct {
	Server      string
	ServerKey   string
	Name        string
	DisplayName string
	Description string
	InputSchema map[string]any
}

type mcpClient struct {
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	reader        *bufio.Reader
	stderr        bytes.Buffer
	nextID        int
	httpClient    *http.Client
	postURL       string
	sseCancel     context.CancelFunc
	processCancel context.CancelFunc
	messages      chan []byte
	messageErrors chan error
}

func normalizeMCPServers(in map[string]MCPServerConfig) map[string]MCPServerConfig {
	if len(in) == 0 {
		return nil
	}
	out := map[string]MCPServerConfig{}
	for name, cfg := range in {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		cfg.Type = strings.ToLower(strings.TrimSpace(cfg.Type))
		if cfg.Type == "" {
			if strings.TrimSpace(cfg.URL) != "" {
				cfg.Type = "sse"
			} else {
				cfg.Type = "stdio"
			}
		}
		cfg.Command = strings.TrimSpace(expandHome(cfg.Command))
		cfg.URL = strings.TrimSpace(cfg.URL)
		if cfg.Type == "stdio" && cfg.Command == "" {
			continue
		}
		if cfg.Type != "stdio" && cfg.URL == "" {
			continue
		}
		for i := range cfg.Args {
			cfg.Args[i] = expandHome(cfg.Args[i])
		}
		out[name] = cfg
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (a *app) mcpToolSpecs(ctx context.Context, workdir string) []map[string]any {
	tools, err := a.discoverMCPTools(ctx, workdir)
	if err != nil {
		return nil
	}
	specs := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		schema := tool.InputSchema
		if len(schema) == 0 {
			schema = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true}
		}
		specs = append(specs, map[string]any{
			"type":        "function",
			"name":        mcpFullToolName(tool.ServerKey, tool.Name),
			"description": strings.TrimSpace("MCP server " + tool.Server + ": " + firstNonEmpty(tool.Description, tool.DisplayName, tool.Name)),
			"parameters":  schema,
		})
	}
	return specs
}

func (a *app) discoverMCPTools(ctx context.Context, workdir string) ([]mcpTool, error) {
	servers := a.mcpServersForWorkdir(workdir)
	var out []mcpTool
	for name, cfg := range servers {
		if cfg.Disabled {
			continue
		}
		client, err := startMCPClient(ctx, workdir, cfg)
		if err != nil {
			log.Printf("mcp discovery: start server %s: %v", name, err)
			continue
		}
		tools, err := client.listTools(ctx, name)
		_ = client.close()
		if err != nil {
			log.Printf("mcp discovery: list tools from server %s: %v", name, err)
			continue
		}
		out = append(out, tools...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ServerKey != out[j].ServerKey {
			return out[i].ServerKey < out[j].ServerKey
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (a *app) callMCPTool(ctx context.Context, workdir, fullName, rawArgs string) (string, error) {
	serverName, toolName, cfg, err := a.resolveMCPTool(ctx, workdir, fullName)
	if err != nil {
		return toolJSON(map[string]any{"error": "mcp_tool_not_found", "message": err.Error()}), nil
	}
	var args map[string]any
	if strings.TrimSpace(rawArgs) != "" {
		if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
			return toolJSON(map[string]any{"error": "validation_error", "message": err.Error()}), nil
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	client, err := startMCPClient(ctx, workdir, cfg)
	if err != nil {
		return toolJSON(map[string]any{"error": "mcp_start_failed", "message": err.Error()}), nil
	}
	defer client.close()
	result, err := client.callTool(ctx, toolName, args)
	if err != nil {
		return toolJSON(map[string]any{"error": "mcp_call_failed", "server": serverName, "tool": toolName, "message": err.Error()}), nil
	}
	return toolJSON(map[string]any{"server": serverName, "tool": toolName, "result": result}), nil
}

func (a *app) mcpServersSnapshot() map[string]MCPServerConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return normalizeMCPServers(a.settings.MCPServers)
}

func (a *app) mcpServersForWorkdir(workdir string) map[string]MCPServerConfig {
	servers := a.mcpServersSnapshot()
	if servers == nil {
		servers = map[string]MCPServerConfig{}
	}
	if getenv("KAROZ_TRUST_PROJECT_MCP", "") == "1" {
		for name, cfg := range loadProjectMCPServers(workdir) {
			servers[name] = cfg
		}
	}
	return normalizeMCPServers(servers)
}

func (a *app) resolveMCPTool(ctx context.Context, workdir, fullName string) (string, string, MCPServerConfig, error) {
	servers := a.mcpServersForWorkdir(workdir)
	name := strings.TrimPrefix(fullName, "mcp__")
	type candidate struct {
		server string
		key    string
		cfg    MCPServerConfig
	}
	var candidates []candidate
	for server, cfg := range servers {
		key := sanitizeMCPName(server)
		prefix := key + "__"
		if strings.HasPrefix(name, prefix) {
			candidates = append(candidates, candidate{server: server, key: key, cfg: cfg})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return len(candidates[i].key) > len(candidates[j].key) })
	for _, candidate := range candidates {
		sanitizedTool := strings.TrimPrefix(name, candidate.key+"__")
		client, err := startMCPClient(ctx, workdir, candidate.cfg)
		if err != nil {
			continue
		}
		tools, err := client.listTools(ctx, candidate.server)
		_ = client.close()
		if err != nil {
			continue
		}
		for _, tool := range tools {
			if sanitizeMCPName(tool.Name) == sanitizedTool {
				return candidate.server, tool.Name, candidate.cfg, nil
			}
		}
	}
	return "", "", MCPServerConfig{}, fmt.Errorf("unknown MCP tool %s", fullName)
}

func loadProjectMCPServers(workdir string) map[string]MCPServerConfig {
	workdir = filepath.Clean(expandHome(strings.TrimSpace(workdir)))
	if workdir == "." || workdir == "" {
		return nil
	}
	path := filepath.Join(workdir, ".mcp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var raw struct {
		MCPServers      map[string]MCPServerConfig `json:"mcpServers"`
		MCPServersSnake map[string]MCPServerConfig `json:"mcp_servers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	if len(raw.MCPServers) > 0 {
		return normalizeMCPServers(raw.MCPServers)
	}
	return normalizeMCPServers(raw.MCPServersSnake)
}

func mcpFullToolName(server, tool string) string {
	return "mcp__" + sanitizeMCPName(server) + "__" + sanitizeMCPName(tool)
}

func sanitizeMCPName(value string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if ok {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}
