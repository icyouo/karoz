# State ownership map

Status: P2 working inventory

This is the P0/P2 ownership record for the current refactor. A map has one
writer owner even when several application flows read it. New code must use
the owner's port instead of taking an additional direct write path.

## Conversation

| State / durable authority | Current writer boundary | Intended owner | Readers / projections | Migration order |
|---|---|---|---|---|
| `agent-session-events.json` / `agentSessionEvents` | session append and replay helpers | ConversationService | visible messages, transcript, session summary/window, Studio history, provider context | first |
| `agentMessages` | event projection only | ConversationService | chat API, archives, terminal process receipt | completed — `app.agentMessages` removed |
| `agentTranscripts` | event projection only | ConversationService | provider history, context meter | completed |
| `agentSessions` | event projection/checkpoint only | ConversationService | prompt summary/window, checkpoint scheduler | completed — `app.agentSessions` removed |
| archive history | checkpoint boundary + event projection | ConversationService | archive API, archive search, memory tools | completed — `agent-archive-messages.json` removed; old file is rejected |

Conversation invariants:

1. Every conversation mutation appends exactly one durable session event before
   its derived message/transcript/session projection is observed.
2. Event replay never invokes a model, tool, shell, filesystem mutation, or
   network request.
3. A checkpoint state is a session-event projection; no second session state
   file is read or written.
4. Command and probe source material is never copied into approval events.
5. `app` does not own a parallel session/checkpoint map; checkpoint updates use
   a detached ConversationService snapshot and commit only after durable event
   persistence succeeds.

## Collaboration

| State / durable authority | Current writer boundary | Intended owner | Readers / projections | Migration order |
|---|---|---|---|---|
| inbox / handoff state | `internal/collaboration.HandoffService` repository adapter | CollaborationService | scheduler, resident tools, inbox API, session event sink | completed — `app.inbox` removed |
| groups and group inbox | group application handlers | CollaborationService | team UI, group routing, plans | completed — `app.groups` and `app.groupInbox` removed |
| blackboard | blackboard mutation helpers | CollaborationService | activity UI, Karoz idle reconcile | completed — `app.blackboard` removed |
| plans and routes | plan/route mutation helpers | CollaborationService | scheduler, prompts, Studio | completed — `app.plans` and `app.agentRoutes` removed |

## Agent and Task directories

| State / durable authority | Current writer boundary | Intended owner | Readers / projections | Migration order |
|---|---|---|---|---|
| project Agents / `agents.json` | `internal/agent.Service` repository | `agentDirectory` | prompts, monitor ownership, artifacts, runtime configuration | completed — `app.agents` removed |
| project Tasks / `tasks.json` | `internal/task.Service` repository | `projectTaskCoordinator` | task lifecycle, blackboard, plans, Studio | completed — `app.tasks` removed |
| Task completion hooks / `task-hooks.json` | task terminal coordinator | `projectTaskCoordinator` | Agent task notifications, scheduler | completed — `app.taskHooks` removed |
| task integration locks and test handoff seam | task integration coordinator | `projectTaskCoordinator` | worktree/merge serialization | completed — `app` no longer owns task integration state |
| project aliases / `project-aliases.json` | project registration/import | `projectRegistry` | project scanning, import recovery, Studio | completed — `app.projectAliases` removed |
| project registration serialization and import/settings test seams | project registration/import | `projectRegistry` | project creation/import and settings registry updates | completed — `app` no longer owns project-registry synchronization state |
| project Artifacts / `.karoz/artifacts.json` | `internal/artifact.Service` repository | `artifactCatalog` | task artifact policy, preview, blackboard, Studio | completed — `app.artifacts` removed |
| curated Agent memory / `agent-memory.json` | memory lifecycle and search helpers | `memoryStore` | prompt context, memory/archive tools, Studio | completed — `app.memories` removed |

## Runtime and operation state

| State / durable authority | Current writer boundary | Intended owner | Migration dependency |
|---|---|---|---|
| active Runs, control handles, terminal waiters, SSE runtime subscribers/ledgers, coordination hooks, checkpoint claims/backoff, and scheduler test synchronization hooks | `runtime.RunLifecycle` / `RunControl` adapters | `agentRuntimeCoordinator` | completed — `app` Run, hook, subscription, checkpoint, and scheduler-runtime fields removed; durable transitions remain Conversation events |
| Scheduled Run persistence serialization and Agent Run worker test hooks | Agent runtime execution | `agentRuntimeCoordinator` | scheduler snapshots and worker-result arbitration | completed — `app` no longer owns these runtime seams |
| one-time Resident Bash approval tokens | Resident Bash runtime policy | `agentRuntimeCoordinator` | approval resolution and session-event audit | completed — `app.residentBashApprovals` removed |
| deleting background owner fence | Agent deletion/runtime policy | `agentRuntimeCoordinator` | process, monitor, approval, and Run cancellation | completed — `app.backgroundOwner*` removed |
| process-output observation channel, monitor cursors/baselines, and output-gap recovery | process output runtime | `processOutputCoordinator` | monitor evaluation and durable gap diagnostics | completed — `app` no longer owns process-output local state |
| terminal-outbox wake, worker, and drain serialization | process terminal delivery runtime | `processTerminalOutboxCoordinator` | durable process terminal event delivery | completed — `app` no longer owns terminal-outbox local control state |
| process persistence, live supervisor, and scoped persistence fault seam | background process operation runtime | `processRuntimeCoordinator` | durable process records, recovery, release, and supervision | completed — `app` composes the coordinator instead of defining process-operation state |
| monitor registry, probe authorization records, and script-probe local handles | monitor runtime | `monitorRuntimeCoordinator` | monitor lifecycle, probe security, runtime-event/process-output matching | completed — `app` composes the monitor coordinator instead of defining monitor state |
| scheduled-run queue and local executors | scheduler persistence and queue | `agentRuntimeCoordinator` | completed — `app.schedulerQueue` and `app.schedulerExecutors` removed; durable job snapshots remain the restart authority |
| background process journal/outbox | process runtime persistence | OperationService | operation lifecycle contract |
| durable Task projection | `projectTaskCoordinator` + `task.Service` repository | ProjectTaskService | completed — `app.tasks` removed; runtime claim handles remain in `task.Service` |

## Explicitly not shared state

- Git worktree/integration policy remains ProjectTaskService domain state; it
  is not a generic operation payload.
- Probe source and Resident Bash command text stay in their secure/ephemeral
  authorities. Session events retain only immutable hashes and identifiers.
- HTTP/SSE channels and process cancellation functions are ephemeral handles,
  never durable session or operation records.
