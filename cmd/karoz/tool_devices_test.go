package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	devicedomain "github.com/karoz/karoz/internal/device"
)

func TestResidentDeviceToolsExecutionAndPolicy(t *testing.T) {
	runner := &stubDeviceRunner{}
	a, project := newTestDeviceApp(t, runner)
	devCtx := ResidentToolContext{
		Project:       project,
		Agent:         Agent{ID: "karoz", ProjectID: project.ID, Name: "karoz"},
		Workdir:       project.Path,
		TurnType:      "dev",
		EnforcePolicy: true,
	}

	// device_list works in ask & dev
	askCtx := devCtx
	askCtx.TurnType = "ask"
	listRes, err := a.executeResidentTool(context.Background(), askCtx, codexToolCall{
		Name:      "device_list",
		Arguments: `{"backend":"avd"}`,
	})
	if err != nil || !strings.Contains(listRes, "avd:Pixel_7") {
		t.Fatalf("device_list failed: %s err=%v", listRes, err)
	}

	// ask turn must forbid mutating device_action
	forbiddenRes, err := a.executeResidentTool(context.Background(), askCtx, codexToolCall{
		Name:      "device_action",
		Arguments: `{"device_id":"avd:Pixel_7","kind":"home"}`,
	})
	if err != nil || !strings.Contains(forbiddenRes, `"tool_forbidden"`) {
		t.Fatalf("expected ask turn tool_forbidden, got %s err=%v", forbiddenRes, err)
	}

	// device_screenshot writes PNG into .karoz/screenshots
	shotRes, err := a.executeResidentTool(context.Background(), devCtx, codexToolCall{
		Name:      "device_screenshot",
		Arguments: `{"device_id":"avd:Pixel_7"}`,
	})
	if err != nil {
		t.Fatalf("device_screenshot err=%v", err)
	}
	var shotPayload struct {
		Path  string `json:"path"`
		Bytes int    `json:"bytes"`
	}
	_ = json.Unmarshal([]byte(shotRes), &shotPayload)
	if shotPayload.Path == "" || shotPayload.Bytes == 0 {
		t.Fatalf("unexpected screenshot tool payload: %s", shotRes)
	}
	if _, statErr := os.Stat(shotPayload.Path); statErr != nil {
		t.Fatalf("expected saved screenshot at %q: %v", shotPayload.Path, statErr)
	}

	// device_logs works
	logsRes, err := a.executeResidentTool(context.Background(), askCtx, codexToolCall{
		Name:      "device_logs",
		Arguments: `{"device_id":"avd:Pixel_7","lines":25}`,
	})
	if err != nil || !strings.Contains(logsRes, "I/Test: ready") {
		t.Fatalf("device_logs failed: %s err=%v", logsRes, err)
	}

	// device_action tap works
	actRes, err := a.executeResidentTool(context.Background(), devCtx, codexToolCall{
		Name:      "device_action",
		Arguments: `{"device_id":"avd:Pixel_7","kind":"tap","x":120,"y":340}`,
	})
	if err != nil || !strings.Contains(actRes, `"ok":true`) {
		t.Fatalf("device_action failed: %s err=%v", actRes, err)
	}
	if !runner.hasCall("adb -s emulator-5554 shell input tap 120 340") {
		t.Fatal("expected resident device_action tap to reach runner")
	}

	// android_adb_exec works with devicedomain scoped args
	adbRes, err := a.executeResidentTool(context.Background(), devCtx, codexToolCall{
		Name:      "android_adb_exec",
		Arguments: `{"device_id":"avd:Pixel_7","args":["shell","id"]}`,
	})
	if err != nil {
		t.Fatalf("android_adb_exec err=%v", err)
	}
	var parsed devicedomain.AndroidADBExecResult
	_ = json.Unmarshal([]byte(adbRes), &parsed)
	if !strings.Contains(parsed.Stdout, "-s emulator-5554 shell 'id'") {
		t.Fatalf("unexpected android_adb_exec payload: %s", adbRes)
	}
}
