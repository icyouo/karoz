# Background Processes and Monitors

Status: revised proposal (2026-07-29). Nothing in this document is implemented
yet. This revision incorporates the useful process-watcher mechanics in Codex
while deliberately changing Codex's turn-scoped lifetime into a Karoz
server-scoped lifetime.
The safety boundary it must hold is recorded in DECISION 0003
(`.ufoo/context/decisions/0003-*.md`); read that first, because most of the
design below exists to satisfy it.

Two capabilities, deliberately specified as one feature because the second
consumes the first:

- **Background processes** — a resident agent starts a host command that
  outlives the turn, then observes its log and exit result on later turns.
- **Monitors** — a user-defined watch on a runtime condition that fires a hook
  when it matches. Background process output and exit are two of the conditions
  a monitor can watch, which is why these ship together rather than separately.

### Non-negotiable runtime boundary

The browser is only a subscriber. Closing a tab, losing the SSE connection,
navigating away, or completing the Run that created a process or monitor must
not cancel it. A background process is owned by the Karoz server process and a
monitor is owned by its persisted registry.

The boundaries are therefore:

| boundary | background process | monitor |
| --- | --- | --- |
| HTTP/SSE disconnect | continues | continues |
| creating Run completes or is interrupted | continues | continues |
| browser closes | continues | continues |
| explicit stop/disable/delete | stops | stops evaluating |
| lifetime/expiry/rate ceiling | terminal/disabled | terminal/disabled |
| Karoz graceful shutdown | child group is terminated; record becomes `interrupted` | active probe is terminated; definition survives and is re-armed |
| Karoz hard crash/SIGKILL | parent-death guard kills the descendant process group; record recovers as `interrupted` | probe guard kills the invocation; definition survives and is re-armed |
| machine powers off | child is lost; record recovers as `interrupted` | definition survives and is re-armed |

This is intentionally different from Codex Unified Exec. Codex provides a good
reference for bounded output streaming, process IDs, polling, trailing-output
drain, and a single terminal event, but it terminates its retained processes
when the current turn finishes. Karoz must not attach supervisor cancellation to
the Run, request, SSE subscriber, or browser connection.

## Motivation

`bash` is synchronous and bounded: 60s default timeout, 300s hard cap, inside a
tool phase that only gets 90s total (`residentToolPhaseTimeout`). Dev servers,
file watchers, long builds, and full test suites do not fit. Today an agent asked
to "start the dev server and check the logs" either blocks until the tool-phase
budget kills it, or reports a success it cannot have observed — which is exactly
the failure mode the prompt's evidence rules keep trying to talk the model out
of.

The reactive side has the same shape of gap. `emitRuntimeStateChanged` already
attempts to broadcast run, task, handoff, and plan transitions, and
`notifyTaskRuntimeHooks` already demonstrates the full pattern of "terminal state
wakes an agent through a scheduled run with a dedup key". But the only subscriber
is the hardcoded Karoz idle-reconcile hook. A user cannot say "when this task
fails, wake the reviewer" or "when this line shows up in the server log, tell
me". The event bus exists; it has no subscription surface.

## Part 1: Background processes

### Domain model (`internal/process`)

A process is owned by `(project, agent)`, records the Run that started it as
provenance only, and moves through a deliberately small state machine.

```go
type State string

const (
    StateStarting State = "starting"     // durable reservation; no usable child yet
    StateRunning State = "running"
    StateExited  State = "exited"      // exit code 0
    StateFailed  State = "failed"      // nonzero exit, spawn failure, lifetime cap
    StateKilled  State = "killed"      // explicit stop_process
    StateInterrupted State = "interrupted" // server restarted while running
)

// starting -> {running, failed, interrupted}
// running  -> {exited, failed, killed, interrupted}
// Terminal states are final, so a late exit callback can neither resurrect nor
// relabel a stopped process.
func CanTransition(from, to State) bool

type Process struct {
    ID, ProjectID, AgentID string
    RunID        string    // provenance; the process is NOT cancelled with it
    Command      string
    Workdir      string
    Description  string
    PID          int
    GuardPID     int
    PGID         int
    State        State
    ExitCode     int
    Error        string
    LogPath      string
    LogBytes     int64
    LogLines     int64
    LogTruncated bool      // output exceeded the cap; process kept running
    OutputSeq    uint64    // monotonically increasing complete-line sequence
    OutputGaps   []SeqRange // last 32 exact monitor-evaluation gaps
    OutputGapCount, OutputLostLines uint64
    OutputGapOldestSeq, OutputGapNewestSeq uint64
    LifetimeMS   int64
    StartedAt    time.Time
    UpdatedAt    time.Time
    EndedAt      *time.Time
}

type SeqRange struct { Start, End uint64 }
```

`interrupted` is not cosmetic. A live record is valid only while its in-memory
guard handle belongs to the current Karoz instance. `process.Normalize` forces
recovered `starting` and `running` records to `interrupted` at startup. It never
trusts or signals a recovered PID/PGID, because the operating system may have
reused them.

`State.Succeeded()` exists so callers never infer success from an exit code and
accidentally treat `killed` or `interrupted` as fine.

### Output capture (`internal/process.OutputBuffer`)

The pure half of log capture, so it is testable without spawning anything: it
accumulates writes, emits only *complete* lines (a partial write is held until
its newline arrives, so the monitor matcher and the tail view never see a torn
line), keeps a bounded in-memory tail, and enforces a byte cap. It returns the
lines it accepted and the byte count the caller should persist; the caller owns
the file handle.

A pending line is capped at 8 KiB. If a producer writes a longer line without a
newline, the matcher/tail retain the first 8 KiB with `line_truncated=true` and
discard the remainder through the next newline. The disk log remains governed
by its independent total byte cap. This prevents a newline-free stream from
turning the "partial line" buffer into unbounded memory.

```go
func NewOutputBuffer(tailLimit int, byteLimit int64) *OutputBuffer
func (b *OutputBuffer) Append(chunk string) (accepted []string, accountedBytes int64)
func (b *OutputBuffer) Flush() (accepted []string, accountedBytes int64) // trailing partial line
func (b *OutputBuffer) Tail(limit int) []string
func (b *OutputBuffer) Stats() (bytes, lines int64, truncated bool)
```

Past the byte cap the process keeps running and the record is marked truncated.
We never fill the disk, and a log is never inlined wholesale into a prompt or
tool result.

### Supervisor (`cmd/karoz/process_supervisor.go`)

- Spawns a Karoz-owned process guard as a new process-group leader, and the
  guard launches `bash -lc` in that same group. `Setpgid` alone is not a
  parent-death mechanism: ordinary Unix parent death does not kill a separate
  child process group.
- Owns a server-lifetime `supervisorCtx`. A child context is derived only from
  this context plus the process lifetime deadline. It must never be derived
  from an HTTP request, SSE subscriber, browser session, Agent Run, or resident
  tool-phase context.
- Returns a process ID after spawn and registration. The creating Run records
  provenance but owns no cancellation authority.
- Streams output through `OutputBuffer` into
  `<Settings.DataDir>/process-logs/<safe-project-key>/<id>.log`, under the same
  runtime-owned no-follow directory boundary as the reservation ledger.
- A runtime log-write failure is terminal: the collector records the bounded
  error, kills/waits the process group and transitions to `failed`. Karoz does
  not leave an unobservable daemon running after its audit/log sink is lost.
- Keeps the last 200 lines in memory to back the monitor output matcher and
  cheap `list_processes` summaries without touching disk.
- Publishes only complete output lines as `process_output` events. Every event
  carries `(process_id, output_seq)`; the sequence is incremented under the same
  lock that accepts the line, so concurrent stdout/stderr reads cannot reorder
  monitor input. Stdout and stderr retain a `stream` label even though the log
  view is aggregated.
- Enforces a per-project concurrency cap so a runaway agent cannot fork without
  bound.
- `stop_process` sends SIGTERM to the group, waits a grace period, then SIGKILL.
- A per-process exit watcher waits for the child, gives output readers a bounded
  trailing-output drain window, flushes a final partial line, performs one legal
  terminal transition, persists it, and emits one `process_changed` event. Its
  stable event ID is `process/<id>/terminal`; late exit callbacks and concurrent
  stop/lifetime callbacks are idempotent.
- A process hitting its lifetime cap is terminated as a group and becomes
  `failed` with `Error = "lifetime exceeded"`. Explicit user/agent stop becomes
  `killed`; server loss is the only source of `interrupted`.
- Graceful Karoz shutdown first stops accepting new processes, then terminates
  all registered process groups within a bounded grace period, persists
  `interrupted`, and only then closes the log/event pipeline.

#### Hard-crash containment

On Unix, Karoz creates a watchdog pipe for every background child. The parent
keeps only the close-on-exec write end. The `karoz process-guard` helper receives
only the read end, becomes the process-group leader, marks that fd close-on-exec before
starting the actual command, and watches it concurrently with the child:

- EOF means the Karoz parent exited or was SIGKILLed; the guard immediately
  sends SIGKILL to its own process group and exits;
- a normal stop arrives through the supervisor, which sends SIGTERM then
  SIGKILL to the same group;
- if the guard exits unexpectedly while Karoz is alive, the supervisor treats
  it as failure and kills/waits the recorded PGID.

On Windows, the equivalent is a Job Object configured with
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`; the Karoz parent owns the final job handle.
Unsupported platforms fail `run_background` closed rather than silently falling
back to an unguarded child.

The watchdog fd is never inherited by the command, and the guard never accepts
commands or data after spawn. A hard-crash integration test starts a
child-plus-grandchild fixture, sends SIGKILL to the Karoz fixture parent, and
asserts both descendants disappear. Graceful-shutdown testing is not a
substitute for this gate.

#### Spawn transaction

Spawn is a rollback-safe transaction:

1. Through the terminal-reservation coordinator, validate
   ownership/approval/caps, reserve a process ID plus central-ledger token, and
   atomically persist `starting` plus the matching empty reserved slot in the
   process-authority snapshot. Then create the capped log and watchdog
   primitive; either failure follows the same terminal transition and later
   token-release path rather than erasing the admission record.
2. Spawn the guard/Job Object and child.
3. Immediately arm the output collector and waiter against a closed
   **registration gate**. They may buffer bounded early stdout/stderr and capture
   an already-available exit result, but cannot publish events or mutate the
   durable record yet. Arming is synchronous; there is no interval in which an
   instant child can exit without a waiter capable of reaping it.
4. Still before returning the process ID, persist the `running` record with the
   live guard handle's diagnostic PID/PGID.
5. Open the registration gate. The collector flushes buffered output in order.
   If the waiter already observed exit, it performs the normal
   running-to-terminal transaction and durable terminal outbox before
   `run_background` returns; the tool returns the process ID plus its terminal
   state/exit code rather than claiming it is running.

Gate release is arbitrated by one supervisor coordinator: it first marks the
record registered, then lets the collector assign sequences and drain buffered
bytes through EOF (or the bounded exit-drain deadline), and only then releases
the waiter to perform the single terminal transition. The waiter may capture an
exit before registration but cannot overtake collector drain or publish a
terminal event. A collector failure after registration kills/reaps the group
and enters the same single terminal path. Thus `true`, a one-write process and a
long-running process all have one ordering contract rather than timing-specific
branches.

Every failure after OS spawn but before durable registration closes pipes/logs,
closes the registration gate in rollback mode, SIGKILLs the whole group or
closes the Job Object, drains/reaps it, discards buffered monitor/UI events, and
persists a terminal failure. A hard crash in that window closes the watchdog
owner and the guard kills the group. Log-open, spawn, collector-arm,
waiter-arm and registration-save failpoints must prove that no child or zombie
survives.

The supervisor has three separate responsibilities, matching the separation
that works in Codex:

1. **Process registry** — handles, ownership, concurrency and lifetime.
2. **Output collector** — bounded disk log, bounded in-memory tail and live
   deltas.
3. **Exit watcher** — output drain, terminal transition and exactly one terminal
   event during a live server instance.

Keeping these separate prevents a slow UI subscriber or a failed event publish
from blocking `Wait`, leaking a zombie, or losing the terminal state.

### Approval

Same gate as foreground bash, with one deliberate tightening. Start approval is
the exact structured subject
`background_start:<project>:<agent>:<canonical-workdir>:sha256:<command-digest>`.
Stop approval is
`background_stop:<project>:<agent>:<process-id>:sha256:<command-digest>`.
A foreground approval cannot start a daemon, and approval to stop one process
cannot stop another. This requires turning the approval subject into a structured value
(`residentBashSubject`) instead of a raw string; the single-use,
project-scoped, agent-scoped, exact-match contract from DECISION 0001 is
otherwise unchanged and must keep passing its existing regression tests.
The displayed strings above are labels only: equality uses a versioned
canonical JSON structure with explicit operation/project/agent/workdir/process
and command-digest fields, never delimiter-based string parsing.

### Tools

| tool | allowed turns | behavior |
| --- | --- | --- |
| `run_background` | dev, or approved subject | start a process, return its id immediately |
| `list_processes` | any | state, exit code, runtime, log size, last line |
| `read_process_log` | any | bounded window; `tail` mode default, or by line offset |
| `stop_process` | dev, or approved subject | SIGTERM then SIGKILL the group |

`read_process_log` and `list_processes` are read-only and therefore available in
ask/plan turns: an agent that may not *start* a process should still be able to
explain what a running one is doing. Both are added to the read-only exemption
list in `residentToolHasSideEffects`; `run_background` and `stop_process` are
effectful and must trip the effects barrier.

### Configuration

| variable | default | ceiling |
| --- | --- | --- |
| `KAROZ_PROCESS_MAX_CONCURRENT` | 8 per project | — |
| `KAROZ_PROCESS_MAX_LIFETIME` | 1h | 24h |
| `KAROZ_PROCESS_LOG_MAX_BYTES` | 8 MiB | — |
| `KAROZ_PROCESS_TAIL_LINES` | 200 | — |
| `KAROZ_PROCESS_OUTPUT_EVENT_MAX_BYTES` | 8 KiB | 64 KiB |
| `KAROZ_PROCESS_EXIT_DRAIN` | 250ms | 2s |
| `KAROZ_PROCESS_TERMINAL_MAX_RECORDS` | 200 per project | 2,000 |
| `KAROZ_PROCESS_TERMINAL_RETENTION` | 7d | 30d |
| `KAROZ_PROCESS_LOG_TOTAL_BYTES` | 256 MiB per project | 2 GiB |

Cleanup considers only terminal records, oldest first. It removes the log and
record under the registry lock with a tombstone so a concurrent reader gets
`gone`, never a partially deleted log. Active/starting records and logs with an
open reader are never selected. A monitor reference does not pin a terminal log
past the hard cap: cleanup first disables that monitor with
`ErrorCode=target_retired`, then removes the record/log. Probe snapshots and
durable approval receipts are retained only while referenced. After monitor
delete/replacement and active-invocation drain, exact source bytes and the
snapshot are deleted; a redacted digest/principal/time approval audit remains.

## Part 2: Monitors

### Domain model (`internal/monitor`)

One trigger, one action, plus the guards. All matching and guard logic is pure
so it is enforced by the runtime and covered by tests, never by prompt wording.

```go
type TriggerKind string
const (
    TriggerRuntimeEvent  TriggerKind = "runtime_event"   // the existing event bus
    TriggerProcessExit   TriggerKind = "process_exit"
    TriggerProcessOutput TriggerKind = "process_output"   // regex per line
    TriggerScriptProbe   TriggerKind = "script_probe"     // immutable, bounded, recurring
)

type ActionKind string
const (
    ActionNotifyAgent ActionKind = "notify_agent"  // schedule a run with a briefing
    ActionBlackboard  ActionKind = "blackboard"    // post a signal, wake nobody
)

type Action struct {
    Revision int
    Kind     ActionKind
    AgentID  string // notify_agent target; must belong to the same project
    TurnType string // notify_agent: "ask" | "plan"; never unattended "dev"
    Topic    string // blackboard topic
    Template string // bounded text; event detail is data, never instructions
}

type State string // active | disabled | exhausted | expired | error

type ProbeApprovalReceipt struct {
    ID, ProjectID, AgentID string
    MonitorID              string
    TriggerRevision        int
    ApprovalFlow           string // agent_choice | ui_challenge
    ApprovalRunID, ChoiceRequestID string
    OperatorSessionID, ChallengeID string
    CanonicalWorkdir       string
    Language               string
    SnapshotPath           string
    SnapshotDevice, SnapshotInode uint64
    NormalizedSource       []byte // exact approved bytes, max 64 KiB; never API/prompt exposed
    SourceSHA256           string
    IntervalMS, TimeoutMS  int64
    ApprovedBy             string // v1: local_operator:<installation-id>; later authenticated user ID
    ApprovedAt             time.Time
    ExpiresAt              time.Time // unclaimed receipt only
    ClaimedMutationID      string
    ClaimedAt              *time.Time
    RevokedAt              *time.Time
}
```

There is intentionally **no** command-running action. The only monitor-owned
code execution is the approval-gated probe itself. The "bounded recurring
command probe" authorized by DECISION 0003 is concretely implemented as an
immutable `script_probe`, not as a mutable shell command stored inline in the
monitor. Adding a `run_command` action would turn a monitor into a persistent
unapproved shell, which is the specific escalation DECISION 0003 forbids.

```go
type Trigger struct {
    Kind TriggerKind
    Revision int

    // runtime_event
    EventKinds           []string // max 16 registered kinds/authority subscriptions
    EntityID             string
    FromState, ToState   string
    IncludeMonitorEvents bool   // off by default: this is the loop class

    // process_exit / process_output
    ProcessID   string
    FailureOnly bool
    Pattern     string

    // script_probe
    ProbeLanguage   string // "shell" | "javascript"; no arbitrary interpreter path
    ProbePath       string // runtime-owned immutable snapshot, not user input
    ProbeSHA256     string
    IntervalMS      int64
    TimeoutMS       int64
    ApprovalReceiptID string
}

type Monitor struct {
    ID, ProjectID, AgentID, Name string
    Revision int
    Trigger Trigger
    Action  Action
    State   State

    CooldownMS  int64
    MaxTriggers int

    TriggerCount int
    Sequence     int
    LastMatch    string
    LastError    string
    ErrorCode    string
    SourceGaps   map[string]SourceGapStatus // key: canonical authority ID + event kind; max 16
    LastCheckedAt *time.Time
    ConsecutiveProbeErrors int
    LastFiredAt  *time.Time
    RecentFires  []time.Time  // rolling window backing the rate ceiling
    PendingFires []PendingFire

    NextCheckAt *time.Time // script_probe only; missed ticks are not replayed
    ProbeRunning bool `json:"-"` // runtime-only; concurrent ticks coalesce

    ExpiresAt *time.Time
    CreatedAt, UpdatedAt time.Time
}

type SourceGapStatus struct {
    SourceKind, AuthorityID string
    GapVersion, FirstVersion, LastVersion uint64
    LostCount uint64
    AcknowledgedGapVersion uint64
    AcknowledgedAt *time.Time
    AcknowledgedBy string
    ResumeAfterVersion uint64
}
```

The source registry resolves every `runtime_event` kind to one project-scoped
authority partition at monitor validation time. `EventKinds` is deduplicated and
capped at 16, so `SourceGaps` has the same hard bound. Entity filters narrow
matching inside that authority; they do not create per-entity gap keys. The map
key uses canonical length-prefixed encoding of `(AuthorityID, SourceKind)`, not
delimiter parsing. Trigger mutation is rejected while any gap entry is
unacknowledged; after all entries are acknowledged, changing/removing a
subscription moves their summaries to bounded audit history and clears the map
before installing the new at-current-version baselines.

### Script probe contract

A `script_probe` lets the user or agent write temporary code that answers one
question: "does the monitor condition match now?" It is a trigger evaluator,
not an action runner.

Creation follows this contract:

1. In an approval-preparation step, reserve a new `MonitorID` (or lock an
   existing monitor ID for replacement) and its exact next `Trigger.Revision`.
   The reservation is random, project/agent scoped, expires after 10 minutes and
   does not create, arm or mutate a monitor. At most one live trigger reservation
   exists per monitor; it blocks a competing trigger replacement but not action
   edits, runtime counters, disable or delete. Delete revokes it, and claim also
   verifies the previous trigger revision still immediately precedes the
   approved target revision. Accept source plus an allowlisted language
   (`shell` or `javascript` in v1).
   The interpreter is selected by Karoz (`bash` or `node`); source cannot choose
   an interpreter path or shebang. Source is capped at 64 KiB before approval.
2. Resolve and canonicalize the workdir, normalize line endings once, calculate
   SHA-256 over the exact normalized bytes, and show those bytes plus monitor ID,
   trigger revision, language, project, agent, canonical workdir, interval and
   timeout in the approval UI.
3. Complete one of the two approval flows below. Before minting authorization,
   atomically mark the challenge `staging` in the registry and reserve one of
   the bounded snapshot-file slots. Then atomically write/fsync the approved
   bytes to
   `<Settings.DataDir>/monitor-probes/<safe-project-key>/receipts/<receipt>/<digest>.<ext>`
   with mode `0600` through the same runtime-owned, component-by-component
   no-follow/reparse-rejecting boundary,
   then consume the challenge and mint a **durable** `ProbeApprovalReceipt` in
   one registry save. The receipt records who approved, when, that immutable
   path/identity, the reserved monitor ID, exact trigger revision, normalized
   source bytes and every execution field above.
   Persisting an `ApprovalSubject` string is not a grant.
4. A `create_monitor` or `update_monitor` call in a **dev turn** supplies a
   unique mutation ID and atomically claims that receipt while creating/updating
   exactly its bound monitor ID and trigger revision. A claimed receipt cannot
   be copied to another monitor, revision or mutation; retrying the same mutation
   ID is idempotent. Claim first verifies that the already-durable immutable file
   still matches the receipt, so no filesystem write follows an armed monitor
   mutation. UI HTTP routes cannot perform this claim or create/replace a
   `script_probe`. The public API and prompt show the language, digest and
   summary, never the full source.
5. On every tick, open the runtime-owned path with no-follow semantics, require
   a regular file with the expected owner/mode, read at most 64 KiB from that
   opened fd, and compare the exact bytes/hash plus all execution fields to the
   durable receipt, including `MonitorID`, `Trigger.Revision` and non-empty claim
   metadata. Symlink, file-type, bytes/hash, canonical-workdir, language,
   interval, timeout, identity, revision, claim or receipt mismatch immediately
   moves the monitor to `state=error`; it is not treated as a transient
   three-strike probe error.
6. Execute the bytes just read and verified against the receipt's exact source
   through stdin (`bash -s --` or `node -`) while retaining the opened file
   identity for diagnostics. Never hash a path and then ask the interpreter to
   reopen that pathname. This closes the swap-after-hash TOCTOU window.

Changing source, language, canonical workdir, interval or timeout revokes the old
receipt and requires a new interactive approval. Restart loads the durable
receipt; it does not silently mint a replacement from the old in-memory
single-use approval.
Re-enable re-validates the receipt and immutable bytes first; it cannot clear an
integrity/authorization error until a valid newly approved revision exists.

Approval flows are explicit:

- **Agent dev turn:** reserve the monitor identity/revision, consume the existing
  single-use, Run-scoped choice for that exact canonical payload and set
  `ApprovalFlow=agent_choice`, `ApprovalRunID` and `ChoiceRequestID`. The same
  dev turn (or a later dev turn before expiry) may claim the resulting receipt.
- **UI preparation only:** the loopback UI first obtains an HttpOnly,
  SameSite=Strict
  operator-session cookie. Its value is a server-generated 256-bit random token,
  represented server-side by an expiring session record bound to the local
  installation; a client-supplied identifier is never accepted as a principal.
  `prepare probe approval` reserves the monitor identity/revision and creates a
  random, 10-minute, single-use challenge bound to that session and the exact
  canonical approval payload. A separate confirmation request with the same
  cookie and allowed loopback Origin consumes it and mints an unclaimed receipt
  with `ApprovalFlow=ui_challenge`, `OperatorSessionID` and `ChallengeID`.
  Confirmation still does not create or arm a monitor; the product instructs
  the user to complete the claim through a dev turn.

In current single-user mode `ApprovedBy=local_operator:<installation-id>`;
multi-user auth replaces it with the authenticated user ID. Agent receipts
require Run+choice IDs and no UI IDs; UI receipts require session+challenge IDs
and no Run IDs. Both bind the same reserved monitor ID/trigger revision. There
is no direct UI create-and-approve path, and a challenge cannot be replayed from
another browser session.

Operator sessions, reservations, challenges and receipts live beside monitor
definitions in the mode-`0600` `MonitorRegistry` snapshot. Challenge confirmation
atomically records `ConsumedReceiptID` and mints the unclaimed receipt in one
save after the immutable source file has been atomically installed and fsynced.
The prior durable `staging` record owns that path and capacity slot, so a crash
never leaves an uncounted file. Failure of the consume/mint save immediately
attempts to delete the file and clear `staging`; if either cleanup fails, the
staging record remains capacity-accounted and the sweeper retries. A crash after
the consume/mint save creates both records, and an idempotent same-session retry
returns that receipt. Receipt claim plus monitor create/update is likewise one
registry save. A different mutation ID, monitor ID or trigger revision receives
`approval_already_claimed` or `approval_subject_mismatch`; no partial claim
survives a save failure.

V1 `script_probe` is Unix-only. Windows background processes still use the Job
Object contract, and non-script monitors remain available, but script-probe
prepare/create/enable/check return `unsupported_platform` before persisting a
challenge, receipt or process. This avoids pretending Unix `0600`,
no-follow/owner checks have a Windows equivalent. Windows support may be added
only with owner-only ACL validation, reparse-point rejection, immutable-handle
reads and equivalent regression tests; there is no permissive fallback.
Loading a Unix-created probe definition on Windows normalizes it to
`state=error`, `ErrorCode=unsupported_platform` and never executes it.

Each invocation is a fresh, non-interactive child process using the same
sandbox/environment policy as approved resident bash and the same
parent-death guard/Job Object as background processes. It has:

- default interval 60s, hard minimum 10s;
- default timeout 5s, hard maximum 30s;
- no overlap: if the previous invocation is still running, the next tick is
  coalesced rather than queued;
- bounded stdout plus stderr (16 KiB each);
- a process group that is killed on timeout;
- no catch-up execution after sleep, restart or scheduler delay.

Creation fails closed when the selected Karoz-managed interpreter is not
available. Runtime availability is checked again on every tick; falling out of
availability follows the same consecutive-error policy as other probe failures.

Successful stdout must contain exactly one JSON object:

```json
{"matched": true, "detail": "health changed to degraded"}
```

`matched` is required and boolean. `detail` is optional, plain text, capped at
1 KiB and treated as untrusted data when inserted into an Agent Run. Additional
stdout, malformed JSON, timeout, signal, or nonzero exit is a probe error, not a
match. Consecutive probe errors increment an error counter; three consecutive
errors move the monitor to `state=error` and require explicit re-enable. Raw
stdout/stderr is available only through a bounded diagnostic view and is never
automatically injected into a model prompt.

This replaces the previous `exit_nonzero`, `exit_change`, and `output_match`
probe modes. Those modes force the monitor runtime to interpret arbitrary
program output and make temporary code less portable. A script can implement
any of them while returning one provider-neutral result shape.

### Loop and escalation guards

This is the part that has to be right. A monitor whose trigger is
`agent_run_changed` and whose action wakes an agent is an infinite loop, and it
would be trivially easy to write.

1. **Origin tagging.** A monitor-triggered run carries structured
   `Origin{Kind:"monitor", MonitorID:id, FireID:fireID}` in its Run context.
   Its JSON form is exactly
   `{"kind":"monitor","monitor_id":"...","fire_id":"..."}`; no component
   parses colon-delimited origin strings. Every event emitter and every
   tool-produced task, handoff, plan, process, scheduled-run and blackboard event
   derives and propagates that origin; tool handlers may not synthesize
   `origin=runtime` when a monitor origin exists. `MatchEvent` drops
   monitor-originated events unless the monitor sets `IncludeMonitorEvents`.
   This eliminates the whole feedback class structurally, rather than
   blacklisting event kinds one at a time.
2. **Self-exclusion.** A monitor never matches an event it caused, even with
   `IncludeMonitorEvents` set.
3. **No implicit wildcard.** An empty `EventKinds` list means "no event", not
   "every event"; validation requires at least one kind. A monitor that watches
   everything is a loop waiting to happen.
4. **Cooldown.** Default 60s, hard floor 5s.
5. **Rate ceiling.** Max 6 fires per rolling 5 minutes; exceeding it moves the
   monitor to `state=error` with an explanatory `LastError`. There is no env var
   to disable this. With the 5s cooldown floor a monitor could otherwise fire 60
   times in that window.
6. **Lifetime cap.** Optional `MaxTriggers`, after which the monitor becomes
   `exhausted`.
7. **Dedup.** Notify actions schedule with the stable fire ID derived from the
   monitor and source event, so a repeated delivery attempt collapses through
   the existing persisted `SchedulerQueue` dedup.
8. **Probe authorization.** Probe bytes/hash, language, project, agent,
   canonical workdir, interval and timeout are bound to a durable approval
   receipt and re-validated on every tick. Editing any bound field revokes the
   receipt; changing a monitor can never substitute new code or a more aggressive
   cadence into an approved probe. Probes are creatable only in dev turns.
9. **Probe serialization.** At most one invocation per probe may run. A late
   invocation is not queued, and missed ticks are not replayed after restart.
10. **Event identity.** Every candidate match has a stable event/fire identity.
    Runtime events use their event ID, process output uses
    `(process_id, output_seq)`, process exit uses `process/<id>/terminal`, and a
    probe uses its scheduled tick timestamp. The identity feeds scheduler dedup
    and diagnostics.

The fire path is a single pure function returning a decision, so every guard is
directly testable:

```go
type Decision struct {
    Monitor      Monitor
    Fire         bool
    Changed      bool   // persist even when suppressed: suppression can auto-disable
    Suppressed   string // "cooldown" | "rate_limit" | "exhausted" | "expired" | state
    AutoDisabled bool
    DedupKey     string
    Detail       string
}

func Fire(item Monitor, detail string, now time.Time) Decision
```

Guard order matters and is asserted by tests: expiry, then state, then
`MaxTriggers`, then cooldown, then the rate ceiling, then fire.

`MatchProbe` validates the structured `ProbeResult` and returns the updated
monitor even when it does not fire, because check time, consecutive errors and
next-check time are observable state.

### Tools

`list_monitors` is available in every turn. `validate_monitor` is a read-only
plan/dev tool that validates a proposed definition without saving or arming it.
`create_monitor`, `update_monitor`, `delete_monitor`, enable/disable and source
replacement are effectful dev-only tools; `acknowledge_monitor_gap` is also
effectful, requires `(monitor_id, authority_id, source_kind,
expected_gap_version)`, and establishes one explicit baseline described below.
Plan turns can describe or validate them but cannot mutate live monitor state. Product UI
mutations are direct user actions and cross the HTTP CSRF/origin boundary, but
HTTP create rejects `TriggerKind=script_probe`; HTTP PATCH of an existing probe
allows only safe state/action/guard metadata and rejects any trigger, source,
execution-field or receipt field with `dev_turn_required`. The UI may only
reserve, inspect and confirm a probe approval. `script_probe` creation or
trigger/execution-field replacement must occur through a dev-turn tool and
additionally requires the durable, single-monitor interactive approval above.

The monitor engine owns a server-lifetime `monitorCtx`. Event subscriptions,
probe timers, matching and dispatch derive from that context, never from the
request or Run that created the monitor. Closing a subscriber only removes that
subscriber. Finishing or interrupting the creating turn changes no monitor
state. Graceful server shutdown stops new matches, cancels active probe process
groups, persists the registry, and allows bootstrap to re-arm definitions.

Resource limits are runtime-enforced:

| resource | v1 limit |
| --- | --- |
| monitor definitions | 100 per project |
| active monitors | 64 per project |
| enabled script probes | 16 per project |
| concurrently executing probes | 4 per project |
| operator UI sessions | 16 per project; 30m idle / 8h absolute expiry |
| active probe reservations/challenges | 64 per project; 10m expiry |
| unclaimed probe receipts | 64 per project; 10m expiry |
| claimed probe receipts | at most one per probe definition; definition cap applies |
| immutable/staging/retired probe snapshot files | 228 per project, 64 KiB each; every path owns a registry slot |
| pending fires | 32 per monitor / 256 per project |
| durable source-outbox events | 4,096 per project |
| reserved terminal-event slots | 4,096 per-project central-ledger slots, allocated before an entity becomes active |
| live process-output evaluation queue | 1,024 lines per project |
| applied-event receipts | 8,192 per project; retained only until source outbox ack |
| sink admission receipts | 512 per project; retained only until pending-fire removal |
| audit history | last 100 fires/monitor plus 168 hourly count buckets (7d) |
| redacted probe-approval audits | 1,024 per project, retained at most 7d |

Creating/enabling past a definition or enabled-probe cap fails before mutation.
Creating a session/reservation/challenge/receipt first prunes expired records,
then fails with `approval_capacity` rather than evicting a live authorization
record. Expired unclaimed receipts and reservations are revoked; claimed
receipts remain only while their exact probe definition references them.
Consumed challenge tombstones remain for the challenge's original 10-minute
window to make confirmation retries idempotent, then are pruned. The snapshot
cap covers 100 currently referenced definitions, 64 staging/unclaimed
reservations and 64 retired snapshots awaiting deletion. Replacement fails
before claim if no retired slot is available. Confirmation-save or replacement
cleanup failure performs immediate deletion; a once-per-minute,
before-every-approval-operation and bootstrap sweep retries staging/retired
cleanup. Failed deletion retains its registry slot, so disk use cannot grow
through repeated failures. An unreferenced on-disk file is adopted into a
retired slot before further approval writes; if physical files and registry
slots cannot be reconciled within 228, approvals fail closed with
`snapshot_capacity_corrupt`. Probe ticks past the execution cap coalesce and
increment a visible
`resource_suppressed` counter; they never queue an unbounded backlog.

Receipt classes are not one seven-day bucket:

- An **applied-event receipt** exists only across the source-outbox
  apply/ack crash window. After source acknowledgement is durable, no event can
  replay, so the receipt is deleted. At most 4,096 normal outbox events plus
  4,096 preallocated terminal events can be awaiting acknowledgement, so the
  8,192 receipt cap covers every exact event. Native authorities' fixed gaps and
  the shared journal's at-most-16 authority-keyed gaps use monotonic per-key
  `LastAppliedGapVersion` cursors rather than this receipt pool.
- A **sink admission receipt** exists only while the matching pending fire is
  present. After the registry durably removes/cancels that fire, the receipt is
  deleted. The 512 cap covers the 256 pending-fire limit plus admitting/recovery
  headroom.
- Seven-day product history is not idempotency evidence. It retains only the
  last 100 fire summaries per monitor plus one count/status bucket per hour
  (168/monitor), so 64 maximally active monitors remain bounded. Older exact
  fire rows are summarized, not falsely advertised as fully retained.
- Redacted probe-approval audits contain no source bytes. Past 1,024 rows, the
  oldest rows are collapsed into hourly principal/digest-count buckets because
  they are audit summaries, not live authorization grants.

At the legal rate ceiling, a single monitor can fire 12,096 times/week and 64
can fire 774,144. No receipt limit or retention claim assumes those events fit
in a 10k ring.

### Event and delivery contract

Registered durable runtime and terminal sources enter the matcher through one
bounded envelope:

```go
type Event struct {
    ID                  string // stable project/authority/kind/generation/event identity
    ProjectID           string
    AuthorityID         string
    AuthorityGeneration uint64 // meaningful only inside this authority
    Kind                 string
    EntityID             string
    Origin               Origin
    At                   time.Time
    Payload              json.RawMessage // kind-specific, size-capped before publication
}

type Origin struct {
    Kind      string `json:"kind"` // user | runtime | monitor
    MonitorID string `json:"monitor_id,omitempty"`
    FireID    string `json:"fire_id,omitempty"`
}

type FrozenAction struct {
    MonitorRevision, ActionRevision int
    Kind, AgentID, TurnType         string
    Topic, RenderedBriefing         string
    ActionPayload                   json.RawMessage
}

type PendingFire struct {
    ID, MonitorID, EventID string
    Sequence               int
    DedupKey               string
    EventBriefing          json.RawMessage
    Action                 FrozenAction
    CreatedAt              time.Time
    Attempts               int
    Status, ClaimToken     string // pending | admitting | cancelled
}

type Admission string
const (
    AdmissionAdmitted       Admission = "admitted"
    AdmissionAlreadyPresent Admission = "already_present"
    AdmissionFailed         Admission = "failed"
)
```

Origin validation requires both IDs for `kind=monitor` and forbids them for
`user`/`runtime`. Existing unstructured user/runtime events are normalized at
their emitter boundary; an unstructured monitor origin or one missing a fire ID
is rejected before event publication.

Event payloads and frozen action payloads are independently capped at 16 KiB.
The `PendingFire` freezes the matching event briefing and exact monitor/action
revision. Editing the monitor after a match can never redirect a retry to a new
agent/topic/template.

Every accepted monitor mutation increments `Monitor.Revision`; every action
mutation also increments `Action.Revision`, and every semantic trigger mutation
increments `Trigger.Revision`. Runtime-only counters do not change the trigger
revision. A probe approval binds the next trigger revision calculated while its
identity reservation is held. Clients PATCH with the monitor revision they
read, and a stale revision receives a conflict instead of overwriting concurrent
runtime counters or approval state.

All monitor definitions, counters, applied-event receipts, pending fires,
operator sessions, approval reservations/challenges and probe receipts share one
mode-`0600` `MonitorRegistry` snapshot. This is the atomic boundary for
challenge-consume/receipt-mint and receipt-claim/monitor-mutation. A single
registry writer lock serializes mutation and `monitors.json` save; per-monitor
locks alone are forbidden because two concurrent saves can overwrite each
other's counters, approval claims or pending fires. Lock ordering is:

1. source mutation/outbox commit, with no monitor lock held;
2. registry writer lock: read committed events, evaluate every eligible monitor,
   append frozen pending fires/applied-event receipt, atomically save, unlock;
3. project dispatch gate, then registry writer lock to claim one pending fire
   with a token; release the registry lock, call the local bounded sink, then
   reacquire the registry lock to record admission/retry state;
4. explicit disable/delete/owner deletion acquires the same project dispatch
   gate before cancelling pending fires, so it cannot race a not-yet-admitted
   action into existence.

No path acquires the dispatch gate while holding the registry lock. Sink calls
never hold a source or registry lock.

When `Fire` returns true, the registry atomically persists its guard/counter
updates plus the `PendingFire` before dispatch. The fire ID is:

`monitor/<monitor-id>/<event-id>`.

Dispatch is action-specific:

- `notify_agent` calls a new `AdmitMonitorRun(fireID, frozenAction)` boundary.
  It returns the tri-state `Admission`, not the current ambiguous boolean.
  Scheduler admission persists a fire-ID receipt/tombstone through successful
  job removal until the monitor registry has durably removed the matching
  pending fire, so every possible retry returns `already_present`. The scheduled
  input carries the frozen briefing, `monitor_fire_id` and monitor origin.
- `blackboard` calls `PutMonitorSignalOnce(fireID, frozenAction)`, with the same
  tri-state contract and a durable unique source ID. Monitor signals are
  `actionable=false`; `maybeTriggerKarozIdleReconcile` must ignore them, so
  "blackboard, wake nobody" remains true.

The pending record is removed only after its specific sink returns `admitted` or
`already_present`. On startup, pending fires retry with identical fire IDs and
frozen actions. Scheduler/blackboard receipts make that retry idempotent through
completed effects, rather than only while a queue entry happens to exist.
Applied-event receipts are pruned after source-outbox acknowledgement; sink
receipts are pruned after pending-fire removal. Reaching either hard cap stops
new monitor evaluation/admission with `receipt_capacity` instead of deleting
live idempotency evidence; the independent bounded audit summaries remain.

Pruning is deliberately ordered and crash-safe. Native single-authority
outboxes persist their contiguous `AckedThroughAuthorityGeneration`; the shared
workspace journal persists its independent contiguous
`AckedThroughJournalSeq`. Only then does the corresponding applied receipt
become eligible for deletion. Sink cleanup observes a durably
absent/cancelled pending fire before deleting the sink receipt; it never deletes
the receipt first. A crash between either proof and deletion leaves a harmless
orphan, not a duplicate window. Bootstrap and periodic cleanup remove only
applied receipts proven at or below the correct cursor domain and sink receipts
whose fire ID is absent from every pending/admitting record. Missing proof
retains the receipt and surfaces `receipt_orphan_unproven`.

Failures are explicit:

- matcher/validation failure updates `LastError` and never dispatches;
- monitor-state persistence failure suppresses dispatch;
- scheduler admission failure leaves the pending fire for bounded retry;
- a full pending-fire cap moves the monitor to `error` instead of dropping old
  notifications silently;
- explicit disable, delete or owner deletion cancels undispatched pending fires
  and records why, but does not cancel an Agent Run already admitted/started;
- automatic `expired`/`exhausted` and non-dispatch `error` states stop new
  matching but continue draining fires accepted before that state;
  `ErrorCode=dispatch_error` pauses its retained pending fires after the bounded
  retry budget, and explicit re-enable resumes them with the same IDs.

Delete is a soft tombstone until cancelled fires are removed and their sink
receipts are compacted; only then may cleanup remove the definition. Exact probe
receipt source/snapshot bytes are then deleted, leaving only the separately
bounded redacted approval audit.

Admission retries use capped exponential backoff (1s through 1m). After five
failed attempts the monitor moves to `error` and retains the pending fire for
inspection; an explicit re-enable retries it with the same fire ID.

### Durable source boundary

The current `emitRuntimeStateChanged` broadcast is not the monitor source of
truth: it is in-memory and may drop on a full channel. Monitor creation validates
every requested event kind against a source registry:

| source/events | exact state authority | monitor durability boundary |
| --- | --- | --- |
| `task_changed` | `Settings.DataDir/tasks.json` project partition | native atomic snapshot gains bounded outbox/gap; terminal ledger slot while active |
| `plan_changed` | `Project.Path/.karoz/plans.json` | coordinated workspace source; exact event/manifest retained in runtime-owned source journal |
| `handoff_created/changed` | `Settings.DataDir/agent-inbox.json` project partition | native atomic snapshot gains bounded outbox/gap |
| `memory_changed` | `Settings.DataDir/agent-memory.json` project partition | native atomic snapshot gains bounded outbox/gap |
| `artifact_changed` | `Project.Path/.karoz/artifacts.json` | coordinated workspace source; exact event/manifest retained in runtime-owned source journal |
| `blackboard_changed` | `Settings.DataDir/agent-blackboard.json` project partition | native atomic snapshot gains bounded outbox/gap |
| `group_coordinator_changed` | `Project.Path/.karoz/groups.json` + `plans.json` | coordinated two-workspace-store source; one event only after both versions commit |
| `agent_run_queued` / `scheduled_run_changed` | `Settings.DataDir/agent-run-queue.json` | native atomic snapshot gains bounded outbox/gap; terminal ledger slot while job active |
| terminal `agent_run_changed` | `Settings.DataDir/agent-run-terminals.json` | native atomic terminal snapshot/outbox; terminal ledger slot while Run active |
| terminal `process_changed` | `Settings.DataDir/processes.json` | native atomic terminal snapshot/outbox; terminal ledger slot while process active |
| nonterminal `agent_run_changed` UI deltas | in-memory broadcast | live-only; rejected as monitor triggers in v1 |
| `process_output` | bounded live line queue | live/best-effort with explicit gaps |
| `script_probe` | periodic invocation | live periodic; result applied atomically to monitor state |

Unregistered kinds are rejected at monitor creation. The UI may still show
ephemeral Run progress, but a user cannot create a monitor that the runtime
would later pretend is durable.

Event emitters use typed constructors tied to this registry; new raw string
kinds fail tests until assigned a class/authority/gap policy. The legacy
`emitRuntimeStateChanged(RuntimeEvent{Kind: ...})` call shape is not used by new
monitor-aware mutations.

`Project.Path/.karoz/group-inbox.json` has no registered v1 monitor event kind.
It is not silently treated as `handoff_created/changed`, whose authority is the
runtime-owned `agent-inbox.json`. Adding a group-inbox event later requires its
own registry row and coordinated workspace-source contract.

The 4,096 normal-event project budget is a fixed sum, not 4,096 per file:

| normal outbox partition | slots/project |
| --- | ---: |
| tasks | 1,024 |
| handoffs | 512 |
| memories | 256 |
| blackboard | 512 |
| scheduler nonterminal | 512 |
| shared workspace source journal (plan/artifact/group) | 1,280 |
| **total** | **4,096** |

Changing these allocations requires a versioned migration that preserves the
same total. Terminal Run/job/task/process events use the separate 4,096
reservation ledger and never compete with these normal partitions.

For a **native atomic** source, its existing JSON schema is migrated to a
versioned snapshot containing the domain state, per-project generation,
bounded outbox and fixed gap record. Legacy map/list JSON is accepted only by
the one-time migration loader and is immediately saved in the new form before
monitor dispatch. Afterwards, missing/corrupt snapshots fail closed; the generic
"missing means empty" loader is not used. A task terminal transition, for
example, cannot be durable without either its exact outbox event or an explicit
gap marker in the same `tasks.json` snapshot.

Workspace-backed sources use option B: their product state stays portable under
`Project.Path/.karoz`, but monitor durability never does. Each safe-key runtime
directory contains one no-follow, mode-`0600`
`source-journal.json`. That atomic snapshot holds the exact bounded event
outbox, a project-journal acknowledgement cursor, bounded per-authority gap
records and a manifest for each registered workspace authority:

```go
type WorkspaceAuthorityManifest struct {
    Project RuntimeProjectIdentity
    AuthorityID, Kind, CanonicalPath, PathSHA256 string
    Generation uint64 // authority-local; never compared with another manifest
    SnapshotSHA256, LastOperationID string
    ExpectedPresent bool
}

type JournalEntry struct {
    JournalSeq          uint64 // per-project delivery order only
    AuthorityID         string
    SourceKind          string
    AuthorityGeneration uint64 // authority-local identity/barrier domain
    Event               Event
}

type SourceJournalGap struct {
    AuthorityID, SourceKind string
    GapVersion              uint64
    FirstAuthorityGeneration, LastAuthorityGeneration uint64
    LastAppliedGapVersion   uint64
    LostCount               uint64
    DetectedAt              time.Time
}

type RuntimeSourceJournal struct {
    Project RuntimeProjectIdentity
    NextJournalSeq         uint64 // starts at 1; never reused or decreased
    AckedThroughJournalSeq uint64
    Entries                []JournalEntry
    Gaps                   map[string]SourceJournalGap // canonical authority+kind; max 16
    Workspace map[string]WorkspaceAuthorityManifest
    NativeExpected map[string]bool // persisted after one-time native migration
}
```

The two counters are different domains and are never substituted:

- `JournalSeq` is monotonic across this project's workspace journal. It is used
  only for bounded delivery order, contiguous acknowledgement and compaction.
- `AuthorityGeneration` is monotonic only inside one canonical
  `(AuthorityID, SourceKind)`. It is used for manifest validation, event IDs,
  acknowledgement barriers and loss ranges. A plan generation of 100 has no
  ordering relationship to an artifact generation of 1.

Appending an exact entry validates `Event.ProjectID` against `Journal.Project`
and validates that the wrapper and `Event` carry the same authority, kind and
authority generation. It assigns the current `NextJournalSeq`, then increments
`NextJournalSeq` in the same atomic save.
`NextJournalSeq` must be greater than every retained entry and the acknowledged
cursor; regression, reuse or overflow fails closed. Full-journal handling does
not allocate a sequence and then leave a hole: it updates/coalesces the
authority-keyed gap, disables affected monitors, and leaves the next sequence
available for the next exact entry.

Consumers may wake and evaluate entries out of order, but each successful
registry application records the entry's `JournalSeq`. The source consumer
advances `AckedThroughJournalSeq` only through the longest contiguous applied
prefix. For example, applying sequence 3 before 1 and 2 leaves the cursor
unchanged; after 1 it advances to 1, and only after 2 may it advance through 3.
No entry or applied receipt above the first missing sequence is compacted.
Crash recovery derives the same prefix from durable applied receipts and the
journal; it never compares an authority generation with this cursor.

`Gaps` uses the same canonical length-prefixed `(AuthorityID, SourceKind)` key
as `Monitor.SourceGaps` and is capped at the registered source-kind limit of 16.
A later loss for the same key coalesces its authority-generation range and
count while incrementing `GapVersion`; losses for different authorities coexist
and never overwrite each other. Copying/acknowledging a journal gap follows the
per-key gap-version contract below and is independent of
`AckedThroughJournalSeq`.

A coordinated workspace mutation is:

1. persist a project-scoped intent with the exact event payload, expected and
   target workspace generations/digests;
2. save each workspace snapshot idempotently with its generation and
   `LastOperationID`;
3. verify every intended workspace version, then atomically append the exact
   journal entry, allocate its project `JournalSeq`, and update each affected
   authority manifest in runtime-owned `source-journal.json`;
4. mark the operation committed.

It uses the same per-project coordinator lane and single-store steps:
`runtime-mutations intent → workspace authority A save → authority B save (when
needed) → runtime source-journal save → runtime-mutations commit`. Each owning
lock is released before the next store is opened or written; crash/retry safety
comes from operation IDs and generations, not nested locks.

Crash after a workspace save but before step 3 is recovered by the durable
intent plus matching `LastOperationID`; crash after step 3 replays the journal
event idempotently. `group_coordinator_changed` lists both `groups.json` and
`plans.json` in one intent and becomes monitor-visible only after both match.
No event is synthesized after unrelated saves with an undefined crash window.

The manifest is validated before initial workspace-source monitor creation,
before every coordinated read/mutation, at bootstrap before any legacy loader
can default a missing file to empty, and by a 30-second integrity reconciler
(filesystem notifications are wake-up hints only). Once a manifest baseline
exists, missing, replaced, symlinked/reparsed, generation-regressed or
digest-mismatched workspace state atomically appends or coalesces that
authority's runtime-owned `SourceJournalGap`, disables affected monitors and
refuses to overwrite the last known in-memory state with empty defaults. The
exact committed event journal survives workspace deletion and remains until
contiguous journal acknowledgement.

When no manifest exists yet, initial monitor creation runs a coordinator
baseline transaction: it either migrates the existing regular JSON snapshot to
the versioned form or creates and fsyncs an explicit versioned empty snapshot,
then records its digest/generation in `source-journal.json`. It never infers
"missing means empty" without persisting that baseline first.

The same runtime journal records `NativeExpected[kind]=true` after each native
snapshot's one-time migration/baseline. Because neither that marker nor the
workspace manifests can prove that their own file used to exist, an independent
runtime index is also required:

```go
type ProjectRuntimeSentinel struct {
    Project             RuntimeProjectIdentity
    InitializationGeneration uint64 // CAS identity for initializing -> ready
    SourceJournalState  string // initializing | ready
    SourceJournalSchema int
    InitializedAt       time.Time
}

type ProjectRuntimeIndex struct {
    SchemaVersion int
    Projects map[string]ProjectRuntimeSentinel // safe-project-key
}
```

`Settings.DataDir/project-runtime/index.json` is a no-follow, mode-`0600`
runtime-owned file outside all project workspaces. First installation is allowed
only when the canonical project has no sentinel and no persisted
monitor/process/ledger/coordinator record that requires durable source state.
Initialization first fsyncs `state=initializing` in the independent index, then
creates/fsyncs the baseline source journal and manifests, and finally fsyncs
`state=ready`. A crash in `initializing` must resume or roll back this explicit
operation; it is never treated as a fresh untracked install. Recovery may
complete when the identity/generation-matching baseline journal is valid.
Rollback may remove the sentinel only when the initialization generation owns
no journal and no persisted monitor or project-runtime reference; any ambiguous
partial state fails closed.

For `ready`, a missing, corrupt, symlinked, schema-invalid or identity-mismatched
`source-journal.json` fails closed and produces project-visible source
degradation; it is never recreated by baselining current workspace files. A
missing/corrupt index while monitors or any project runtime record exists also
fails closed. Thus deletion of the file containing `NativeExpected` cannot
erase the evidence that it was expected. Only the proven empty first-install
case may create a new baseline.

Monitor backpressure must not wedge core runtime. Each project owns a bounded
normal outbox plus its own persisted `TerminalReservationLedger` with exactly
4,096 slots. Every durable aggregate writes through its authority's project
partition, references a token from that same project's ledger and keeps a
bounded gap cursor:

- a bounded normal outbox partition;
- one reserved terminal slot for every currently active task, scheduled job, Run
  or process;
- one fixed-size, coalescing `MonitorGap` for a single native authority, or the
  shared workspace journal's bounded map of at most 16 keyed
  `SourceJournalGap` records.

The ledger and coordinator journal live under the runtime-owned data directory,
never under the project/worktree in which agent commands execute. Resolve the
canonical loaded `Project` first, then calculate:

```
safe-project-key =
  lower-hex(SHA256("karoz-project-runtime-v1\x00" + canonical Project.ID))

<Settings.DataDir>/project-runtime/<safe-project-key>/terminal-reservations.json
<Settings.DataDir>/project-runtime/<safe-project-key>/runtime-mutations.json
<Settings.DataDir>/project-runtime/<safe-project-key>/source-journal.json
<Settings.DataDir>/project-runtime/index.json
```

The full 64-character hex key is the only derived path component; raw project
IDs and request values are never path components. Each per-project file is
single-writer for that project; the shared index has one global serialized
writer:

```go
type RuntimeProjectIdentity struct {
    ProjectID            string
    CanonicalProjectPath string
    CanonicalPathSHA256  string
    SafeProjectKey       string
}

type TerminalReservation struct {
    Slot        uint16 // 0..4095
    Token       string // random, unique
    ProjectID, ProjectIdentitySHA256, OperationID string
    AuthorityID, EntityID, EventKind string
    State       string // allocating | active | terminal_unacknowledged | releasing
}

type TerminalReservationLedger struct {
    Project RuntimeProjectIdentity
    Capacity  uint16 // exactly 4096
    Slots map[uint16]TerminalReservation // hard reject above 4,096
}

type RuntimeMutationOperation struct {
    ID string
    Project RuntimeProjectIdentity
    Kind, State   string
    ReservationToken string
    AuthorityID, EntityID string
}
```

An authority snapshot stores the same slot/token beside its entity and reserved
outbox entry plus the same `RuntimeProjectIdentity` (or its exact ID/path digest
fields). Equality is exact structured-field equality, never a derived string.
On load and every mutation, the freshly resolved canonical project identity must
equal the ledger, operation, reservation and authority-record identity; a
mismatch is corruption and cannot be associated by coincidentally equal
authority/entity IDs. The project mutation coordinator owns ledger
allocation/release; source authorities never independently guess or scan for a
free project slot.

Before loading any ledger, bootstrap requires a one-to-one mapping between
canonical `Project.Path` and canonical `Project.ID`. Aliases resolve to their
primary project and do not create another runtime identity; two distinct
project IDs pointing at the same canonical path disable background/monitor
runtime for both until configuration is corrected. A stored path-identity
mismatch is never silently rebound while reservations or coordinator operations
exist.

`Settings.DataDir/project-runtime` must resolve outside every canonical
`Project.Path`; otherwise background/monitor runtime fails closed before
admission. It is created owner-only (`0700` directories, `0600` files). Unix
opens walk the runtime-owned root, safe-key directory and file with
`openat`/no-follow semantics, verify owner and directory/regular-file type at
each component, and perform atomic rename/fsync relative to already-verified
directory descriptors. Windows performs the equivalent owner-only ACL and
reparse-point rejection. A symlink/reparse component, ownership/type mismatch,
or safe-key/directory identity mismatch disables only that project's runtime and
is never followed to another target.

This boundary prevents ordinary workspace commands such as `git clean -fdx`,
build cleanup, replacement of `Project.Path/.karoz`, or a workspace symlink from
touching safety-control state. It is not represented as an OS sandbox against a
deliberately malicious same-user command that targets an absolute runtime path;
the exact-command approval boundary remains responsible for that threat class.
The runtime data path is not injected into agent prompts, command environment or
tool results.

Admission into an active state uses a coordinator-backed cross-store
transaction:

1. persist a project-scoped operation intent in that project's coordinator
   journal;
2. allocate one token in that project's ledger and persist the operation ID;
3. in the authority's atomic project partition, persist the new active entity plus an
   empty reserved terminal-outbox slot referencing that token;
4. mark the coordinator operation committed.

A crash completes or rolls back the operation idempotently: a ledger token with
no active authority record is released, while an authority record is never made
admissible until its token and reserved slot are durable. If no token is
available, the *new* task/job/Run/process is rejected before its effect starts.
The monitor subsystem therefore never creates an active entity whose completion
it could later wedge.

Runtime serialization is explicit. Each project has one coordinator actor/lane,
but no journal, ledger, authority or monitor mutex is held while another store is
read or written. Every step acquires only its owning store lock, reloads and
validates `(ProjectID, OperationID, generation)`, performs one atomic save, then
unlocks before the next step:

`project coordinator lane → journal step → ledger step → authority step → journal commit`.

Source-journal initialization follows the same no-nested-lock rule: the global
index writer CAS-saves `initializing` and unlocks; the project lane creates and
fsyncs the identity-matching journal; then the index writer reloads, verifies
the same initialization generation, CAS-saves `ready` and unlocks. Concurrent
initialization of different projects cannot overwrite index entries, and no
index lock is held across a project-store write.

The lane serializes allocation/release intent for one project; different
projects have independent lanes and ledgers. A shared legacy authority file
(for example a global task snapshot with project partitions) retains its own
single lock, but the coordinator never holds a project ledger/journal lock while
waiting for it. Terminal transitions take only the authority lock. Source
acknowledgement saves its authority marker, unlocks, and then enqueues the
release operation. Monitor evaluation starts only after the source commit and
likewise never nests these locks. Retries use compare-and-swap generation checks
and idempotent operation states rather than relying on a mutex surviving I/O or
restart.

The terminal transition of an already-active entity atomically replaces its
empty reserved slot with the exact terminal event in the **same authority
snapshot** as the terminal state, even when the normal outbox is full. After
source acknowledgement, the authority snapshot atomically replaces the event
with an acknowledged release marker/watermark. A coordinator operation then
marks the ledger token `releasing`, removes the authority marker and frees the
ledger slot; crash recovery can prove either side from the operation ID and
watermark. A recovered legacy active entity without a reservation is marked
`monitoring_degraded` at bootstrap and uses the fixed gap record at completion
rather than blocking reaping.

If a nonterminal event cannot enter the normal outbox, or an unavoidable
transition lacks an exact reserved slot because of legacy/corrupt state, the
authoritative transition still commits with an updated gap record (`kinds`,
first/last version, lost count). The source is marked `monitoring_degraded`,
affected monitors are disabled with `ErrorCode=source_gap`, and the UI reports
lost coverage. This is fail-closed for monitoring without blocking user-direct
Run completion, task completion, scheduler cleanup or process reaping. A
disk/atomic-save failure remains a source durability fault; no monitor layer can
make an unwritable source durable.

`source_gap` is recoverable only through explicit loss acknowledgement; a plain
enable request is rejected while any entry is outstanding. Applying a gap
disables each affected monitor and inserts/updates the entry keyed by canonical
`(AuthorityID, SourceKind)` in `SourceGaps`. A first gap creates the entry. A
newer gap for the same key expands the range/count when the current entry is
still unacknowledged. If the current entry was already acknowledged, the newer
gap replaces its outstanding range/count, advances `GapVersion` and clears the
old `AcknowledgedGapVersion`, principal/time and barrier; the prior
acknowledgement remains only in bounded
audit history. A gap from a second authority/kind creates a second entry and
never overwrites the first.
The UI lists every exact outstanding interval and offers acknowledgement per
entry, never a misleading aggregate retry.

After the registry durably copies a gap into every monitor subscribed to that
authority/kind at that version, the source consumer may acknowledge/compact the
global gap by advancing that authority's `LastAppliedGapVersion`; each
per-monitor map entry is the durable recovery record. A monitor created later
records the current version of every subscribed authority as its initial
baseline and is not retroactively disabled for an older, already-applied gap.

`acknowledge_monitor_gap` must name `AuthorityID`, `SourceKind` and the exact
expected `GapVersion`; it acknowledges only that map entry. Acknowledgement and
baseline capture use `RuntimeMutationCoordinator`:

1. persist an operation ID and pause that monitor for the named authority/kind;
2. recheck the expected gap version, then have that source authority persist its
   current version as a barrier, ensuring every later version remains in its
   exact outbox/reserved slot;
3. atomically save that entry's acknowledged gap version, principal/time and
   `ResumeAfterVersion=barrier`, then commit the operation;
4. leave the monitor disabled until a separate explicit enable.

Enable succeeds only when every entry has
`AcknowledgedGapVersion == GapVersion` and every named authority still validates
the saved barrier. The monitor intentionally ignores each authority's versions through
its acknowledged barrier and starts at the next version independently. If any
newer gap appeared during or after acknowledgement, enable fails with
`source_gap_newer` and names the stale entry; already-valid acknowledgements for
other keys remain intact. Crash recovery completes or rolls back each
coordinator operation idempotently, so there is neither an unrecorded baseline
jump nor a window between baseline and exact outbox delivery. Deleting the
monitor is the only alternative to acknowledging every loss. Successful enable
clears `ErrorCode=source_gap` and returns to `active` but retains the bounded,
acknowledged map as audit/diagnostic state.

The monitor consumer handles an event as follows:

1. read an outbox event; the in-memory broadcast is only a wake-up optimization;
2. under the registry writer lock, skip it if an applied-event receipt already
   exists; otherwise evaluate all eligible monitors and atomically save their
   counters/pending fires plus that receipt;
3. remove/ack the source outbox entry in a later source-snapshot save.

A crash before step 2 leaves the outbox event. A crash after step 2 but before
step 3 replays it, but the registry receipt prevents a second evaluation. Source
outboxes and applied-event receipts are bounded and compact only after both the
receipt and source acknowledgement are durable. Gap records follow the same
apply/ack protocol and remain visible after affected monitors are disabled.

`process_output` is explicitly a live, bounded stream rather than an fsync per
line. The collector must always drain/write the child log even when monitor
evaluation is slow. It feeds a bounded evaluation queue with `(process_id,
output_seq)`. On overflow it records a durable gap range, increments metrics,
and updates affected monitor diagnostics to `output_gap`; it never claims those
lines were evaluated. Gap ranges are coalesced off the collector hot path and
persisted by the process-registry writer, while affected monitor diagnostics go
through the monitor-registry writer; the stdout/stderr reader waits for neither.
Output monitors needing crash-complete guarantees should use a bounded script
probe against durable state instead.

`Process.OutputGaps` keeps at most the last 32 exact contiguous ranges.
`OutputGapCount` is the cumulative number of ranges ever opened and increments
once when a lost line is not contiguous with the current last range;
`OutputLostLines` increments for every lost line. When an alternating
overflow/success pattern creates the 33rd range, the oldest exact range is
evicted only after its bounds are folded into
`OutputGapOldestSeq`/`OutputGapNewestSeq`; its range and line counts are already
represented in the cumulative fields and are not incremented again. Successful
lines never cause two separate gaps to be falsely merged. The cumulative
summary and recent 32 ranges follow the terminal process record's retention
lifecycle.

A completed script-probe result is applied under the registry writer lock:
`LastCheckedAt`, error/cooldown counters, next-check time and any pending fire
are one atomic monitor snapshot save. A crash before that save loses the one
tick, and the no-catch-up rule intentionally does not replay it; a crash after
the save recovers the pending fire. This at-most-once check boundary is shown in
the product contract rather than described as durable event delivery.

## How the two parts compose

Process exit publishing `process_changed` on the existing bus means the second
feature needs no special case for the first:

```
run_background "npm test"
  └─ process exits nonzero
       └─ emits process_changed (entity=<process id>, to=failed)
            ├─ monitor A: process_exit + failure_only  → notify_agent(reviewer)
            └─ monitor B: runtime_event ["process_changed"] → blackboard signal
```

And `process_output` gives the log-watching case (`notify me when the server
prints a stack trace`) without polling, because the supervisor already sees every
line.

## Product and prompt surface

Project detail gains one **Background activity** entry with two views:

- **Processes** — command summary, owner, state, runtime, exit code, log size,
  last line, `View log`, and `Stop`. Starting a process shows a persistent
  "Runs on the Karoz server; closing this browser will not stop it" notice.
- **Monitors** — human-readable trigger/action, state, owner, last check, next
  check, last match, trigger count, cooldown/expiry, and `Pause`, `Resume`,
  `Edit`, `Run check now`, `Delete`. `process_output` monitors carry a
  **Live / best effort** badge and show cumulative lost-line count plus the most
  recent sequence gaps; any gap changes their state to visible
  `coverage degraded`. Durable-source `MonitorGap` records disable affected
  monitors and display `disabled: source coverage gap` with source kind and
  version range. It renders one row per authority/kind gap.
  `Acknowledge lost coverage` names that row and shows the baseline that will be
  skipped; acknowledgement and `Resume` are separate actions, and Resume stays
  disabled until every row is acknowledged.

The monitor editor defaults to no-code trigger forms for runtime events,
process exit and output matching. **Temporary code** is an Advanced
`script_probe` approval editor with language, interval, timeout, workdir, code
and expected JSON result. It can reserve and confirm an approval but cannot save
or enable the probe; after confirmation it displays the bound monitor ID,
trigger revision and “Complete creation in a dev agent turn.” `Run check now`
on an already-created probe executes the same
immutable approved snapshot and returns a transient result, but it never
executes the action and never mutates `LastCheckedAt`, `NextCheckAt`,
`ConsecutiveProbeErrors`, state, trigger/cooldown/rate counters or pending fires.
It consumes the same concurrent-probe capacity and requires the same receipt.
Even an authorization/integrity failure is returned without mutating the dry-run
target; the next scheduled tick applies the fail-closed state transition. The UI
labels the result as a dry run so testing temporary code cannot accidentally
wake an agent.

On Windows the script-probe editor is disabled with "Not supported in v1";
event, process-exit and live process-output monitors remain available.

The header shows compact counts (`2 processes · 3 monitors`). Reconnecting after
a browser close loads persisted state from the APIs; the UI does not attempt to
infer liveness from an old SSE connection. An interrupted process is displayed
as "Karoz restarted; process was not resumed", never as still running.

`buildResidentAgentPromptWithMemoryQuery` gains two compact sections, rendered
only when non-empty:

- running and recently-finished background processes: id, state, exit code,
  runtime, last line;
- active monitors: id, one-line trigger summary, trigger count, last match.

The agent learns outcomes by observation instead of asserting them. Note this
adds to the prompt bloat described in `agent-runtime-review.md` (S2-1); both
sections should be capped hard (e.g. 5 processes, 5 monitors) and are candidates
for the "behind a tool rather than always injected" treatment if that finding is
addressed first.

Probe source, raw stdout/stderr, full logs, approval subjects and environment
values are never prompt-injected. The agent uses the read-only tools when it
needs bounded diagnostics.

## Observability

Structured logs always include `project_id`, `agent_id`, and `process_id` or
`monitor_id`; dispatch logs also include `monitor_fire_id`. They log the source
digest but never probe source, environment values, raw approval subjects or
unbounded command output.

Required counters/gauges:

- processes started, active, terminal by state, spawn failures, forced kills,
  parent-death guard activations, lifetime expiries, concurrency rejections,
  retention cleanup and truncated log bytes;
- monitor evaluations, matches, fires and suppressions by reason;
- probe duration, timeout/error count and skipped-overlap ticks;
- pending-fire depth, scheduler-admission retries and monitors auto-disabled by
  rate/error/pending-cap guards;
- source-outbox depth, applied/sink receipt depth, output sequence gaps and
  resource-cap rejections;
- terminal-ledger occupancy/recovery mismatches, outstanding source gaps by
  authority/kind, and staging/retired probe snapshot slots/deletion failures.

The product state and logs must distinguish `matched`, `suppressed`, `dispatch
pending`, `scheduled`, and `probe error`; collapsing all five into "last run"
would make operational failures look like a condition that did not match.

## Persistence and recovery

The new global `Settings.DataDir/processes.json` and mode-`0600`
`Settings.DataDir/monitors.json`, each project's runtime-owned
`Settings.DataDir/project-runtime/<safe-project-key>/{terminal-reservations,runtime-mutations,source-journal}.json`,
the independent `Settings.DataDir/project-runtime/index.json`, source outboxes
and sink receipts use atomic-file persistence through the no-follow directory
boundary above.
The monitor snapshot includes the bounded security registry (sessions,
reservations, challenges and receipts); it is never returned wholesale by an
API. Logs stay in separate files so snapshots remain small. Each aggregate has
one owning lock and one writer; callers never copy a snapshot, unlock, and later
overwrite a newer snapshot.

Startup (`bootstrap`):

1. Enumerate canonical project records first. Reject duplicate canonical paths
   and a runtime data root nested in a project before loading any monitor-aware
   state. Strictly load the independent project-runtime index before any
   per-project journal; if monitors or another runtime record exist while the
   index is missing/corrupt, fail closed rather than infer first installation.
2. For each project, derive its safe key and walk the runtime-owned directory
   without following links/reparse points. Resolve an `initializing` sentinel
   through its explicit initialization operation. A `ready` sentinel requires a
   valid identity-matching source journal. Load that journal, the coordinator
   journal and 4,096-slot terminal ledger (including workspace manifests)
   **before** any `plans.json`, `artifacts.json`, `groups.json` or generic
   missing-as-empty loader runs. Require every embedded project identity field
   to match.
3. Validate each workspace manifest and then load its authority snapshot
   strictly. Missing/replaced/symlinked/regressed state appends the exact
   authority gap to the runtime source journal and marks that source
   unavailable; it is never installed as an empty in-memory value. Complete or
   roll back incomplete workspace-source coordinator intents idempotently.
4. Load/migrate native atomic source snapshots, process, monitor/security,
   run-terminal, scheduler/admission-receipt and sink-receipt stores. After
   migration these loaders reject missing/corrupt monitor-aware snapshots rather
   than quarantining and defaulting empty.
5. Project by project, before normalization or new admission, complete or roll
   back every incomplete terminal-reservation operation and verify a bijection
   between that project's occupied ledger tokens and its active,
   terminal-unacknowledged or coordinated release-marker authority slots.
   Corruption disables background/monitor admission for that project and creates
   its explicit source gap; it cannot consume, release or disable another
   project's slots. Unproven mismatches are never guessed free.
6. For every recovered `starting`/`running` process, consume its reserved
   terminal slot and atomically save `state=interrupted`, cleared
   PID/PGID/guard fields and the stable
   `process/<id>/terminal` outbox event in the **same process snapshot**. A
   legacy/corrupt record without a reservation writes the fixed source gap
   instead, never a silent state-only normalization. Also clear runtime-only
   `ProbeRunning`; expire monitors and approval security records; prune
   `RecentFires` by timestamp without resetting the rate ceiling; validate every
   active probe's claimed, monitor/revision-bound receipt.
7. **Durably save all normalized snapshots before any notification, event
   dispatch, pending-fire drain or probe re-arm.** If this save fails, bootstrap
   fails closed with background/monitor runtimes disabled.
8. Consume durable native/source-journal outboxes through applied-event receipts
   and gap records through their per-key monotonic applied cursor. Native
   authority generations advance only that authority's cursor; workspace
   entries advance `AckedThroughJournalSeq` only across the contiguous applied
   journal prefix. Then acknowledge successfully applied entries/versions.
9. Drain retained pending fires through the action-specific tri-state sinks.
   Disabled/deleted/owner-deleted cancellations remain cancelled;
   expired/exhausted/error monitors drain already-accepted fires as specified
   above.
10. Using the loaded source acknowledgement watermarks and monitor pending set,
   prune only provably orphaned applied/sink receipts and finish or roll back
   incomplete approval-claim and gap-baseline coordinator operations. Sweep
   staging/retired probe snapshot records and files; a failed unlink retains its
   counted slot and is retried later.
11. Notify each owning agent about interrupted processes through a durable,
   idempotent recovery notice keyed `process/<id>/recovery-notice`; response-loss
   or another bootstrap cannot duplicate it.
12. Re-arm active, receipt-valid script probes at `now + interval`. Do not replay
   ticks missed while Karoz was stopped.

PID values are diagnostic only after restart and are always cleared. Karoz
never probes or kills a recovered PID because the operating system may already
have reused it.

## HTTP surface

Under `handleProjectScoped`, matching existing conventions:

```
GET    /api/projects/{id}/processes
GET    /api/projects/{id}/processes/{pid}
GET    /api/projects/{id}/processes/{pid}/log?offset=&limit=&tail=
POST   /api/projects/{id}/processes/{pid}/stop
GET    /api/projects/{id}/monitors
POST   /api/projects/{id}/monitors
PATCH  /api/projects/{id}/monitors/{mid}
DELETE /api/projects/{id}/monitors/{mid}
POST   /api/projects/{id}/monitors/{mid}/check
POST   /api/projects/{id}/monitors/{mid}/acknowledge-gap
POST   /api/projects/{id}/monitor-probe-approvals/prepare
POST   /api/projects/{id}/monitor-probe-approvals/{challenge}/confirm
```

The monitor POST route accepts only non-script triggers. PATCH may update an
existing script monitor's name, action, guards, pause/disable state, or resume
state (subject to valid receipt and gap checks), but the **presence** of any
trigger, source, execution-field or approval-receipt mutation in an HTTP PATCH
is rejected with `dev_turn_required`, even if the supplied value is unchanged.
Probe approval routes reserve/confirm authorization data but never create,
replace, enable or claim a probe; only the dev-turn tool boundary can perform
that claim. The gap route requires
`{authority_id, source_kind, expected_gap_version}`, records only that loss
acknowledgement/baseline, and does not enable the monitor.

These are new state-changing routes with no authentication, which makes finding
S1-1 in `agent-runtime-review.md` strictly worse: `POST .../processes/{pid}/stop`
and monitor creation become additional CSRF targets. **The Origin/content-type
middleware should land before or with this feature**, not after.

Every `POST`, `PATCH`, and `DELETE` above, including the dry-run check, requires
an allowed loopback Origin and JSON content type before body parsing or lookup.
No route is exempt because it is "only pause", "only delete", or "only test".
Unknown/missing Origin, form posts and text/plain simple requests are rejected.

All project-scoped lookups verify that the process/monitor belongs to the path
project before returning or mutating it. List/read responses omit probe source
and approval data. There is no generic endpoint that reads, replaces, creates or
claims executable probe source; the preparation endpoint accepts bounded source
only to mint the exact dev-turn-claimable approval described above.

## Test plan

Pure-domain tests (no processes, no models):

- process state machine: `starting` transition rules; terminal states reject
  further transitions; `Normalize` converts `starting`/`running` to
  `interrupted`; `Succeeded` is false for killed and interrupted.
- `OutputBuffer`: partial lines held until newline; `\r\n` handling; byte cap
  marks truncated and drops overflow; tail ring evicts oldest; `Flush` emits a
  trailing partial line; concurrent stdout/stderr acceptance assigns unique
  monotonically increasing sequences.
- monitor matching: each trigger kind, entity/state filters, empty
  `EventKinds` never matches, invalid regex never matches.
- monitor guards: cooldown suppression; the rate ceiling auto-disables at the
  7th fire in-window; `MaxTriggers` exhaustion; expiry; self-origin exclusion;
  monitor-origin exclusion unless opted in; guard precedence.
- `ProbeAuthorized` requires a claimed durable receipt and rejects monitor ID,
  trigger revision, claim mutation, identity, language, project, agent,
  canonical workdir, interval, timeout, source bytes/hash, symlink, mode or
  runtime-owned-file mismatch; restart with the valid receipt remains
  authorized.
- script result parsing: exact valid object, `matched` true/false, bounded detail,
  extra output, malformed JSON, nonzero exit, timeout and consecutive-error
  disable.
- probe timing: no overlap, late ticks coalesce, restart schedules one future
  tick and never catches up missed ticks.
- pending-fire recovery preserves stable IDs, retries scheduler admission, and
  errors rather than silently overflowing; frozen action/event revisions do not
  change after monitor edit.
- disabled/deleted/owner-deleted cancellation versus
  expired/exhausted/error draining semantics.
- definition, active-monitor, enabled-probe, concurrent-probe, operator-session,
  approval reservation/challenge/unclaimed-receipt, probe-snapshot/retired-file,
  pending-fire, terminal-reservation and terminal-retention caps at every
  boundary. Exhausted terminal reservations reject a new active admission before
  its effect starts; already-active terminal transitions never block.
- tri-state sink admission for notify and non-actionable blackboard signals,
  including completed-receipt lookup.
- receipt-capacity math and compaction: 4,096 normal source events plus 4,096
  reserved terminal events fit exactly within 8,192 applied receipts; the gap
  cursor consumes none, 256 pending fires fit within 512 sink receipts, and
  774,144 legal weekly fires affect only bounded last-100/hourly audit summaries.
- terminal-ledger validation requires the same non-empty project ID on ledger,
  canonical-path identity on ledger, operation, reservation and authority
  record; equal authority/entity IDs from different projects never compare
  equal. Safe-key derivation always produces exactly 64 lowercase hex characters
  even for traversal-shaped project IDs. Two independent ledgers can each
  allocate all 4,096 slots.
- source registry rejects unregistered/live-only event kinds and classifies each
  durable aggregate with its exact native or coordinated path; a completeness
  test fails when an emitter kind lacks authority/class/gap policy.
  `group-inbox.json` is rejected because it has no v1 event kind. Structured Origin JSON
  validation/round-trip requires both monitor and fire IDs.
- source-journal cursor policy keeps project `JournalSeq` separate from
  authority generation: plan generation 100, artifact generation 1 and group
  generation 50 receive independent contiguous journal sequences; out-of-order
  applied receipts cannot advance or compact past a missing sequence. Full
  capacity creates no sequence hole, and keyed gaps for all three authorities
  coexist/coalesce independently.
- source-gap acknowledgement records the exact authority/kind/gap version and
  coordinated source barrier, never enables implicitly, rejects a newer
  intervening gap and resumes only at `barrier+1`. Interleaved task+plan gaps
  occupy two entries; acknowledging either cannot overwrite or satisfy the
  other, and the 16-kind validation cap bounds the map.
- alternating output gaps retain exactly 32 recent ranges while cumulative
  range count, lost-line count and oldest/newest bounds remain exact without
  double-counting evicted ranges.

Runtime tests in `cmd/karoz`:

- process spawn transaction failpoints: log open, watchdog creation, OS spawn,
  collector arm, waiter arm and running-record persistence. Every post-spawn failure must
  kill and wait the group, close descriptors, persist failure and leave no
  zombie;
- run `true` and a one-write/instant-exit fixture across every registration
  failpoint; early exit is reaped, short output is retained exactly once after a
  successful registration, and no output/event escapes a rolled-back spawn;
- force a log-write failure after the child starts; assert the group is killed
  and the record becomes failed;
- hard-crash fixture: start a child and grandchild, SIGKILL the Karoz parent
  without graceful shutdown, and assert the watchdog/Job Object removes the
  entire descendant group;
- a background process survives the Run that started it (finish the run, assert
  the process is still `running`);
- interrupt the creating Run while the process is running; assert the child,
  registry record and watcher remain active;
- close the HTTP/SSE response and a real browser tab while a process and monitor
  are active; assert both continue, then reconnect and observe current state;
- a non-dev turn gets a `choice_request` for `background:<command>` and a
  foreground approval for the same command does **not** authorize it; start and
  stop subjects cannot authorize the other operation or a different process;
- in normal live operation, exit emits one `process_changed` event and schedules
  one matching monitor fire;
- a notify action produces one scheduled run with the expected dedup key, and a
  duplicate fire within the window does not produce a second;
- source-event crash windows: crash after source+outbox save but before monitor
  apply, and after registry apply but before source acknowledgement; recover
  exactly one pending fire with one applied-event receipt;
- for native `tasks.json`, `agent-inbox.json`, `agent-memory.json`,
  `agent-blackboard.json` and `agent-run-queue.json`, migrate legacy JSON once,
  persist version/state/outbox/gap atomically, then prove missing/corrupt
  post-migration files fail closed rather than load empty;
- for plan, artifact and group workspace authorities, crash before/after intent,
  each workspace save, runtime source-journal save and operation commit; recover
  either the exact event once or an explicit gap. The two-store group mutation
  is never visible after only `groups.json` or only `plans.json` commits;
- interleave plan authority generation 100, artifact generation 1 and group
  generation 50 in one project journal. Deliver their journal sequences with
  out-of-order wakeups and crash before/after each applied receipt and
  acknowledgement save; prove no authority generation is compared with the
  journal cursor, no entry is skipped/pruned before the contiguous prefix, and
  every exact event is applied once;
- apply journal sequence 3 before 1 and 2 and assert the acknowledgement remains
  unchanged, advances only to 1 after sequence 1, then advances through 3 after
  sequence 2. Restart at every step and prove receipt cleanup follows the same
  prefix. Regression/reuse/overflow of `NextJournalSeq` fails closed;
- fill the shared journal while plan, artifact and group authorities each lose
  events. Assert their canonical keyed gaps coexist, repeated loss coalesces
  only the matching authority-generation range, and copying/acknowledging one
  gap cannot overwrite, unblock or baseline another;
- distinguish first installation from source-journal loss: an empty project
  writes and fsyncs `initializing → baseline journal → ready`; crash at each
  boundary and recover explicitly. After `ready`, delete/corrupt/replace the
  journal and assert startup fails closed with visible source degradation
  instead of recreating a baseline. Delete/corrupt the independent runtime index
  while monitors or runtime records exist and assert the same fail-closed
  result;
- with active plan/artifact/group monitors, run `git clean -fdx`, delete each
  workspace authority, and replace each file or `.karoz` component with a
  symlink/reparse fixture between every crash point. The runtime-owned exact
  event journal survives; bootstrap/preflight/periodic reconciliation produces
  the correct authority+kind `source_gap`, never an empty default or false
  acknowledgement;
- initial workspace-source monitor creation against an absent file persists an
  explicit versioned-empty snapshot plus manifest baseline before arming. A
  subsequent absence is therefore distinguishable and gaps;
- bootstrap a recovered running process across crash points before/after the
  normalized process snapshot and before/after monitor apply; it consumes one
  reserved terminal slot, persists one stable interrupted terminal event (or an
  explicit legacy gap), fires each matching monitor exactly once and never
  emits a state-only interruption;
- fill a source normal outbox; the transition commits with a durable coalesced
  gap and affected monitors disable rather than silently missing coverage.
  Reserved Run/task/scheduled-job/process terminal slots still commit exact
  terminal events; exhaust the reservation path and assert the core terminal
  transition still completes through the fixed gap record rather than wedging.
  Atomic-store write failpoints produce no partial state/event pair and surface
  an explicit source durability fault;
- exercise central terminal-ledger admission across crashes before/after token
  allocation, authority active+reserved-slot save and coordinator commit, plus
  terminal-event acknowledgement/token release. Recovery preserves a bijection,
  releases only proven unused tokens and never admits an entity without its
  authority-local reserved slot;
- exhaust project A's 4,096 slots while project B still allocates its own full
  capacity; corrupt A's ledger/journal and prove B continues unchanged. Use the
  same authority/entity strings in both projects and prove recovery cannot
  cross-match, consume or release the other project's token. Duplicate canonical
  project paths and embedded mismatched project IDs fail closed before ledger
  mutation;
- with an active background/task entity, run `git clean -fdx`, delete/rewrite
  `Project.Path/.karoz`, and replace it with a symlink from inside the workspace;
  the runtime-owned ledger/journal remain intact, the terminal event persists
  and token release completes. A `Settings.DataDir/project-runtime` root nested
  under any project fails admission;
- replace the runtime root, safe-key directory and each ledger/journal file in
  turn with a Unix symlink or Windows reparse-point fixture; no target is
  followed or mutated, the affected project fails closed, and another project's
  runtime continues;
- concurrently allocate, terminalize, acknowledge and release within one project
  and across two projects using delayed/failing instrumented stores. Lock hooks
  assert no journal/ledger/authority/monitor locks overlap across a store write;
  repeated `-race` runs and a bounded deadlock timeout prove the documented
  coordinator-lane ordering;
- exercise one single-aggregate and one coordinated multi-store mutation across
  every crash point; only committed authoritative versions become visible;
- interleave `task_changed` and `plan_changed` gaps for one monitor, crash during
  each independent acknowledgement/barrier transaction, inject a newer task gap
  after task acknowledgement, and prove enable remains blocked until both
  current entries are acknowledged with valid barriers;
- sink crash windows for both actions: crash after pending persistence, after
  sink admission and after completed effect; tri-state admission and retained
  receipts must not redirect or repeat the effect;
- crash after source acknowledgement/pending removal but before receipt cleanup;
  bootstrap prunes the proven applied/sink orphan. A receipt without the
  corresponding ack watermark or absent-pending proof is retained and diagnosed;
- edit the target/template/topic after pending persistence; retry uses the
  frozen action revision, and a monitor blackboard signal never starts idle
  reconciliation;
- overflow the runtime/output evaluation channels; durable source events remain
  in outbox, output collection keeps draining, and output monitors show the
  exact skipped sequence gap;
- concurrently match two monitors and mutate a third; repeated/race runs prove
  the serialized registry writer loses no counters, applied-event receipts or
  pending fires;
- a monitor Run creates a task, handoff and blackboard entry through tools;
  every downstream event round-trips the structured monitor+fire origin and
  cannot re-trigger the source monitor; malformed/unstructured monitor origins
  are rejected;
- restart recovery: `running` becomes `interrupted`, its stale PID is never
  signalled, the owner is notified, monitors re-arm without catch-up, and the
  rolling rate window is not reset;
- stopping, lifetime expiry and natural exit racing together produce one legal
  terminal state and one terminal event;
- durable claimed receipt survives restart; changing
  interval/timeout/workdir/language/source increments trigger revision and
  requires a new receipt. Copying the receipt to an identical second monitor,
  replaying it after claim, or claiming it for a different monitor/revision all
  fail without increasing cadence;
- UI approval preparation requires the same operator session for
  prepare/confirm; changed-payload, cross-session and expired challenges fail.
  Same-session confirmation retry returns the original receipt, while a new
  replay cannot mint another. Crash failpoints around challenge consume/receipt
  mint produce both or neither authorization records. Staging-write,
  confirmation-save, immediate-delete and sweep failpoints retain a counted slot
  and repeated approval attempts never exceed 228 files/slots; next-operation,
  periodic and bootstrap sweeps remove provable orphans. Receipt-claim/monitor-
  mutation failpoints produce both or neither. Agent Run/choice flow cannot be
  substituted for the UI flow;
- HTTP monitor create rejects `script_probe` even with a valid UI receipt;
  confirm leaves no monitor armed, and only a dev-turn tool can atomically claim
  it. Safe action/guard/pause/resume PATCH of an existing probe succeeds, while
  any trigger/source/execution/receipt field is rejected even when unchanged.
  Plan/ask tools and UI cannot perform the claim;
- replace/symlink/swap the approved probe after open/hash but before execution;
  execution uses the exact verified bytes from the opened fd/stdin, while every
  integrity/receipt mismatch immediately errors without executing;
- Windows rejects script-probe prepare/create/enable/check before writing
  challenge, receipt or process state; non-script monitors and Job-Object
  background processes continue to work, and a loaded Unix probe normalizes to
  unsupported error without execution;
- hit monitor/probe/concurrency/pending and terminal/log-retention caps; no
  unbounded goroutine, subprocess, snapshot or log growth occurs;
- dry-run returns its result while leaving every persisted scheduling,
  counter/error/state/next-check field byte-for-byte unchanged;
- delete/disable while a probe tick is running, then release the child; no
  post-disable action fires;
- delete an owning agent during a background process and during a probe tick;
  both groups are killed/waited, processes terminate, monitors disable, pending
  fires cancel, and no identity is reassigned;
- each new mutating HTTP route rejects missing/cross-origin Origin,
  `text/plain`, form and non-JSON requests before mutation, including both
  UI approval challenge endpoints and gap acknowledgement.

Manual product gate:

1. Start a server through `run_background`.
2. Create a monitor for one log line. Prepare/confirm a script-probe approval in
   the UI, verify it is not armed, then complete its bound creation in a dev
   agent turn.
3. Close Chrome completely, wait past at least two probe intervals, reopen it,
   and verify the process, checks and trigger history continued.
4. Finish and separately interrupt the creating Agent Run; repeat the same
   assertion.
5. Stop the Karoz server, restart it, and verify the process is `interrupted`
   while monitors resume from persisted definitions without replaying missed
   ticks.

## Rollout plan

1. **Domain gate:** `internal/process` + `internal/monitor`, source outbox,
   receipts, event identities, frozen pending-fire/action model, tri-state sink
   admission and pure tests. No subprocess or dispatch wiring.
2. **Background-process gate:** transactional spawn, parent-death guard/Job
   Object, supervisor, output collector, exit watcher, retention, log store,
   tools/APIs and recovery. Release only after hard-crash orphan, failpoint,
   browser-disconnect and turn-end survival tests pass.
3. **Event-monitor gate:** monitor registry, `runtime_event` and `process_exit`,
   durable source outboxes/applied receipts, action-specific pending dispatch,
   tools/APIs and origin/loop guards. No process-output or executable probes yet.
4. **Output-monitor gate:** complete-line `process_output`, sequence identity,
   regex matching and truncation tests.
5. **Script-probe gate:** immutable source store, durable exact-field approval
   receipts, same-fd/stdin execution, allowlisted interpreters, resource caps,
   structured result, non-overlap scheduler and diagnostics. This lands last
   because it extends DECISION 0001's unattended execution boundary.
6. **Product gate:** Background activity UI, reconnect behavior, manual
   browser-close/turn-end verification, CSRF gates for every mutation, focused
   Go tests, full Go/race/vet and JS suites.

Each gate is independently revertible. Disabling monitor evaluation leaves
background processes usable; disabling script probes leaves event/output
monitors usable. Rollback never deletes persisted definitions or logs: it stops
new evaluation/spawn, terminates live children safely, and preserves records for
inspection.

### Implementation map

The expected ownership keeps pure policy out of the HTTP/tool layer:

| slice | primary files |
| --- | --- |
| process domain | `internal/process/model.go`, `output_buffer.go`, retention policy, tests |
| monitor domain | `internal/monitor/model.go`, `match.go`, `fire.go`, `probe_result.go`, approval/action snapshots, tests |
| process runtime | `cmd/karoz/process_supervisor.go`, `process_guard.go`, `process_log.go`, Unix watchdog/Windows Job Object helpers |
| monitor runtime | `cmd/karoz/monitor_runtime.go`, `monitor_probe.go`, `monitor_dispatch.go`, source-outbox consumer |
| persistence/bootstrap | `cmd/karoz/application.go`, `types.go`, `store.go`, source outboxes, receipts, scheduler integration |
| tools and approval | `tool_background.go`, `tool_monitor.go`, `tool_catalog.go`, `tool_registry_adapter.go`, existing bash approval helpers |
| HTTP | `api_project.go` plus focused process/monitor handlers and local-boundary tests |
| product | `static/index.html`, `static/js/panels.js` or a dedicated `background-activity.js`, `static/css/app.css` |

`cmd/karoz` owns orchestration only. Process state transitions, matching,
guards, source-hash validation inputs and fire decisions remain pure domain
functions so race-heavy runtime tests are not the only proof of correctness.

## Non-goals

- No restart or supervision policy (`restart: always`). A dead process stays
  dead and the agent decides what to do.
- No cross-project monitors.
- No monitor action that mutates a repository or runs a command.
- No log indexing or search beyond the regex matcher and bounded reads.
- No process resurrection across restarts.
- No claim that a background process survives the Karoz server or machine.
- No arbitrary interpreter path, uploaded binary, shebang-selected runtime or
  mutable script file for probes.
- No exactly-once claim across crash recovery; delivery is explicitly
  at-least-once with a stable fire identity.

## Resolved v1 choices

- Ownership remains `(project, agent)` for accountability. Deleting an agent
  first blocks new owner work, disables its monitors/cancels undispatched fires,
  cancels and waits active probe guards, terminates and waits live background
  groups, durably saves `LastError = "owner deleted"` and terminal process
  states, and only then removes the agent. Karoz does not silently reassign
  unattended execution to another identity. A future explicit transfer
  operation can be designed separately.
- `notify_agent` may target only `ask` or `plan`. A monitor may not start an
  unattended `dev` turn; the woken agent must request the normal approval before
  effectful work.
- V1 uses one per-project process concurrency cap of 8. It does not classify
  "server" versus "batch" from the command string. Metrics will show rejection
  count and utilization before separate pools are considered.
- A monitor may target only an agent in its own project. Cross-project monitors
  and actions remain out of scope.
