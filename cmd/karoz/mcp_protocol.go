package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func (c *mcpClient) initialize(ctx context.Context) error {
	_, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "karoz", "version": "dev"},
	})
	if err != nil {
		return err
	}
	return c.notify(ctx, "notifications/initialized", map[string]any{})
}

func (c *mcpClient) listTools(ctx context.Context, server string) ([]mcpTool, error) {
	raw, err := c.request(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Tools []struct {
			Name        string         `json:"name"`
			Title       string         `json:"title"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	serverKey := sanitizeMCPName(server)
	tools := make([]mcpTool, 0, len(payload.Tools))
	for _, tool := range payload.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			continue
		}
		tools = append(tools, mcpTool{
			Server:      server,
			ServerKey:   serverKey,
			Name:        tool.Name,
			DisplayName: tool.Title,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}
	return tools, nil
}

func (c *mcpClient) callTool(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	raw, err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *mcpClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.nextID++
	id := c.nextID
	if err := c.write(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		msg, err := c.readMessage(ctx)
		if err != nil {
			stderr := strings.TrimSpace(c.stderr.String())
			if stderr != "" {
				return nil, fmt.Errorf("%w: %s", err, limitString(stderr, 1000))
			}
			return nil, err
		}
		var envelope struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  any             `json:"error"`
		}
		if err := json.Unmarshal(msg, &envelope); err != nil {
			return nil, err
		}
		if envelope.Method != "" || fmt.Sprint(envelope.ID) != strconv.Itoa(id) {
			continue
		}
		if envelope.Error != nil {
			data, _ := json.Marshal(envelope.Error)
			return nil, errors.New(string(data))
		}
		return envelope.Result, nil
	}
}

func (c *mcpClient) notify(ctx context.Context, method string, params any) error {
	return c.write(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *mcpClient) write(ctx context.Context, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if c.postURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.postURL, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return fmt.Errorf("MCP POST status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	_, err = fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n%s", len(data), data)
	return err
}

func (c *mcpClient) readMessage(ctx context.Context) ([]byte, error) {
	if c.messages != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case msg, ok := <-c.messages:
			if !ok {
				select {
				case err := <-c.messageErrors:
					return nil, err
				default:
				}
				return nil, io.EOF
			}
			return msg, nil
		}
	}
	return c.readStdioMessage()
}

func (c *mcpClient) readStdioMessage() ([]byte, error) {
	contentLength := -1
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, err
			}
			contentLength = n
		}
	}
	if contentLength < 0 {
		return nil, errors.New("missing Content-Length")
	}
	data := make([]byte, contentLength)
	_, err := io.ReadFull(c.reader, data)
	return data, err
}

func (c *mcpClient) close() error {
	if c.sseCancel != nil {
		c.sseCancel()
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.processCancel != nil {
		c.processCancel()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	if c.cmd != nil {
		return c.cmd.Wait()
	}
	return nil
}
