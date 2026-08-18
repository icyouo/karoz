# Agent Runtime Candidate Designs

Status: proposal inventory — not approved, not scheduled

Baseline: `7ee3f3d`

Implementation work must not start from this document alone. A candidate moves
to the immediate-remediation document only after its decision questions,
measurement gate, compatibility contract, and owner are resolved.

## 1. Purpose

This document preserves promising runtime ideas that are not yet justified as
defects with a single safe implementation. It separates evidence from a
preferred solution so future work does not accidentally treat one reviewer's
proposal as an approved product contract.

## 2. Promotion criteria

A candidate may be promoted only when:

1. a production-visible failure or measured cost is demonstrated;
2. the product behavior and ownership rules are explicit;
3. at least one alternative has been evaluated;
4. persistence and backward compatibility are defined;
5. rollback does not discard user or agent data;
6. focused acceptance tests are written before implementation.

## 3. CD-1 — Memory write correctness

### Evidence

`createMemory` appends unconditionally. Repeated facts can occupy several
retrieval slots, and two active decisions can contradict one another.

### Candidate options

- Normalize and collapse exact duplicates.
- Offer near-duplicate suggestions without silently merging.
- Add explicit `supersedes`/`superseded_by` links for decisions.
- Exclude explicitly superseded entries from active retrieval.
- Introduce layer-specific ranking rather than global recency decay.

### Open decisions

- Which layer may update versus append?
- Who may supersede a decision?
- Does supersession archive history or merely remove it from active retrieval?
- How are concurrent writers resolved?
- Is recency meaningful for durable decisions, or only for facts/pending work?

### Promotion experiment

Measure duplicate-slot consumption and contradictory active decisions in real
projects. Start with exact duplicate detection; do not auto-merge fuzzy matches
without a false-merge evaluation set.

## 4. CD-2 — Layered memory retrieval

### Evidence

Active `fact`, `decision`, and `done` entries share one relevance-ranked limit,
although their semantics differ. `Priority` is displayed but does not
participate in retrieval ranking.

### Candidate options

- Give each layer an explicit bounded budget.
- Pin a small, bounded set of active decisions.
- Retrieve facts by relevance.
- Keep done/history entries out of proactive injection and available through
  archive tools.
- Either define `Priority` semantics per layer or remove the field.

### Open decisions

- Maximum pinned decision count and overflow behavior.
- Whether priority is user-set, model-set, or runtime-derived.
- How a decision is demoted when it is no longer active.
- Whether pending work belongs in memory or only in WorkPlan/Task state.

### Non-option

Unbounded injection of all decisions is not acceptable.

## 5. CD-3 — Project-shared memory

### Evidence

Memory is keyed by `(project, agent)`. Agents currently exchange facts through
handoffs and inbox messages.

### Candidate options

- A project fact tier with author attribution.
- Explicit publish-to-project from an agent-private fact.
- Read-only project facts curated by a coordinator/reviewer.
- Continue using handoffs if measured duplication is low.

### Open decisions

- Write, edit, supersede, and delete authority.
- Conflict resolution between agents.
- Whether private or sensitive agent context may ever be promoted.
- Retrieval precedence between project and agent facts.
- Audit trail and UI visibility.

### Promotion experiment

Measure how often agents resend established facts and how often missing shared
facts cause rework. “Multi-agent exists” alone is not evidence that shared
memory is required.

## 6. CD-4 — Model-generated checkpoint summaries

### Evidence

The current rolling summary is deterministic truncation, not semantic
compression. Old content can leave the short-term window.

### Candidate options

- Preserve pinned decisions/pending state separately and retain deterministic
  message compaction.
- Generate summaries asynchronously with a cheaper model.
- Generate summaries only after a measured archive-retrieval miss.
- Use structured extraction rather than free-form summary prose.

### Open decisions

- Cost and model selection.
- Summary versioning and replacement rules.
- Race behavior while a new user turn starts.
- Failure/timeout fallback.
- Evaluation for omission and fabricated constraints.
- Privacy and retention of summary inputs.

### Dependency

CJK archive retrieval must be correct before summary work is evaluated. A
searchable archive is the fallback for anything omitted from a summary.

## 7. CD-5 — Memory inverted index

### Evidence

Lexical retrieval scans the agent's in-memory entries. No current latency or
entry-count threshold demonstrates a scaling problem.

### Candidate options

- Keep the scan.
- Build an in-memory derived term index at load.
- Persist a rebuildable index only if load cost becomes material.

### Promotion gate

Define entry-count and p95 retrieval-latency thresholds first. The index must be
derived, discardable state and must use exactly the same tokenizer as queries.

## 8. CD-6 — Tool state gating

### Evidence

Turn-type and capability gating exist, but tools may still be advertised when
their required runtime state is absent, such as inbox resolution with no
pending inbox item.

### Candidate options

- Add state predicates to advertisement while retaining handler validation.
- Gate only high-confusion/high-token tools.
- Leave stable always-on tools untouched to improve provider cache stability.

### Open decisions

- Which state changes require rebuilding the tool list?
- Is token reduction worth changing the request tool schema between rounds?
- How does dynamic gating interact with provider prompt/tool caching?

### Invariant

Advertisement is a hint, never an authorization boundary. Handlers must reject
invalid, stale, replayed, or hallucinated calls.

## 9. CD-7 — Tool cluster consolidation

### Evidence

Some collaboration and memory tools have adjacent semantics and lengthy
descriptions.

### Candidate options

- Keep distinct verbs and shorten descriptions.
- Consolidate inbox terminal actions under an outcome enum.
- Consolidate memory writes under a layer enum.
- Improve state gating before changing names.

### Open decisions

- Whether enum selection is measurably more reliable than distinct tool names.
- Permission and audit differences between reply, decline, and acknowledgement.
- Conditional schema complexity, especially required body fields.
- Compatibility with in-flight responses and persisted transcript context.
- Alias lifetime and telemetry for removal.

### Promotion experiment

Build an evaluation set of real miscalls and compare the current contract,
shortened descriptions, state gating, and consolidated schemas. Do not rename
tools based only on source size.

## 10. CD-8 — Uniform tool-result envelope

### Evidence

Handlers expose different success and error shapes to the shared adapter.

### Candidate options

- Normalize only at the provider/registry boundary.
- Introduce typed internal results and map them centrally.
- Migrate every handler to a shared result type.

### Open decisions

- Compatibility for model-visible historical behavior.
- Binary/streaming or very large results.
- Domain-specific partial success.
- Stable error-code taxonomy and `retryable` ownership.

### Preferred first experiment

Normalize validation and transient errors at the adapter boundary for a small
tool subset. A whole-registry migration is not the first step.

## 11. CD-9 — Two-level tool-output accounting

### Correct current state

`MaxToolOutputChars` is currently applied by
`executeResidentToolCall` to each individual result. Despite the field name,
the implementation does not maintain one cumulative character pool for the
entire turn.

### Candidate design

- Rename the existing field to make the per-call contract explicit.
- Add an optional cumulative per-turn model-visible output allowance.
- Include structured truncation metadata and remaining allowance.
- Decide whether callbacks/UI receive full or model-truncated results.

### Open decisions

- Evidence that cumulative output is currently harming turns.
- Per-tool versus global caps.
- Character, byte, or token accounting.
- Handling for structured JSON that becomes invalid when naively truncated.

### Promotion gate

Collect per-call and per-turn output distributions before selecting limits.

## 12. CD-10 — Retrieval recency and priority

### Evidence

`UpdatedAt` currently breaks score ties and `Priority` does not affect
retrieval.

### Candidate options

- Use explicit supersession for decisions with no time decay.
- Apply bounded recency weighting to volatile fact layers.
- Make priority a layer-specific ranking input.
- Delete priority if no actor can assign it consistently.

### Non-option

A global recency decay across durable decisions and ordinary facts is not
acceptable; old decisions do not become less binding merely because time
passed.

## 13. Suggested research order

Research order is not implementation order:

1. Exact memory duplicates and contradictory decisions.
2. Layered retrieval and explicit decision lifecycle.
3. Tool state-gating measurements.
4. Adapter-level error normalization.
5. Per-turn output distribution.
6. Shared-memory collaboration evidence.
7. Model-summary evaluation.
8. Tool consolidation evaluation.
9. Retrieval indexing only after a demonstrated latency threshold.

Every promoted item receives its own implementation slice and review gate. No
candidate is a dependency of the current immediate-remediation release unless
new evidence changes its classification.
