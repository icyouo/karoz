package device

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedCall struct {
	name string
	args []string
}

type fakeProcessHandle struct {
	killed bool
}

func (f *fakeProcessHandle) Kill() error {
	f.killed = true
	return nil
}
func (f *fakeProcessHandle) Wait() error { return nil }

type fakeRunner struct {
	mu      sync.Mutex
	calls   []recordedCall
	runFn   func(ctx context.Context, name string, args ...string) ([]byte, error)
	startFn func(name string, args ...string) (ProcessHandle, error)
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{name: name, args: append([]string(nil), args...)})
	f.mu.Unlock()
	if f.runFn != nil {
		return f.runFn(ctx, name, args...)
	}
	return nil, nil
}

func (f *fakeRunner) Start(name string, args ...string) (ProcessHandle, error) {
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{name: name, args: append([]string(nil), args...)})
	f.mu.Unlock()
	if f.startFn != nil {
		return f.startFn(name, args...)
	}
	return &fakeProcessHandle{}, nil
}

func (f *fakeRunner) hasCall(name string, args ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call.name == name && reflect.DeepEqual(call.args, args) {
			return true
		}
	}
	return false
}

func TestManagerListDevicesAcrossBackendsAndProjectIsolation(t *testing.T) {
	redroidMetaProjA, _ := json.Marshal(map[string]any{
		"State": map[string]any{"Running": true},
		"Config": map[string]any{
			"Labels": map[string]string{
				"karoz.kind":       "android-device",
				"karoz.project_id": "proj-a",
			},
		},
		"NetworkSettings": map[string]any{
			"Networks": map[string]any{
				"bridge": map[string]any{"IPAddress": "172.17.0.9"},
			},
		},
	})
	redroidMetaProjB, _ := json.Marshal(map[string]any{
		"State": map[string]any{"Running": true},
		"Config": map[string]any{
			"Labels": map[string]string{
				"karoz.kind":       "android-device",
				"karoz.project_id": "proj-b",
			},
		},
	})

	runner := &fakeRunner{
		runFn: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := name + " " + strings.Join(args, " ")
			switch cmd {
			case "adb devices -l":
				return []byte("List of devices attached\nemulator-5554          device product:sdk model:Pixel_7 transport_id:1\n192.168.1.50:5555      device model:Galaxy_S24\n"), nil
			case "adb -s emulator-5554 emu avd name":
				return []byte("Pixel_7_API_34\nOK\n"), nil
			case "adb -s emulator-5554 shell wm size":
				return []byte("Physical size: 1080x2400\n"), nil
			case "adb -s 192.168.1.50:5555 shell wm size":
				return []byte("Physical size: 1440x3120\n"), nil
			case "emulator -list-avds":
				return []byte("Pixel_7_API_34\nTablet_API_34\n"), nil
			case "docker ps -a --filter label=karoz.kind=android-device --filter label=karoz.project_id=proj-a --format {{.Names}}":
				return []byte("karoz-redroid-proj-a-dev\n"), nil
			case "docker inspect --format {{json .}} -- karoz-redroid-proj-a-dev":
				return redroidMetaProjA, nil
			case "docker inspect --format {{json .}} -- karoz-redroid-proj-b-other":
				return redroidMetaProjB, nil
			default:
				return nil, fmt.Errorf("unexpected cmd: %s", cmd)
			}
		},
	}

	mgr := NewManager(WithCommandRunner(runner))
	devices, err := mgr.ListDevices(context.Background(), "proj-a")
	if err != nil {
		t.Fatalf("ListDevices failed: %v", err)
	}
	if len(devices) != 4 {
		t.Fatalf("got %d devices, want 4: %+v", len(devices), devices)
	}

	byID := make(map[string]Device)
	for _, dev := range devices {
		byID[dev.ID] = dev
	}
	if dev := byID["host:192.168.1.50:5555"]; dev.Backend != BackendADBHost || dev.Width != 1440 || dev.Height != 3120 {
		t.Fatalf("unexpected adb_host device: %+v", dev)
	}
	if dev := byID["avd:Pixel_7_API_34"]; dev.Backend != BackendAVD || dev.Status != StatusReady || dev.Height != 2400 {
		t.Fatalf("unexpected running avd device: %+v", dev)
	}
	if dev := byID["avd:Tablet_API_34"]; dev.Backend != BackendAVD || dev.Status != StatusStopped {
		t.Fatalf("unexpected stopped avd device: %+v", dev)
	}
	if dev := byID["redroid:karoz-redroid-proj-a-dev"]; dev.Backend != BackendRedroid || dev.Serial != "172.17.0.9:5555" {
		t.Fatalf("unexpected redroid device: %+v", dev)
	}

	if _, err := mgr.inspectProjectRedroidTarget(context.Background(), "proj-a", "karoz-redroid-proj-b-other"); err == nil {
		t.Fatal("expected cross-project redroid inspection to fail")
	}
}

func TestManagerLifecycleActionsScreenshotLogsAndExec(t *testing.T) {
	proc := &fakeProcessHandle{}
	hasStoppedRedroid := false
	redroidInspect, _ := json.Marshal(map[string]any{
		"State": map[string]any{"Running": false},
		"Config": map[string]any{
			"Labels": map[string]string{
				"karoz.kind":       "android-device",
				"karoz.project_id": "proj-1",
			},
		},
	})
	fakePNG := append([]byte(nil), pngSignature...)
	fakePNG = append(fakePNG, 1, 2, 3, 4)

	runner := &fakeRunner{
		runFn: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := name + " " + strings.Join(args, " ")
			switch cmd {
			case "adb devices -l":
				return []byte("List of devices attached\nemulator-5554 device model:Pixel\n"), nil
			case "adb -s emulator-5554 emu avd name":
				return []byte("Pixel_AVD\nOK\n"), nil
			case "adb -s emulator-5554 shell wm size":
				return []byte("Physical size: 1080x1920\n"), nil
			case "emulator -list-avds":
				return []byte("Pixel_AVD\nSecond_AVD\n"), nil
			case "adb -s emulator-5554 exec-out screencap -p":
				return fakePNG, nil
			case "adb -s emulator-5554 logcat -d -t 50":
				return []byte("I/ActivityManager: Displayed com.example/.Main\n"), nil
			case "adb -s emulator-5554 shell input tap 120 340":
				return nil, nil
			case "adb -s emulator-5554 emu kill":
				return nil, nil
			case "docker ps -a --filter label=karoz.kind=android-device --filter label=karoz.project_id=proj-1 --format {{.Names}}":
				if hasStoppedRedroid {
					return []byte("karoz-redroid-proj-1-dev\n"), nil
				}
				return nil, errors.New("docker unavailable")
			case "docker inspect --format {{json .}} -- karoz-redroid-proj-1-dev":
				return redroidInspect, nil
			case "docker rm -f karoz-redroid-proj-1-dev":
				hasStoppedRedroid = false
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected cmd: %s", cmd)
			}
		},
		startFn: func(name string, args ...string) (ProcessHandle, error) {
			return proc, nil
		},
	}

	var capturedADB []string
	mgr := NewManager(
		WithCommandRunner(runner),
		WithADBExec(func(ctx context.Context, args []string, limit int) (*AndroidADBExecResult, error) {
			capturedADB = args
			return &AndroidADBExecResult{Stdout: "package:com.example\n"}, nil
		}),
	)
	defer mgr.Close()

	started, err := mgr.StartDevice(context.Background(), "proj-1", StartRequest{Backend: BackendAVD, AVDName: "Second_AVD", Headless: true})
	if err != nil {
		t.Fatalf("StartDevice failed: %v", err)
	}
	if started.ID != "avd:Second_AVD" || started.Status != StatusStarting {
		t.Fatalf("unexpected started device: %+v", started)
	}
	if !runner.hasCall("emulator", "-avd", "Second_AVD", "-no-window", "-no-audio") {
		t.Fatal("expected headless emulator launch arguments")
	}

	png, err := mgr.Screenshot(context.Background(), "proj-1", "avd:Pixel_AVD")
	if err != nil || len(png) != len(fakePNG) {
		t.Fatalf("Screenshot failed: len=%d err=%v", len(png), err)
	}
	logs, err := mgr.Logs(context.Background(), "proj-1", "avd:Pixel_AVD", 50)
	if err != nil || !strings.Contains(logs, "ActivityManager") {
		t.Fatalf("Logs failed: %q err=%v", logs, err)
	}
	x, y := 120, 340
	if err := mgr.PerformAction(context.Background(), "proj-1", "avd:Pixel_AVD", ActionRequest{Kind: "tap", X: &x, Y: &y}); err != nil {
		t.Fatalf("PerformAction tap failed: %v", err)
	}
	if !runner.hasCall("adb", "-s", "emulator-5554", "shell", "input", "tap", "120", "340") {
		t.Fatal("expected adb input tap command")
	}

	execRes, err := mgr.ExecADB(context.Background(), "proj-1", "avd:Pixel_AVD", AndroidADBExecRequest{
		Args: []string{"shell", "pm", "list", "packages"},
	})
	if err != nil || execRes.Stdout != "package:com.example\n" {
		t.Fatalf("ExecADB failed: %+v err=%v", execRes, err)
	}
	if !reflect.DeepEqual(capturedADB, []string{"-s", "emulator-5554", "shell", "'pm' 'list' 'packages'"}) {
		t.Fatalf("unexpected scoped ADB args: %v", capturedADB)
	}

	if err := mgr.StopDevice(context.Background(), "proj-1", "avd:Second_AVD"); err != nil {
		t.Fatalf("StopDevice AVD failed: %v", err)
	}
	if !proc.killed {
		t.Fatal("expected started AVD process to be killed on stop")
	}

	hasStoppedRedroid = true
	if err := mgr.StopDevice(context.Background(), "proj-1", "redroid:karoz-redroid-proj-1-dev"); err != nil {
		t.Fatalf("StopDevice stopped redroid container failed: %v", err)
	}
	if !runner.hasCall("docker", "rm", "-f", "karoz-redroid-proj-1-dev") {
		t.Fatal("expected docker rm -f on stopped project redroid container")
	}
}

func TestManagerInjectTouchUsesScrcpyFastPathAndFallsBack(t *testing.T) {
	runner := &fakeRunner{
		runFn: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := name + " " + strings.Join(args, " ")
			switch cmd {
			case "adb devices -l":
				return []byte("List of devices attached\nemulator-5554 device model:Pixel\n"), nil
			case "adb -s emulator-5554 emu avd name":
				return []byte("Pixel_AVD\nOK\n"), nil
			case "adb -s emulator-5554 shell wm size":
				return []byte("Physical size: 1080x1920\n"), nil
			case "emulator -list-avds",
				"docker ps -a --filter label=karoz.kind=android-device --filter label=karoz.project_id=proj-1 --format {{.Names}}":
				return nil, nil
			case "adb -s emulator-5554 shell input tap 540 960":
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected cmd: %s", cmd)
			}
		},
	}

	mgr := NewManager(WithCommandRunner(runner))
	defer mgr.Close()

	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	inj := NewScrcpyTouchInjector("emulator-5554", clientConn)
	mgr.SetScrcpyTouchInjector("emulator-5554", inj)

	received := make(chan []byte, 4)
	go func() {
		for {
			buf := make([]byte, 32)
			if _, err := io.ReadFull(serverConn, buf); err != nil {
				return
			}
			received <- buf
		}
	}()

	for _, phase := range []string{"down", "move", "up"} {
		if err := mgr.InjectTouch(context.Background(), "proj-1", "avd:Pixel_AVD", TouchEvent{
			Phase:          phase,
			X:              100,
			Y:              200,
			ViewportWidth:  540,
			ViewportHeight: 960,
		}); err != nil {
			t.Fatalf("InjectTouch %s failed: %v", phase, err)
		}
	}

	wantActions := []byte{ScrcpyMotionActionDown, ScrcpyMotionActionMove, ScrcpyMotionActionUp}
	for i, wantAction := range wantActions {
		select {
		case packet := <-received:
			if packet[0] != ScrcpyControlInjectTouch || packet[1] != wantAction {
				t.Fatalf("packet[%d] header = [%x %x], want [%x %x]", i, packet[0], packet[1], ScrcpyControlInjectTouch, wantAction)
			}
			gotX := binary.BigEndian.Uint32(packet[10:14])
			gotY := binary.BigEndian.Uint32(packet[14:18])
			if gotX != 200 || gotY != 400 {
				t.Fatalf("packet[%d] coords = (%d, %d), want (200, 400)", i, gotX, gotY)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for scrcpy packet %d", i)
		}
	}
	if runner.hasCall("adb", "-s", "emulator-5554", "shell", "input", "tap", "200", "400") {
		t.Fatal("expected scrcpy fast-path without spawning adb shell input")
	}

	// Simulate dropped scrcpy control connection -> clear broken injector & fall back to adb input tap.
	_ = serverConn.Close()
	if err := mgr.InjectTouch(context.Background(), "proj-1", "avd:Pixel_AVD", TouchEvent{
		Phase:          "tap",
		X:              540,
		Y:              960,
		ViewportWidth:  1080,
		ViewportHeight: 1920,
	}); err != nil {
		t.Fatalf("InjectTouch fallback failed: %v", err)
	}
	if !runner.hasCall("adb", "-s", "emulator-5554", "shell", "input", "tap", "540", "960") {
		t.Fatal("expected adb input tap fallback after dropped scrcpy control socket")
	}
}
