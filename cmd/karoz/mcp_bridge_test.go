package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeMCPBridgeListsAndForwardsKarozTools(t *testing.T) {
	tempDir, err := os.MkdirTemp("/tmp", "kb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })
	specsPath := filepath.Join(tempDir, "tools.json")
	specs := []map[string]any{{"name": "repo_list", "description": "List files", "input_schema": map[string]any{"type": "object"}}}
	raw, _ := json.Marshal(specs)
	if err := os.WriteFile(specsPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(tempDir, "bridge.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		var request claudeBridgeRequest
		_ = json.NewDecoder(connection).Decode(&request)
		_ = json.NewEncoder(connection).Encode(claudeBridgeResponse{Result: `{"entries":[]}`})
	}()
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"repo_list","arguments":{"path":"."}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := runClaudeMCPBridge([]string{"--socket", socketPath, "--token", "secret", "--specs", specsPath}, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(strings.NewReader(output.String()))
	var responses []map[string]any
	for scanner.Scan() {
		var response map[string]any
		if json.Unmarshal(scanner.Bytes(), &response) == nil {
			responses = append(responses, response)
		}
	}
	if len(responses) != 3 {
		t.Fatalf("responses=%s", output.String())
	}
	listed := responses[1]["result"].(map[string]any)["tools"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["name"] != "repo_list" {
		t.Fatalf("tools=%+v", listed)
	}
	callResult := responses[2]["result"].(map[string]any)
	if callResult["isError"] != false || !strings.Contains(output.String(), "entries") {
		t.Fatalf("call response=%+v", callResult)
	}
}

func TestClaudeCLILineDelta(t *testing.T) {
	delta, cliErr := claudeCLILineDelta(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}`)
	if delta != "hello" || cliErr != "" {
		t.Fatalf("delta=%q err=%q", delta, cliErr)
	}
	_, cliErr = claudeCLILineDelta(`{"type":"result","is_error":true,"result":"failed"}`)
	if cliErr != "failed" {
		t.Fatalf("error=%q", cliErr)
	}
}
