package device

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultDeviceWidth  = 1080
	defaultDeviceHeight = 1920
	redroidADBPort      = "5555"
	redroidDefaultImage = "redroid/redroid:12.0.0-latest"
)

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// CommandRunner abstracts host CLI invocations (`adb`, `emulator`, `docker`) so
// unit tests can exercise device discovery and control without hardware.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	Start(name string, args ...string) (ProcessHandle, error)
}

type ProcessHandle interface {
	Kill() error
	Wait() error
}

type execProcessHandle struct {
	cmd *exec.Cmd
}

func (p *execProcessHandle) Kill() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func (p *execProcessHandle) Wait() error {
	if p == nil || p.cmd == nil {
		return nil
	}
	return p.cmd.Wait()
}

type osCommandRunner struct{}

func (osCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.Bytes(), fmt.Errorf("%s: %w", msg, err)
		}
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}

func (osCommandRunner) Start(name string, args ...string) (ProcessHandle, error) {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcessHandle{cmd: cmd}, nil
}

type adbExecFunc func(ctx context.Context, args []string, limit int) (*AndroidADBExecResult, error)

type Manager struct {
	mu                  sync.Mutex
	runner              CommandRunner
	execADB             adbExecFunc
	touchSessions       map[string]*TouchSession
	touchSessionFactory func(ctx context.Context, adbTarget string) (*TouchSession, error)
	scrcpyTouch         map[string]*ScrcpyTouchInjector
	startedAVDs         map[string]ProcessHandle
	devicesByID         map[string]Device
}

func NewManager(opts ...func(*Manager)) *Manager {
	m := &Manager{
		runner:        osCommandRunner{},
		execADB:       RunBoundedADBCommand,
		touchSessions: make(map[string]*TouchSession),
		scrcpyTouch:   make(map[string]*ScrcpyTouchInjector),
		startedAVDs:   make(map[string]ProcessHandle),
		devicesByID:   make(map[string]Device),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	if m.touchSessionFactory == nil {
		if _, isDefault := m.runner.(osCommandRunner); isDefault {
			m.touchSessionFactory = NewTouchSession
		}
	}
	return m
}

func WithCommandRunner(runner CommandRunner) func(*Manager) {
	return func(m *Manager) {
		if runner != nil {
			m.runner = runner
		}
	}
}

func WithTouchSessionFactory(fn func(ctx context.Context, adbTarget string) (*TouchSession, error)) func(*Manager) {
	return func(m *Manager) {
		m.touchSessionFactory = fn
	}
}

func WithADBExec(fn func(ctx context.Context, args []string, limit int) (*AndroidADBExecResult, error)) func(*Manager) {
	return func(m *Manager) {
		if fn != nil {
			m.execADB = fn
		}
	}
}

// ListDevices discovers connected host ADB devices, local AVDs (`emulator -list-avds`),
// and optional Docker redroid containers labeled for this project.
func (m *Manager) ListDevices(ctx context.Context, projectID string) ([]Device, error) {
	now := time.Now().UTC()
	out := make([]Device, 0)
	seen := make(map[string]bool)

	if adbOut, err := m.runner.Run(ctx, "adb", "devices", "-l"); err == nil {
		for _, dev := range m.parseADBDevices(ctx, projectID, string(adbOut), now) {
			seen[dev.ID] = true
			if dev.AVDName != "" {
				seen["avd:"+dev.AVDName] = true
			}
			m.saveDevice(dev)
			out = append(out, dev)
		}
	}

	if avdOut, err := m.runner.Run(ctx, "emulator", "-list-avds"); err == nil {
		for _, line := range strings.Split(string(avdOut), "\n") {
			avd := strings.TrimSpace(line)
			if avd == "" {
				continue
			}
			id := "avd:" + avd
			if seen[id] {
				continue
			}
			seen[id] = true
			status := StatusStopped
			m.mu.Lock()
			_, running := m.startedAVDs[avd]
			m.mu.Unlock()
			if running {
				status = StatusStarting
			}
			dev := Device{
				ID:        id,
				ProjectID: projectID,
				Name:      avd,
				AVDName:   avd,
				Backend:   BackendAVD,
				Status:    status,
				Width:     defaultDeviceWidth,
				Height:    defaultDeviceHeight,
				UpdatedAt: now,
			}
			m.saveDevice(dev)
			out = append(out, dev)
		}
	}

	if redroids := m.discoverRedroidContainers(ctx, projectID, now); len(redroids) > 0 {
		for _, dev := range redroids {
			if seen[dev.ID] {
				continue
			}
			seen[dev.ID] = true
			m.saveDevice(dev)
			out = append(out, dev)
		}
	}

	m.mu.Lock()
	for id, dev := range m.devicesByID {
		if seen[id] {
			continue
		}
		if dev.ProjectID != "" && projectID != "" && dev.ProjectID != projectID {
			continue
		}
		if dev.Status == StatusStarting || dev.Status == StatusReady || dev.Status == StatusRunning {
			out = append(out, dev)
		}
	}
	m.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Backend != out[j].Backend {
			return out[i].Backend < out[j].Backend
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (m *Manager) GetDevice(ctx context.Context, projectID, deviceID string) (Device, error) {
	devices, err := m.ListDevices(ctx, projectID)
	if err != nil {
		return Device{}, err
	}
	for _, dev := range devices {
		if dev.ID == deviceID || dev.Serial == deviceID || dev.AVDName == deviceID {
			return dev, nil
		}
	}
	return Device{}, fmt.Errorf("device %q not found", deviceID)
}

func (m *Manager) StartDevice(ctx context.Context, projectID string, req StartRequest) (Device, error) {
	now := time.Now().UTC()
	backend := req.Backend
	if backend == "" {
		switch {
		case strings.TrimSpace(req.Serial) != "":
			backend = BackendADBHost
		case strings.TrimSpace(req.AVDName) != "" || strings.HasPrefix(req.DeviceID, "avd:"):
			backend = BackendAVD
		case strings.HasPrefix(req.DeviceID, "host:"):
			backend = BackendADBHost
		default:
			backend = BackendAVD
		}
	}

	switch backend {
	case BackendADBHost:
		serial := strings.TrimSpace(req.Serial)
		if serial == "" && strings.HasPrefix(req.DeviceID, "host:") {
			serial = strings.TrimPrefix(req.DeviceID, "host:")
		}
		if serial == "" {
			return Device{}, fmt.Errorf("adb_host requires device serial")
		}
		if strings.Contains(serial, ":") {
			if _, err := m.runner.Run(ctx, "adb", "connect", serial); err != nil {
				return Device{}, fmt.Errorf("adb connect %s: %w", serial, err)
			}
		}
		return m.GetDevice(ctx, projectID, "host:"+serial)

	case BackendAVD:
		avdName := strings.TrimSpace(req.AVDName)
		if avdName == "" && strings.HasPrefix(req.DeviceID, "avd:") {
			avdName = strings.TrimPrefix(req.DeviceID, "avd:")
		}
		if avdName == "" {
			avdsOut, err := m.runner.Run(ctx, "emulator", "-list-avds")
			if err != nil {
				return Device{}, fmt.Errorf("list avds: %w", err)
			}
			for _, line := range strings.Split(string(avdsOut), "\n") {
				if trimmed := strings.TrimSpace(line); trimmed != "" {
					avdName = trimmed
					break
				}
			}
		}
		if avdName == "" {
			return Device{}, fmt.Errorf("no Android AVD found; create an AVD in Android Studio or connect a physical device over ADB")
		}
		args := []string{"-avd", avdName}
		if req.Headless {
			args = append(args, "-no-window", "-no-audio")
		}
		proc, err := m.runner.Start("emulator", args...)
		if err != nil {
			return Device{}, fmt.Errorf("start emulator %s: %w", avdName, err)
		}
		m.mu.Lock()
		m.startedAVDs[avdName] = proc
		dev := Device{
			ID:        "avd:" + avdName,
			ProjectID: projectID,
			Name:      nonEmpty(req.Name, avdName),
			AVDName:   avdName,
			Backend:   BackendAVD,
			Status:    StatusStarting,
			Width:     defaultDeviceWidth,
			Height:    defaultDeviceHeight,
			UpdatedAt: now,
		}
		m.devicesByID[dev.ID] = dev
		m.mu.Unlock()
		return dev, nil

	case BackendRedroid:
		name := sanitizeContainerSuffix(nonEmpty(req.Name, "android"))
		containerName := fmt.Sprintf("karoz-redroid-%s-%s", sanitizeContainerSuffix(projectID), name)
		image := nonEmpty(req.Image, redroidDefaultImage)
		args := []string{
			"run", "-d", "--privileged",
			"--name", containerName,
			"--label", "karoz.kind=android-device",
			"--label", "karoz.project_id=" + projectID,
			"--label", "karoz.device_name=" + name,
			image,
			"androidboot.redroid_gpu_mode=guest",
			"androidboot.use_redroid_stream=1",
			"androidboot.redroid_virtual_input=1",
		}
		out, err := m.runner.Run(ctx, "docker", args...)
		if err != nil {
			return Device{}, fmt.Errorf("start redroid container: %w", err)
		}
		containerID := strings.TrimSpace(string(out))
		if containerID == "" {
			containerID = containerName
		}
		target, _ := m.inspectProjectRedroidTarget(ctx, projectID, containerID)
		status := StatusStarting
		if target != "" {
			if _, connErr := m.runner.Run(ctx, "adb", "connect", target); connErr == nil {
				status = StatusReady
			}
		}
		dev := Device{
			ID:          "redroid:" + containerName,
			ProjectID:   projectID,
			Name:        nonEmpty(req.Name, containerName),
			Serial:      target,
			ContainerID: containerID,
			Backend:     BackendRedroid,
			Status:      status,
			Width:       defaultDeviceWidth,
			Height:      defaultDeviceHeight,
			UpdatedAt:   now,
		}
		m.saveDevice(dev)
		return dev, nil

	default:
		return Device{}, fmt.Errorf("unsupported device backend %q", backend)
	}
}

func (m *Manager) StopDevice(ctx context.Context, projectID, deviceID string) error {
	dev, err := m.GetDevice(ctx, projectID, deviceID)
	if err != nil {
		return err
	}
	if dev.Serial != "" {
		m.clearTouchSession(dev.Serial)
	}
	switch dev.Backend {
	case BackendRedroid:
		if _, err := m.inspectProjectRedroidTarget(ctx, projectID, dev.ContainerID); err != nil && !errors.Is(err, errContainerStopped) {
			return err
		}
		if _, err := m.runner.Run(ctx, "docker", "rm", "-f", dev.ContainerID); err != nil {
			return fmt.Errorf("remove redroid container: %w", err)
		}
	case BackendAVD:
		if dev.Serial != "" {
			_, _ = m.runner.Run(ctx, "adb", "-s", dev.Serial, "emu", "kill")
		}
		m.mu.Lock()
		if proc := m.startedAVDs[dev.AVDName]; proc != nil {
			_ = proc.Kill()
			delete(m.startedAVDs, dev.AVDName)
		}
		m.mu.Unlock()
	case BackendADBHost:
		if strings.Contains(dev.Serial, ":") {
			_, _ = m.runner.Run(ctx, "adb", "disconnect", dev.Serial)
		}
	}
	m.mu.Lock()
	dev.Status = StatusStopped
	dev.UpdatedAt = time.Now().UTC()
	m.devicesByID[dev.ID] = dev
	m.mu.Unlock()
	return nil
}

func (m *Manager) Screenshot(ctx context.Context, projectID, deviceID string) ([]byte, error) {
	serial, err := m.resolveSerial(ctx, projectID, deviceID)
	if err != nil {
		return nil, err
	}
	out, err := m.runner.Run(ctx, "adb", "-s", serial, "exec-out", "screencap", "-p")
	if err != nil {
		return nil, err
	}
	if len(out) < len(pngSignature) {
		return nil, fmt.Errorf("empty or truncated screenshot")
	}
	// Normalize accidental CRLF mangling only if needed; normal exec-out emits raw PNG.
	if !bytes.HasPrefix(out, pngSignature) {
		return nil, fmt.Errorf("screenshot output is not a valid PNG")
	}
	return out, nil
}

func (m *Manager) Logs(ctx context.Context, projectID, deviceID string, lines int) (string, error) {
	serial, err := m.resolveSerial(ctx, projectID, deviceID)
	if err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 120
	}
	if lines > 2000 {
		lines = 2000
	}
	out, err := m.runner.Run(ctx, "adb", "-s", serial, "logcat", "-d", "-t", strconv.Itoa(lines))
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (m *Manager) PerformAction(ctx context.Context, projectID, deviceID string, req ActionRequest) error {
	args, wait, err := adbCommandForAction(req)
	if err != nil {
		return err
	}
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	serial, err := m.resolveSerial(ctx, projectID, deviceID)
	if err != nil {
		return err
	}
	full := append([]string{"-s", serial}, args...)
	_, err = m.runner.Run(ctx, "adb", full...)
	return err
}

func (m *Manager) InjectTouch(ctx context.Context, projectID, deviceID string, ev TouchEvent) error {
	dev, err := m.GetDevice(ctx, projectID, deviceID)
	if err != nil {
		return err
	}
	if dev.Serial == "" {
		return fmt.Errorf("device %q has no active ADB serial", dev.ID)
	}
	phase := strings.ToLower(strings.TrimSpace(ev.Phase))
	x, y := clampDeviceCoords(ev.X, ev.Y, ev.ViewportWidth, ev.ViewportHeight, dev.Width, dev.Height)

	if inj := m.scrcpyTouchInjector(dev.Serial); inj != nil {
		var injErr error
		switch phase {
		case "down", "move", "up":
			injErr = inj.Inject(ctx, phase, x, y, dev.Width, dev.Height)
		case "tap":
			if injErr = inj.Inject(ctx, "down", x, y, dev.Width, dev.Height); injErr == nil {
				injErr = inj.Inject(ctx, "up", x, y, dev.Width, dev.Height)
			}
		default:
			return fmt.Errorf("unsupported touch phase %q", ev.Phase)
		}
		if injErr == nil {
			return nil
		}
		m.ClearScrcpyTouchInjector(dev.Serial, inj)
	}

	switch phase {
	case "down", "tap":
		if session := m.getOrCreateTouchSession(ctx, dev.Serial); session != nil {
			if err := session.Tap(ctx, x, y); err == nil {
				return nil
			}
			m.clearTouchSession(dev.Serial)
		}
		return m.PerformAction(ctx, projectID, dev.ID, ActionRequest{Kind: "tap", X: &x, Y: &y})
	case "up", "move":
		return nil
	default:
		return fmt.Errorf("unsupported touch phase %q", ev.Phase)
	}
}

func (m *Manager) SetScrcpyTouchInjector(serial string, inj *ScrcpyTouchInjector) {
	key := strings.TrimSpace(serial)
	if key == "" {
		return
	}
	m.mu.Lock()
	prev := m.scrcpyTouch[key]
	if inj == nil {
		delete(m.scrcpyTouch, key)
	} else {
		m.scrcpyTouch[key] = inj
	}
	m.mu.Unlock()
	if prev != nil && prev != inj {
		_ = prev.Close()
	}
}

func (m *Manager) ClearScrcpyTouchInjector(serial string, inj *ScrcpyTouchInjector) {
	key := strings.TrimSpace(serial)
	if key == "" {
		return
	}
	m.mu.Lock()
	current := m.scrcpyTouch[key]
	if current == nil || (inj != nil && current != inj) {
		m.mu.Unlock()
		return
	}
	delete(m.scrcpyTouch, key)
	m.mu.Unlock()
	_ = current.Close()
}

func (m *Manager) scrcpyTouchInjector(serial string) *ScrcpyTouchInjector {
	key := strings.TrimSpace(serial)
	if key == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scrcpyTouch[key]
}

func (m *Manager) getOrCreateTouchSession(ctx context.Context, serial string) *TouchSession {
	key := strings.TrimSpace(serial)
	if key == "" {
		return nil
	}
	m.mu.Lock()
	session := m.touchSessions[key]
	factory := m.touchSessionFactory
	m.mu.Unlock()
	if session != nil {
		return session
	}
	if factory == nil {
		return nil
	}
	created, err := factory(ctx, key)
	if err != nil || created == nil {
		return nil
	}
	m.mu.Lock()
	if existing := m.touchSessions[key]; existing != nil {
		m.mu.Unlock()
		_ = created.Close()
		return existing
	}
	m.touchSessions[key] = created
	m.mu.Unlock()
	return created
}

func (m *Manager) ExecADB(ctx context.Context, projectID, deviceID string, req AndroidADBExecRequest) (*AndroidADBExecResult, error) {
	if err := ValidateAndroidADBExecRequest(req); err != nil {
		return nil, err
	}
	serial, err := m.resolveSerial(ctx, projectID, deviceID)
	if err != nil {
		return nil, err
	}
	seconds := req.TimeoutSeconds
	if seconds == 0 {
		seconds = 30
	}
	limit := req.MaxOutputBytes
	if limit == 0 {
		limit = AndroidADBDefaultOutputBytes
	}
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	return m.execADB(execCtx, ScopedADBArguments(serial, req.Args), limit)
}

func (m *Manager) Close() {
	m.mu.Lock()
	sessions := make([]*TouchSession, 0, len(m.touchSessions))
	for serial, session := range m.touchSessions {
		sessions = append(sessions, session)
		delete(m.touchSessions, serial)
	}
	injectors := make([]*ScrcpyTouchInjector, 0, len(m.scrcpyTouch))
	for serial, inj := range m.scrcpyTouch {
		injectors = append(injectors, inj)
		delete(m.scrcpyTouch, serial)
	}
	m.mu.Unlock()
	for _, session := range sessions {
		_ = session.Close()
	}
	for _, inj := range injectors {
		_ = inj.Close()
	}
}

func (m *Manager) resolveSerial(ctx context.Context, projectID, deviceID string) (string, error) {
	dev, err := m.GetDevice(ctx, projectID, deviceID)
	if err != nil {
		return "", err
	}
	if dev.Serial == "" {
		return "", fmt.Errorf("device %q is not online (status=%s)", dev.ID, dev.Status)
	}
	if dev.Status != StatusReady && dev.Status != StatusRunning {
		return "", fmt.Errorf("device %q is %s", dev.ID, dev.Status)
	}
	return dev.Serial, nil
}

func (m *Manager) clearTouchSession(serial string) {
	key := strings.TrimSpace(serial)
	m.mu.Lock()
	session := m.touchSessions[key]
	delete(m.touchSessions, key)
	inj := m.scrcpyTouch[key]
	delete(m.scrcpyTouch, key)
	m.mu.Unlock()
	if session != nil {
		_ = session.Close()
	}
	if inj != nil {
		_ = inj.Close()
	}
}

func (m *Manager) saveDevice(dev Device) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.devicesByID[dev.ID] = dev
}

func (m *Manager) parseADBDevices(ctx context.Context, projectID, raw string, now time.Time) []Device {
	var out []Device
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		serial := fields[0]
		adbState := fields[1]
		status := StatusReady
		if adbState != "device" {
			status = StatusOffline
		}
		modelName := ""
		for _, field := range fields[2:] {
			if strings.HasPrefix(field, "model:") {
				modelName = strings.ReplaceAll(strings.TrimPrefix(field, "model:"), "_", " ")
			}
		}
		backend := BackendADBHost
		avdName := ""
		if strings.HasPrefix(serial, "emulator-") && status == StatusReady {
			backend = BackendAVD
			if avdRaw, err := m.runner.Run(ctx, "adb", "-s", serial, "emu", "avd", "name"); err == nil {
				avdName = firstNonEmptyLine(string(avdRaw))
			}
		}
		width, height := defaultDeviceWidth, defaultDeviceHeight
		if status == StatusReady {
			if sizeOut, err := m.runner.Run(ctx, "adb", "-s", serial, "shell", "wm", "size"); err == nil {
				width, height = ParseWMSize(string(sizeOut))
			}
		}
		id := "host:" + serial
		if avdName != "" {
			id = "avd:" + avdName
		}
		out = append(out, Device{
			ID:        id,
			ProjectID: projectID,
			Name:      nonEmpty(avdName, nonEmpty(modelName, serial)),
			Serial:    serial,
			AVDName:   avdName,
			Backend:   backend,
			Status:    status,
			Width:     width,
			Height:    height,
			Model:     modelName,
			UpdatedAt: now,
		})
	}
	return out
}

func (m *Manager) discoverRedroidContainers(ctx context.Context, projectID string, now time.Time) []Device {
	if projectID == "" {
		return nil
	}
	out, err := m.runner.Run(ctx, "docker", "ps", "-a",
		"--filter", "label=karoz.kind=android-device",
		"--filter", "label=karoz.project_id="+projectID,
		"--format", "{{.Names}}",
	)
	if err != nil {
		return nil
	}
	var devices []Device
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		target, err := m.inspectProjectRedroidTarget(ctx, projectID, name)
		status := StatusReady
		if err != nil {
			if errors.Is(err, errContainerStopped) {
				status = StatusStopped
			} else {
				continue
			}
		}
		devices = append(devices, Device{
			ID:          "redroid:" + name,
			ProjectID:   projectID,
			Name:        name,
			Serial:      target,
			ContainerID: name,
			Backend:     BackendRedroid,
			Status:      status,
			Width:       defaultDeviceWidth,
			Height:      defaultDeviceHeight,
			UpdatedAt:   now,
		})
	}
	return devices
}

var errContainerStopped = errors.New("redroid container stopped")

func (m *Manager) inspectProjectRedroidTarget(ctx context.Context, projectID, containerID string) (string, error) {
	if strings.TrimSpace(containerID) == "" {
		return "", fmt.Errorf("container id is required")
	}
	raw, err := m.runner.Run(ctx, "docker", "inspect", "--format", "{{json .}}", "--", containerID)
	if err != nil {
		return "", err
	}
	var info struct {
		State           struct{ Running bool }
		Config          struct{ Labels map[string]string }
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", fmt.Errorf("invalid container metadata: %w", err)
	}
	if info.Config.Labels["karoz.kind"] != "android-device" || info.Config.Labels["karoz.project_id"] != projectID {
		return "", fmt.Errorf("container is not an android device owned by project %s", projectID)
	}
	if !info.State.Running {
		return "", errContainerStopped
	}
	for _, netInfo := range info.NetworkSettings.Networks {
		if ip := net.ParseIP(netInfo.IPAddress); ip != nil && ip.To4() != nil {
			return ip.String() + ":" + redroidADBPort, nil
		}
	}
	return "", fmt.Errorf("redroid container has no IPv4 address")
}

func clampDeviceCoords(x, y, vw, vh, dw, dh int) (int, int) {
	if dw <= 0 {
		dw = defaultDeviceWidth
	}
	if dh <= 0 {
		dh = defaultDeviceHeight
	}
	if vw > 0 && vw != dw {
		x = x * dw / vw
	}
	if vh > 0 && vh != dh {
		y = y * dh / vh
	}
	if x < 0 {
		x = 0
	} else if x >= dw {
		x = dw - 1
	}
	if y < 0 {
		y = 0
	} else if y >= dh {
		y = dh - 1
	}
	return x, y
}

func firstNonEmptyLine(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.EqualFold(trimmed, "OK") {
			return trimmed
		}
	}
	return ""
}

func nonEmpty(primary, fallback string) string {
	if trimmed := strings.TrimSpace(primary); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(fallback)
}

func sanitizeContainerSuffix(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "dev"
	}
	if len(out) > 24 {
		return out[:24]
	}
	return out
}
