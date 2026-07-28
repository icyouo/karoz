# Agent Runtime Design Review

Status: review (2026-07-27), supplemented 2026-07-28. Scope: the resident agent
runtime as of `37d6a15` — run lifecycle, provider/tool loop, scheduler,
collaboration, task execution, persistence, and the HTTP surface that drives
them. Method: full read of `cmd/karoz` (~14.2k lines, excluding tests) and
`internal/` (~1.9k lines), plus `go build ./...` and `go test ./...` (all
packages pass, ~10s).

The 2026-07-28 supplement re-reviews the tool system, context architecture, and
memory architecture against the working tree that landed M1–M5 of
`docs/agent-runtime-remediation-plan.md` (`go build`, `go vet`, and `go test
./...` all pass, ~21s). Those three areas were scored in the original review but
only partially filed as findings, so the plan's disposition table had no entry
for the tool system at all. See "Findings (supplement)".

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

## Findings (supplement, 2026-07-28)

### Status of the original findings

M1–M5 of the remediation plan have landed in the working tree: `waiting_merge`
plus a locked integration path (`task_worktree.go`, `task_integration_claim.go`),
task cancel/cleanup routes (`task_api.go`), a per-Run event ledger with
`GET .../runs/{runID}/events` replay (`agent_run_ledger.go`,
`agent_run_worker.go`), per-turn-type budgets (`resident_budget.go`), a
`Sec-Fetch-Site`/content-type boundary (`http_helpers.go`), a bounded scheduler
start deadline (`scheduler_worker.go`), and a non-blocking memory query path
(`memory_analysis.go`). S1-1, S1-2, S2-3, S3-1, S3-4, and S3-5 are addressed.

S2-1 is half addressed and is re-filed below as S2-4. S2-2 remains open. S3-2
and S3-3 remain deliberately deferred.

The supplement adds findings in three areas. Memory is the weakest of the three
and contains the only finding here that is a functional failure today.

### S1-3 Chinese-language memory retrieval returns nothing

`relevantMemoriesFor` and `searchArchive` both score through
`memoryMatchScore`, which derives its terms from `strings.Fields`:

```149:164:oss/karoz/cmd/karoz/tool_memory.go
func memoryMatchScore(query string, terms []string, text string) int {
	text = strings.ToLower(text)
	if strings.Contains(text, query) {
		return len(terms) + 4
	}
	score := 0
	for _, term := range terms {
		if len([]rune(term)) < 2 {
			continue
		}
		if strings.Contains(text, term) {
			score++
		}
	}
	return score
}
```

Chinese text has no interword whitespace, so `strings.Fields` returns the entire
question as a *single* term. Both the phrase branch and the per-term branch then
reduce to the same predicate: the stored memory must contain the whole query
verbatim. Scoring the function in isolation against one stored decision
(`决定：登录页的主按钮统一使用品牌蓝 #1A73E8，不再使用绿色。`):

| Query | Terms | Score |
|---|---|---|
| `登录页的按钮用什么颜色` | 1 | **0** |
| `记得我们之前定的登录页按钮颜色吗` | 1 | **0** |
| `登录页的主按钮` (verbatim substring) | 1 | 5 |
| English equivalent memory + `what color is the login page button` | 7 | 5 |

English retrieves; Chinese retrieves nothing unless the query is a literal
substring. The `memoryCueTerms` path (`记得`, `之前`, `上次`) does not help: a cue
only decides *whether* to run retrieval, and retrieval then scores 0 anyway.

The inconsistency is local to one file. `memoryWordCount` is CJK-aware and
counts each Han/Hiragana/Katakana/Hangul rune as a word, so the skip filter
handles Chinese correctly; its sibling scorer does not. The cheap pre-filter
understands Chinese and the scorer does not.

Severity is S1 because this destroys the usable value of memory for a primary
input language while every layer reports success, and because it removes the
safety net that S2-7 depends on.

Direction: segment CJK runs into rune bigrams and add them to the term set,
keeping whitespace splitting for ASCII runs, and weight a bigram hit below a
whole-word hit so fragments do not outrank real matches. Fix the table above into
a test. Retrieval is currently an O(n) scan of every entry on every turn; once
terms are well-defined, an inverted index makes it a set intersection.

### S2-4 The structured transcript is flattened back into prose at the wire

`AgentTranscriptItem` carries `ToolCallID`, `ToolName`, `ToolArguments`,
`ToolResult`, and `ToolSuccess`, so the storage half of the structured-history
work is done and legacy messages merge in lazily. The wire half is not:

```58:71:oss/karoz/cmd/karoz/agent_transcript_prompt.go
	case "tool_call":
		...
		return "tool_call id=" + callID + " name=" + name + " arguments=" + limitString(arguments, 1400)
	case "tool_result":
		...
		return "tool_result call_id=" + callID + " name=" + name + " success=" + success + " result=" + compactToolResultForPrompt(result)
```

`buildResidentAgentPromptWithMemoryQuery` concatenates these lines as
`ROLE: body`, and `runResidentAgentTurn` sends the result as a single `Prompt`
string. Structured records are therefore re-serialized into prose for the model
to re-parse. Only the *current* turn's tool calls reach the provider as native
items; every earlier turn degrades to text.

This costs twice. The `id=` / `name=` / `success=` literals repeat per item per
turn, and providers are trained on native `function_call` / `function_output`
items (Claude: `tool_use` / `tool_result` blocks), so prose trajectories get
weaker adherence than the format the model was optimized on.

Direction: have `agentTranscriptForModel` feed a provider-neutral item list, and
let each wire adapter map it to the Codex `input` array or Claude message blocks.
The plan's M6 tests already require exactly this ("provider-neutral transcript",
"tool call/result pairs survive the next user turn"); the missing piece is the
adapter mapping, not the data model.

### S2-5 Reasoning content is requested, billed, and discarded

```168:171:oss/karoz/cmd/karoz/provider_codex_stream.go
		"store":               false,
		...
		"include":             []string{"reasoning.encrypted_content"},
		"reasoning":           map[string]any{"effort": thinkingEffort, "summary": "auto"},
```

`provider_codex_sse.go` parses only `message` and `function_call` items; it has
no reasoning branch, and nothing in the tree stores or replays a reasoning item.
Combined with `store: false` and no `previous_response_id`, the model re-derives
its plan on every tool round of a multi-step turn while the runtime pays for the
encrypted reasoning it then drops.

Direction: capture reasoning items from the stream and replay them in order in
subsequent requests *within the same turn*; encrypted payloads need to be passed
through, not interpreted. Dropping them across turn boundaries is fine.

### S2-6 Memory writes have no dedup, supersede, or decay semantics

`createMemory` appends unconditionally:

```31:34:oss/karoz/cmd/karoz/tool_memory.go
	key := projectAgentKey(projectID, agentID)
	a.mu.Lock()
	a.memories[key] = append(a.memories[key], entry)
	a.mu.Unlock()
```

The write path is entirely model discretion across five tools that all funnel
here. Two consequences follow, and neither is a storage-size problem.

Duplicates reduce recall. The prompt injects `relevantMemoriesFor(..., 6)`, so
one fact recorded four times consumes four of six slots and evicts genuinely
relevant entries.

Superseded knowledge outranks its correction. `record_decision("button is
green")` followed by `record_decision("button is now blue")` leaves both
`active` and both matching "button colour". Ranking compares score first and
only falls back to `UpdatedAt` when scores tie, so a longer stale entry with more
literal overlap wins outright and the model receives two contradictory
constraints with no way to order them.

Direction, cheapest first: near-duplicate detection on write (update instead of
append); `supersedes` / `superseded_by` links with `record_decision` accepting a
`supersedes_id` and archiving the predecessor, with retrieval excluding anything
superseded; and an explicit recency decay in the score rather than a tie-break.
The extraction/consolidation pipeline in `docs/memory-write-path.md` is the right
direction but is step four, not step one — automatic extraction without the first
three items only fills the store with duplicates faster.

### S2-7 Checkpoint summarization is truncation, and composes with S1-3

```329:336:oss/karoz/cmd/karoz/agent_messages.go
func compactAgentSummaryLine(msg AgentMessage) string {
	content := promptAgentMessageBody(msg)
	...
	return fmt.Sprintf("seq %d %s: %s", msg.Seq, strings.TrimSpace(msg.Role), limitString(content, 280))
}
```

The rolling summary is the first 280 characters of each of at most 24 messages,
with no model involvement. `normalizeResidentSummary` then keeps lines from the
*end* backwards when the result exceeds its character budget, so the oldest lines
are silently dropped rather than compressed.

The two failures compose into real amnesia:

```
an early decision
  -> evicted from the rolling summary by normalizeResidentSummary
  -> survives only in the archive
  -> archive search scores through memoryMatchScore
  -> unreachable for a Chinese query (S1-3)
```

The decision is then absent from the system, while every layer individually
reports correct behavior. The remediation plan covers the two halves in separate
milestones (M5 and M6) and no milestone owns the intersection.

Direction: fix S1-3 first, because a searchable archive is the safety net. Then
replace mechanical truncation with one cheap model call producing a real summary,
run off the first-token path — the same pattern M5 applied to the memory gate.

### S2-8 The tool surface has no owner, and boundaries are set by prose

The plan's disposition table has no tool-system entry, so this area has no
milestone. Its state:

45 static resident tools (36 in `tool_catalog.go`, 10 in `tool_plans.go`, plus 4
agent-management), about 29KB of spec source. Turn gating covers 12 names:

```240:251:oss/karoz/cmd/karoz/tool_catalog.go
	switch name {
	case "write_workspace_file", "show_preview":
		return turnType == "dev"
	case "create_task", "update_task_status":
		return turnType == "dev"
	case "save_plan_draft", "submit_plan", "advance_plan", "reconcile_plan_history":
		return turnType == "plan"
	case "list_agent_templates", "add_agent", "create_agent_team", "delete_agent":
		return capabilitiesForAgent(toolCtx.Agent).CanManageAgents
	default:
		return true
	}
```

Everything else ships on every turn. Two structural problems sit behind that
number.

**Overlapping verbs disambiguated by description prose.** The collaboration
cluster is eight tools with adjacent semantics — `send_to`, `reply_to`,
`decline_handoff`, `ack_inbox`, `report_activity`, `mark_activity`,
`send_to_group`, `list_groups` — and their descriptions carry the disambiguation:

```161:164:oss/karoz/cmd/karoz/tool_catalog.go
		residentToolSpec("ack_inbox", "Silently consume one inbox delivery after handling it when there is no useful detail to send back. Ack is internal state only: it never creates a peer message and must not be used for substantive results.", map[string]any{
```

A sentence explaining that a tool "never creates a peer message" is prose
patching a bad decomposition. The memory cluster has the same shape: five tools
(`remember_fact`, `record_decision`, `mark_done`, `add_pending`, `drop_pending`)
that all call `createMemory` and differ only by a layer argument.

**Gating has one dimension.** The prompt builder already knows whether an inbox
item is unresolved, whether a plan draft exists, and whether an artifact awaits
review, but none of that reaches tool availability. `reply_to`,
`decline_handoff`, and `ack_inbox` ship with an empty inbox; `submit_plan` and
`advance_plan` ship with no draft. Each irrelevant tool is both a misfire
opportunity and unconditional token cost.

Direction: demote "pick the right tool" to "fill the right enum", which is a
choice models make far more reliably than selecting among similarly described
names — for example `inbox_resolve(message_id, outcome: reply|decline|ack,
body?)` and `memory_write(layer, summary, detail, priority?, supersedes?)` plus
`memory_update(id, state)`. Then add state predicates to `residentToolAllowed`
so availability follows runtime state. Both shrink the per-turn spec payload as a
side effect, and the memory consolidation gives `supersedes` (S2-6) a home.

### S3-6 Three memory layers share one retrieval budget, and `Priority` is dead

`relevantMemoriesFor` admits `fact`, `decision`, and `done` on equal terms and
caps the result at six, but the three differ in kind. A `decision` is a
constraint whose violation is a defect and should be small and near-resident. A
`fact` is background and suits relevance ranking. A `done` entry is a log and
should generally be reachable only through `search_archive`. Today a "finished
the login page last week" entry can evict "the login page must use brand blue".

`Priority` compounds it. Only `add_pending` passes a non-zero value; the other
three layers are hardcoded to `0` in `tool_registry_adapter.go`, and no ranking
path reads the field — its only uses in the tree are display
(`agent_prompt.go:216`, `runtime_hooks.go:332`, `helpers.go:471`). It presents
controllability that does not exist.

Direction: per-layer budgets instead of one shared `limit=6`, and either wire
`Priority` into scoring or delete it.

### S3-7 Memory has no project tier

Entries are keyed by `projectAgentKey(projectID, agentID)`, so every agent holds
a private store and the only cross-agent channel is inbox prose. A fact
established by one agent is invisible to its reviewer. A project-scoped `fact`
layer with agent attribution is a small change relative to teams re-narrating
facts to each other.

### S3-8 The stable-prefix boundary is not byte-stable across turn types

`agent_prompt.go` records `stablePrefixChars` and logs `stable_prefix_tokens`,
which is the right instinct. The boundary is taken after the role blocks, but
`## Current chat turn type: ` is written near the top of the builder — inside the
prefix. The prefix is therefore stable per `(agent, turnType)` pair and changes
wholesale when the user moves between ask, plan, and dev.

Direction: move every per-turn-varying field after the boundary, then assert in a
test that two builds for the same `(agent, turnType)` produce byte-identical
prefixes. Without that assertion, anyone adding a timestamp to the preamble
silently destroys cache hit rate and the only visible symptom is the bill.

### S3-9 The tool contract is billed twice

`renderProviderNeutralToolContract` writes tool names and usage rules into the
prompt text while `residentToolSpecsForContext` supplies full JSON schemas in the
`tools` array. The same contract is described twice per turn and the two
descriptions can drift. Keep the `tools` array as the single schema source and
retain only orchestration rules — the things a schema cannot express — in prose.

### S3-10 Tool output budget is per-turn only

`resident_budget.go` sets `MaxToolOutputChars` per turn (ask 12000, plan 18000,
dev 24000) with no per-call cap. One `repo_read` of a large file or one verbose
`bash` can consume the whole turn's allowance and starve later calls, and the
model is never told this happened.

Direction: a per-call cap under the per-turn cap, and report the remaining
allowance in truncated tool results (`{"truncated": true,
"remaining_output_chars": N}`) so the model can narrow its next query. This
completes the M4 mechanism rather than adding a new one.

### S3-11 Tool results have no uniform envelope

Handlers return a mix of `{"error": "...", "message": "..."}`, bare domain
objects, and — for `createMemory` — a Go `error` whose model-visible form
depends on the caller. There is no single convention for failure, so the model
cannot learn one recovery behavior.

Direction: one envelope, `{ok, data?, error?{code, message, retryable}}`, with
`retryable` stating whether to retry or change approach, enforced through a
helper and a test that walks every registered handler asserting the shape.

## Proposed order of work (supplement)

Ordered by payoff over cost, to be interleaved with the remediation plan's
remaining milestones:

1. **S1-3.** CJK bigram segmentation. Roughly half a day, repairs a functional
   failure that is live today, and restores the archive safety net that S2-7
   depends on.
2. **S2-4 + S2-5.** Native tool-trajectory wire format and in-turn reasoning
   replay. This is where M6's value actually lands; the data model is already in
   place.
3. **S2-6 + S3-6.** Write-path dedup, supersede links, layered retrieval
   budgets. Prerequisites for trusting memory, and for automatic extraction.
4. **S2-8.** Tool cluster consolidation and state-conditional gating. Needs a new
   milestone, since the plan has no tool-system entry.
5. **S3-8 + S3-10.** Prefix byte-stability test and per-call output budget. Small
   changes that prevent regressions.
6. **S2-7 (summary half) and the `memory-write-path.md` extraction pipeline.**
   Last, because both depend on the items above being true.
