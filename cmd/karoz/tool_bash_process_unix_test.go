//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResidentBashCancellationKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel only once the child has actually been spawned and reported its
	// pid; a fixed timeout is flaky on slow runners (the echo may lose the
	// race against SIGKILL).
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(pidFile); err == nil {
				cancel()
				return
			}
			if time.Now().After(deadline) {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	started := time.Now()
	result := runResidentBashTool(ctx, dir, "sleep 30 & child=$!; echo $child > child.pid; wait", 30000, 1024)
	if time.Since(started) > 5*time.Second {
		t.Fatalf("cancelled process group took too long to stop: %s", time.Since(started))
	}
	if result.OK {
		t.Fatalf("cancelled command unexpectedly succeeded: %+v", result)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child pid file not written before cancellation: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("child pid output = %q: %v", data, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d survived process-group cancellation: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
