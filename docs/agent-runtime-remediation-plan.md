# Agent Runtime Remediation Plan

> Historical M1–M6 execution plan. New runtime findings are split into
> `docs/agent-runtime-immediate-remediation.md` (implementation-ready) and
> `docs/agent-runtime-candidate-designs.md` (not approved or scheduled).

Status: historical. M1–M6 were approved for phased implementation on
2026-07-28. The later M7/M8 amendment is superseded by the two documents above.

Owner: `builder`

Review gate: `reviewer`

Source review: `docs/agent-runtime-review.md`

## 1. Product boundary

Karoz is currently a single-user local web Studio. This plan improves local
execution safety and runtime correctness without turning Karoz into a hosted
multi-user service.

In scope:

- safe task worktree integration;
- task cancellation and recoverable worktree lifecycle;
- resident Run lifetime independent from the browser connection;
- coherent turn/tool budgets and bounded scheduler waits;
- checkable runtime policy enforcement;
- non-blocking memory retrieval;
- structured cross-turn model history and prompt-size control;
- small loopback/local-request hardening that does not introduce login.

Out of scope:

- login, accounts, JWT, OAuth for the Studio, RBAC, tenant isolation;
- network deployment or LAN access;
- database migration solely for scale;
- a general rewrite of the `app` object or `internal/` packages;
- parsing model prose to decide whether the model made an unsupported claim;
- embedding-based or vector memory retrieval.

## 2. Review finding disposition

| Finding | Disposition | Milestone |
|---|---|---|
| S1-1 cross-site access to host tools | Reduce to local defense-in-depth; no auth/token system | M4 |
| S1-2 unsafe final merge/concurrency/cancellation/cleanup | Required | M1, M2 |
| S2-1 flat, repeated prompt/history | Required quality milestone after lifecycle work | M6 |
| S2-2 prompt prose as policy | Audit and enforce only mechanically checkable invariants | M5 |
| S2-3 Run owned by SSE request | Required | M3 |
| S3-1 incoherent fixed budgets | Required | M4 |
| S3-2 global mutex/whole-map JSON | Defer; optimize only measured hot spots | — |
| S3-3 nominal package layering | Defer; reuse is not a current product goal | — |
| S3-4 unbounded scheduler wait | Required | M4 |
| S3-5 blocking memory gate | Required latency fix | M5 |

## 3. Target task lifecycle

The coding executor continues to work exclusively in the task worktree.

```text
pending
  -> running
  -> verifying
  -> committed
      -> merging -> done
      -> waiting_merge

running/verifying -> cancelling -> cancelled
waiting_merge --POST /merge--> merging -> done | waiting_merge
```

`committed` and `cancelling` may be persisted or short-lived implementation
states, but emitted runtime events must preserve the transitions.

Automatic merge is allowed only when all conditions are true under one
per-project integration lock:

1. the task has a valid task commit and recorded base branch/base commit;
2. the primary checkout has no tracked, staged, unstaged, or untracked changes;
3. the primary checkout is currently on the recorded base branch;
4. the recorded base commit remains an ancestor of the task commit;
5. the current base ref has not been rewritten away from the recorded base
   history;
6. the merge succeeds without unresolved conflicts.

The base branch may advance normally after the task starts. Karoz may merge the
task branch into the newer base while holding the integration lock. A rewritten
base or a merge conflict enters `waiting_merge`.

`waiting_merge` is not a failed or terminal task. It must not:

- deliver resident task-completion hooks;
- mark a WorkPlan task attempt terminal;
- advance a WorkPlan step to review;
- be treated as a runnable task that starts the coding executor again;
- keep the whole runtime non-quiescent when no process is running.

It must remain visible in the task list/detail and expose a concrete reason:

- `workspace_dirty`;
- `branch_mismatch`;
- `base_rewritten`;
- `merge_conflict`;
- `integration_busy`;
- `integration_failed`.

## 4. M1 — Safe task integration

### Backend

1. Extend `internal/task.Task` compatibly with:

   - `base_commit`;
   - `merge_blocked_reason`;
   - `merge_blocked_detail`;
   - `merge_attempts`;
   - existing `task_branch`, `commit_sha`, and `merged_at` remain authoritative.

2. Resolve and persist the base branch and base commit before creating the task
   worktree. Do not auto-`git add` or auto-commit the user's primary checkout
   when the repository has no `HEAD`; return a clear task failure telling the
   user to initialize the repository.

3. Split `runDevelopmentTask` into explicit phases:

   - prepare worktree;
   - invoke executor;
   - verify;
   - commit task branch;
   - attempt integration.

4. Add a per-project integration lock. Parallel task execution remains allowed;
   only final integration and repository worktree maintenance are serialized.

5. Under the lock, repeat every merge precondition immediately before mutation.
   Never run `git checkout` in the primary checkout.

6. When the primary checkout is clean and already on the base branch, run the
   no-fast-forward merge. Record the pre-merge `HEAD`.

7. If the merge conflicts:

   - run `git merge --abort`;
   - verify that `HEAD`, index, worktree status, and current branch equal the
     pre-merge snapshot;
   - enter `waiting_merge` with conflict detail;
   - if rollback verification fails, use `integration_failed`, emit an error,
     and preserve all recovery data. Do not silently claim the checkout is safe.

8. Dirty checkout, wrong branch, rewritten base, or a held integration lock
   transitions directly to `waiting_merge`; it is not task execution failure.

9. Add `POST /api/projects/{projectID}/tasks/{taskID}/merge`. It accepts only
   `waiting_merge`, re-runs the same locked preflight, and is idempotent:

   - already merged returns the current `done` task;
   - still blocked remains `waiting_merge`;
   - duplicate concurrent requests produce only one merge commit.

10. Update status classifiers, runtime backlog, plan integration, task hooks,
    recovery logic, and resident task tools for `waiting_merge`.

### Studio UI

- Show “Waiting to merge” and the blocked reason/detail.
- Show task branch, commit, base branch, and base commit.
- Show a `Retry merge` button only for `waiting_merge`.
- Refresh the task after retry and surface dirty/branch/conflict guidance.
- Do not present `Run task` as the recovery action for `waiting_merge`.

### Required tests

Use temporary real Git repositories:

- clean primary checkout on base branch auto-merges and produces one merge commit;
- dirty tracked file blocks without changing `HEAD`, index, branch, or files;
- staged-only, untracked-only, and deleted-file dirtiness also block;
- clean checkout on another branch blocks without checkout;
- base advances normally and a non-conflicting task still merges;
- rewritten base blocks;
- conflict abort restores the exact pre-merge checkout;
- two concurrent integrations serialize and cannot corrupt the index;
- duplicate merge API calls are idempotent;
- `waiting_merge` sends no completion hook and does not complete a plan step;
- repository without `HEAD` does not auto-commit user files.

### M1 review gate

Reviewer must independently reproduce the dirty, wrong-branch, conflict, and
concurrency cases with real Git commands. M1 is not accepted from mocked command
fragments alone.

## 5. M2 — Task cancellation and worktree lifecycle

### Backend

1. Give every running task an owned cancellable context stored by
   `projectID/taskID`. Pass it through:

   - task executor;
   - verification command;
   - Git operations that can block.

2. Add `POST /api/projects/{projectID}/tasks/{taskID}/cancel`.

   - `running`/`verifying` transition to `cancelling`, cancel the process tree,
     then persist `cancelled`;
   - a task already entering the locked merge section returns `409` rather than
     cancelling midway through primary-checkout mutation;
   - cancellation and the transition into `merging` linearize through the same
     task/project lock boundary: a cancellation accepted before that transition
     must result in zero primary-checkout mutation, while a cancellation arriving
     after it must be rejected rather than reported as accepted;
   - repeated cancel is idempotent.

3. Replace bare `context.Background()` in task execution with the task context.
   Keep the existing provider timeout as a child deadline, not the owner.

4. Worktree retention:

   - successful merged tasks: remove the task worktree after commit metadata is
     durable;
   - `waiting_merge`: preserve branch and commit; the worktree may be removed
     only after confirming it is clean and the commit is reachable;
   - failed/cancelled task with uncommitted changes: preserve the worktree and
     mark it recoverable;
   - failed/cancelled task with a clean worktree: allow explicit cleanup;
   - provide `POST .../tasks/{taskID}/cleanup` for safe, status-checked cleanup.

5. Startup recovery keeps the existing fail-closed behavior for interrupted
   processes but must preserve recoverable worktree metadata.

### Studio UI

- Show `Cancel` for running/verifying tasks.
- Show cancellation progress and final state.
- Show whether a failed/cancelled worktree contains recoverable changes.
- Offer cleanup only when the backend reports it safe.

### Required tests

- cancellation stops Codex/Claude aliases and descendant processes (table the
  same process-group behavior over both provider aliases);
- cancellation during verification stops the verifier;
- cancel/cancel and cancel/finish races have one terminal result;
- a barrier-driven real-Git case forces cancellation after the old final
  context check but before integration-lock acquisition and proves that primary
  HEAD, index, and worktree remain unchanged;
- concurrent stale start plus cancellation has exactly one owned child process,
  retains its current cancellation handle, and terminates that process;
- cancellation cannot interrupt the merge critical section;
- recoverable dirty work is never deleted automatically;
- successful cleanup removes only the exact registered task worktree.

### M2 review gate

Reviewer must inspect process termination and filesystem preservation, not only
task status JSON.

## 6. M3 — Detach resident Run lifetime from SSE

### Runtime contract

The Run owns execution. HTTP connections are observers.

1. Introduce a per-Run event ledger:

   - monotonic `seq`;
   - event type and payload;
   - bounded count/bytes;
   - terminal marker;
   - subscriber channels that may disconnect independently.

2. The message POST continues returning SSE for compatibility, but it:

   - creates the Run;
   - starts a background Run worker with a Run-owned context;
   - subscribes the request to the Run ledger;
   - removes only the subscriber when the HTTP request closes.

3. Add
   `GET /api/projects/{projectID}/agents/{agentID}/runs/{runID}/events?after={seq}`
   for replay plus live subscription.

4. Existing explicit Run cancellation remains the only user disconnect path.
   Scheduler cancellation and stale-run compare-and-set rules remain intact.

5. On ledger truncation, emit a `reset` event instructing the UI to reload
   persisted chat and resume from the current ledger floor.

6. The Run worker, not the POST handler, owns terminal transition and final
   message persistence. Remove request-level `defer finishAgentRun`.

### Implementation boundary

Keep this as an in-memory, per-process delivery ledger; it is not a second
chat-history store.

- Key ledgers by immutable `runID`. A ledger owns `nextSeq`, retention floor,
  bounded events/bytes, terminal state, and observer channels. Each emitted
  event carries its `run_id` and assigned sequence.
- Creating a Run installs both the ledger and a Run-owned context derived from
  `context.Background()`, before starting the worker. Persist the user message
  before the worker can assemble its context. An HTTP request context is never
  the parent of this context.
- A single worker function is shared by direct POST and scheduled Runs. Its
  provider/tool callbacks append to the ledger; they do not write an HTTP
  response. Its one guarded finalization path persists at most one assistant
  result, appends the terminal event, and performs the existing Run
  compare-and-set transition.
- Subscription must atomically decide truncation, capture replay, and register
  the observer, so no event can fall between replay and live follow. Slow or
  disconnected observers are detached only; they cannot block or cancel the
  worker.
- If `after` precedes the ledger floor, deliver a typed `reset` with the new
  floor before live events. The client reloads persisted chat, sets its cursor
  to that floor, and reconnects rather than attempting to reconstruct dropped
  deltas locally.
- Explicit Run cancel signals the Run-owned context. It is the worker that
  records the cancelled terminal event and closes observers. Request write or
  read errors merely unsubscribe that request.
- Keep a bounded terminal-ledger retention window sufficient for immediate
  reconnect. Once evicted, the UI falls back to persisted chat; eviction must
  not alter the durable AgentMessage history or active-Run compare-and-set
  behavior.

### Studio UI

- Store active `run_id` and last event sequence.
- On project/agent load, query active Run and attach to its event stream.
- A refresh reconstructs persisted chat, replays missing Run events, and
  continues rendering deltas/tool cards without duplication.
- The current-turn token ledger must reconcile replayed and live events exactly
  once.
- Replay is a renderer input, not merely a signal to poll message history:
  transient assistant deltas and tool cards must be rendered through the same
  sequence-deduplicated path as a locally initiated stream. Persisted history
  is the terminal reconciliation source, not a replacement for in-flight
  deltas.
- On `reset(floor)`, define `floor` as the last discarded sequence, so the
  first retained event is `floor + 1`. Rebuild durable history, set the dedupe
  cursor to `floor`, and then apply retained replay/live events exactly once.

### Required tests

- cancelling the POST/SSE request does not cancel the Run;
- explicit cancel still cancels it;
- reconnect with `after` replays each event once and then follows live events;
- reconnect during a tool call preserves tool start/result ordering;
- two observers do not duplicate backend execution;
- terminal Run persists one assistant result and one terminal event;
- ledger truncation produces a deterministic reset path;
- retained ledger bytes as well as event count are bounded, including an
  oversized payload case;
- a slow observer is explicitly detached (never silently dropped), then can
  reconnect from its cursor and receive a contiguous replay;
- reset followed by the first retained event, `floor + 1`, renders it exactly
  once;
- explicit cancellation and successful worker completion race through one
  atomic outcome: either cancellation wins with no assistant success commit,
  or success wins and cancellation is not reported as accepted;
- a real browser refresh during an in-flight delta plus tool call renders the
  partial assistant content and each tool event once, then reconciles to one
  terminal persisted result;
- provider contract tests pass for Codex and Claude.

### M3 review gate

Reviewer must exercise a real browser refresh during assistant and tool
streaming and verify no duplicate/missing visible events or console errors.

## 7. M4 — Budgets, scheduler bounds, and local HTTP boundary

### Turn budgets

1. Replace global constants with a `ResidentTurnBudget` selected by normalized
   turn type:

   - total duration;
   - tool-phase duration;
   - final-response reserve;
   - maximum model/tool rounds;
   - maximum model-visible tool output.

2. Preserve current ask behavior as the compatibility baseline. Give plan/dev
   larger configurable budgets. Environment configuration is sufficient; no
   settings UI is required.

3. All provider calls and tools derive deadlines from the remaining total
   budget. A Bash request timeout is:

   `min(requested timeout, remaining tool-phase time)`.

4. Emit a typed `budget_exhausted` result with phase and elapsed/limit fields.
   Always reserve enough time for a concise final response.

### Scheduler

1. Replace the 25 ms indefinite polling loop with a Run-finished wake signal
   plus a bounded start deadline.

2. The wait deadline starts when the scheduled job is claimed, not after the
   Agent Run starts.

3. Waiting for a busy Agent is not an effect and must not consume an effectful
   retry. On deadline, persist a visible blocked/failed reason and follow the
   existing retry policy without bypassing the effects barrier.

4. Preserve per-agent FIFO order and add tests proving a later job cannot pass a
   blocked earlier job.

### Local HTTP boundary

This is defense-in-depth, not authentication:

- fail startup on a non-loopback listen address;
- reject `Sec-Fetch-Site: cross-site` on state-changing routes;
- reject a present non-matching `Origin`;
- require `application/json` for JSON mutations and allow multipart only on the
  attachment route;
- do not add users, login, cookies, session tokens, RBAC, or CORS configuration.

Requests without browser fetch headers may remain available to same-machine CLI
clients when their Content-Type is valid.

### Required tests

- ask/plan/dev choose the expected budget;
- requested Bash timeout is clamped to remaining budget;
- final response reserve survives tool exhaustion;
- scheduler wait terminates, preserves FIFO, and respects effects-started rules;
- non-loopback startup fails;
- cross-site browser mutation and `text/plain` JSON are rejected;
- same-origin JSON, multipart upload, and local CLI JSON still work.

## 8. M5 — Runtime policy audit and non-blocking memory

### Policy

Audit prompt rules and classify them:

1. Mechanically checkable action invariants belong in tool handlers:

   - target/route restrictions;
   - turn-type tool availability;
   - stale Run scope;
   - one terminal handoff action;
   - effects barrier;
   - task/status transition validity.

2. Behavioral guidance remains concise prompt text:

   - do not parse final prose to decide whether the model “claimed” a handoff;
   - do not reject otherwise valid responses through keyword matching;
   - retain tool-result evidence in the transcript instead.

3. Remove redundant prompt bullets only after equivalent enforcement and tests
   exist. The existing `reply_to`-Karoz check is already enforced and should not
   be reimplemented.

### Memory latency

1. Remove the blocking side-channel model call from the first-token path.

2. Use the raw user text with the existing cheap lexical scorer for the current
   turn. Explicit memory cues continue to work.

3. If semantic analysis is retained experimentally, run it outside the current
   response critical path and apply it only to a future turn/cache. It must not
   delay the current first token.

4. Log retrieval duration, selected memory count, and prompt contribution
   without logging sensitive memory content.

### Required tests

- every checkable policy has a handler-level negative test;
- both providers receive identical allowed tool contracts;
- memory analyzer failure or slowness adds no current-turn latency;
- explicit memory cues and ordinary lexical retrieval still return expected
  entries.

## 9. M6 — Structured, bounded cross-turn history

This is the largest blast-radius milestone and starts only after M3 and M5 pass.

### Data model

Introduce a provider-neutral transcript item with:

- sequence and Run ID;
- role;
- item kind: message, tool call, tool result, interrupt, status;
- tool call ID/name/arguments/result/success where applicable;
- visible vs model-only marker;
- timestamp.

Keep the current visible `AgentMessage` API compatible while migrating model
context to the structured transcript. Existing stored messages load as ordinary
message items; no destructive migration is allowed.

### Prompt assembly

- stable prefix: runtime rules, agent identity, provider-neutral tool contract;
- bounded short-term structured transcript;
- dynamic project context with explicit per-section size limits;
- preserve tool call/result relationships across turns;
- stop scanning the complete global inbox during prompt construction; query the
  current project/group and cap before rendering;
- move optional bulky context behind existing read tools where doing so does not
  break handoff ownership or pending-work visibility.

Do not promise provider prompt caching as a correctness requirement. Measure:

- total estimated input tokens;
- stable-prefix tokens;
- transcript tokens;
- each dynamic section;
- build duration.

### Required tests

- Codex and Claude adapters render equivalent provider-neutral transcripts;
- tool call/result pairs survive the next user turn;
- interrupt ordering survives persistence/reload;
- legacy message JSON migrates losslessly;
- prompt sections obey limits under large inbox/memory/history fixtures;
- no full global inbox scan occurs while building one project prompt;
- existing context counter matches the model-bound transcript estimate within
  the documented estimator tolerance.

## 10. Cross-cutting observability

Use structured logs with identifiers, not new external infrastructure.

Task fields:

- project/task ID;
- base branch/base commit/current base commit;
- task branch/commit;
- lifecycle transition;
- merge preflight result and blocked reason;
- integration-lock wait;
- cancellation/cleanup outcome.

Run fields:

- project/agent/Run ID;
- provider/model/turn type;
- event sequence;
- observer attach/detach;
- phase budget/elapsed;
- terminal reason.

Scheduler fields:

- job ID/kind/agent;
- queue age;
- Run-start wait;
- attempt/effects-started;
- terminal reason.

## 11. Persistence and compatibility

- New JSON fields use `omitempty` where absence has an unambiguous legacy
  meaning.
- Unknown legacy task statuses continue loading; normalization happens through
  one status helper.
- `waiting_merge` is persisted and restart-safe.
- Active task/Run process recovery remains fail-closed; Karoz must not claim a
  process survived server restart.
- Existing message/history files are backed up by the atomic store before any
  one-time transformation. Prefer lazy compatibility conversion over rewriting
  the whole file at startup.
- HTTP additions are backward-compatible; existing message POST streaming
  remains available through M3.

## 12. Rollback

- Each milestone is a separate commit/review unit.
- M1 rollback preserves task branches and commits; never reset or delete the
  user's primary checkout.
- M2 cancellation/cleanup failure defaults to preserving the worktree.
- M3 may retain a temporary `KAROZ_DETACHED_RUNS=0` compatibility switch for one
  review cycle, removed after browser reconnect acceptance.
- M5 can restore lexical-only retrieval immediately without data migration.
- M6 keeps legacy visible messages authoritative until both provider contract
  suites and migration fixtures pass.

## 13. Execution and review order

```text
M1 safe integration
  -> reviewer Git/data-safety gate
M2 cancellation + cleanup
  -> reviewer process/filesystem gate
M3 detached Run + reconnect
  -> reviewer browser/runtime gate
M4 budgets + scheduler + local HTTP boundary
  -> reviewer timing/HTTP gate
M5 policy + memory latency
  -> reviewer contract/latency gate
M6 structured history
  -> reviewer provider/migration/context gate
```

Builder must:

- begin each milestone from the current reviewed tree;
- report exact changed files and tests;
- stop at each review gate;
- address must-fix feedback before starting the next dependent milestone;
- preserve unrelated working-tree changes.

Minimum validation at every milestone:

```bash
go test ./...
go vet ./...
```

Also run the milestone-specific real Git, process, HTTP, provider-contract, or
browser checks described above. Source-fragment assertions are guardrails only
and do not replace behavioral tests.
