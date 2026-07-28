//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package main

import (
	"context"
	"io"
	"testing"

	processdomain "github.com/karoz/karoz/internal/process"
)

type unsupportedProcessStore struct{}

func (unsupportedProcessStore) CreateStarting(processdomain.Process) error { return nil }
func (unsupportedProcessStore) MarkRunning(processdomain.Process) error    { return nil }
func (unsupportedProcessStore) MarkTerminal(processdomain.Process) error   { return nil }
func (unsupportedProcessStore) Reserve(processdomain.Process) error        { return nil }
func (unsupportedProcessStore) Abort(processdomain.Process) error          { return nil }

func TestBackgroundSupervisorUnsupportedPlatformFailsClosed(t *testing.T) {
	store := unsupportedProcessStore{}
	if _, err := newProcessSupervisor(
		context.Background(), store, store,
		func(processdomain.Process) (io.WriteCloser, error) { return nil, nil },
		processSupervisorConfig{},
	); err == nil {
		t.Fatal("unsupported platform enabled background processes")
	}
}
