//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

const backgroundProcessSupported = true

func backgroundShellCommand(command string) []string {
	return []string{"bash", "-lc", command}
}

type unixProcessBoundary struct {
	read      *os.File
	write     *os.File
	pgid      int
	closeOnce sync.Once
	closeErr  error
}

func newBackgroundProcessBoundary(cmd *exec.Cmd) (processBoundary, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{read}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	return &unixProcessBoundary{read: read, write: write}, nil
}

func (boundary *unixProcessBoundary) AfterStart(cmd *exec.Cmd) error {
	boundary.pgid = cmd.Process.Pid
	return boundary.read.Close()
}

func (boundary *unixProcessBoundary) Signal(signal os.Signal) error {
	if boundary.pgid <= 0 {
		return errors.New("invalid process group id")
	}
	value, ok := signal.(syscall.Signal)
	if !ok {
		return errors.New("unsupported process signal")
	}
	err := syscall.Kill(-boundary.pgid, value)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (boundary *unixProcessBoundary) Close() error {
	boundary.closeOnce.Do(func() {
		_ = boundary.read.Close()
		boundary.closeErr = boundary.write.Close()
	})
	return boundary.closeErr
}

func runBackgroundProcessGuard(args []string) int {
	if len(args) < 2 || args[0] != "--" {
		return 2
	}
	watchdog := os.NewFile(3, "karoz-parent-watchdog")
	if watchdog == nil {
		return 2
	}
	defer watchdog.Close()
	syscall.CloseOnExec(int(watchdog.Fd()))

	child := exec.Command(args[1], args[2:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		return 127
	}
	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()
	watchdogDone := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(io.Discard, watchdog)
		watchdogDone <- struct{}{}
	}()
	select {
	case err := <-childDone:
		if err == nil {
			return 0
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return 1
	case <-watchdogDone:
		_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
		select {}
	}
}
