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
	"path/filepath"
	"strings"
	"time"

	executiondomain "github.com/karoz/karoz/internal/execution"
)

func (a *app) startMCPClient(ctx context.Context, workdir string, cfg MCPServerConfig) (*mcpClient, error) {
	return startMCPClientWithRunner(a.streamRunnerOrDefault(), ctx, workdir, cfg)
}

func startMCPClientWithRunner(runner executiondomain.StreamRunner, ctx context.Context, workdir string, cfg MCPServerConfig) (*mcpClient, error) {
	if cfg.Type == "sse" || cfg.Type == "http" || cfg.Type == "streamable_http" {
		return startSSEMCPClient(ctx, cfg)
	}
	if cfg.Command == "" {
		return nil, errors.New("command is required")
	}
	processCtx, processCancel := context.WithCancel(ctx)
	env := os.Environ()
	for key, value := range cfg.Env {
		env = append(env, key+"="+value)
	}
	stream, err := runner.Start(processCtx, executiondomain.StreamCommandRequest{
		Name: cfg.Command, Args: cfg.Args, Dir: filepath.Clean(workdir), Env: env, StdinPipe: true,
		Configure: configureResidentCommand,
	})
	if err != nil {
		processCancel()
		return nil, err
	}
	client := &mcpClient{
		process: stream, stdin: stream.Stdin, reader: bufio.NewReader(stream.Stdout), processCancel: processCancel,
		messages: make(chan []byte, 64), messageErrors: make(chan error, 1),
	}
	go func() {
		_, _ = io.Copy(&client.stderr, stream.Stderr)
		_ = stream.Stderr.Close()
	}()
	go client.readStdio(processCtx)
	initCtx, initCancel := context.WithTimeout(ctx, 20*time.Second)
	defer initCancel()
	if err := client.initialize(initCtx); err != nil {
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
		httpClient:    httpClient,
		sseCancel:     cancel,
		messages:      make(chan []byte, 64),
		messageErrors: make(chan error, 1),
	}
	endpointCh := make(chan string, 1)
	go c.readSSE(clientCtx, sseURL, endpointCh)
	var endpoint string
	select {
	case endpoint = <-endpointCh:
	case err := <-c.messageErrors:
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
	initCtx, initCancel := context.WithTimeout(ctx, 20*time.Second)
	defer initCancel()
	if err := c.initialize(initCtx); err != nil {
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

func (c *mcpClient) readStdio(ctx context.Context) {
	defer close(c.messages)
	for {
		message, err := c.readStdioMessage()
		if err != nil {
			if ctx.Err() == nil {
				c.reportMessageError(err)
			}
			return
		}
		select {
		case c.messages <- message:
		case <-ctx.Done():
			return
		}
	}
}

func (c *mcpClient) readSSE(ctx context.Context, sseURL string, endpointCh chan<- string) {
	defer close(c.messages)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sseURL, nil)
	if err != nil {
		c.reportMessageError(err)
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			c.reportMessageError(err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		c.reportMessageError(fmt.Errorf("MCP SSE status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
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
	if ctx.Err() != nil {
		return
	}
	if err := scanner.Err(); err != nil {
		c.reportMessageError(err)
		return
	}
	c.reportMessageError(io.EOF)
}

func (c *mcpClient) reportMessageError(err error) {
	if err == nil || c.messageErrors == nil {
		return
	}
	select {
	case c.messageErrors <- err:
	default:
	}
}
