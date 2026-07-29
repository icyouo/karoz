//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
)

const scriptProbeSupported = true

func readMonitorProbeSnapshot(
	store *secureRuntimeStore,
	relative string,
) (monitorProbeSnapshot, error) {
	file, err := store.openRead(relative)
	if err != nil {
		return monitorProbeSnapshot{}, err
	}
	defer file.Close()
	statFile, ok := file.(interface {
		Stat() (os.FileInfo, error)
	})
	if !ok {
		return monitorProbeSnapshot{}, errors.New("probe snapshot handle cannot be verified")
	}
	info, err := statFile.Stat()
	if err != nil {
		return monitorProbeSnapshot{}, err
	}
	system, ok := info.Sys().(*syscall.Stat_t)
	if !ok || system.Dev == 0 || system.Ino == 0 {
		return monitorProbeSnapshot{}, errors.New("probe snapshot identity is unavailable")
	}
	source, err := io.ReadAll(io.LimitReader(
		file,
		monitordomain.MaxProbeSourceBytes+1,
	))
	if err != nil {
		return monitorProbeSnapshot{}, err
	}
	if len(source) == 0 || len(source) > monitordomain.MaxProbeSourceBytes {
		return monitorProbeSnapshot{}, errors.New("probe snapshot size is invalid")
	}
	return monitorProbeSnapshot{
		Source:   source,
		Device:   uint64(system.Dev),
		Inode:    uint64(system.Ino),
		OwnerUID: system.Uid,
		Mode:     info.Mode().Perm(),
	}, nil
}

func executeMonitorProbe(
	parent context.Context,
	language, workdir string,
	source []byte,
	timeout time.Duration,
) (monitordomain.ProbeExecution, string, error) {
	interpreter := "bash"
	args := []string{"-s", "--"}
	if language == "javascript" {
		interpreter = "node"
		args = []string{"-"}
	}
	path, err := exec.LookPath(interpreter)
	if err != nil {
		return monitordomain.ProbeExecution{}, "", err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = workdir
	cmd.Stdin = bytes.NewReader(source)
	prepareResidentBashProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	stdout := &probeBoundedBuffer{limit: 16 << 10}
	stderr := &probeBoundedBuffer{limit: 16 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	_ = stopResidentBashProcessTree(cmd)
	execution := monitordomain.ProbeExecution{
		Stdout:   append([]byte(nil), stdout.data...),
		TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
	}
	if exitErr := new(exec.ExitError); errors.As(err, &exitErr) {
		execution.ExitCode = exitErr.ExitCode()
		if exitErr.ProcessState != nil {
			status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus)
			execution.Signaled = ok && status.Signaled()
		}
	} else if err != nil && !execution.TimedOut {
		return execution, string(stderr.data), err
	}
	if stdout.overflow || stderr.overflow {
		return execution, string(stderr.data), errors.New("probe output exceeded 16 KiB")
	}
	return execution, string(stderr.data), nil
}
