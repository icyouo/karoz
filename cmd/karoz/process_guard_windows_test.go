//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

var procGetExitCodeProcess = kernel32.NewProc("GetExitCodeProcess")

func TestWindowsProcessGuardEntry(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "process-guard" {
			index = i
			break
		}
	}
	if index >= 0 {
		os.Exit(runBackgroundProcessGuard(os.Args[index+1:]))
	}
}

func TestWindowsJobOwnerHelper(t *testing.T) {
	if os.Getenv("KAROZ_WINDOWS_JOB_HELPER") != "1" {
		return
	}
	dir := os.Getenv("KAROZ_WINDOWS_JOB_DIR")
	parentPath := filepath.Join(dir, "parent.pid")
	childPath := filepath.Join(dir, "child.pid")
	script := fmt.Sprintf(
		"$p=Start-Process powershell.exe -ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 30' -PassThru; "+
			"Set-Content -Path '%s' -Value $PID; Set-Content -Path '%s' -Value $p.Id; Wait-Process -Id $p.Id",
		strings.ReplaceAll(parentPath, "'", "''"),
		strings.ReplaceAll(childPath, "'", "''"),
	)
	cmd := exec.Command(
		os.Args[0], "-test.run=TestWindowsProcessGuardEntry", "--",
		"process-guard", "--", "powershell.exe", "-NoProfile", "-Command", script,
	)
	boundary, err := newBackgroundProcessBoundary(cmd)
	if err != nil {
		os.Exit(3)
	}
	if err := cmd.Start(); err != nil {
		_ = boundary.Close()
		os.Exit(4)
	}
	if err := boundary.AfterStart(cmd); err != nil {
		_ = boundary.Close()
		os.Exit(5)
	}
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0o600); err != nil {
		_ = boundary.Close()
		os.Exit(6)
	}
	select {}
}

func TestWindowsJobObjectKillsDescendantsWhenOwnerCrashes(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestWindowsJobOwnerHelper")
	cmd.Env = append(os.Environ(), "KAROZ_WINDOWS_JOB_HELPER=1", "KAROZ_WINDOWS_JOB_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForWindowsFile(t, filepath.Join(dir, "ready"))
	parentPath := filepath.Join(dir, "parent.pid")
	childPath := filepath.Join(dir, "child.pid")
	waitForWindowsFile(t, parentPath)
	waitForWindowsFile(t, childPath)
	parentPID := readWindowsPID(t, parentPath)
	childPID := readWindowsPID(t, childPath)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	waitForWindowsProcessGone(t, parentPID)
	waitForWindowsProcessGone(t, childPID)
}

func waitForWindowsFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("file %s did not appear", path)
}

func readWindowsPID(t *testing.T, path string) int {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(value)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func waitForWindowsProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		handleValue, _, _ := procOpenProcess.Call(processQueryInformation, 0, uintptr(uint32(pid)))
		if handleValue == 0 {
			return
		}
		handle := syscall.Handle(handleValue)
		var exitCode uint32
		ok, _, _ := procGetExitCodeProcess.Call(uintptr(handle), uintptr(unsafe.Pointer(&exitCode)))
		_ = syscall.CloseHandle(handle)
		if ok != 0 && exitCode != 259 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d survived job owner crash", pid)
}
