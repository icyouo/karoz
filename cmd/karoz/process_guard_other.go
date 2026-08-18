//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package main

import (
	"errors"
	"os/exec"
)

const backgroundProcessSupported = false

func backgroundShellCommand(string) []string { return nil }

func newBackgroundProcessBoundary(_ *exec.Cmd) (processBoundary, error) {
	return nil, errors.New("background processes are unsupported on this platform")
}
func runBackgroundProcessGuard(_ []string) int { return 2 }
