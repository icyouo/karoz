package main

import (
	"context"
	"errors"
	"testing"
	"time"

	executiondomain "github.com/karoz/karoz/internal/execution"
)

type deadlineCommandRunner struct {
	inspect func(context.Context)
}

func (runner deadlineCommandRunner) Run(ctx context.Context, _ executiondomain.CommandRequest) (executiondomain.CommandResult, error) {
	runner.inspect(ctx)
	return executiondomain.CommandResult{Stdout: "completed"}, ctx.Err()
}

func TestTaskExecutorPreservesTaskDeadlineAndUnlimitedPolicy(t *testing.T) {
	// A request-level CLI timeout must not override an admitted Task's policy.
	t.Setenv("KAROZ_TASK_TIMEOUT", "1ns")
	for _, provider := range []string{"codex", "claude"} {
		for _, unlimited := range []bool{false, true} {
			name := provider + "/finite"
			if unlimited {
				name = provider + "/unlimited"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				var wantDeadline time.Time
				if !unlimited {
					wantDeadline = time.Now().Add(24 * time.Hour)
					var cancel context.CancelFunc
					ctx, cancel = context.WithDeadline(ctx, wantDeadline)
					defer cancel()
				}
				called := false
				a := newApp(Settings{DataDir: t.TempDir()})
				a.commandRunner = deadlineCommandRunner{inspect: func(got context.Context) {
					called = true
					deadline, hasDeadline := got.Deadline()
					if hasDeadline == unlimited || (!unlimited && !deadline.Equal(wantDeadline)) || got.Err() != nil {
						t.Fatalf("executor deadline=%v present=%t err=%v; unlimited=%t expected=%v", deadline, hasDeadline, got.Err(), unlimited, wantDeadline)
					}
				}}
				if _, err := a.invokeTaskExecutor(ctx, CLI2APIRequest{Provider: provider, Workdir: t.TempDir(), Mode: "edit", Prompt: "task"}); err != nil {
					t.Fatal(err)
				}
				if !called {
					t.Fatal("task did not reach the executor")
				}
			})
		}
	}
}

func TestTaskExecutorObservesParentCancellation(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := newApp(Settings{DataDir: t.TempDir()})
			a.commandRunner = deadlineCommandRunner{inspect: func(ctx context.Context) {
				cancel()
				<-ctx.Done()
			}}
			_, err := a.invokeTaskExecutor(ctx, CLI2APIRequest{Provider: provider, Workdir: t.TempDir(), Mode: "edit"})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error=%v", err)
			}
		})
	}
}

func TestNonTaskCLIRequestsRetainRequestTimeout(t *testing.T) {
	t.Setenv("KAROZ_TASK_TIMEOUT", "1m")
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			a := newApp(Settings{DataDir: t.TempDir()})
			a.commandRunner = deadlineCommandRunner{inspect: func(ctx context.Context) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > time.Minute || time.Until(deadline) < 50*time.Second {
					t.Fatalf("request deadline=%v present=%t", deadline, ok)
				}
			}}
			if _, err := a.invokeCLI2API(context.Background(), CLI2APIRequest{Provider: provider, Workdir: t.TempDir(), Prompt: "question"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
