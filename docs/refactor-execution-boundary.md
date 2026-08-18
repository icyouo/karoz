# Execution boundary refactor

Status: complete for the planned execution, task-runtime, stream, and Agent Run lifecycle/control/terminal slices

This slice introduces `internal/execution.Runner` as the synchronous host
command port and `internal/execution.StreamRunner` for long-lived processes.
`cmd/karoz` supplies the platform-specific process-group hooks; the adapters
own command construction, cancellation propagation, pipe wiring, bounded
output capture, exit codes, duration, and finalization.

Migrated callers:

- task worktree Git commands, verification, and deployment;
- resident foreground Bash;
- synchronous Codex/Claude task CLI calls;
- app-owned command helpers and the folder picker.
- background Supervisor process construction, stdout/stderr pipe wiring, and
  process start.

The `app.commandRunner` field is injectable, so application tests can assert
the command contract without spawning a process. All application command
callers now go through the app-owned execution port; no legacy free-function
fallback remains.

Runtime ownership now has a matching lifecycle seam. `internal/runtime.RunLifecycle`
owns Run creation, busy-agent arbitration, legal state transitions, terminal
finalization, and interrupt queue consumption. `cmd/karoz` supplies an adapter
over the existing active-run map and keeps cancellation handles, ledgers,
watchers, transcript writes, and runtime events as application side effects.
This preserves the current snapshot shape while making Run policy testable
without the HTTP/application aggregate.

The adjacent `internal/runtime.RunControl` seam now owns worker claiming,
context binding, cancellation, result commits, and terminal cleanup. It
atomically installs the direct worker's cancellation handle, keeps scheduler
parent cancellation separate from explicit Run cancellation, rejects stale or
duplicate workers, and removes terminal control handles under the same adapter
lock as the active Run.

Cancellation and successful result commits now use the same control port as
their linearization boundary. `RequestCancel` marks a Run as cancelling before
calling the provider context, while `CommitResult` runs the app's durable
assistant-message callback and removes the active Run under one repository
lock. Therefore a cancellation claim and a result claim cannot both win.

Remaining intentionally application-specific:

- Claude's stream process and MCP stdio bridge now use `StreamRunner`; their
  protocol-specific readers/writers remain in the provider layer;
- monitor probes use `Runner`; probe-specific parsing, signal reporting,
  and platform semantics remain in the monitor layer;
- the process guard, which is the containment primitive itself.

The planned refactor is now complete. Claude CLI authentication probes and
stream processes use the injectable execution ports; MCP stdio already uses
the same `StreamRunner` seam. The task service owns List/Find/Insert/Update and
claim/cancel runtime ownership, the agent service owns CRUD plus default-agent
hydration, and Run lifecycle/control own the active state machine, interrupts,
worker claim, context binding, cancellation, result linearization, and
terminal cleanup. The app maps are the concrete JSON-backed repositories;
agent deletion and runtime-linked topology cleanup remain application-level
orchestration by design.

The next architecture phase is intentionally tracked separately. See
[`post-refactor-architecture-roadmap.md`](post-refactor-architecture-roadmap.md)
for the current comparison with deepseek-harness and the state-ownership,
composition-root, operation, capability, sandbox, and verification plan. This
document remains the completion record for the execution-boundary slice.
