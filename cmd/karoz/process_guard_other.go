//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package main

import (
	"errors"
	"os"
	"os/exec"
)

const backgroundProcessSupported = false

func prepareBackgroundGuardProcess(_ *exec.Cmd) {}

func signalBackgroundProcessGroup(_ int, _ os.Signal) error {
	return errors.New("background processes are unsupported on this platform")
}

func runBackgroundProcessGuard(_ []string) int { return 2 }
