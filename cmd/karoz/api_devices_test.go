package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	devicedomain "github.com/karoz/karoz/internal/device"
)

type stubDeviceRunner struct {
	mu    sync.Mutex
	calls []string
}

func (r *stubDeviceRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := name + " " + strings.Join(args, " ")
	r.mu.Lock()
	r.calls = append(r.calls, cmd)
	r.mu.Unlock()
	switch cmd {
	case "adb devices -l":
		return []byte("List of devices attached\nemulator-5554 device model:Pixel_7\n"), nil
	case "adb -s emulator-5554 emu avd name":
		return []byte("Pixel_7\nOK\n"), nil
	case "adb -s emulator-5554 shell wm size":
		return []byte("Physical size: 1080x1920\n"), nil
	case "emulator -list-avds":
		return []byte("Pixel_7\n"), nil
	case "adb -s emulator-5554 exec-out screencap -p":
		return []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1, 2, 3, 4}, nil
	case "adb -s emulator-5554 logcat -d -t 25":
		return []byte("I/Test: ready\n"), nil
	case "adb -s emulator-5554 shell input tap 120 340":
		return nil, nil
	default:
		if strings.HasPrefix(cmd, "docker ps -a --filter label=karoz.kind=android-device") {
			return nil, errors.New("docker absent")
		}
		return nil, fmt.Errorf("unexpected cmd: %s", cmd)
	}
}

func (r *stubDeviceRunner) Start(name string, args ...string) (devicedomain.ProcessHandle, error) {
	return nil, errors.New("not implemented")
}

func (r *stubDeviceRunner) hasCall(want string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if call == want {
			return true
		}
	}
	return false
}

func newTestDeviceApp(t *testing.T, runner devicedomain.CommandRunner) (*app, Project) {
	t.Helper()
	a, project := newHandlerTestApp(t)
	t.Cleanup(func() { a.devices().Close() })

	a.mu.Lock()
	a.deviceManager = devicedomain.NewManager(
		devicedomain.WithCommandRunner(runner),
		devicedomain.WithADBExec(func(ctx context.Context, args []string, limit int) (*devicedomain.AndroidADBExecResult, error) {
			return &devicedomain.AndroidADBExecResult{Stdout: strings.Join(args, " ")}, nil
		}),
	)
	a.mu.Unlock()
	return a, project
}

func TestDevicesRESTAndSecurityBoundaries(t *testing.T) {
	runner := &stubDeviceRunner{}
	a, project := newTestDeviceApp(t, runner)
	handler := withRecovery(a.httpHandler())
	baseOrigin := "http://127.0.0.1:43123"
	host := "127.0.0.1:43123"

	doReq := func(method, path, body, origin string) *httptest.ResponseRecorder {
		var bodyReader io.Reader
		if body != "" {
			bodyReader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, baseOrigin+path, bodyReader)
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// GET devices
	rec := doReq(http.MethodGet, "/api/projects/"+project.ID+"/devices", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET devices status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listBody struct {
		Devices []devicedomain.Device `json:"devices"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listBody)
	if len(listBody.Devices) != 1 || listBody.Devices[0].ID != "avd:Pixel_7" {
		t.Fatalf("unexpected devices list: %+v", listBody.Devices)
	}

	// Missing Origin rejected by requireProcessMutationBoundary
	unauthRec := doReq(http.MethodPost, "/api/projects/"+project.ID+"/devices/avd:Pixel_7/actions", `{"kind":"home"}`, "")
	if unauthRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for missing Origin, got %d body=%s", unauthRec.Code, unauthRec.Body.String())
	}

	// Valid loopback Origin action succeeds
	actRec := doReq(http.MethodPost, "/api/projects/"+project.ID+"/devices/avd:Pixel_7/actions", `{"kind":"tap","x":120,"y":340}`, baseOrigin)
	if actRec.Code != http.StatusOK {
		t.Fatalf("expected 200 action, got %d body=%s", actRec.Code, actRec.Body.String())
	}
	if !runner.hasCall("adb -s emulator-5554 shell input tap 120 340") {
		t.Fatal("expected tap command to reach device runner")
	}

	// Screenshot & logs & adb exec
	shotRec := doReq(http.MethodGet, "/api/projects/"+project.ID+"/devices/avd:Pixel_7/screenshot", "", "")
	if shotRec.Code != http.StatusOK || shotRec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("unexpected screenshot response: status=%d ct=%s", shotRec.Code, shotRec.Header().Get("Content-Type"))
	}

	logsRec := doReq(http.MethodGet, "/api/projects/"+project.ID+"/devices/avd:Pixel_7/logs?lines=25", "", "")
	if logsRec.Code != http.StatusOK {
		t.Fatalf("unexpected logs response: status=%d body=%s", logsRec.Code, logsRec.Body.String())
	}
	var logsBody struct {
		Logs string `json:"logs"`
	}
	_ = json.Unmarshal(logsRec.Body.Bytes(), &logsBody)
	if !strings.Contains(logsBody.Logs, "I/Test: ready") {
		t.Fatalf("unexpected logs payload: %q", logsBody.Logs)
	}

	adbRec := doReq(http.MethodPost, "/api/projects/"+project.ID+"/devices/avd:Pixel_7/adb", `{"args":["shell","id"]}`, baseOrigin)
	if adbRec.Code != http.StatusOK {
		t.Fatalf("unexpected adb response: status=%d body=%s", adbRec.Code, adbRec.Body.String())
	}
	var adbBody devicedomain.AndroidADBExecResult
	_ = json.Unmarshal(adbRec.Body.Bytes(), &adbBody)
	if !strings.Contains(adbBody.Stdout, "-s emulator-5554 shell 'id'") {
		t.Fatalf("unexpected adb stdout: %+v", adbBody)
	}
}

type hijackRecorder struct {
	*httptest.ResponseRecorder
	serverConn net.Conn
	hijacked   bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	rw := bufio.NewReadWriter(bufio.NewReader(h.serverConn), bufio.NewWriter(h.serverConn))
	return h.serverConn, rw, nil
}

func TestDevicesWebSocketStreamTouchAndHostCheck(t *testing.T) {
	runner := &stubDeviceRunner{}
	a, project := newTestDeviceApp(t, runner)
	handler := withRecovery(a.httpHandler())
	baseOrigin := "http://127.0.0.1:43123"
	host := "127.0.0.1:43123"

	// Non-loopback Host header must be rejected
	reqBadHost := httptest.NewRequest(http.MethodGet, baseOrigin+"/api/projects/"+project.ID+"/devices/avd:Pixel_7/ws", nil)
	reqBadHost.Host = "evil.example:8080"
	reqBadHost.Header.Set("Connection", "Upgrade")
	reqBadHost.Header.Set("Upgrade", "websocket")
	reqBadHost.Header.Set("Sec-WebSocket-Version", "13")
	reqBadHost.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	badRec := httptest.NewRecorder()
	handler.ServeHTTP(badRec, reqBadHost)
	if badRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback Host, got %d body=%s", badRec.Code, badRec.Body.String())
	}

	// Real WebSocket handshake via recoveryWriter.Hijack() over in-memory net.Pipe()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wsKey := "dGhlIHNhbXBsZSBub25jZQ=="
	wsReq := httptest.NewRequest(http.MethodGet, baseOrigin+"/api/projects/"+project.ID+"/devices/avd:Pixel_7/ws", nil).WithContext(ctx)
	wsReq.Host = host
	wsReq.Header.Set("Origin", baseOrigin)
	wsReq.Header.Set("Connection", "Upgrade")
	wsReq.Header.Set("Upgrade", "websocket")
	wsReq.Header.Set("Sec-WebSocket-Version", "13")
	wsReq.Header.Set("Sec-WebSocket-Key", wsKey)

	hijackRec := &hijackRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		serverConn:       serverConn,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(hijackRec, wsReq)
	}()

	br := bufio.NewReader(clientConn)
	var statusLine string
	var acceptLine string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read handshake: %v", err)
		}
		if statusLine == "" {
			statusLine = line
		}
		if strings.HasPrefix(line, "Sec-WebSocket-Accept:") {
			acceptLine = strings.TrimSpace(strings.TrimPrefix(line, "Sec-WebSocket-Accept:"))
		}
		if line == "\r\n" {
			break
		}
	}
	if !strings.Contains(statusLine, "101 Switching Protocols") {
		t.Fatalf("unexpected handshake status: %q", statusLine)
	}
	expectedSum := sha1.Sum([]byte(wsKey + wsGUID))
	expectedAccept := base64.StdEncoding.EncodeToString(expectedSum[:])
	if acceptLine != expectedAccept {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", acceptLine, expectedAccept)
	}

	readServerFrame := func() (byte, []byte, error) {
		var hdr [2]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return 0, nil, err
		}
		opcode := hdr[0] & 0x0f
		length := uint64(hdr[1] & 0x7f)
		if length == 126 {
			var ext [2]byte
			_, _ = io.ReadFull(br, ext[:])
			length = uint64(binary.BigEndian.Uint16(ext[:]))
		}
		payload := make([]byte, length)
		_, err := io.ReadFull(br, payload)
		return opcode, payload, err
	}

	writeMaskedClientText := func(text string) error {
		payload := []byte(text)
		mask := [4]byte{0x12, 0x34, 0x56, 0x78}
		var out bytes.Buffer
		out.WriteByte(0x81)
		out.WriteByte(0x80 | byte(len(payload)))
		out.Write(mask[:])
		for i, b := range payload {
			out.WriteByte(b ^ mask[i%4])
		}
		_, err := clientConn.Write(out.Bytes())
		return err
	}

	gotPNG := false
	for i := 0; i < 4; i++ {
		op, payload, err := readServerFrame()
		if err != nil {
			t.Fatalf("readServerFrame %d failed: %v", i, err)
		}
		if op == 0x2 && bytes.HasPrefix(payload, []byte{0x89, 'P', 'N', 'G'}) {
			gotPNG = true
			break
		}
	}
	if !gotPNG {
		t.Fatal("expected binary PNG frame on device websocket")
	}

	if err := writeMaskedClientText(`{"type":"touch","phase":"down","x":120,"y":340,"viewport_width":1080,"viewport_height":1920}`); err != nil {
		t.Fatalf("writeMaskedClientText touch failed: %v", err)
	}
	if err := writeMaskedClientText(`{"type":"ping"}`); err != nil {
		t.Fatalf("writeMaskedClientText ping failed: %v", err)
	}
	gotPong := false
	for i := 0; i < 6; i++ {
		op, payload, err := readServerFrame()
		if err != nil {
			t.Fatalf("readServerFrame pong failed: %v", err)
		}
		if op == 0x1 && strings.Contains(string(payload), `"pong"`) {
			gotPong = true
			break
		}
	}
	if !gotPong {
		t.Fatal("expected pong JSON frame")
	}
	if !runner.hasCall("adb -s emulator-5554 shell input tap 120 340") {
		t.Fatal("expected websocket touch to inject adb input tap")
	}
	_ = clientConn.Close()
	<-done
}
