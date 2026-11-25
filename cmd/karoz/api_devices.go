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
	"strconv"
	"strings"
	"sync"
	"time"

	devicedomain "github.com/karoz/karoz/internal/device"
)

const (
	wsGUID               = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxInboundPayload  = 1 << 20
	wsScreenshotInterval = 250 * time.Millisecond
)

func (a *app) handleDevices(w http.ResponseWriter, r *http.Request, project Project, parts []string) {
	mgr := a.devices()
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			devices, err := mgr.ListDevices(r.Context(), project.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, map[string]any{"devices": devices})
		case http.MethodPost:
			var req devicedomain.StartRequest
			if err := readJSON(r, &req); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			dev, err := mgr.StartDevice(r.Context(), project.ID, req)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, dev)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	deviceID := strings.TrimSpace(parts[0])
	if deviceID == "" {
		http.NotFound(w, r)
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			dev, err := mgr.GetDevice(r.Context(), project.ID, deviceID)
			if err != nil {
				writeError(w, http.StatusNotFound, err)
				return
			}
			writeJSON(w, dev)
		case http.MethodDelete:
			if err := mgr.StopDevice(r.Context(), project.ID, deviceID); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, map[string]any{"stopped": true, "device_id": deviceID})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}

	switch parts[1] {
	case "start":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req devicedomain.StartRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(req.DeviceID) == "" {
			req.DeviceID = deviceID
		}
		dev, err := mgr.StartDevice(r.Context(), project.ID, req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, dev)

	case "stop":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := mgr.StopDevice(r.Context(), project.ID, deviceID); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"stopped": true, "device_id": deviceID})

	case "screenshot":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		pngBytes, err := mgr.Screenshot(r.Context(), project.ID, deviceID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(pngBytes)

	case "logs":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		lines := 120
		if raw := strings.TrimSpace(r.URL.Query().Get("lines")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				lines = n
			}
		}
		logs, err := mgr.Logs(r.Context(), project.ID, deviceID, lines)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"device_id": deviceID, "logs": logs})

	case "actions":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req devicedomain.ActionRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := mgr.PerformAction(r.Context(), project.ID, deviceID, req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "device_id": deviceID, "kind": req.Kind})

	case "adb":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req devicedomain.AndroidADBExecRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		res, err := mgr.ExecADB(r.Context(), project.ID, deviceID, req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, res)

	case "ws":
		a.handleDeviceWebSocket(w, r, project, deviceID)

	default:
		http.NotFound(w, r)
	}
}

func (a *app) handleDeviceWebSocket(w http.ResponseWriter, r *http.Request, project Project, deviceID string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackHostHeader(r.Host) {
		writeError(w, http.StatusForbidden, errors.New("device websocket requires a loopback Host header"))
		return
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		writeError(w, http.StatusForbidden, errors.New("cross-site device websocket connections are not allowed"))
		return
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		if !loopbackProcessOrigin(origin) || !requestOriginMatchesRequest(origin, r) {
			writeError(w, http.StatusForbidden, errors.New("device websocket Origin does not match local Studio"))
			return
		}
	}
	wsKey := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if !headerContainsToken(r.Header.Get("Connection"), "upgrade") ||
		!strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") ||
		strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")) != "13" ||
		wsKey == "" {
		writeError(w, http.StatusBadRequest, errors.New("invalid websocket upgrade request"))
		return
	}

	mgr := a.devices()
	dev, err := mgr.GetDevice(r.Context(), project.ID, deviceID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("websocket hijack failed: %w", err))
		return
	}
	defer conn.Close()

	sum := sha1.Sum([]byte(wsKey + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	handshake := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(handshake); err != nil {
		return
	}
	if err := rw.Flush(); err != nil {
		return
	}

	ws := &rawDeviceWSConn{conn: conn, r: rw.Reader}
	_ = ws.writeJSON(map[string]any{
		"type":   "device",
		"device": dev,
		"width":  dev.Width,
		"height": dev.Height,
	})
	_ = ws.writeJSON(map[string]any{
		"type":   "video_config",
		"codec":  "png",
		"width":  dev.Width,
		"height": dev.Height,
	})

	wsCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	refreshCh := make(chan struct{}, 1)
	triggerRefresh := func() {
		select {
		case refreshCh <- struct{}{}:
		default:
		}
	}
	triggerRefresh()

	go func() {
		ticker := time.NewTicker(wsScreenshotInterval)
		defer ticker.Stop()
		var lastFrame []byte
		sendFrame := func() {
			shotCtx, shotCancel := context.WithTimeout(wsCtx, 3*time.Second)
			pngBytes, err := mgr.Screenshot(shotCtx, project.ID, dev.ID)
			shotCancel()
			if err != nil || len(pngBytes) == 0 {
				return
			}
			if bytes.Equal(pngBytes, lastFrame) {
				return
			}
			lastFrame = append(lastFrame[:0], pngBytes...)
			if err := ws.writeBinary(pngBytes); err != nil {
				cancel()
			}
		}
		for {
			select {
			case <-wsCtx.Done():
				return
			case <-refreshCh:
				sendFrame()
			case <-ticker.C:
				sendFrame()
			}
		}
	}()

	type inboundEnvelope struct {
		Type           string                      `json:"type"`
		Phase          string                      `json:"phase,omitempty"`
		X              int                         `json:"x,omitempty"`
		Y              int                         `json:"y,omitempty"`
		ViewportWidth  int                         `json:"viewport_width,omitempty"`
		ViewportHeight int                         `json:"viewport_height,omitempty"`
		Action         *devicedomain.ActionRequest `json:"action,omitempty"`
	}

	for {
		opcode, payload, err := ws.readFrame()
		if err != nil {
			return
		}
		switch opcode {
		case 0x8: // Close
			_ = ws.writeControl(0x8, nil)
			return
		case 0x9: // Ping
			if err := ws.writeControl(0xA, payload); err != nil {
				return
			}
		case 0x1: // Text
			var msg inboundEnvelope
			if err := json.Unmarshal(payload, &msg); err != nil {
				_ = ws.writeJSON(map[string]any{"type": "error", "error": "invalid JSON control message"})
				continue
			}
			switch strings.ToLower(strings.TrimSpace(msg.Type)) {
			case "ping":
				_ = ws.writeJSON(map[string]any{"type": "pong"})
			case "touch":
				err := mgr.InjectTouch(wsCtx, project.ID, dev.ID, devicedomain.TouchEvent{
					Phase:          msg.Phase,
					X:              msg.X,
					Y:              msg.Y,
					ViewportWidth:  msg.ViewportWidth,
					ViewportHeight: msg.ViewportHeight,
				})
				if err != nil {
					_ = ws.writeJSON(map[string]any{"type": "error", "error": err.Error()})
					continue
				}
				triggerRefresh()
			case "action":
				if msg.Action == nil {
					_ = ws.writeJSON(map[string]any{"type": "error", "error": "missing action payload"})
					continue
				}
				if err := mgr.PerformAction(wsCtx, project.ID, dev.ID, *msg.Action); err != nil {
					_ = ws.writeJSON(map[string]any{"type": "error", "error": err.Error()})
					continue
				}
				triggerRefresh()
			}
		}
	}
}

func headerContainsToken(header, want string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), want) {
			return true
		}
	}
	return false
}

type rawDeviceWSConn struct {
	conn net.Conn
	r    *bufio.Reader
	mu   sync.Mutex
}

func (c *rawDeviceWSConn) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeFrame(0x1, data)
}

func (c *rawDeviceWSConn) writeBinary(data []byte) error {
	return c.writeFrame(0x2, data)
}

func (c *rawDeviceWSConn) writeControl(opcode byte, payload []byte) error {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	return c.writeFrame(opcode, payload)
}

func (c *rawDeviceWSConn) writeFrame(opcode byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var hdr [10]byte
	hdr[0] = 0x80 | (opcode & 0x0f)
	hdrLen := 2
	n := len(payload)
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 0xffff:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hdrLen = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hdrLen = 10
	}
	if _, err := c.conn.Write(hdr[:hdrLen]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := c.conn.Write(payload)
		return err
	}
	return nil
}

func (c *rawDeviceWSConn) readFrame() (byte, []byte, error) {
	var msgBuf bytes.Buffer
	var msgOpcode byte
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
			return 0, nil, err
		}
		fin := hdr[0]&0x80 != 0
		opcode := hdr[0] & 0x0f
		masked := hdr[1]&0x80 != 0
		length := uint64(hdr[1] & 0x7f)

		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.r, ext[:]); err != nil {
				return 0, nil, err
			}
			length = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.r, ext[:]); err != nil {
				return 0, nil, err
			}
			length = binary.BigEndian.Uint64(ext[:])
		}
		if length > wsMaxInboundPayload || uint64(msgBuf.Len())+length > wsMaxInboundPayload {
			return 0, nil, errors.New("websocket frame exceeds limit")
		}
		var maskKey [4]byte
		if masked {
			if _, err := io.ReadFull(c.r, maskKey[:]); err != nil {
				return 0, nil, err
			}
		}
		payload := make([]byte, length)
		if length > 0 {
			if _, err := io.ReadFull(c.r, payload); err != nil {
				return 0, nil, err
			}
			if masked {
				for i := range payload {
					payload[i] ^= maskKey[i%4]
				}
			}
		}
		if opcode >= 0x8 {
			return opcode, payload, nil
		}
		if opcode != 0 {
			msgOpcode = opcode
		}
		msgBuf.Write(payload)
		if fin {
			return msgOpcode, msgBuf.Bytes(), nil
		}
	}
}
