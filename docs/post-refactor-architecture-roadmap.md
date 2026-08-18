# Post-refactor architecture roadmap

Status: implementation in progress — P1 conversation cutover started

Date: 2026-08-14

Inputs:

- the current Karoz working tree after the execution, task, Agent Run, and
  compatibility cleanup;
- `docs/refactor-execution-boundary.md`;
- a source-level comparison with the local deepseek-harness checkout at
  `/Users/icy/Code/deepseek-harness`.
- [`state-ownership.md`](state-ownership.md), the current P2 writer/reader
  inventory and migration record.

## Implementation progress

### 2026-08-14 — P1 conversation cutover

Implemented the first canonical-event slice:

- `agent-session-events.json` is now the only persisted source for visible
  Agent messages and provider-neutral transcript items;
- visible messages and hidden scheduled model inputs are appended as typed
  session events, then projected into the existing in-memory chat and
  transcript views;
- bootstrap rebuilds both projections from the event log and no longer reads
  `agent-messages.json` or `agent-transcripts.json`;
- a data directory containing only either superseded file fails loudly rather
  than silently hiding history or reviving a dual-read compatibility path;
- Agent Run creation, legal state transitions, result completion, failure, and
  cancellation now append replayable Run-state events without entering the
  model transcript;
- tool calls and results already enter the event log through their structured
  transcript record, including Run and tool-call correlation;
- handoff creation and every lifecycle transition now append replayable events
  to the target Agent session without entering the model transcript;
- one-time Resident Bash approvals now append request, decision, consumption,
  expiry, and Run-revocation events without copying command text or canonical
  workdirs into the log; command identity is represented only by SHA-256;
- durable process terminal delivery already enters the log as an idempotent
  `process_terminal` system message; its durable outbox remains the delivery
  authority until that message is acknowledged;
- monitor-probe Agent choices now append request and receipt-approval events
  containing the challenge, receipt, monitor revision, language, and source
  SHA-256 only; scripts remain in the secure receipt store;
- checkpoint commits now append the complete session summary/window state;
  bootstrap rebuilds message, transcript, and session projections from the
  event log, and no longer reads or writes `agent-session-state.json`.
- checkpoint archive history is now derived from canonical message events up
  to the persisted checkpoint boundary; `agent-archive-messages.json` has
  been removed and is rejected even when an event log is present.

### 2026-08-14 — P2 ConversationService extraction

- `app` no longer owns the canonical session-event map; a dedicated
  `ConversationService` owns event storage, append order, replay loading, and
  event persistence;
- structured transcripts are now a ConversationService projection with its
  own read/write/replace/reset boundary; `app.agentTranscripts` was removed;
- visible Agent messages and session/checkpoint state are ConversationService
  projections; `app.agentMessages` and `app.agentSessions` were removed;
- chat pagination, model context, process-terminal admission, checkpoint
  summarization, archive reads, and event replay now use the conversation
  boundary rather than an app-owned mutable map.
- archive API/search results are a deterministic event projection, not a
  persisted copy of messages; this closes the remaining conversation dual-write
  path and makes the event file the only durable session authority.

### 2026-08-14 — P2 CollaborationService inbox extraction

- `CollaborationService` now owns the mutable handoff/inbox projection and its
  lock; `app.inbox` has been removed;
- `internal/collaboration.HandoffService` persists through that service's
  repository adapter, while the application composes durable JSON persistence
  and cross-domain Session/Runtime event sinks;
- inbox listing, scheduler admission/recovery, prompt/tool availability,
  audit, blackboard derivation, and terminal-handling scans consume detached
  service snapshots rather than an app map;
- `agent-inbox.json` remains the one durable inbox projection, with loading
  normalization and persistence routed through the new owner.

### 2026-08-14 — P2 CollaborationService blackboard extraction

- `CollaborationService` now owns blackboard projection state as well as the
  inbox; `app.blackboard` has been removed;
- explicit and derived activity, startup projection rebuilds, prompt/tool
  availability, audit reads, and monitor admission all use service methods;
- monitor admission preserves its former atomic failure behavior by restoring
  the service projection if durable blackboard persistence fails.

### 2026-08-14 — P2 CollaborationService group inbox extraction

- group-delivery records are owned by `CollaborationService`; `app.groupInbox`
  has been removed;
- project coordination load/reset/save, group delegation, group-inbox API, and
  delivery-to-plan linking use the service projection;
- plan state remains in its existing versioned owner until its task-integration
  transaction boundary can move as one slice.

### 2026-08-14 — P2 CollaborationService plans extraction

- `CollaborationService` now owns versioned WorkPlan projections; `app.plans`
  has been removed;
- project-local coordination loading/saving, plan reads, creation, replacement,
  group coordinator transfer, prompt/tool policy checks, task integration, and
  scheduled plan-event selection consume the service owner;
- CAS/version comparisons and task-hook orchestration remain application
  coordinators, but no longer mutate an app-owned plan map.

### 2026-08-14 — P2 CollaborationService routes extraction

- directed Agent route topology is now a CollaborationService projection;
  `app.agentRoutes` has been removed;
- route loading/saving, validation, lookup/sorting, and agent-deletion cleanup
  use the service owner, while HTTP and resident tools remain application
  consumers.

### 2026-08-14 — P3 shared deadline policy

- `internal/runtime.Deadline` now represents an owned operation's finite or
  unlimited execution budget; a nil value selects the caller's durable
  default, while zero explicitly means unlimited;
- Task claiming and scheduled-worker execution both bind their contexts through
  this policy. This retains the model-visible Task contract: omitted defaults
  to one hour, positive values are finite, and `0` has no timeout.

### 2026-08-14 — P3 common operation view

- `internal/runtime.Operation` is the shared lifecycle projection for durable
  execution owners. It records stable identity, owner, class, state, deadline,
  start/update timestamps, and terminal classification without flattening the
  source entity;
- `Task`, `ScheduledRun`, and durable background `Process` now expose this
  view. Their original JSON records remain the durable authorities for
  worktree/task metadata, scheduler retry/effect metadata, and process output
  cursor/gap metadata respectively.
- Task claim now records `started_at` durably for each execution attempt, and
  the shared Operation projection carries it through. This prevents a queued
  record from being indistinguishable from an actively consuming unlimited
  task after a refresh or recovery inspection.

### 2026-08-14 — P4 fail-closed execution sandbox capability

- `internal/execution.CommandRequest` carries an explicit `SandboxPolicy`
  describing requested filesystem, network, and process isolation;
- the host adapter has no containment implementation and therefore rejects a
  non-empty policy before starting the command with typed
  `SandboxUnavailableError`; it cannot silently execute on the host;
- production callers that do not request a sandbox retain their deliberate
  host-execution behavior. Task-level policy selection will be wired through
  the task creation/execution contract in the next P4 slice.

### 2026-08-14 — P4 Task sandbox policy wiring

- Task creation now persists `sandbox_mode` (`host`, the default, or
  `required`) through HTTP and the model-visible task-creation tool;
- a `required` Task validates containment before worktree creation or provider
  startup. With the current host-only adapter it terminates as failed with the
  typed sandbox-unavailable reason, leaving worktree and commit state empty;
- the sandbox enforcer is composed at the application root: the default host
  adapter rejects required isolation, while a future platform enforcer can be
  injected without changing Task lifecycle policy;
- this is intentionally stricter than Git worktree isolation: `required`
  requests filesystem, network, and process enforcement together and cannot
  silently become a host task.

### 2026-08-14 — P5 Task entry-point parity gate

- an assembled application test creates a task through both the project HTTP
  endpoint and the resident model tool, proving both persist
  `max_runtime_ms: 0` and `sandbox_mode: required` identically;
- the existing replay, restart, process-recovery, browser-fixture, and
  corrupted-state suites remain the complementary assembled gates for their
  respective durable boundaries.

### 2026-08-14 — P2 CollaborationService groups extraction

- group topology is now a CollaborationService projection; `app.groups` has
  been removed;
- coordination load/save, group lookup/upsert, coordinator transfer, and
  policy consumers use the service owner, completing the planned collaboration
  projection extraction.

### 2026-08-14 — P2 RuntimeCoordinator Agent Run extraction

- resident Agent Run state now belongs to `agentRuntimeCoordinator`: the
  active-run registry, cancellation and worker-context handles, cancellation
  and result claims, terminal waiters, and bounded SSE ledgers are no longer
  fields on `app`;
- `runtime.RunLifecycle` and `runtime.RunControl` are composed against that
  owner through the existing repository port, preserving their first-terminal
  result arbitration while removing the duplicate application aggregate state;
- the coordinator deliberately holds only ephemeral runtime handles. Durable
  Run transitions remain in `agent-session-events.json`, so restart behavior
  cannot mistake an in-memory control handle for persisted operation truth.

### 2026-08-14 — P2 ProjectTaskCoordinator state extraction

- the durable Task projection is owned by `projectTaskCoordinator`; `app.tasks`
  has been removed;
- JSON load/save, task repository CRUD, interrupted-task recovery, merge
  claim, blackboard rebuild, runtime backlog/quiescence checks, and resident
  task-status transitions all use the same owner;
- `internal/task.Service` remains the owner of active claim/cancel handles and
  deadline binding, keeping persisted Task records separate from ephemeral
  execution control.

### 2026-08-14 — P2 Agent directory extraction

- `agentDirectory` now owns the durable per-project Agent projection;
  `app.agents` has been removed;
- JSON load/save, `internal/agent.Service` repository CRUD, prompt/runtime
  configuration reads, Agent deletion, monitor ownership checks, artifact
  reconciliation, and resident-command approval lookups share that owner;
- runtime Runs remain deliberately separate in `agentRuntimeCoordinator`, so
  durable Agent identity/configuration cannot be conflated with a local worker
  handle.

### 2026-08-14 — P2 Artifact catalog extraction

- `artifactCatalog` owns the durable per-project Artifact projection;
  `app.artifacts` has been removed;
- project-local artifact loading/saving, revision registration, status
  transitions, workspace reconciliation, artifact policy checks, and blackboard
  startup projection use the same catalog;
- the existing `internal/artifact.Service` keeps revision and approval policy
  above the catalog repository rather than duplicating mutation rules in the
  application aggregate.

### 2026-08-14 — P2 Memory store extraction

- `memoryStore` owns the curated Agent-memory projection and its
  `agent-memory.json` persistence; `app.memories` has been removed;
- creation, exact deduplication, decision supersession, pending-memory
  archival, scope validation, prompt retrieval, and lexical archive-search
  snapshots read and mutate the one store;
- semantic memories remain intentionally separate from the canonical session
  event stream: they are curated records with lifecycle/scope policy, while
  historical conversation is derived directly from session events.

### 2026-08-14 — P2 Task hook extraction

- persisted Task completion hooks now live beside Task records in
  `projectTaskCoordinator`; `app.taskHooks` has been removed;
- hook registration, terminal delivery marking, and `task-hooks.json`
  load/save share that owner while the application continues to coordinate the
  cross-domain message append and scheduled Agent notification effects.

### 2026-08-14 — P2 Project registry extraction

- `projectRegistry` owns durable project display aliases;
  `app.projectAliases` has been removed;
- normal project scanning, transactional external-project import, import
  recovery, and `project-aliases.json` load/save use the registry, preserving
  the existing settings/alias digest recovery contract.

### 2026-08-14 — P2 Resident Bash approval runtime extraction

- one-time Resident Bash approval tokens now belong to
  `agentRuntimeCoordinator`; `app.residentBashApprovals` has been removed;
- expiry, owner/re-run revocation, decision and consumption all use the
  runtime owner, while their immutable audit facts remain durable session
  events and command source stays out of the event log.

### 2026-08-14 — P2 Runtime hook and subscriber extraction

- idle-reconciliation hooks and project-scoped SSE runtime subscribers now
  belong to `agentRuntimeCoordinator`; `app.runtimeHooks` and
  `app.runtimeWatchers` have been removed;
- the coordinator is now the single owner for ephemeral Agent Run control,
  approval, coordination, and runtime-observation handles. It remains
  explicitly non-durable: emitted facts continue to flow through the session
  event and blackboard projections before clients observe them.

### 2026-08-15 — P2 Scheduled Run runtime extraction

- the in-memory Scheduled Run queue and per-kind local executor overrides now
  belong to `agentRuntimeCoordinator`; `app.schedulerQueue` and
  `app.schedulerExecutors` have been removed;
- scheduled-run persistence still writes the queue's durable snapshot, while
  restart recovery reconstructs the coordinator-owned queue. This preserves
  the distinction between durable operation facts and a process-local worker
  dispatch handle.

### 2026-08-15 — P2 Checkpoint coordination extraction

- resident-session checkpoint claims, retry-not-before backoff, and
  Scheduled Run synchronization hooks now belong to
  `agentRuntimeCoordinator`; the corresponding `app` fields have been
  removed;
- checkpoint summaries remain durable session-event projections. The
  coordinator only prevents concurrent local compaction and does not create a
  second recovery authority.

### 2026-08-15 — P2 Domain transaction-lock extraction

- task integration serialization is now held by `projectTaskCoordinator`,
  artifact revision serialization by `artifactCatalog`, and handoff/reply
  serialization by `collaborationService`; the corresponding application-wide
  mutexes and test seams have been removed;
- the background-owner deletion fence and its lock now belong to
  `agentRuntimeCoordinator`, keeping cancellation, approvals, monitors, and
  owned-process shutdown under the same runtime boundary.

### 2026-08-15 — P2 Process output runtime extraction

- process-output observation channels, monitor coverage cursors/baselines,
  bounded pending-gap aggregation, and their local workers now belong to
  `processOutputCoordinator`; the equivalent `app` fields have been removed;
- the coordinator is reconstructed for each server lifetime. Monitor error
  diagnostics and process gap records remain persisted through their existing
  stores, so no in-memory output cursor becomes recovery truth.

### 2026-08-15 — P2 Process terminal outbox extraction

- terminal-outbox wake signalling, worker start-once, and drain
  serialization now belong to `processTerminalOutboxCoordinator`; the durable
  terminal event is still owned by `processRuntimePersistence` until delivery
  is acknowledged;
- this removes the last terminal-delivery control handles from `app` without
  weakening crash recovery or cross-project outbox isolation.

### 2026-08-15 — P2 Process operation runtime extraction

- `processRuntimeCoordinator` now owns the persistence adapter, live process
  supervisor, and persistence-fault seam for durable background operations;
  `app` only composes that coordinator;
- process startup, shutdown, recovery, terminal delivery, and release APIs
  retain their existing operation contracts, including persisted lifetime and
  output-gap recovery behavior.

### 2026-08-15 — P2 Monitor runtime extraction

- `monitorRuntimeCoordinator` now owns the monitor registry, durable probe
  authorization projections, and process-local script-probe contexts,
  cancellation handles, slots, and wait group; `app` only composes it;
- monitor persistence, authorization validation, and process-output/runtime
  event matching remain unchanged, preserving the durable monitor contract
  while separating it from HTTP/application composition.

### 2026-08-15 — P2 Remaining synchronization-seam extraction

- project registration/import serialization and project-settings test seams now
  belong to `projectRegistry`; Scheduled Run snapshot serialization and Agent
  Run worker hooks belong to `agentRuntimeCoordinator`;
- the session-checkpoint save seam now belongs to `conversationService`.
  These changes remove the remaining domain synchronization and fault-injection
  state from the application aggregate without introducing forwarding owners.

### 2026-08-15 — Assembled acceptance evidence

The current assembled gates exercise the externally visible contracts rather
than only unit-level representations:

- `TestAgentSessionEventsReplayRunLifecycleWithoutPollutingTranscript`,
  `TestAgentSessionEventsReplayHandoffLifecycleWithoutPollutingTranscript`,
  and `TestSemanticCheckpointCommitsBoundedProviderOutputAndReloads` verify
  replayable session facts, transcript separation, handoff lifecycle, and
  checkpoint/archive reconstruction after reload. The checkpoint fixture now
  writes its source messages through `agent-session-events.json`, preventing a
  test-only projection from masquerading as replay authority;
- `TestAssembledSessionEventReplayRebuildsModelHistoryAndActivity` is the P1
  exit-gate scenario: one real application instance records message turns,
  tool call/result, Run cancellation, handoff delivery, and checkpoint state;
  a fresh instance rebuilds the exact event stream, model history, and
  Studio-visible activity from `agent-session-events.json` alone;
- `TestTaskCreationHTTPAndModelToolPreserveRuntimeAndSandboxContract`,
  `TestTaskRunCanBeUnlimited`, and
  `TestRequiredTaskSandboxFailsClosedBeforeWorktreeMutation` verify that both
  entry points preserve `max_runtime_ms: 0`, manual cancellation remains
  effective, and required isolation never falls back to host execution;
- `TestBackgroundProcessOutlivesRunCreatorAndSSERequest` verifies that process
  lifetime is owned by the durable background operation rather than the
  creator context or an SSE observer;
- `TestRunEventsEndpointReplaysTerminalLedgerWithoutWaitingForObserverContext`,
  `TestGate6BrowserFixture`, and the static replay contract verify the
  assembled Studio/Run replay behavior.
- `go test -race ./cmd/karoz` passes after the coordinator extractions,
  providing a package-level concurrency gate for the new ownership and worker
  boundaries.

### 2026-08-15 — P3 direct Agent Run operation projection

- direct provider/subagent Runs now expose `runtime.Operation` alongside Task,
  Process, and Scheduled Run. The projection has a stable Run/owner identity,
  class, lifecycle state, start/update timestamps, and the configured
  provider-turn deadline supplied by the caller;
- `TestAgentRunExposesSharedOperationSnapshot` closes the prior coverage gap
  without flattening the provider-specific Run model or changing its existing
  budget policy.

## 1. Decision summary

Karoz and deepseek-harness should not converge on the same product shape.

Karoz is a project-oriented local product: resident Agents, project plans,
tasks, Git worktrees, verification, commits, local integration, handoffs,
artifacts, monitors, and durable background processes are first-class product
concepts. A small Go deployment and a focused Studio are deliberate strengths.

deepseek-harness is a general Agent runtime and composition platform. Its
strongest architectural properties are the append-only session event log,
explicit Service/Provider/Consumer seams, replaceable capabilities, lifecycle
effects, sandbox providers, reusable job and subagent protocols, and strict
test/release governance.

Karoz will borrow those invariants, not deepseek-harness's package count or
plugin topology. In particular, this roadmap does not introduce a Cordis-style
container, split every feature into a package, or turn Karoz into a generic
framework before its own product boundaries require it.

## 2. Current comparison

| Dimension | Karoz after the current refactor | deepseek-harness | Direction |
|---|---|---|---|
| Product workflow | Project/task/worktree delivery is native | General capabilities require product-level composition | Preserve Karoz |
| Execution boundary | Injectable synchronous and streaming runners; Run lifecycle/control and task ownership are separated | Mature capability graph with provider replacement throughout | Continue the same seam pattern |
| Task runtime policy | Per-task maximum runtime is explicit and model-visible | Background work normally runs until completion, cancellation, or owner teardown | Preserve Karoz contract |
| Background work | Durable process supervision, output cursors, quotas, recovery, and lifetime policy | Generic owner-scoped Job registry, but process-local by default | Generalize Karoz without losing durability |
| Session truth | Messages, transcripts, Run ledgers, runtime events, and snapshots still have overlapping ownership | Append-only typed session events are authoritative; history and UI are projections | Adopt a canonical event log |
| Multi-Agent model | Product-level Agents, handoffs, inbox, groups, plans, and blackboard | General subagents, fork/continue, ACP, and multiple providers | Keep product semantics; standardize lifecycle underneath |
| Isolation | Git worktrees and process controls; host-command isolation is limited | Explicit OS sandbox providers with fail-closed enforcement | Add a sandbox capability seam |
| Extensibility | Selected Go interfaces are now injectable; much orchestration remains app-owned | Service/Provider/Consumer and plugin contracts are pervasive | Add seams only at measured boundaries |
| Verification | Strong focused Go tests, but fewer assembled replay and product-entry tests | Coverage, snapshots, browser tests, real-provider E2E, and release gates | Add assembled and replay-based gates |
| Operating cost | Small dependency surface and simple local deployment | Large TypeScript/pnpm workspace and higher composition cost | Preserve Karoz |

The comparison is about architecture and product behavior, not source-line or
package-count parity. At the comparison snapshot, Karoz still has hundreds of
methods on `*app`; deepseek-harness has more than two hundred packages. Neither
number is itself a target. The relevant question is whether each piece of
state has one owner and whether each effect has one replaceable boundary.

## 3. Contracts that must not regress

### 3.1 Per-task maximum runtime

Task creation exposes `max_runtime_ms` to both HTTP clients and the model tool
schema:

- omitted: default to one hour;
- positive value: enforce that per-run maximum;
- `0`: unlimited, ending only on completion or explicit cancellation;
- negative value: reject as invalid.

The timeout is task execution policy, not an HTTP/SSE connection timeout and
not a timeout on waiting for output. Browser disconnects must not shorten it.
The Studio must display the effective policy, including `Unlimited`.

Background processes retain their parallel `lifetime_ms` contract. A configured
administrative ceiling may reject an unlimited process, but the model-visible
schema and the rejection must be explicit.

### 3.2 Product workflow

The following remain Karoz domain concepts rather than generic plugin data:

- project and repository ownership;
- task worktree, verification, commit, and merge lifecycle;
- resident Agent identity and collaboration topology;
- handoffs, plans, blackboard entries, and artifacts;
- monitors and durable background process supervision.

### 3.3 Compatibility policy for this refactor

New internal APIs have one supported path. Do not add forwarding functions,
parallel repositories, dual writes, or fallback branches to preserve deleted
internal abstractions.

If a persisted format is replaced by the canonical event log, use a declared
one-time cutover and fail loudly on unsupported old data. Do not indefinitely
maintain two authoritative formats. Any destructive conversion or deletion of
user project data still requires an explicit release note and backup/rollback
procedure; “no compatibility shim” does not authorize silent data loss.

## 4. Target ownership model

The application must move from a large shared `app` aggregate toward a small
composition root with bounded coordinators:

```text
HTTP / Studio / model tools
            |
            v
      application ports
            |
   +--------+---------+----------------+----------------+
   |                  |                |                |
RuntimeCoordinator  ProjectTask     Collaboration    Conversation
                    Service          Service          Service
   |                  |                |                |
   +------------------+------ OperationService -------+
                              |
             execution / persistence / sandbox providers
```

`app` may construct providers, wire handlers, start and stop services, and own
process-wide configuration. It must not remain the owner of domain mutations
or duplicate repositories already owned by a service.

Proposed responsibilities:

- `RuntimeCoordinator`: provider turns, Run admission, interrupts, tool-loop
  callbacks, scheduler activation, and terminal coordination;
- `ProjectTaskService`: projects, task policy, worktree lifecycle,
  verification, integration, and task recovery;
- `CollaborationService`: Agents, handoffs, inbox, groups, plans, blackboard,
  and cross-Agent delivery;
- `ConversationService`: canonical session events, message projections,
  transcripts, compaction, checkpoints, and model-history construction;
- `OperationService`: shared execution ownership, deadline policy,
  cancellation, output cursors, terminal result arbitration, persistence, and
  restart behavior for work that outlives one request;
- provider adapters: command execution, streaming processes, persistence,
  filesystem, sandbox, model provider, and clock/ID generation where tests
  need deterministic control.

This is a responsibility map, not a requirement to create exactly five large
types. A boundary is accepted only when callers use its port and the previous
owner no longer mutates the same state.

## 5. Phased implementation

### P0 — Freeze invariants and map ownership

Before moving state:

1. inventory every mutable map, persisted file, event stream, worker handle,
   and cancellation handle currently reachable from `app`;
2. record exactly one intended owner and all readers for each item;
3. identify model-visible facts and the current path that persists each one;
4. add characterization tests for task timeout/unlimited behavior, Run result
   versus cancellation arbitration, process restart, handoff delivery, and
   transcript reconstruction;
5. prohibit new direct `app` map access outside the current owning adapter.

Exit gate: the ownership table has no state with two proposed writers, and the
characterization suite passes without provider credentials.

### P1 — Canonical append-only session event log

Introduce a typed, append-only event stream for every model-visible session
fact. At minimum it must represent:

- user, assistant, system, and collaboration messages;
- provider request boundaries and terminal outcomes;
- tool calls, tool results, approvals, and execution errors;
- Run creation, transition, cancellation, interruption, and completion;
- handoff/inbox delivery that enters model context;
- compaction/checkpoint events and their source ranges.

Rules:

1. persistence succeeds before a model-visible effect is acknowledged;
2. an event has one stable ID, session ID, causal/parent identity, timestamp,
   type, schema version, and typed payload;
3. messages, transcripts, UI activity, and model history are projections, not
   competing sources of truth;
4. replay performs no provider, shell, filesystem, or network side effects;
5. projection rebuilds are deterministic and idempotent;
6. a partial final record is either atomically absent or detectably invalid;
7. old snapshot support follows the explicit cutover policy in section 3.3.

Exit gate: a recorded multi-turn session with tools, cancellation, handoff,
and compaction can be replayed into the same model history and Studio-visible
activity from an empty projection store.

### P2 — Reduce `app` to a composition root

Move one vertical slice at a time, starting with conversation/session state,
then collaboration, project/task orchestration, and runtime coordination.

For each slice:

1. define the consumer-facing port before moving implementation;
2. move the state and its lock to the new owner;
3. migrate HTTP handlers, model tools, schedulers, and callbacks to the port;
4. remove the old field and method rather than forwarding to the new service;
5. test concurrent mutations at the new linearization boundary;
6. keep cross-domain workflows in an application coordinator instead of
   letting services mutate one another's repositories.

Exit gate: `app` performs construction, routing, lifecycle startup/shutdown,
and cross-service wiring only. Domain tests can construct each service without
an HTTP server or the complete application aggregate.

### P3 — Durable operation contract

Extract the common lifecycle beneath task runs, background processes,
scheduled work, and subagent/provider processes where their semantics truly
match. The contract must include:

- stable operation and owner identity;
- queued/running/stopping/terminal state transitions;
- finite or unlimited deadline policy;
- explicit cancellation with process-tree propagation;
- first-terminal-result-wins arbitration;
- bounded output storage with cursor/gap semantics;
- subscriptions that do not own the operation lifetime;
- durable recovery policy and observable recovery outcome;
- quotas by owner and operation class.

A project Task consumes this contract for execution but retains its own
worktree, verification, commit, integration, and recovery state machine. Do
not flatten `Task` into a generic Job.

Exit gate: the HTTP request, SSE subscriber, or model tool call that starts an
operation may disconnect without changing its configured lifetime; duplicate
cancel/result races have one deterministic terminal outcome; restart behavior
is covered by tests.

### P4 — Capability and sandbox seams

Standardize provider contracts only for effects that need replacement,
containment, or deterministic testing:

- synchronous command and streaming process execution;
- filesystem and Git operations where policy enforcement is required;
- model providers and MCP transports;
- persistence/event storage;
- OS sandbox policy and enforcement;
- clock, IDs, and selected host inspection.

Sandbox policy must distinguish Git worktree isolation from OS process
isolation. When a task requires an enabled sandbox, unavailable enforcement
must fail closed with a typed error; silently running on the host is not an
acceptable fallback. Platform adapters may differ while exposing the same
policy result and audit event.

Exit gate: task and resident shell execution can be tested against fake
providers, and supported production platforms can report whether requested
filesystem/network/process restrictions were fully enforced.

### P5 — Assembled verification and release gates

Keep focused package tests and add tests at the product boundary:

- keyless recorded-session replay through real application services;
- HTTP and model-tool contract parity, including `max_runtime_ms`;
- Studio browser tests for task lifetime, Run cancellation, inbox/activity,
  preview state, and background output recovery;
- real temporary-repository worktree/merge tests;
- real process-tree cancellation, timeout, unlimited lifetime, and restart
  tests;
- projection rebuild and corrupted-tail recovery tests;
- sandbox enforcement tests that verify effects outside the sandbox, not only
  adapter return values;
- shutdown tests proving that all owned workers either finish or are durably
  recoverable.

Exit gate: the key product flows run from assembled entry points in CI without
network credentials, while a smaller opt-in suite validates real providers.

## 6. Sequencing constraints

- Do P0 before another broad extraction. Moving code without an ownership map
  risks preserving the same shared state behind more interfaces.
- Define the event contract before moving conversation state in P2; otherwise
  the new service will inherit multiple truths.
- The operation contract may reuse `execution.Runner`, `StreamRunner`,
  `runtime.RunLifecycle`, and `runtime.RunControl`; it must not reimplement or
  wrap them through compatibility fallbacks.
- Do not delay sandbox design until after every executor is generalized. The
  execution contract must carry enough policy to prevent an adapter from
  silently dropping requested restrictions.
- Plugin discovery and third-party extension loading are explicitly deferred
  until at least two replaceable implementations exist for a capability.
- Package count, interface count, and reduction in file size are not success
  metrics. Single ownership, deterministic replay, cancellation correctness,
  and external-effect verification are.

## 7. Explicit non-goals

- adopting Cordis or reproducing the deepseek-harness workspace structure;
- converting Karoz into a hosted multi-tenant service;
- adding authentication solely as part of this architecture refactor;
- replacing all JSON persistence before the event/operation ownership is
  settled;
- flattening project Tasks, handoffs, plans, or monitors into generic plugins;
- adding an interface for every type without a second implementation,
  containment boundary, or test-control need;
- maintaining deleted internal APIs through aliases or fallback branches.

## 8. Completion definition

This roadmap is complete only when:

1. every model-visible session fact has one durable canonical event;
2. all projections can be rebuilt deterministically;
3. `app` is a composition root rather than a domain-state owner;
4. task, process, scheduled, and provider execution share explicit lifetime
   and cancellation semantics where appropriate;
5. finite and unlimited task runtime remain model-controllable and verified;
6. sandbox requirements cannot silently degrade;
7. assembled entry-point tests verify the externally visible workflow;
8. no compatibility shim or dual-write path remains from the replaced
   architecture.
