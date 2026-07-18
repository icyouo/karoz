//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResidentBashCancellationKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	result := runResidentBashTool(ctx, t.TempDir(), "sleep 30 & child=$!; echo $child; wait", 5000, 1024)
	if time.Since(started) > 3*time.Second {
		t.Fatalf("cancelled process group took too long to stop: %s", time.Since(started))
	}
	if result.OK {
		t.Fatalf("cancelled command unexpectedly succeeded: %+v", result)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(result.Stderr))
	if err != nil {
		t.Fatalf("child pid output = %q: %v", result.Stderr, err)
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
