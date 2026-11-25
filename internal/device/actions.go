package device

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const touchSessionMarker = "__KAROZ_TOUCH_OK__"

var (
	validAndroidSymbolRE = regexp.MustCompile(`^[A-Za-z0-9_.$:/-]+$`)
	validAndroidKeyRE    = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// TouchSession holds a persistent `adb -s <target> shell` process so live pointer
// gestures reuse the shell instead of paying fork/connect latency per event.
type TouchSession struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	closed bool
	target string
}

func NewTouchSession(parent context.Context, adbTarget string) (*TouchSession, error) {
	cmd := exec.Command("adb", "-s", adbTarget, "shell")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("touch session stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("touch session stdout: %w", err)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("touch session start: %w", err)
	}
	ts := &TouchSession{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		target: adbTarget,
	}
	if err := ts.exec(parent, "echo "+touchSessionMarker, 2*time.Second); err != nil {
		_ = ts.Close()
		return nil, err
	}
	return ts, nil
}

func (s *TouchSession) exec(ctx context.Context, command string, timeout time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("touch session closed")
	}
	line := command + "; echo " + touchSessionMarker + "\n"
	if _, err := io.WriteString(s.stdin, line); err != nil {
		s.closed = true
		return fmt.Errorf("touch session write: %w", err)
	}
	type readResult struct {
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		for {
			text, err := s.stdout.ReadString('\n')
			if err != nil {
				ch <- readResult{err: err}
				return
			}
			if strings.Contains(text, touchSessionMarker) {
				ch <- readResult{}
				return
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		s.closed = true
		return ctx.Err()
	case <-timer.C:
		s.closed = true
		return fmt.Errorf("touch session timeout")
	case r := <-ch:
		if r.err != nil {
			s.closed = true
			return fmt.Errorf("touch session read: %w", r.err)
		}
		return nil
	}
}

func (s *TouchSession) Tap(ctx context.Context, x, y int) error {
	return s.exec(ctx, "input tap "+strconv.Itoa(x)+" "+strconv.Itoa(y), 2*time.Second)
}

func (s *TouchSession) Swipe(ctx context.Context, fromX, fromY, toX, toY, durationMS int) error {
	cmd := "input swipe " +
		strconv.Itoa(fromX) + " " + strconv.Itoa(fromY) + " " +
		strconv.Itoa(toX) + " " + strconv.Itoa(toY) + " " +
		strconv.Itoa(durationMS)
	return s.exec(ctx, cmd, time.Duration(durationMS+2000)*time.Millisecond)
}

func (s *TouchSession) Key(ctx context.Context, key string) error {
	if !validAndroidKeyRE.MatchString(strings.TrimSpace(key)) {
		return fmt.Errorf("invalid android key %q", key)
	}
	return s.exec(ctx, "input keyevent "+strings.TrimSpace(key), 2*time.Second)
}

func (s *TouchSession) Text(ctx context.Context, text string) error {
	return s.exec(ctx, "input text "+EscapeADBInputText(text), 3*time.Second)
}

func (s *TouchSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		go func() { _ = s.cmd.Wait() }()
	}
	return nil
}

func EscapeADBInputText(value string) string {
	// Android `input text` treats spaces specially in some builds; replace
	// spaces with `%s` inside single-quoted shell text while escaping `'`.
	value = strings.ReplaceAll(value, " ", "%s")
	value = strings.ReplaceAll(value, "'", `'\''`)
	return "'" + value + "'"
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func adbCommandForAction(req ActionRequest) (args []string, wait time.Duration, err error) {
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	switch kind {
	case "tap":
		if req.X == nil || req.Y == nil {
			return nil, 0, fmt.Errorf("tap requires x and y coordinates")
		}
		return []string{"shell", "input", "tap", strconv.Itoa(*req.X), strconv.Itoa(*req.Y)}, 0, nil
	case "double_tap":
		if req.X == nil || req.Y == nil {
			return nil, 0, fmt.Errorf("double_tap requires x and y coordinates")
		}
		x := strconv.Itoa(*req.X)
		y := strconv.Itoa(*req.Y)
		return []string{"shell", "input tap " + x + " " + y + " && input tap " + x + " " + y}, 0, nil
	case "long_press":
		if req.X == nil || req.Y == nil {
			return nil, 0, fmt.Errorf("long_press requires x and y coordinates")
		}
		dur := req.DurationMS
		if dur <= 0 {
			dur = 700
		}
		x := strconv.Itoa(*req.X)
		y := strconv.Itoa(*req.Y)
		return []string{"shell", "input", "swipe", x, y, x, y, strconv.Itoa(dur)}, 0, nil
	case "swipe":
		if req.FromX == nil || req.FromY == nil || req.ToX == nil || req.ToY == nil {
			return nil, 0, fmt.Errorf("swipe requires from_x, from_y, to_x, to_y")
		}
		dur := req.DurationMS
		if dur <= 0 {
			dur = 250
		}
		return []string{
			"shell", "input", "swipe",
			strconv.Itoa(*req.FromX), strconv.Itoa(*req.FromY),
			strconv.Itoa(*req.ToX), strconv.Itoa(*req.ToY),
			strconv.Itoa(dur),
		}, 0, nil
	case "type_text":
		if req.Text == "" {
			return nil, 0, fmt.Errorf("type_text requires non-empty text")
		}
		return []string{"shell", "input text " + EscapeADBInputText(req.Text)}, 0, nil
	case "back":
		return []string{"shell", "input", "keyevent", "KEYCODE_BACK"}, 0, nil
	case "home":
		return []string{"shell", "input", "keyevent", "KEYCODE_HOME"}, 0, nil
	case "app_switch":
		return []string{"shell", "input", "keyevent", "KEYCODE_APP_SWITCH"}, 0, nil
	case "keyevent":
		key := strings.TrimSpace(req.Key)
		if !validAndroidKeyRE.MatchString(key) {
			return nil, 0, fmt.Errorf("invalid keyevent key %q", req.Key)
		}
		return []string{"shell", "input", "keyevent", key}, 0, nil
	case "launch_app":
		pkg := strings.TrimSpace(req.Package)
		if !validAndroidSymbolRE.MatchString(pkg) {
			return nil, 0, fmt.Errorf("invalid android package name")
		}
		if act := strings.TrimSpace(req.Activity); act != "" {
			if !validAndroidSymbolRE.MatchString(act) {
				return nil, 0, fmt.Errorf("invalid android activity name")
			}
			component := pkg + "/" + act
			return []string{"shell", "am", "start", "-n", component}, 0, nil
		}
		return []string{"shell", "monkey", "-p", pkg, "-c", "android.intent.category.LAUNCHER", "1"}, 0, nil
	case "open_url":
		rawURL := strings.TrimSpace(req.URL)
		if rawURL == "" || strings.IndexAny(rawURL, "\x00\r\n") >= 0 {
			return nil, 0, fmt.Errorf("invalid url")
		}
		return []string{"shell", "am start -a android.intent.action.VIEW -d " + shellSingleQuote(rawURL)}, 0, nil
	case "wait":
		ms := req.MS
		if ms <= 0 {
			ms = 500
		}
		if ms > 15000 {
			ms = 15000
		}
		return nil, time.Duration(ms) * time.Millisecond, nil
	default:
		return nil, 0, fmt.Errorf("unsupported action kind %q", req.Kind)
	}
}

func ParseWMSize(raw string) (width, height int) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		idx := strings.LastIndex(line, ":")
		if idx >= 0 {
			line = strings.TrimSpace(line[idx+1:])
		}
		parts := strings.Split(strings.ToLower(line), "x")
		if len(parts) != 2 {
			continue
		}
		w, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
		h, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
		if errW == nil && errH == nil && w > 0 && h > 0 {
			return w, h
		}
	}
	return 1080, 1920
}
