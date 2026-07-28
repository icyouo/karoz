# Agent Runtime Immediate Remediation

Status: implementation-ready

Baseline: `7ee3f3d`

Scope owner: runtime

Review gate: correctness, provider-contract compatibility, privacy, and prompt
regression tests

Related proposals: `docs/agent-runtime-candidate-designs.md`

## 1. Purpose

This document contains only runtime defects or incomplete contracts that are:

- reproducible against the current tree;
- supported by a concrete code path or provider contract;
- bounded enough to implement without choosing a new product architecture;
- independently testable and reversible.

It is both the development specification and the implementation handoff. Items
that still require product policy, ownership, scale evidence, or a broad
model-visible API migration are intentionally excluded and live in the
candidate-design document.

## 2. Immediate scope

| ID | Problem | Priority | Completion evidence |
|---|---|---:|---|
| IR-1 | CJK memory queries degrade to full-sentence substring matching | P0 | Chinese non-verbatim queries retrieve the expected entry without regressing English |
| IR-2 | Codex encrypted reasoning items are requested but not replayed during a stateless tool loop | P1 | A reasoning item from round one appears, unchanged and ordered, in round two only |
| IR-3 | Persisted tool trajectories are flattened into prose across user turns | P1 | Both providers receive bounded native prior-turn tool call/result pairs |
| IR-4 | Turn-varying content sits inside the measured stable prefix and the tool contract is duplicated in prose | P2 | Prefix bytes are stable across turn types and tool schemas occur only in the provider tool field |

The following are not part of this implementation:

- an inverted memory index;
- fuzzy automatic memory merging;
- decision supersession or recency policy;
- project-shared memory;
- model-generated rolling summaries;
- tool renames or cluster consolidation;
- a universal tool-result envelope;
- additional tool-output accounting.

`MaxToolOutputChars` is already applied to each tool result in
`executeResidentToolCall`; it is not currently a per-turn cumulative allowance.
Any two-level per-call/per-turn policy therefore requires a separate design and
measurement rather than a bug fix based on the old field name.

## 3. IR-1 — CJK-aware lexical retrieval

### 3.1 Reproduction

`searchArchive` and `relevantMemoriesFor` derive terms with `strings.Fields`.
For a Chinese sentence this normally produces one term containing the entire
query. `memoryMatchScore` then succeeds only when the stored text contains that
whole sentence verbatim.

Required fixture:

| Stored memory | Query | Expected |
|---|---|---|
| `决定：登录页的主按钮统一使用品牌蓝 #1A73E8，不再使用绿色。` | `登录页的按钮用什么颜色` | match |
| same | `记得我们之前定的登录页按钮颜色吗` | match |
| same | `登录页的主按钮` | match, ranked above partial overlap |
| English equivalent | `what color is the login page button` | existing behavior preserved |
| unrelated Chinese memory | either Chinese query above | no match |

### 3.2 Accepted design

Use one shared query-term function for both retrieval paths:

1. Lowercase and trim the query.
2. Preserve the complete query for exact-phrase scoring.
3. Split non-CJK Unicode letter/number runs on whitespace and punctuation.
   Cyrillic, accented Latin, Arabic, and other scripts must remain searchable;
   this path is not ASCII-only.
4. Split contiguous Han, Hiragana, Katakana, and Hangul runs into rune
   bigrams.
5. Deduplicate terms without changing their first-seen order.
6. Score exact phrase matches above whole non-CJK word matches.
7. Score CJK bigram matches by coverage, with a minimum matched-bigram count,
   so one common fragment does not create a result.

The scorer remains an in-memory scan. No index or persisted derived state is
introduced.

### 3.3 Core implementation

The production implementation should keep segmentation and scoring separate:

```go
type memoryQueryTerms struct {
	Exact      string
	WordTerms  []string
	CJKBigram  []string
}

func parseMemoryQueryTerms(query string) memoryQueryTerms

func memoryMatchScore(query memoryQueryTerms, text string) int
```

Scoring invariants:

```text
exact phrase > complete non-CJK word coverage > strong CJK bigram coverage
strong CJK coverage > one incidental bigram
zero meaningful overlap = zero
```

`searchArchive` and `relevantMemoriesFor` must call the same parser and scorer;
copying term logic into each caller is not accepted.

### 3.4 Failure and compatibility behavior

- Empty queries still return the existing validation/empty result.
- Existing non-CJK Unicode full-phrase and word matches keep their relative
  order.
- Malformed UTF-8 must not panic; Go rune iteration may replace malformed
  bytes, but must produce deterministic output.
- CJK scoring changes no persisted data and is safe to roll back.

### 3.5 Required tests

- Convert the reproduction table into end-to-end retrieval tests for both
  `relevantMemoriesFor` and `searchArchive`.
- Test mixed Chinese/ASCII input such as `登录页 CTA #1A73E8`.
- Test Cyrillic, accented Latin, and Arabic queries to prevent an ASCII-only
  regression.
- Test one incidental bigram does not match an unrelated entry.
- Test exact phrase outranks partial CJK overlap.
- Preserve existing English ranking tests.

## 4. IR-2 — Stateless Codex reasoning replay

### 4.1 Reproduction

The Codex Responses request sets:

```json
{
  "store": false,
  "include": ["reasoning.encrypted_content"]
}
```

The stream parser retains messages and function calls but drops completed
reasoning items. The next tool-loop request is therefore missing provider state
that was explicitly returned for manual context management.

### 4.2 Accepted design

- Capture every completed Codex response output item into one ordered stream.
  At minimum this includes `reasoning` and `function_call`; unknown replayable
  item types must not be silently reordered around them.
- The ordered output-item stream is authoritative. Tool dispatch is a derived
  view of its `function_call` items; the parser must not build one reasoning
  slice and one separately reconstructed tool-call slice.
- Preserve replayable provider items opaquely. Karoz may read the minimum typed
  fields needed to dispatch a function call, but must not inspect or transform
  `encrypted_content`.
- Append the ordered provider output-item group to the next Codex input exactly
  once. Execute function calls in encounter order and append their
  `function_call_output` items, in matching call order, after that provider
  output group. For multiple calls, every output must reference the matching
  non-empty `call_id`.
- Keep them in the in-memory `codexStreamWire` for the current Run only.
- Never add reasoning payloads to `AgentTranscriptItem`, chat messages,
  runtime events, logs, error text, or persisted JSON.
- Drop them when the current user turn terminates.

Claude handling is unchanged by this item. Claude assistant content already
retains provider-native blocks needed by its same-turn tool loop.

### 4.3 Core implementation

Represent the Codex response as one ordered item stream plus a typed dispatch
view:

```go
type codexResponseOutputItem struct {
	Raw      json.RawMessage
	ToolCall *codexToolCall // non-nil only for function_call
}

type codexStreamResult struct {
	Text        string
	OutputItems []codexResponseOutputItem
}
```

The parser records `response.output_item.done` items in event order. A helper
derives dispatchable calls by walking `OutputItems`; it does not become a
second source of ordering truth. The wire replays each `Raw` item in order,
then appends the matching function outputs produced by the shared tool loop.
Because the accepted structure uses `json.RawMessage`, each captured output
item is replayed with the same serialized bytes; the implementation must not
decode and reconstruct it merely to build the next request.

Do not retain the entire SSE event envelope. `Raw` contains only the provider
output item accepted as a later input item. `json.RawMessage` is mandatory from
SSE capture through construction of the next Codex request. Minimal
classification may decode selected outer fields, but the stored and replayed
item remains the original `json.RawMessage`.

### 4.4 Privacy and error behavior

- Reasoning payloads are sensitive model state even though encrypted.
- Raw output-item capture must not parse or interpret the reasoning payload.
  Classification may inspect only the outer item type and the minimum typed
  function-call fields needed for dispatch.
- Every completed item must be classified as either a typed dispatchable
  function call or an opaque replayable item whose `Raw` bytes can be forwarded
  unchanged. If neither classification is possible, fail the model round
  before dispatching any tool call. Never continue with an incomplete ordered
  stream.
- The failure returned to logs/UI is payload-free: it may contain the event
  type, item index, and a stable error code, but never raw item content or
  `encrypted_content`.
- No diagnostic may include the encrypted payload.
- If the provider rejects replay, return the normal provider error; do not
  silently retry a potentially effectful tool round without reasoning state.

### 4.5 Required tests

- Parse a completed reasoning item containing encrypted content.
- Round one `encrypted_content` has exactly the same string value in round two,
  with all replayed item fields preserved. The mandatory `json.RawMessage`
  path preserves the literal serialized bytes of every replayed output item.
- One output stream preserves observed reasoning/function-call order, and tool
  dispatch is derived from that same stream.
- Ordering for one call is provider output group, then matching function output.
- A two-call round preserves both calls in observed order and appends two
  correctly matched outputs in call order.
- An unknown completed output-item fixture is preserved in place or rejected
  explicitly as non-replayable; it is never silently moved.
- A malformed completed item fails the round before tool dispatch, produces a
  payload-free error, and leaks none of the raw/reasoning content.
- Reasoning is absent from a new wire/new user turn.
- Reasoning is absent from transcript persistence and captured logs.
- Message and tool parsing remain correct when no reasoning item is returned.

## 5. IR-3 — Provider-native cross-turn transcript

### 5.1 Reproduction

`AgentTranscriptItem` persists `ToolCallID`, `ToolName`, `ToolArguments`,
`ToolResult`, and `ToolSuccess`, but `promptAgentTranscriptBody` turns them back
into lines such as:

```text
tool_call id=... name=... arguments=...
tool_result call_id=... success=true result=...
```

The provider receives those historical tool interactions as prose inside the
prompt. Only calls made during the current turn use native provider items.

### 5.2 Accepted data flow

```text
bounded AgentTranscriptItem window
        |
        +-- one ordered, bounded provider-neutral history projection
                |
                +-- text/status/interrupt/orphan --> text fallback in place
                |
                +-- valid scoped call/result --> native exchange in place
                        |
                        +-- Codex function_call/function_call_output
                        |
                        +-- Claude assistant tool_use/user tool_result
```

The transcript remains provider-neutral in persistence. Provider-specific
mapping occurs at the wire boundary. `Prompt` contains stable instructions,
dynamic project state, and the current runtime instruction; it must not
independently render any transcript record already present in the ordered
history projection.

The current direct or scheduled input has exactly one owner: it remains in
`Prompt` as the current runtime instruction and is excluded from `History`.
The caller must propagate the persisted current-input item ID and sequence,
plus the current Run ID, into the projection; text equality is not an identity
boundary. The ID/sequence must identify the same persisted record. Every item
belonging to the current Run is also excluded from prior history. If the caller
cannot establish the persisted current-input identity, context construction
fails instead of guessing from body text.

### 5.3 Pairing and bounds

- A native pair requires the same non-empty `SessionID`, the same non-empty
  `RunID`, a matching non-empty call ID, and exact sequence adjacency
  (`result.Seq == call.Seq + 1`).
- The call also requires a non-empty `ToolName` and provider-portable arguments:
  a non-empty, valid JSON object whose complete serialized form fits the
  existing 1,400-character model-visible tool-argument cap. Eligibility is
  checked before any truncation. Empty, scalar, array, malformed, or oversized
  arguments degrade the call and result together into one bounded text-fallback
  projection unit in the same chronological position. The fallback renderer
  may truncate its prose argument field to the cap; native JSON is never
  truncated or partially forwarded. Codex and Claude therefore share the same
  native-selection and budget boundary.
- Duplicate call IDs within one `(SessionID, RunID)` scope invalidate every
  occurrence of that ID in the scope. They all degrade in place to text rather
  than being paired ambiguously.
- Duplicate-ID validation scans the complete eligible session/Run delta before
  any item/character compaction. A duplicate just beyond the retained cutoff
  still invalidates an otherwise in-window occurrence.
- An orphaned call, orphaned result, cross-Run/cross-session ID collision, or
  legacy item without the required scope degrades to compact text in its
  original chronological position. It must never create a provider-invalid
  native item.
- Compaction operates on projection units: a text fallback is one unit and a
  valid call/result exchange is one atomic unit. It never retains half a pair.
- Historical native items count against the existing bounded transcript item
  and character/token budgets.
- Tool results retain their existing model-visible truncation limit. Tool
  arguments use the native-eligibility/fallback rule above so the limit cannot
  produce invalid provider JSON.
- Replayed historical tools are context only. They are never redispatched.

### 5.4 Core implementation

Introduce a provider-neutral model context and one ordered history projection
rather than embedding transcript records into the prompt:

```go
type residentHistoryUnit struct {
	Items      []AgentTranscriptItem // one text item or one valid call/result pair
	Text       string                // non-empty only for text fallback
	NativePair bool
}

type residentModelContext struct {
	Prompt          string
	History         []residentHistoryUnit
	CurrentRunID    string
	CurrentInputID  string
	CurrentInputSeq int64
}
```

Prompt construction returns:

- `Prompt`: stable instructions, dynamic project state, and the current runtime
  instruction, with no history records;
- `History`: one bounded chronological projection containing ordinary text,
  status, interrupts, legacy/orphan fallbacks, and valid native pairs in place.

Projection order is:

1. identify and exclude the exact `(CurrentInputID, CurrentInputSeq)` record
   plus every item whose `RunID` equals `CurrentRunID`;
2. validate duplicate IDs over the complete remaining eligible delta;
3. classify scoped portable pairs versus in-place text fallbacks;
4. compact the resulting units atomically.

Adapter responsibilities:

```go
func codexHistoryInput(units []residentHistoryUnit) []map[string]any
func claudeHistoryMessages(units []residentHistoryUnit) []map[string]any
```

The adapters must share the pairing, duplicate-ID validation, and atomic
compaction projection so provider behavior cannot diverge at the selection
boundary. For multiple valid pairs, Codex emits each call followed by its
matching output; Claude emits an assistant `tool_use` message followed by its
user `tool_result` message for each pair. Adjacent provider messages may be
coalesced only when doing so preserves chronology and call/result validity.

### 5.5 Required tests

- A previous-turn call/result pair is native on the next Codex request.
- The same pair becomes Claude `tool_use`/`tool_result` blocks.
- The pair is absent from prose when mapped natively.
- Legacy and orphaned records degrade to text and do not invalidate requests.
- Pair compaction is atomic at both item and character boundaries.
- Text before, between, and after two native pairs stays in the same order for
  both providers.
- A matching call ID from another Run or session never forms a pair.
- Duplicate IDs in one scope invalidate every ambiguous occurrence.
- A duplicate outside the retained cutoff still invalidates the in-window ID.
- A valid JSON-object argument just over the argument cap degrades the complete
  atomic pair to bounded prose for both Codex and Claude; neither adapter emits
  a native call or exceeds the projection budget.
- One orphan call, one orphan result, and one two-call trajectory each produce
  provider-valid history.
- Direct and scheduled current inputs each appear exactly once: in `Prompt`,
  never again in `History`.
- Interrupt ordering remains correct.
- Existing visible chat and context-meter projections remain compatible.

## 6. IR-4 — Stable prefix and single tool contract

### 6.1 Reproduction

The current stable-prefix marker is recorded after content that varies with:

- chat turn type;
- turn-specific instructions;
- allowed tool names;
- user-text-driven skill injection.

The provider also receives tool names/descriptions in prompt prose and full
schemas in the request `tools` field.

### 6.2 Accepted layout

```text
stable prefix
  session/runtime invariants
  agent identity and durable role
  schema-inexpressible orchestration rules
--- stablePrefixChars ---
dynamic turn section
  current turn type and turn contract
  selected skill instructions
  allowed provider tool schemas (request field, not prose)
  project/team/inbox/memory/current instruction

ordered history projection
  carried beside Prompt in the provider request; never rendered by Prompt
```

Tool schemas and the allowed-tool list have one authority:
`residentToolSpecsForContext`. Prompt prose may explain orchestration rules but
must not enumerate or restate tool schemas.

### 6.3 Core implementation

Split prompt assembly into explicit builders:

```go
func buildResidentStablePrefix(project Project, agent Agent) string
func (a *app) buildResidentDynamicContext(
	project Project,
	agent Agent,
	userText, turnType, memoryQuery string,
) string
```

`stablePrefixChars` is exactly the byte length of the first function's output.
The stable builder must not accept `userText`, `turnType`, time, inbox state, or
the currently allowed tool list.

### 6.4 Required tests

- Ask, plan, and dev builds for the same unchanged agent have identical bytes
  before `stablePrefixChars`.
- Mentioning a skill changes only bytes after the boundary.
- Adding a timestamp to dynamic context does not change the prefix fixture.
- Every advertised tool appears in the request tool field and is absent from
  the prose tool-contract section.
- Schema-inexpressible orchestration guidance remains present.

## 7. Delivery slices

### Slice A — Retrieval correctness

Files:

- `cmd/karoz/tool_memory.go`
- focused memory tests

Gate:

- IR-1 behavioral tests;
- `go test ./cmd/karoz -run 'Memory|Archive'`.

### Slice B — Provider continuation correctness

Files:

- `cmd/karoz/provider_codex_sse.go`
- `cmd/karoz/provider_codex_stream.go`
- shared wire tests

Gate:

- IR-2 parser, ordering, lifetime, and privacy tests.

### Slice C — Structured wire history

Files:

- `cmd/karoz/agent_transcript*.go`
- `cmd/karoz/agent_prompt.go` (removal of transcript prose only)
- provider adapters
- dispatch/context plumbing

Gate:

- IR-3 provider-contract, exactly-once input, and legacy-compatibility tests;
- removal of all transcript-history rendering from `Prompt`.

### Slice D — Prompt boundary cleanup

Files:

- `cmd/karoz/agent_prompt.go`
- prompt/provider-contract tests

Gate:

- IR-4 byte-stability and once-only contract tests.

Slices A and B may proceed independently. Slice C owns the ordered history
projection and removal of transcript history from `Prompt`. Slice D owns only
the stable-prefix boundary and duplicate tool-contract cleanup; it does not
remove or re-render transcript history. Each slice is reviewable and revertible
on its own.

## 8. Full validation

Every slice:

```bash
gofmt -w <changed-go-files>
go test ./...
go vet ./...
```

Before release:

```bash
go test -race ./...
```

Run the repository JavaScript suites when prompt/context projections or browser
context-meter fixtures change.

No source-fragment assertion is sufficient on its own. At least one test per
item must exercise the behavior through the same projection or adapter used by
production.

## 9. Observability

Allowed:

- query script class and term counts;
- retrieval candidate/match counts and elapsed time;
- native versus text-fallback transcript item counts;
- stable/dynamic/transcript estimated token counts;
- reasoning continuation item count;
- provider and turn identifiers.

Forbidden:

- query or memory content;
- tool arguments/results beyond existing protected debug paths;
- reasoning payloads;
- full prompts;
- credentials.

## 10. Rollback

- IR-1 is scoring-only and requires no data migration.
- IR-2 is in-memory only; rollback drops continuation replay.
- IR-3 retains the persisted provider-neutral transcript, so a short-lived
  compatibility switch may restore the existing prose renderer without
  rewriting stored JSON until both provider contract suites pass in release
  conditions.
- IR-4 is prompt layout only; rollback restores the previous prefix boundary and
  prose tool-contract layout.

No immediate item changes user authentication, network exposure, memory
ownership, or the public HTTP API.
