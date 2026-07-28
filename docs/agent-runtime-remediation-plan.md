# Agent Runtime Remediation Plan

Status: approved for phased implementation (2026-07-28); amended 2026-07-28 with
the review supplement (M6 amendment, M7, M8)

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
- small loopback/local-request hardening that does not introduce login;
- memory retrieval that works for the project's primary input languages, and a
  write path that does not accumulate duplicate or superseded knowledge (M7);
- a tool surface whose boundaries are expressed in shape rather than in
  description prose (M8).

Out of scope:

- login, accounts, JWT, OAuth for the Studio, RBAC, tenant isolation;
- network deployment or LAN access;
- database migration solely for scale;
- a general rewrite of the `app` object or `internal/` packages;
- parsing model prose to decide whether the model made an unsupported claim;
- embedding-based or vector memory retrieval, and the automatic
  extraction/consolidation pipeline in `docs/memory-write-path.md`; M7 makes
  lexical retrieval correct first, and both remain candidates afterwards.

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

Supplement findings (2026-07-28). M1–M5 have landed; S2-1 is half addressed and
its remaining wire-format half is re-filed as S2-4.

| Finding | Disposition | Milestone |
|---|---|---|
| S1-3 Chinese memory retrieval returns nothing | Required; schedule ahead of M6 | M7 |
| S2-4 transcript flattened to prose at the wire | Required; completes M6 | M6 amendment |
| S2-5 reasoning content billed and discarded | Required; completes M6 | M6 amendment |
| S2-6 no memory dedup/supersede/decay | Required | M7 |
| S2-7 checkpoint truncation composes with S1-3 | Required; archive fix first | M7 |
| S2-8 tool surface has no owner; prose boundaries | Required; new milestone | M8 |
| S3-6 shared memory retrieval budget; dead `Priority` | Required | M7 |
| S3-7 no project memory tier | Required | M7 |
| S3-8 stable prefix not byte-stable | Required regression guard | M6 amendment |
| S3-9 tool contract billed twice | Required | M8 |
| S3-10 per-turn-only tool output budget | Required; completes M4 | M8 |
| S3-11 no uniform tool result envelope | Required | M8 |

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

### M6 amendment (2026-07-28)

The storage half of M6 has landed: `AgentTranscriptItem` carries tool identity,
arguments, result, and success, and legacy `AgentMessage` records merge in
lazily without rewriting `agent-messages.json`. The remaining work is the wire
half, which is where the capability and the cost saving actually are.

1. Stop rendering transcript items as prose. `promptAgentTranscriptBody`
   currently emits `tool_call id=… name=… arguments=…` text that the model must
   re-parse, and only the current turn reaches the provider as native items.
   `agentTranscriptForModel` must feed a provider-neutral item list, and each
   wire adapter maps it to native items:

   - Codex: `function_call` / `function_output` entries in the `input` array;
   - Claude: `tool_use` / `tool_result` content blocks.

   Legacy items lacking a `ToolCallID` must degrade to plain message items
   rather than emitting an unpaired native tool item that a provider rejects.

2. Capture reasoning items from the provider stream and replay them, in order,
   in subsequent requests *within the same turn*. `include:
   ["reasoning.encrypted_content"]` is already requested and paid for, but
   `provider_codex_sse.go` has no reasoning branch and nothing replays it, so a
   multi-step turn re-derives its plan every round. Encrypted payloads are
   passed through opaquely and never logged or persisted. Dropping reasoning at
   turn boundaries is intended.

3. Remove the duplicate tool contract (S3-9). The `tools` array is the single
   schema source; `renderProviderNeutralToolContract` retains only orchestration
   rules that a schema cannot express.

4. Make the stable prefix byte-stable (S3-8). Move every per-turn-varying field,
   including the current chat turn type, after `stablePrefixChars`. The prefix
   may vary by agent identity and role; it must not vary by turn type or by
   wall-clock time.

### M6 amendment required tests

- a turn following a tool call sends native tool items for the *previous* turn,
  not prose, on both providers;
- a legacy transcript item without a tool call ID never produces an unpaired
  native tool item;
- reasoning items captured in round one are replayed in round two of the same
  turn, and are absent from the next turn;
- reasoning payloads never appear in logs or persisted state;
- two prompt builds for the same `(agent, turn type)` produce byte-identical
  stable prefixes, and a turn-type change alters only bytes after the boundary;
- the tool contract appears once per request; every tool in the `tools` array is
  absent from the prose preamble.

## 10. M7 — Memory retrieval and write-path correctness

M7 addresses the only supplement finding that is a functional failure today.
S1-3 must land before M6, independently of the rest of this milestone: it is
small, it repairs live behavior, and M6's archive fallback is not trustworthy
without it.

### M7.1 CJK-aware scoring (S1-3)

1. `memoryMatchScore` derives terms from `strings.Fields`, so a Chinese question
   becomes one term and scoring degrades to verbatim-substring containment.
   Replace term derivation with a segmenter that splits ASCII runs on whitespace
   and CJK runs into rune bigrams.

2. Weight a bigram hit below a whole-word hit, and keep full-phrase containment
   ranked above both, so fragment noise cannot outrank a real match.

3. Apply the same segmentation to indexing and querying. `relevantMemoriesFor`
   and `searchArchive` must not diverge.

4. `memoryWordCount` is already CJK-aware; the pre-filter and the scorer must
   agree on what a term is.

5. Retrieval currently scans every entry on every turn. Once terms are
   well-defined, add an inverted index from term to entry so retrieval is a set
   intersection. This is a correctness-neutral optimization and may be deferred
   within M7 if measurements do not justify it.

### M7.2 Write path (S2-6)

1. Detect near-duplicates on write and update the existing entry instead of
   appending. Duplicates are a recall problem, not a storage problem: they
   consume the bounded injection budget and evict relevant entries.

2. Add `supersedes` / `superseded_by`. `record_decision` accepts an optional
   `supersedes_id`, archives the predecessor, and retrieval excludes anything
   superseded.

3. Add explicit recency decay to the score. `UpdatedAt` currently breaks ties
   only when scores are equal, so a longer stale entry can outrank its own
   correction.

4. Do not start the `docs/memory-write-path.md` extraction/consolidation
   pipeline in this milestone. It is correct in direction but depends on items
   1–3; automatic extraction without them fills the store with duplicates
   faster.

### M7.3 Layering and scope (S3-6, S3-7)

1. Replace the single `limit=6` retrieval budget with per-layer budgets:
   `decision` small and near-resident, `fact` relevance-ranked, `done`
   reachable through `search_archive` rather than unconditional injection.

2. Either wire `Priority` into ranking or remove the field. Today only
   `add_pending` sets it and no ranking path reads it.

3. Add a project-scoped `fact` tier with agent attribution, so an established
   fact does not have to be re-narrated between agents over the inbox. Existing
   per-agent entries keep their current scope; no destructive migration.

### M7.4 Checkpoint summary (S2-7)

1. Land M7.1 first. A searchable archive is the safety net for anything the
   summary drops.

2. Replace `compactAgentSummaryLine`'s fixed 280-character truncation, and
   `normalizeResidentSummary`'s keep-the-tail eviction, with one cheap model
   call producing a real summary.

3. Run it off the first-token path, following the pattern M5 applied to the
   memory gate. Summarizer failure or slowness must not delay the current turn;
   on failure, fall back to the current mechanical behavior rather than blocking.

### M7 required tests

- the S1-3 evidence table becomes a test: Chinese non-verbatim queries retrieve
  the matching decision, and the previously passing English case still passes;
- explicit cues (`记得`, `之前`, `上次`) retrieve through the same path;
- bigram fragments do not outrank whole-word or full-phrase matches;
- writing the same fact repeatedly yields one entry and does not consume more
  than one injection slot;
- a superseded decision is never returned alongside its replacement;
- a stale entry with greater literal overlap does not outrank a newer correction;
- per-layer budgets hold under a fixture where `done` entries outnumber
  `decision` entries;
- a project-tier fact is visible to a second agent in the same project and not
  to another project;
- a decision evicted from the rolling summary is still retrievable from the
  archive by a Chinese query;
- summarizer failure or timeout adds no current-turn latency and degrades to the
  mechanical summary.

### M7 review gate

Reviewer must run retrieval against Chinese fixtures directly, not only assert
that a scoring helper returns a non-zero integer.

## 11. M8 — Tool surface consolidation

The original disposition table had no tool-system entry, so this area had no
owner. Scope: 45 static resident tools with 12 gated names, overlapping verbs
disambiguated by description prose, and no per-call output accounting.

### M8.1 Collapse overlapping verbs (S2-8)

1. Demote "pick the right tool" to "fill the right enum". Models select a
   required enum value more reliably than they choose among similarly described
   tool names.

2. Consolidate the collaboration cluster. `reply_to`, `decline_handoff`, and
   `ack_inbox` become one `inbox_resolve(message_id, outcome:
   reply|decline|ack, body?)`. `send_to`, `send_to_group`, `report_activity`,
   and `mark_activity` keep distinct effects and stay separate.

3. Consolidate the memory cluster. `remember_fact`, `record_decision`,
   `mark_done`, and `add_pending` all call `createMemory` and differ only by
   layer; they become `memory_write(layer, summary, detail, priority?,
   supersedes?)`, with `drop_pending` generalized to `memory_update(id, state)`.
   This is also where M7.2's `supersedes` argument lands.

4. Keep the old names accepted as aliases for one review cycle so a stale
   provider transcript replayed from history does not fail. Aliases are not
   advertised in the `tools` array.

5. Description prose that exists to explain what a tool does *not* do is a
   symptom of a bad boundary. Remove it as the boundary improves rather than
   rewriting it.

### M8.2 State-conditional availability (S2-8)

1. Extend `residentToolAllowed` with runtime-state predicates alongside turn
   type. The prompt builder already knows the facts required.

2. Minimum set: inbox tools only with an unresolved inbox item; plan submit and
   advance only with an existing draft; artifact review only with a pending
   artifact.

3. Availability is a hint, not the enforcement boundary. Handlers keep rejecting
   invalid calls, because a replayed or hallucinated call can still arrive for a
   tool that is not currently advertised.

### M8.3 Per-call output budget (S3-10)

1. Add a per-call output cap beneath the existing per-turn `MaxToolOutputChars`,
   so one large `repo_read` or verbose `bash` cannot consume the whole turn.

2. Report the remaining allowance in truncated results, for example
   `{"truncated": true, "remaining_output_chars": N}`, so the model can narrow
   its next query instead of retrying blindly.

3. Derive both caps from the M4 budget rather than introducing a parallel
   mechanism.

### M8.4 Uniform result envelope (S3-11)

1. Standardize handler results on `{ok, data?, error?{code, message,
   retryable}}`. `retryable` states whether to retry or change approach.

2. Enforce through one helper, not per-handler convention.

3. `createMemory` currently returns a Go `error` whose model-visible form
   depends on its caller; it must return a typed envelope like every other
   handler.

4. Envelope changes alter model-visible tool output, so land M8.4 with the
   provider contract tests, not before them.

### M8 required tests

- the consolidated tools cover every effect the replaced tools could produce;
- retired tool names still dispatch as aliases and are absent from the `tools`
  array;
- an empty inbox does not advertise inbox tools, and a handler still rejects an
  inbox call that arrives anyway;
- no plan draft does not advertise submit/advance, with the same handler-level
  rejection;
- a single oversized tool result is capped per call and leaves later calls in the
  same turn a usable allowance;
- a truncated result reports the remaining allowance;
- every registered handler returns a schema-valid envelope, asserted by walking
  the registry;
- `retryable` is set correctly for a validation error versus a transient failure;
- Codex and Claude receive identical consolidated tool contracts.

### M8 review gate

Reviewer must confirm the per-turn spec payload shrank and that no consolidated
tool lost a previously reachable effect.

## 12. Cross-cutting observability

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

Memory fields (M7):

- retrieval duration;
- term count and whether CJK segmentation applied;
- candidates scanned, candidates matched, entries injected per layer;
- writes deduplicated and entries superseded;
- summarizer outcome and whether the mechanical fallback was used.

Tool fields (M8):

- tool name, advertised tool count, and why a tool was withheld;
- per-call and per-turn output allowance consumed and remaining;
- truncation events;
- envelope error code and `retryable`;
- alias dispatches, so alias removal is scheduled from evidence.

Never log prompts, attachment contents, tool secrets, memory bodies, memory
summaries, reasoning payloads, or provider credentials by default. Memory and
tool observability records counts and durations, never content.

## 13. Persistence and compatibility

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
- M7 memory fields (`supersedes`, `superseded_by`, project scope) use
  `omitempty`; an absent value keeps its current meaning, so existing
  `memories.json` loads unchanged and no startup rewrite is required.
- M7.1 changes scoring only. It must not rewrite stored entries; any index is
  derived state, rebuilt at load and safe to discard.
- M8 tool renames are additive at the dispatch layer. A persisted transcript
  naming a retired tool must still load and replay through its alias.

## 14. Rollback

- Each milestone is a separate commit/review unit.
- M1 rollback preserves task branches and commits; never reset or delete the
  user's primary checkout.
- M2 cancellation/cleanup failure defaults to preserving the worktree.
- M3 may retain a temporary `KAROZ_DETACHED_RUNS=0` compatibility switch for one
  review cycle, removed after browser reconnect acceptance.
- M5 can restore lexical-only retrieval immediately without data migration.
- M6 keeps legacy visible messages authoritative until both provider contract
  suites and migration fixtures pass.
- The M6 amendment keeps the prose transcript renderer behind a switch for one
  review cycle, so a provider rejecting native item replay degrades to the
  current behavior rather than failing the turn.
- M7.1 is scoring-only and reverts without data migration. M7.2 and M7.3 add
  fields; absence of `supersedes`/`superseded_by` and of a project tier must keep
  their current meaning, so reverting leaves existing entries readable.
- M7.4 falls back to the mechanical summary whenever the summarizer fails, so
  rollback is the default failure path rather than a separate procedure.
- M8.1 retains retired tool names as aliases for one review cycle; rollback
  re-advertises them. M8.4 changes model-visible output and reverts with the
  provider contract suites.

## 15. Execution and review order

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
M7.1 CJK-aware memory scoring        (out of order by design: small, live defect)
  -> reviewer Chinese-fixture retrieval gate
M6 structured history + M6 amendment
  -> reviewer provider/migration/context gate
M7.2-M7.4 memory write path, layering, real summary
  -> reviewer memory-correctness gate
M8 tool surface consolidation
  -> reviewer tool-contract gate
```

M7.1 precedes M6 deliberately. It is a half-day scoring fix for a failure that is
live today, and M6's "fall back to the archive" contract is not trustworthy while
archive search cannot serve a Chinese query. M7.4 must not start before M7.1 is
accepted, for the same reason.

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
