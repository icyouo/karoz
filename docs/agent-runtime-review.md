# Agent Runtime Design Review

> Historical review record. Current work is classified in
> `docs/agent-runtime-immediate-remediation.md` and
> `docs/agent-runtime-candidate-designs.md`. A finding in this document is not
> automatically an approved implementation requirement.

Status: historical review (2026-07-27). Scope: the resident agent runtime as of
`37d6a15` — run lifecycle, provider/tool loop, scheduler, collaboration, task
execution, persistence, and the HTTP surface that drives them. Method: full
read of `cmd/karoz` (~14.2k lines, excluding tests) and `internal/` (~1.9k
lines), plus `go build ./...` and `go test ./...` (all packages pass, ~10s).

This document records findings only. It prescribes no schedule; the closing
sections propose an order of work.

## Verdict

The runtime's design intent is well above comparable OSS agent frameworks in two
places that are usually wrong: it models a *run* as a first-class entity with a
real state machine, and it refuses to reimplement the coding loop, delegating
instead to the native `codex`/`claude` CLI inside a worktree.

Against that, three problems are load-bearing. There is a cross-origin path to
unauthenticated host command execution. The task worktree isolation that the
README sells is broken at the merge step and has no concurrency control. And
behavioral correctness is largely delegated to prompt prose in places where the
runtime already holds the state needed to enforce it in code.

The layering is also nominal rather than real: `internal/` packages exist but are
mostly type holders, while nearly all logic lives as methods on one `*app` god
object behind one mutex.

## Architecture as built

```
HTTP (no auth, no Origin check)
  └─ api_agents.go            POST .../agents/{id}/messages
       ├─ beginAgentRun       one active Run per agent, else interrupt-enqueue
       ├─ bindAgentRunContext run ctx derives from r.Context()
       └─ streamAgentMessage  SSE: delta / tool_start / tool_result / done
            └─ runResidentAgentTurn
                 ├─ memoryRetrievalQueryFor   side-channel classifier call
                 ├─ buildResidentAgentPrompt… one flat text prompt per turn
                 └─ provider.Stream
                      └─ invokeResidentToolLoop   shared across providers
                           ├─ residentStreamWire  codex SSE | claude SSE
                           └─ executeResidentTool ~50 tools, one registry

Scheduler (handoff / task_event / plan_event / idle_reconcile)
  └─ SchedulerQueue  per-agent FIFO, dedup, retry, effects barrier
       └─ SchedulerWorker  ctx = Background, so it survives disconnects

Task execution
  └─ runDevelopmentTask  worktree → native CLI → detect → verify → commit → merge

Persistence
  └─ persistence.JSONStore  whole-file atomic writes under .karoz/
```

## What the design gets right

**Run state machine with single-run-per-agent and interrupt folding.**
`internal/runtime/model.go` defines the states and `run_controller.go` enforces
them with expected-run-id compare-and-set on every transition, so a superseded
run cannot mutate state. `beginAgentRun` refuses a second concurrent run per
agent; a mid-flight user message becomes an `AgentInterrupt` instead of being
queued or dropped. `runResidentStep` (`provider_resident_stream.go`) races a
40ms poller against the streaming request, cancels the in-flight HTTP call when
an interrupt lands, and folds the interrupt into the conversation as the latest
user input. This is better than the queue-or-drop behavior typical of the
category.

**An effects barrier for retry safety.** The single strongest idea in the
codebase. Once a scheduled run performs a side-effecting tool,
`MarkEffectsStarted` records it, and both failure retry
(`scheduler_queue.go` `Complete`) and crash recovery (`Recover`) then refuse to
retry automatically. Side-effect classification is fail-safe: only a whitelist
of read-only tools is exempt, unknown tools count as effectful
(`residentToolHasSideEffects`). This is at-most-once reasoning for effectful
work, which agent frameworks almost never attempt.

**The provider seam is cut in the right place.** `residentStreamWire` lets the
Codex responses API and the Claude messages API share a single tool loop, with
each wire owning its own conversation history and payload shapes. Compare with
frameworks that abstract at the "provider" level and end up with the tool loop
duplicated per provider.

**Careful persistence.** `SaveJSONAtomic` is temp file + fsync + rename + parent
directory fsync, and a file that fails to decode is quarantined to
`<name>.corrupt-<ns>` so a bad state file degrades to defaults instead of
blocking startup.

**Correct SSRF defense.** `webtools.go` validates at `DialContext` time by
re-resolving the host and rejecting private/loopback addresses, validates every
redirect hop, and clears the proxy. Doing the check at dial time rather than on
the URL is what actually defeats DNS rebinding.

**Other things worth keeping:** stale-run rejection on every tool call
(`EnforceRunScope`), a generic `tool.Registry[C]` with startup verification that
every spec has a handler and vice versa (it panics on mismatch), turn-type tool
gating (`residentToolAllowed`), and zero external dependencies.

## Findings

Severity: **S1** exploitable or data-destroying today; **S2** structural, costs
compound; **S3** quality and scaling debt.

### S1-1 Any website can execute commands on the host

Three facts compose into a drive-by RCE:

1. No HTTP authentication (acknowledged in README), and no Origin,
   `Sec-Fetch-Site`, or CSRF token check anywhere in the tree.
2. `readJSON` (`http_helpers.go`) ignores `Content-Type` entirely, and
   `readAgentMessageRequest` falls through to it for any non-multipart body.
3. A `dev` turn executes `bash -lc` with no approval and no sandbox
   (`tool_bash.go`), inheriting everything the Karoz process can reach.

A page the user visits can `fetch()` `POST /api/projects/{id}/agents/karoz/messages`
with `Content-Type: text/plain` and a JSON body specifying `"type":"dev"`. That
is a CORS *simple request*: no preflight is sent, the browser delivers it, and
the handler parses it. The attacker cannot read the response, but the side effect
has already happened. Binding to loopback does not mitigate CSRF, and
`warnIfNonLoopbackAddr` addresses a different threat.

Direction: reject non-`application/json` bodies on state-changing routes;
require `Sec-Fetch-Site: same-origin` or a matching `Origin`; add a
per-session token minted into the served page. All three are cheap relative to
the exposure.

### S1-2 Task isolation breaks at the merge step, with no concurrency control

The README promises "your main checkout stays clean" and "parallel work never
collides". `runDevelopmentTask` (`task_executor.go`) finishes by operating
directly on the user's main working tree:

```
git checkout <baseRef>        # in project.Path
git merge --no-ff <branch>    # in project.Path
```

If the user is on another branch or holds uncommitted changes, this switches
branches under them; the code notices the dirty case, logs it, and proceeds
anyway. Compounding it:

- `runTaskAsync` / `startTaskAsync` spawn bare goroutines with no per-repository
  lock, so two tasks in one project race on the same index and HEAD.
- `invokeTaskExecutor` is called with `context.Background()`, so a task cannot
  be cancelled. There is no stop endpoint; `recoverInterruptedTasks` only
  relabels live tasks as `failed` at startup.
- Failed and interrupted runs leave their worktree and `karoz/task-…` branch
  behind; `.karoz/worktrees/` accumulates.

The worktree boundary itself is the right idea. The commit path should reach the
base branch without touching the working tree (`git fetch`-style ref update or a
dedicated integration worktree), tasks should serialize per repository, and the
executor should take a cancellable context with worktree cleanup on terminal
states.

### S2-1 Every turn re-flattens the whole world into one text prompt

`buildResidentAgentPromptWithMemoryQuery` (`agent_prompt.go`) rebuilds, per
turn: ~40 always-on rule bullets, the control-plane contract, the turn contract,
project and identity blocks, skills, teammates, collaboration topology, recent
team activity, pending handoffs, pending memory, retrieved memory, the
blackboard, a rolling summary, and up to 50 messages of conversation delta — all
concatenated into a single string, sent with `store: false`.

Consequences: prior tool calls and results are not preserved as structured items,
so the model cannot see its own trajectory across turns (only text records of
it); prompt caching is impossible, so every turn pays full input price; and cost
grows with project state rather than with the question. A concrete hot spot:
`renderRecentTeamActivity` walks the entire global inbox map under `a.mu` on
every prompt build.

Direction: keep a structured message array per session with a stable prefix
(rules + identity) so it can be cached, carry tool call/result items forward, and
move volatile sections (blackboard, backlog) behind tools the agent calls when it
needs them rather than unconditional injection.

### S2-2 Prompt prose is used as the policy engine

The prompt carries rules the runtime is already positioned to enforce:

- "Evidence rule: never claim you discussed, aligned with, notified, or handed
  off to another agent unless a successful `send_to`/`reply_to` tool result in
  the current work proves it." The runtime knows exactly which tools this run
  called.
- "never `reply_to` Karoz" — a target check in the tool handler.
- "Do not claim that you created a task unless an explicit tool call has created
  one" — same class.

Unverifiable, drifts with every model change, and inflates the prompt (feeding
S2-1). Where the invariant is checkable, it belongs in the tool handler as a
rejection or in a post-turn validation, with the prompt only explaining *intent*.

### S2-3 A user-initiated run dies with the HTTP connection

`api_agents.go` derives the run context from `r.Context()`, and
`streamAgentMessage` runs the whole turn inside it. Closing the browser tab
cancels the run. That contradicts the "resident engineer" positioning. The
scheduler path already models this correctly (`runScheduledAgentQueue` uses
`context.Background()`); user turns should be decoupled the same way, with the
SSE connection as an *observer* of a run that survives it, and reconnect
replaying from the stored message log.

### S3-1 Global hardcoded budgets, too tight, not per-turn-type

```
maxCodexToolOutputChars  = 12000
maxResidentToolRounds    = 8
residentToolPhaseTimeout = 90 * time.Second
residentFinalTimeout     = 30 * time.Second
```

Ninety seconds covers the *entire* tool phase, while a single `bash` call
defaults to a 60s timeout and may request 300s — one command can consume the
whole turn. `ask` and `dev` turns have legitimately different budgets, and none
of these are configurable. Related: the final-summary fallback string in
`provider_codex_stream.go` is hardcoded Chinese in an otherwise English
codebase, which suggests these constants and messages were set under pressure
and never revisited.

### S3-2 One mutex over one god object, plus whole-map rewrites

`app` (`types.go`) holds 20+ maps guarded by a single `sync.Mutex`, and every
`save*` serializes an entire map for all projects
(`saveTasks`, `saveAgents`, `saveInbox`, …). Fine for one user and one project,
but it is an O(total state) write per mutation and a single serialization point.
The lock is already awkward — `saveArtifacts` carries a comment explaining that
project paths must be resolved *before* taking `a.mu` because `projectByID`
locks it internally. That is a lock-ordering hazard documented rather than
designed away.

### S3-3 The `internal/` layering is nominal

14.2k lines sit in `package main`; 1.9k in `internal/`. The domain packages are
largely anemic — `types.go` is a wall of aliases re-exporting them — while the
behavior lives on `*app`. `internal/runtime` (queue, worker, state machine) and
`internal/tool` are genuine exceptions and show what the rest could look like.
As it stands the runtime cannot be tested or reused without the whole
application.

### S3-4 Scheduler starvation has no bound

`SchedulerWorker.waitUntilStarted` polls `Begin` every 25ms indefinitely, with
`ctx` = `Background` (from `runScheduledAgentQueue`). Because only one run per
agent may be active, a long user turn blocks that agent's handoff queue for as
long as it lasts, and the job's own timeout only starts counting *after* the run
begins. No deadline, no backoff, no fairness or starvation guarantee.

### S3-5 The memory gate inverts its own cost model

`memoryRetrievalQueryFor` inserts a blocking model call (up to 15s) before the
first token of every eligible turn, purely to decide whether to include ~6
memory entries in a prompt that is already very large. The queries it generates
then feed `relevantMemoriesFor`, which is term/substring scoring with no
embeddings. The semantic budget is spent on the gate while retrieval stays
lexical. Either make retrieval semantic and drop the gate, or keep the gate
non-blocking (decide from the previous turn, or run it concurrently with prompt
assembly).

## Proposed order of work

1. **S1-1.** An Origin/`Sec-Fetch-Site` + content-type middleware. Smallest
   change, only finding reachable from outside the machine.
2. **S1-2.** Commit path that never touches the main working tree, per-repository
   task serialization, cancellable executor, worktree cleanup.
3. **S2-2.** Move checkable prompt rules into tool-level enforcement; shrink the
   preamble as a side effect.
4. **S3-1.** Budgets per turn type, configurable, with the bash timeout drawn
   from the remaining tool-phase budget rather than set independently.
5. **S2-3.** Decouple run lifetime from the SSE connection.
6. **S2-1.** Structured, cacheable session history. Largest payoff and largest
   blast radius — worth its own milestone.
