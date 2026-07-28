//go:build windows

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

const (
	backgroundProcessSupported = true

	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
	processTerminate                  = 0x0001
	processSetQuota                   = 0x0100
	processQueryInformation           = 0x0400
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
)

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobObjectIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInformationData struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IOInfo                jobObjectIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type windowsProcessBoundary struct {
	mu        sync.Mutex
	job       syscall.Handle
	handshake io.WriteCloser
}

func newBackgroundProcessBoundary(cmd *exec.Cmd) (processBoundary, error) {
	handshake, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	jobValue, _, callErr := procCreateJobObjectW.Call(0, 0)
	if jobValue == 0 {
		_ = handshake.Close()
		return nil, windowsCallError("CreateJobObjectW", callErr)
	}
	job := syscall.Handle(jobValue)
	info := jobObjectExtendedLimitInformationData{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	ok, _, callErr := procSetInformationJobObject.Call(
		uintptr(job),
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if ok == 0 {
		_ = handshake.Close()
		_ = syscall.CloseHandle(job)
		return nil, windowsCallError("SetInformationJobObject", callErr)
	}
	return &windowsProcessBoundary{job: job, handshake: handshake}, nil
}

func (boundary *windowsProcessBoundary) AfterStart(cmd *exec.Cmd) error {
	process, _, callErr := procOpenProcess.Call(
		processTerminate|processSetQuota|processQueryInformation,
		0,
		uintptr(uint32(cmd.Process.Pid)),
	)
	if process == 0 {
		return windowsCallError("OpenProcess", callErr)
	}
	processHandle := syscall.Handle(process)
	defer syscall.CloseHandle(processHandle)
	ok, _, callErr := procAssignProcessToJobObject.Call(uintptr(boundary.job), process)
	if ok == 0 {
		return windowsCallError("AssignProcessToJobObject", callErr)
	}
	if _, err := boundary.handshake.Write([]byte{1}); err != nil {
		return err
	}
	return boundary.handshake.Close()
}

func (boundary *windowsProcessBoundary) Signal(os.Signal) error {
	boundary.mu.Lock()
	defer boundary.mu.Unlock()
	if boundary.job == 0 {
		return nil
	}
	ok, _, callErr := procTerminateJobObject.Call(uintptr(boundary.job), 1)
	if ok == 0 {
		return windowsCallError("TerminateJobObject", callErr)
	}
	return nil
}

func (boundary *windowsProcessBoundary) Close() error {
	boundary.mu.Lock()
	defer boundary.mu.Unlock()
	_ = boundary.handshake.Close()
	if boundary.job == 0 {
		return nil
	}
	err := syscall.CloseHandle(boundary.job)
	boundary.job = 0
	return err
}

func windowsCallError(operation string, err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return errors.New(operation + " failed")
	}
	return errors.New(operation + ": " + err.Error())
}

func runBackgroundProcessGuard(args []string) int {
	if len(args) < 2 || args[0] != "--" {
		return 2
	}
	var handshake [1]byte
	if _, err := io.ReadFull(os.Stdin, handshake[:]); err != nil {
		return 2
	}
	child := exec.Command(args[1], args[2:]...)
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return 127
	}
	return 0
}
