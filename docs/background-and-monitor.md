# Background Processes and Monitors

Status: proposed (2026-07-27). Nothing in this document is implemented yet.
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

## Motivation

`bash` is synchronous and bounded: 60s default timeout, 300s hard cap, inside a
tool phase that only gets 90s total (`residentToolPhaseTimeout`). Dev servers,
file watchers, long builds, and full test suites do not fit. Today an agent asked
to "start the dev server and check the logs" either blocks until the tool-phase
budget kills it, or reports a success it cannot have observed — which is exactly
the failure mode the prompt's evidence rules keep trying to talk the model out
of.

The reactive side has the same shape of gap. `emitRuntimeStateChanged` already
publishes every run, task, handoff, and plan transition, and
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
    StateRunning State = "running"
    StateExited  State = "exited"      // exit code 0
    StateFailed  State = "failed"      // nonzero exit, spawn failure, lifetime cap
    StateKilled  State = "killed"      // explicit stop_process
    StateInterrupted State = "interrupted" // server restarted while running
)

// running -> {exited, failed, killed, interrupted}. Terminal states are final,
// so a late exit callback can neither resurrect nor relabel a stopped process.
func CanTransition(from, to State) bool

type Process struct {
    ID, ProjectID, AgentID string
    RunID        string    // provenance; the process is NOT cancelled with it
    Command      string
    Workdir      string
    Description  string
    PID          int
    State        State
    ExitCode     int
    Error        string
    LogPath      string
    LogBytes     int64
    LogLines     int64
    LogTruncated bool      // output exceeded the cap; process kept running
    LifetimeMS   int64
    StartedAt    time.Time
    UpdatedAt    time.Time
    EndedAt      *time.Time
}
```

`interrupted` is not cosmetic. Children share the server's process group, so a
`running` record restored from disk is a lie. `process.Normalize` forces it to
`interrupted` at startup and the owning agent is told, rather than the agent
being shown a process that no longer exists.

`State.Succeeded()` exists so callers never infer success from an exit code and
accidentally treat `killed` or `interrupted` as fine.

### Output capture (`internal/process.OutputBuffer`)

The pure half of log capture, so it is testable without spawning anything: it
accumulates writes, emits only *complete* lines (a partial write is held until
its newline arrives, so the monitor matcher and the tail view never see a torn
line), keeps a bounded in-memory tail, and enforces a byte cap. It returns the
lines it accepted and the byte count the caller should persist; the caller owns
the file handle.

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

- Spawns `bash -lc` in the project directory with `Setpgid`, reusing
  `prepareResidentBashProcess` so the whole tree can be signalled at once.
- **Not** bound to the Run context — that is the entire point of the feature.
  It is bounded instead by a lifetime cap and by explicit stop.
- Streams output through `OutputBuffer` into
  `.karoz/process-logs/<project>/<id>.log`.
- Keeps the last 200 lines in memory to back the monitor output matcher and
  cheap `list_processes` summaries without touching disk.
- Enforces a per-project concurrency cap so a runaway agent cannot fork without
  bound.
- `stop_process` sends SIGTERM to the group, waits a grace period, then SIGKILL.
- On exit, emits `process_changed` on the runtime event bus — which is what makes
  process exit monitorable with no extra wiring.

### Approval

Same gate as foreground bash, with one deliberate tightening: the approval
subject becomes `background:<command>` rather than the bare command. A user who
approved a foreground command has not approved a daemon that outlives their
turn. This requires turning the approval subject into a structured value
(`residentBashSubject`) instead of a raw string; the single-use,
project-scoped, agent-scoped, exact-match contract from DECISION 0001 is
otherwise unchanged and must keep passing its existing regression tests.

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
    TriggerCommandProbe  TriggerKind = "command_probe"    // bounded, recurring
)

type ActionKind string
const (
    ActionNotifyAgent ActionKind = "notify_agent"  // schedule a run with a briefing
    ActionBlackboard  ActionKind = "blackboard"    // post a signal, wake nobody
)

type ProbeMatch string
const (
    ProbeExitNonZero ProbeMatch = "exit_nonzero"
    ProbeExitChange  ProbeMatch = "exit_change"   // "tell me when it breaks or recovers"
    ProbeOutputMatch ProbeMatch = "output_match"
)

type State string // active | disabled | exhausted | expired | error
```

There is intentionally **no** command-running action. The only monitor-owned
command execution is the approval-gated probe itself. Adding a `run_command`
action would turn a monitor into a persistent unapproved shell, which is the
specific escalation DECISION 0003 forbids.

```go
type Trigger struct {
    Kind TriggerKind

    // runtime_event
    EventKinds           []string
    EntityID             string
    FromState, ToState   string
    IncludeMonitorEvents bool   // off by default: this is the loop class

    // process_exit / process_output
    ProcessID   string
    FailureOnly bool
    Pattern     string

    // command_probe
    Command         string
    IntervalMS      int64
    ProbeMatch      ProbeMatch
    ApprovalSubject string // what the user actually approved; re-checked per tick
}

type Monitor struct {
    ID, ProjectID, AgentID, Name string
    Trigger Trigger
    Action  Action
    State   State

    CooldownMS  int64
    MaxTriggers int

    TriggerCount int
    Sequence     int
    LastMatch    string
    LastError    string
    LastExitCode *int
    LastFiredAt  *time.Time
    RecentFires  []time.Time  // rolling window backing the rate ceiling

    ExpiresAt *time.Time
    CreatedAt, UpdatedAt time.Time
}
```

### Loop and escalation guards

This is the part that has to be right. A monitor whose trigger is
`agent_run_changed` and whose action wakes an agent is an infinite loop, and it
would be trivially easy to write.

1. **Origin tagging.** A monitor-triggered run tags the events it produces with
   `monitor:<id>`. `MatchEvent` drops monitor-originated events unless the
   monitor sets `IncludeMonitorEvents`. This eliminates the whole feedback class
   structurally, rather than blacklisting event kinds one at a time the way
   `maybeTriggerKarozIdleReconcile` currently has to.
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
7. **Dedup.** Notify actions schedule with key `monitor/<id>/<sequence>`, so a
   burst collapses into one run via the existing `SchedulerQueue` dedup.
8. **Probe authorization.** A probe command is approval-gated at creation *and*
   re-validated against `ApprovalSubject` on every tick, so editing a monitor
   cannot substitute an unapproved command into an already-approved one. Probes
   are creatable only in dev turns.

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

`MatchProbe` returns the updated monitor rather than just a verdict, because
`exit_change` must remember the previous exit code even on ticks that do not
fire.

### Tools

`create_monitor`, `list_monitors`, `update_monitor` (enable/disable/adjust
cooldown/expiry), `delete_monitor`. Creation and update are dev/plan turns only;
listing is always available. `command_probe` creation is dev-only.

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

## Prompt surface

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

## Persistence and recovery

`.karoz/processes.json` and `.karoz/monitors.json` via the existing
`persistence.JSONStore`. Logs stay in separate files so the state files remain
small.

Startup (`bootstrap`):

1. Load processes; `Normalize` forces every `running` record to `interrupted`,
   sets `ExitCode = -1` and an explanatory error, clears the PID.
2. Notify each owning agent about its interrupted processes (an
   `appendAgentMessage` system note, matching the task-hook precedent).
3. Load monitors; `Normalize` clears `RecentFires` (wall-clock time passed while
   the server was down, so the window is meaningless) and `LastExitCode` (so
   `exit_change` cannot fire on a stale comparison), and expires monitors whose
   `ExpiresAt` has passed.
4. Restart probe tickers for armed `command_probe` monitors.

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
```

These are new state-changing routes with no authentication, which makes finding
S1-1 in `agent-runtime-review.md` strictly worse: `POST .../processes/{pid}/stop`
and monitor creation become additional CSRF targets. **The Origin/content-type
middleware should land before or with this feature**, not after.

## Test plan

Pure-domain tests (no processes, no models):

- process state machine: terminal states reject further transitions; `Normalize`
  converts `running` to `interrupted`; `Succeeded` is false for killed and
  interrupted.
- `OutputBuffer`: partial lines held until newline; `\r\n` handling; byte cap
  marks truncated and drops overflow; tail ring evicts oldest; `Flush` emits a
  trailing partial line.
- monitor matching: each trigger kind, entity/state filters, empty
  `EventKinds` never matches, invalid regex never matches.
- monitor guards: cooldown suppression; the rate ceiling auto-disables at the
  7th fire in-window; `MaxTriggers` exhaustion; expiry; self-origin exclusion;
  monitor-origin exclusion unless opted in; guard precedence.
- `ProbeCommandAuthorized` rejects a monitor whose command no longer matches its
  recorded approval subject.

Runtime tests in `cmd/karoz`:

- a background process survives the Run that started it (finish the run, assert
  the process is still `running`);
- a non-dev turn gets a `choice_request` for `background:<command>` and a
  foreground approval for the same command does **not** authorize it;
- exit emits `process_changed` and fires a matching monitor exactly once;
- a notify action produces one scheduled run with the expected dedup key, and a
  duplicate fire within the window does not produce a second;
- an agent-run event caused by a monitor does not re-trigger that monitor
  (the loop regression test);
- restart recovery: `running` becomes `interrupted` and the owner is notified.

## Rollout plan

1. `internal/process` + `internal/monitor` domain packages with their tests.
   Pure, no wiring — the guards get proven before anything can spawn.
2. Supervisor, log store, the four process tools, prompt section, persistence and
   restart recovery. Ship background processes alone; they are useful without
   monitors.
3. Monitor registry, event-bus evaluation, notify/blackboard dispatch through the
   scheduler, monitor tools and API. Start with `runtime_event` and
   `process_exit` only.
4. `process_output` matching, then `command_probe` with its approval plumbing —
   the probe last, because it is the piece that most extends the DECISION 0001
   boundary.

## Non-goals

- No restart or supervision policy (`restart: always`). A dead process stays
  dead and the agent decides what to do.
- No cross-project monitors.
- No monitor action that mutates a repository or runs a command.
- No log indexing or search beyond the regex matcher and bounded reads.
- No process resurrection across restarts.

## Open questions

- Should background processes be owned by the *project* rather than the agent,
  so a deleted agent does not orphan a running dev server? Current lean: keep
  agent ownership for accountability, and reassign to `karoz` on agent deletion.
- Should a `notify_agent` action be allowed to target a `dev` turn? It grants
  unapproved bash to a run no user initiated. Current lean: restrict monitor
  notify actions to `ask` and `plan`, forcing the woken agent to request approval
  for anything effectful.
- Does the per-project concurrency cap need to distinguish long-lived servers
  from short batch jobs? A single cap of 8 may be simultaneously too low for
  batch and too high for servers.
