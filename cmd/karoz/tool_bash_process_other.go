//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package main

import (
	"errors"
	"os"
	"os/exec"
)

func prepareResidentBashProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		return stopResidentBashProcessTree(cmd)
	}
}

func stopResidentBashProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
