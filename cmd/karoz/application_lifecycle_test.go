//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	processdomain "github.com/karoz/karoz/internal/process"
)

func TestServeApplicationSignalStopsAndPersistsBackgroundProcess(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := projectFromPath(projectPath, root, "main")
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	if err := a.bootstrap(); err != nil {
		t.Fatal(err)
	}
	request := startRequest("lifecycle-process", "sleep 30", projectPath)
	request.ProjectID = project.ID
	if _, err := a.processSupervisor.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	signals, stopSignals := applicationSignals()
	defer stopSignals()
	done := make(chan error, 1)
	go func() {
		done <- serveApplication(
			a,
			&http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})},
			listener,
			signals,
			5*time.Second,
		)
	}()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	record, err := a.processRecord(project.ID, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != processdomain.StateInterrupted || record.EndedAt == nil {
		t.Fatalf("graceful shutdown record = %+v", record)
	}
}

func TestProcessRuntimeShutdownFailureDoesNotCancelAndCanRetry(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	if err := a.bootstrap(); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	a.processSupervisor.shutdownGate <- struct{}{}
	if err := a.shutdownProcessRuntime(cancelled); err == nil {
		t.Fatal("cancelled shutdown unexpectedly succeeded")
	}
	<-a.processSupervisor.shutdownGate
	select {
	case <-a.supervisorCtx.Done():
		t.Fatal("failed shutdown cancelled supervisor ownership")
	default:
	}
	ctx, retryCancel := context.WithTimeout(context.Background(), time.Second)
	defer retryCancel()
	if err := a.shutdownProcessRuntime(ctx); err != nil {
		t.Fatalf("retry shutdown: %v", err)
	}
	select {
	case <-a.supervisorCtx.Done():
	default:
		t.Fatal("successful shutdown did not cancel application supervisor context")
	}
}

func TestServeApplicationReportsBoundedHTTPShutdownFailure(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()})
	if err := a.bootstrap(); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})}
	signals := make(chan os.Signal, 1)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveApplication(a, server, listener, signals, 25*time.Millisecond)
	}()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, requestErr := http.Get("http://" + listener.Addr().String())
		if requestErr == nil {
			_ = response.Body.Close()
		}
	}()
	<-entered
	signals <- os.Interrupt
	started := time.Now()
	err = <-serveDone
	if err == nil {
		t.Fatal("HTTP shutdown deadline was hidden")
	}
	if time.Since(started) > time.Second {
		t.Fatalf("HTTP shutdown failure was not bounded: %v", time.Since(started))
	}
	close(release)
	<-requestDone
}
