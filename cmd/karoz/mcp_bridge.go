package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

type claudeBridgeRequest struct {
	Token     string `json:"token"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type claudeBridgeResponse struct {
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

func runClaudeMCPBridge(args []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("mcp-bridge", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	socketPath := flags.String("socket", "", "bridge socket")
	token := flags.String("token", "", "bridge token")
	specsPath := flags.String("specs", "", "tool specs")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *socketPath == "" || *token == "" || *specsPath == "" {
		return errors.New("mcp bridge requires socket, token, and specs")
	}
	raw, err := os.ReadFile(*specsPath)
	if err != nil {
		return err
	}
	var specs []map[string]any
	if err := json.Unmarshal(raw, &specs); err != nil {
		return err
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      any             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			continue
		}
		if request.ID == nil {
			continue
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		switch request.Method {
		case "initialize":
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(request.Params, &params)
			response["result"] = map[string]any{"protocolVersion": firstNonEmpty(params.ProtocolVersion, "2025-06-18"), "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": "karoz", "version": "1"}}
		case "ping":
			response["result"] = map[string]any{}
		case "tools/list":
			tools := make([]map[string]any, 0, len(specs))
			for _, spec := range specs {
				tools = append(tools, map[string]any{"name": spec["name"], "description": spec["description"], "inputSchema": spec["input_schema"]})
			}
			response["result"] = map[string]any{"tools": tools}
		case "tools/call":
			var params struct {
				Name      string `json:"name"`
				Arguments any    `json:"arguments"`
			}
			if err := json.Unmarshal(request.Params, &params); err != nil {
				response["error"] = map[string]any{"code": -32602, "message": "invalid tool arguments"}
				break
			}
			arguments, _ := json.Marshal(params.Arguments)
			result, callErr := callClaudeBridge(*socketPath, claudeBridgeRequest{Token: *token, Name: params.Name, Arguments: string(arguments)})
			if callErr != nil {
				response["result"] = map[string]any{"content": []map[string]any{{"type": "text", "text": callErr.Error()}}, "isError": true}
			} else {
				response["result"] = map[string]any{"content": []map[string]any{{"type": "text", "text": result}}, "isError": false}
			}
		default:
			response["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func callClaudeBridge(socketPath string, request claudeBridgeRequest) (string, error) {
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return "", err
	}
	var response claudeBridgeResponse
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		return "", err
	}
	if response.Error != "" {
		return response.Result, fmt.Errorf("%s", response.Error)
	}
	return response.Result, nil
}
