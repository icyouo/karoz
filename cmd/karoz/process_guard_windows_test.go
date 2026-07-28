//go:build windows

package main

import (
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
	if os.Getenv("KAROZ_WINDOWS_GUARD") == "1" {
		os.Exit(runBackgroundProcessGuard([]string{
			"--", os.Args[0], "-test.run=TestWindowsDescendantParentHelper",
		}))
	}
}

func TestWindowsJobOwnerHelper(t *testing.T) {
	if os.Getenv("KAROZ_WINDOWS_JOB_HELPER") != "1" {
		return
	}
	dir := os.Getenv("KAROZ_WINDOWS_JOB_DIR")
	cmd := exec.Command(os.Args[0], "-test.run=TestWindowsDescendantParentHelper")
	cmd.Env = append(
		os.Environ(),
		"KAROZ_WINDOWS_DESC_PARENT=1",
		"KAROZ_WINDOWS_DESC_DIR="+dir,
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
	for {
		time.Sleep(time.Second)
	}
}

func TestWindowsDescendantParentHelper(t *testing.T) {
	if os.Getenv("KAROZ_WINDOWS_DESC_PARENT") != "1" {
		return
	}
	dir := os.Getenv("KAROZ_WINDOWS_DESC_DIR")
	if err := os.WriteFile(
		filepath.Join(dir, "parent.pid"),
		[]byte(strconv.Itoa(os.Getpid())),
		0o600,
	); err != nil {
		os.Exit(7)
	}
	child := exec.Command(os.Args[0], "-test.run=TestWindowsDescendantSleepHelper")
	child.Env = append(os.Environ(), "KAROZ_WINDOWS_DESC_SLEEP=1")
	if err := child.Start(); err != nil {
		os.Exit(8)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "child.pid"),
		[]byte(strconv.Itoa(child.Process.Pid)),
		0o600,
	); err != nil {
		_ = child.Process.Kill()
		os.Exit(9)
	}
	_ = child.Wait()
}

func TestWindowsDescendantSleepHelper(t *testing.T) {
	if os.Getenv("KAROZ_WINDOWS_DESC_SLEEP") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
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
