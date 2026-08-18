package main

import (
	"context"
	osexec "os/exec"
	"time"

	executiondomain "github.com/karoz/karoz/internal/execution"
)

const applicationCommandWaitDelay = 2 * time.Second

func (a *app) commandRunnerOrDefault() executiondomain.Runner {
	if a != nil && a.commandRunner != nil {
		return a.commandRunner
	}
	return executiondomain.NewHostRunner()
}

func (a *app) streamRunnerOrDefault() executiondomain.StreamRunner {
	if a != nil && a.streamRunner != nil {
		return a.streamRunner
	}
	return executiondomain.NewHostStreamRunner()
}

func commandRequest(dir, name string, args ...string) executiondomain.CommandRequest {
	return executiondomain.CommandRequest{
		Name:           name,
		Args:           args,
		Dir:            dir,
		CombinedOutput: true,
		WaitDelay:      applicationCommandWaitDelay,
		Configure:      prepareResidentBashProcess,
	}
}

func configureResidentCommand(cmd *osexec.Cmd) error {
	prepareResidentBashProcess(cmd)
	return nil
}

func (a *app) runCapturedCommand(ctx context.Context, dir, name string, args ...string) (executiondomain.CommandResult, error) {
	return a.commandRunnerOrDefault().Run(ctx, commandRequest(dir, name, args...))
}
