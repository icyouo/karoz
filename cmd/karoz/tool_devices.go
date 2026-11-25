package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	devicedomain "github.com/karoz/karoz/internal/device"
)

func residentDeviceToolSpecs() []map[string]any {
	return []map[string]any{
		residentToolSpec("device_list", "List host ADB devices, local Android AVDs, and project Redroid containers.", map[string]any{
			"backend": map[string]any{"type": "string", "description": "Optional backend filter: adb_host, avd, or redroid."},
		}, nil),
		residentToolSpec("android_device_start", "Start a local Android AVD emulator, connect a host ADB target, or launch a project Redroid container.", map[string]any{
			"backend":   map[string]any{"type": "string", "description": "Optional backend: avd, adb_host, or redroid."},
			"device_id": map[string]any{"type": "string", "description": "Optional target device id such as avd:Pixel_7 or host:127.0.0.1:5555."},
			"avd_name":  map[string]any{"type": "string", "description": "Optional Android AVD name from emulator -list-avds."},
			"serial":    map[string]any{"type": "string", "description": "Optional ADB serial or host:port for adb_host backend."},
			"name":      map[string]any{"type": "string", "description": "Optional friendly device or container name."},
			"image":     map[string]any{"type": "string", "description": "Optional Docker image for redroid backend."},
			"headless":  map[string]any{"type": "boolean", "description": "Run emulator without a GUI window (-no-window -no-audio)."},
		}, nil),
		residentToolSpec("device_screenshot", "Capture a PNG screenshot from an online Android device and save it inside .karoz/screenshots/.", map[string]any{
			"device_id": map[string]any{"type": "string", "description": "Target device id or ADB serial."},
		}, []string{"device_id"}),
		residentToolSpec("device_logs", "Read recent Android logcat output from an online device.", map[string]any{
			"device_id": map[string]any{"type": "string", "description": "Target device id or ADB serial."},
			"lines":     map[string]any{"type": "integer", "description": "Trailing lines to return (1-2000, default 120)."},
		}, []string{"device_id"}),
		residentToolSpec("device_action", "Perform an Android UI or intent action: tap, double_tap, long_press, swipe, type_text, back, home, app_switch, keyevent, launch_app, open_url, or wait.", map[string]any{
			"device_id":   map[string]any{"type": "string", "description": "Target device id or ADB serial."},
			"kind":        map[string]any{"type": "string", "description": "Action kind: tap, double_tap, long_press, swipe, type_text, back, home, app_switch, keyevent, launch_app, open_url, or wait."},
			"x":           map[string]any{"type": "integer"},
			"y":           map[string]any{"type": "integer"},
			"from_x":      map[string]any{"type": "integer"},
			"from_y":      map[string]any{"type": "integer"},
			"to_x":        map[string]any{"type": "integer"},
			"to_y":        map[string]any{"type": "integer"},
			"duration_ms": map[string]any{"type": "integer"},
			"text":        map[string]any{"type": "string"},
			"key":         map[string]any{"type": "string"},
			"package":     map[string]any{"type": "string"},
			"activity":    map[string]any{"type": "string"},
			"url":         map[string]any{"type": "string"},
			"ms":          map[string]any{"type": "integer"},
		}, []string{"device_id", "kind"}),
		residentToolSpec("android_adb_exec", "Run a bounded device-scoped ADB command (shell, exec-out, logcat, get-state, get-serialno, features, root/unroot, remount, verity, reboot) against one target device.", map[string]any{
			"device_id":        map[string]any{"type": "string", "description": "Target device id or ADB serial."},
			"args":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Device-local ADB subcommand arguments (omit adb and -s)."},
			"timeout_seconds":  map[string]any{"type": "integer", "description": "Optional timeout 1-60 seconds."},
			"max_output_bytes": map[string]any{"type": "integer", "description": "Optional byte limit up to 64000."},
		}, []string{"device_id", "args"}),
	}
}

func (a *app) executeResidentDeviceListTool(ctx context.Context, toolCtx ResidentToolContext, args map[string]any) string {
	devices, err := a.devices().ListDevices(ctx, toolCtx.Project.ID)
	if err != nil {
		return toolJSON(map[string]any{"error": err.Error()})
	}
	backendFilter := strings.ToLower(toolStringArg(args, "backend", 32))
	if backendFilter != "" {
		filtered := make([]devicedomain.Device, 0, len(devices))
		for _, dev := range devices {
			if strings.EqualFold(string(dev.Backend), backendFilter) {
				filtered = append(filtered, dev)
			}
		}
		devices = filtered
	}
	return toolJSON(map[string]any{"devices": devices})
}

func (a *app) executeResidentAndroidDeviceStartTool(ctx context.Context, toolCtx ResidentToolContext, args map[string]any) string {
	req := devicedomain.StartRequest{
		Backend:  devicedomain.BackendKind(strings.ToLower(toolStringArg(args, "backend", 32))),
		DeviceID: toolStringArg(args, "device_id", 200),
		AVDName:  toolStringArg(args, "avd_name", 200),
		Serial:   toolStringArg(args, "serial", 200),
		Name:     toolStringArg(args, "name", 120),
		Image:    toolStringArg(args, "image", 240),
		Headless: toolBoolArg(args, "headless", false),
	}
	dev, err := a.devices().StartDevice(ctx, toolCtx.Project.ID, req)
	if err != nil {
		return toolJSON(map[string]any{"error": err.Error()})
	}
	return toolJSON(map[string]any{"device": dev})
}

func (a *app) executeResidentDeviceScreenshotTool(ctx context.Context, toolCtx ResidentToolContext, args map[string]any) string {
	deviceID := toolStringArg(args, "device_id", 200)
	if deviceID == "" {
		return toolJSON(map[string]any{"error": "device_id is required"})
	}
	pngBytes, err := a.devices().Screenshot(ctx, toolCtx.Project.ID, deviceID)
	if err != nil {
		return toolJSON(map[string]any{"error": err.Error()})
	}
	savedPath := ""
	baseDir := strings.TrimSpace(toolCtx.Project.Path)
	if baseDir == "" {
		baseDir = strings.TrimSpace(toolCtx.Workdir)
	}
	if baseDir != "" {
		shotDir := filepath.Join(baseDir, ".karoz", "screenshots")
		if mkErr := os.MkdirAll(shotDir, 0755); mkErr == nil {
			outPath := filepath.Join(shotDir, sanitizeDeviceFileSegment(deviceID)+".png")
			if wErr := os.WriteFile(outPath, pngBytes, 0644); wErr == nil {
				savedPath = outPath
			}
		}
	}
	return toolJSON(map[string]any{
		"device_id": deviceID,
		"mime_type": "image/png",
		"bytes":     len(pngBytes),
		"path":      savedPath,
	})
}

func (a *app) executeResidentDeviceLogsTool(ctx context.Context, toolCtx ResidentToolContext, args map[string]any) string {
	deviceID := toolStringArg(args, "device_id", 200)
	if deviceID == "" {
		return toolJSON(map[string]any{"error": "device_id is required"})
	}
	lines := clampToolInt(args, "lines", 120, 1, 2000)
	logs, err := a.devices().Logs(ctx, toolCtx.Project.ID, deviceID, lines)
	if err != nil {
		return toolJSON(map[string]any{"error": err.Error()})
	}
	return toolJSON(map[string]any{
		"device_id": deviceID,
		"lines":     lines,
		"logs":      logs,
	})
}

func (a *app) executeResidentDeviceActionTool(ctx context.Context, toolCtx ResidentToolContext, args map[string]any) string {
	deviceID := toolStringArg(args, "device_id", 200)
	if deviceID == "" {
		return toolJSON(map[string]any{"error": "device_id is required"})
	}
	req := devicedomain.ActionRequest{
		Kind:       toolStringArg(args, "kind", 64),
		X:          optionalToolIntPtr(args, "x"),
		Y:          optionalToolIntPtr(args, "y"),
		FromX:      optionalToolIntPtr(args, "from_x"),
		FromY:      optionalToolIntPtr(args, "from_y"),
		ToX:        optionalToolIntPtr(args, "to_x"),
		ToY:        optionalToolIntPtr(args, "to_y"),
		DurationMS: clampToolInt(args, "duration_ms", 0, 0, 60000),
		Text:       toolStringArg(args, "text", 2000),
		Key:        toolStringArg(args, "key", 64),
		Package:    toolStringArg(args, "package", 256),
		Activity:   toolStringArg(args, "activity", 256),
		URL:        toolStringArg(args, "url", 2000),
		MS:         clampToolInt(args, "ms", 0, 0, 15000),
	}
	if err := a.devices().PerformAction(ctx, toolCtx.Project.ID, deviceID, req); err != nil {
		return toolJSON(map[string]any{"error": err.Error()})
	}
	return toolJSON(map[string]any{"ok": true, "device_id": deviceID, "kind": req.Kind})
}

func (a *app) executeResidentAndroidADBExecTool(ctx context.Context, toolCtx ResidentToolContext, args map[string]any) string {
	deviceID := toolStringArg(args, "device_id", 200)
	if deviceID == "" {
		return toolJSON(map[string]any{"error": "device_id is required"})
	}
	rawArgs := adbStringSliceArg(args, "args", 64, 4096)
	req := devicedomain.AndroidADBExecRequest{
		Args:           rawArgs,
		TimeoutSeconds: clampToolInt(args, "timeout_seconds", 0, 0, 60),
		MaxOutputBytes: clampToolInt(args, "max_output_bytes", 0, 0, devicedomain.AndroidADBMaxOutputBytes),
	}
	res, err := a.devices().ExecADB(ctx, toolCtx.Project.ID, deviceID, req)
	if err != nil {
		return toolJSON(map[string]any{"error": err.Error()})
	}
	return toolJSON(res)
}

func optionalToolIntPtr(args map[string]any, key string) *int {
	raw, exists := args[key]
	if !exists || raw == nil {
		return nil
	}
	var v int
	switch n := raw.(type) {
	case float64:
		v = int(n)
	case int:
		v = n
	case json.Number:
		parsed, err := n.Int64()
		if err != nil {
			return nil
		}
		v = int(parsed)
	default:
		return nil
	}
	return &v
}

func adbStringSliceArg(args map[string]any, key string, maxItems, maxItemLen int) []string {
	switch raw := args[key].(type) {
	case []string:
		if len(raw) > maxItems {
			raw = raw[:maxItems]
		}
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			out = append(out, limitString(item, maxItemLen))
		}
		return out
	case []any:
		limit := len(raw)
		if limit > maxItems {
			limit = maxItems
		}
		out := make([]string, 0, limit)
		for _, entry := range raw[:limit] {
			if s, ok := entry.(string); ok {
				out = append(out, limitString(s, maxItemLen))
			}
		}
		return out
	default:
		return nil
	}
}

func sanitizeDeviceFileSegment(deviceID string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(deviceID) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "device"
	}
	return out
}
