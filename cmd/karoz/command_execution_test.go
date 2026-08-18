package main

import (
	"context"
	"testing"

	executiondomain "github.com/karoz/karoz/internal/execution"
)

type recordingCommandRunner struct {
	requests []executiondomain.CommandRequest
	result   executiondomain.CommandResult
	err      error
}

func (runner *recordingCommandRunner) Run(_ context.Context, request executiondomain.CommandRequest) (executiondomain.CommandResult, error) {
	runner.requests = append(runner.requests, request)
	return runner.result, runner.err
}

func TestAppCommandExecutionUsesInjectedRunner(t *testing.T) {
	runner := &recordingCommandRunner{
		result: executiondomain.CommandResult{Stdout: "ok", ExitCode: 0},
	}
	a := &app{commandRunner: runner}
	out, err := a.runTaskCommand(context.Background(), "/workspace", "git", "status", "--short")
	if err != nil || out != "ok" {
		t.Fatalf("command result = %q err=%v", out, err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("requests = %d", len(runner.requests))
	}
	request := runner.requests[0]
	if request.Name != "git" || request.Dir != "/workspace" || len(request.Args) != 2 || request.Args[0] != "status" {
		t.Fatalf("unexpected request: %+v", request)
	}
	if !request.CombinedOutput || request.Configure == nil {
		t.Fatal("application command contract did not request combined output and process configuration")
	}
}

func TestResidentBashUsesInjectedRunner(t *testing.T) {
	runner := &recordingCommandRunner{
		result: executiondomain.CommandResult{Stdout: "hello", ExitCode: 0},
	}
	a := &app{commandRunner: runner}
	result := a.runResidentBashTool(context.Background(), "/workspace", "printf hello", 1000, 128)
	if !result.OK || result.Stdout != "hello" {
		t.Fatalf("bash result = %+v", result)
	}
	if len(runner.requests) != 1 || runner.requests[0].Name != "bash" || runner.requests[0].MaxOutputBytes != 128 {
		t.Fatalf("unexpected bash request: %+v", runner.requests)
	}
}
