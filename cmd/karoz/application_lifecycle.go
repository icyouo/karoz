package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const applicationShutdownTimeout = 10 * time.Second

func applicationSignals() (<-chan os.Signal, func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	return signals, func() { signal.Stop(signals) }
}

func serveApplication(
	a *app,
	server *http.Server,
	listener net.Listener,
	signals <-chan os.Signal,
	shutdownTimeout time.Duration,
) error {
	if shutdownTimeout <= 0 {
		return errors.New("application shutdown timeout must be positive")
	}
	serveDone := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveDone <- err
	}()

	select {
	case err := <-serveDone:
		processErr := shutdownProcessRuntimeBounded(a, shutdownTimeout)
		return errors.Join(err, processErr)
	case <-signals:
		httpCtx, httpCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		httpErr := server.Shutdown(httpCtx)
		httpCancel()

		processErr := shutdownProcessRuntimeBounded(a, shutdownTimeout)
		var serveErr error
		wait := time.NewTimer(shutdownTimeout)
		defer wait.Stop()
		select {
		case serveErr = <-serveDone:
		case <-wait.C:
			serveErr = errors.New("HTTP server did not stop within the shutdown deadline")
		}
		return errors.Join(httpErr, processErr, serveErr)
	}
}

func shutdownProcessRuntimeBounded(a *app, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return a.shutdownProcessRuntime(ctx)
}
