package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func startMCPClient(ctx context.Context, workdir string, cfg MCPServerConfig) (*mcpClient, error) {
	if cfg.Type == "sse" || cfg.Type == "http" || cfg.Type == "streamable_http" {
		return startSSEMCPClient(ctx, cfg)
	}
	if cfg.Command == "" {
		return nil, errors.New("command is required")
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	cmd := exec.CommandContext(cmdCtx, cfg.Command, cfg.Args...)
	cmd.Cancel = func() error {
		cancel()
		if cmd.Process != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	if strings.TrimSpace(workdir) != "" {
		cmd.Dir = filepath.Clean(workdir)
	}
	cmd.Env = os.Environ()
	for key, value := range cfg.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	client := &mcpClient{cmd: cmd, stdin: stdin, reader: bufio.NewReader(stdout)}
	cmd.Stderr = &client.stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	if err := client.initialize(ctx); err != nil {
		_ = client.close()
		return nil, err
	}
	return client, nil
}

func startSSEMCPClient(ctx context.Context, cfg MCPServerConfig) (*mcpClient, error) {
	sseURL := strings.TrimSpace(cfg.URL)
	if sseURL == "" {
		return nil, errors.New("url is required")
	}
	parsed, err := url.Parse(sseURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("only http and https MCP URLs are supported")
	}
	clientCtx, cancel := context.WithCancel(ctx)
	httpClient := &http.Client{Timeout: 0}
	c := &mcpClient{
		httpClient: httpClient,
		sseCancel:  cancel,
		messages:   make(chan []byte, 64),
	}
	endpointCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go c.readSSE(clientCtx, sseURL, endpointCh, errCh)
	var endpoint string
	select {
	case endpoint = <-endpointCh:
	case err := <-errCh:
		cancel()
		return nil, err
	case <-time.After(15 * time.Second):
		cancel()
		return nil, errors.New("timed out waiting for MCP SSE endpoint")
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
	postURL, err := resolveMCPEndpointURL(sseURL, endpoint)
	if err != nil {
		cancel()
		return nil, err
	}
	c.postURL = postURL
	if err := c.initialize(ctx); err != nil {
		_ = c.close()
		return nil, err
	}
	return c, nil
}

func resolveMCPEndpointURL(base, endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", errors.New("empty MCP SSE endpoint")
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	endpointURL, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	return baseURL.ResolveReference(endpointURL).String(), nil
}

func (c *mcpClient) readSSE(ctx context.Context, sseURL string, endpointCh chan<- string, errCh chan<- error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		errCh <- err
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		errCh <- err
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		errCh <- fmt.Errorf("MCP SSE status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	event := "message"
	var data []string
	flush := func() {
		if len(data) == 0 {
			event = "message"
			return
		}
		payload := strings.Join(data, "\n")
		switch event {
		case "endpoint":
			select {
			case endpointCh <- payload:
			default:
			}
		default:
			select {
			case c.messages <- []byte(payload):
			case <-ctx.Done():
			}
		}
		event = "message"
		data = nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		errCh <- err
	}
}
