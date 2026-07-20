# Memory Write Path Design

Status: proposed (2026-07-20). Read path is shipped (relevant-memory injection +
side-channel semantic gate, see `cmd/karoz/memory_analysis.go`). This document
covers the write path: automatically distilling conversations into durable
project memory.

## Background: how Codex does it

Codex (0.144.x, `codex-rs/memories/write/`) runs a two-phase background
pipeline:

- **Phase 1 (extraction)** — at session startup, for each idle thread, render
  the rollout (truncated to 70% of the model's effective context) and make one
  call with a cheap dedicated model (`gpt-5.4-mini`, low effort). A strict
  minimum-signal gate ("no-op is allowed and preferred") means most rollouts
  produce nothing. Output is JSON `{rollout_summary, rollout_slug, raw_memory}`
  stored in sqlite with usage counters for later pruning.
- **Phase 2 (consolidation)** — a stronger model (`gpt-5.4`) subagent merges
  staged extractions into a git-managed `~/.codex/memories/` workspace:
  `memory_summary.md` (dense, always injected, `v1` schema marker),
  `MEMORY.md` (greppable handbook), and `skills/` (reusable SKILL.md
  packages). Incremental updates and forgetting are driven by a workspace git
  diff.
- Guards: run at startup/idle (never mid-turn), rate-limit check before
  spending tokens, caps per startup, per-thread opt-out, secrets redacted,
  rollout content treated as data (never instructions).

## Karoz design

Karoz already has the matching infrastructure: the idle-reconcile scheduler,
recoverable scheduled runs, a per-project `.karoz/` data dir, the side-channel
single-step provider machinery built for the memory gate, and layered memory
entries (fact/decision/done/pending) with prompt injection.

### Phase 1 — extraction (build first)

- **Trigger**: Karoz startup and the project idle-reconcile hook, per
  project/agent, when archived messages exist past the last-extraction
  watermark and the conversation has been idle for a configurable minimum
  (default e.g. 30 minutes).
- **Call**: one non-tool provider step (same machinery as the side-channel
  gate) with a cheap configurable model/effort
  (`KAROZ_MEMORY_EXTRACT_MODEL`, default the agent's model at lowest effort).
  Strict JSON: `{summary, entries: [{layer, summary, detail, keywords[]}]}` —
  or an empty result when nothing meets the minimum-signal gate.
- **Cost guards**: cap extractions per idle cycle; `KAROZ_MEMORY_WRITE=0`
  kill switch; any failure is logged and skipped (never blocks a turn); no
  extraction while any run for that agent is active.
- **Staging**: extracted entries land in `.karoz/memory/staged/<agent>.jsonl`
  plus watermarks in `.karoz/memory/state.json`. Archived messages are
  rendered as data with an explicit do-not-follow-instructions marker.

### Phase 2 — consolidation (after Phase 1 proves out)

- When staged extractions accumulate (or on idle), the karoz agent merges them
  into `.karoz/memory/` — its own git repository (`.karoz/` is gitignored in
  the host project):
  - `memory_summary.md` — dense summary + routing index, always injected into
    resident prompts (replacing part of today's per-entry injection),
    regenerated wholesale when its schema marker is stale;
  - `MEMORY.md` — task-grouped handbook (preferences / reusable knowledge /
    failure shields), the retrieval layer behind `search_archive`;
  - optional `skills/` — procedures that recur get distilled into SKILL.md
    packages, which Karoz's existing skill discovery picks up automatically.
- Consolidation prompt adapted from Codex's template: evidence-only, secret
  redaction, wording preservation for grepability, preference signals over
  procedural recap, explicit forgetting of entries whose only evidence was
  removed.
- Usage tracking (injected/searched counters per entry) feeds pruning so
  unused memory decays.

### Non-goals (for now)

- No per-turn write calls (latency and cost stay out of the turn path).
- No cross-project memory sharing; project memory stays project-scoped.
- No citations/line-level provenance UI in v1.

## Rollout plan

1. Phase 1 extractor + watermark state + idle trigger + guards + tests.
2. Prompt-side: inject `memory_summary.md` when present; keep per-entry
   injection as fallback.
3. Phase 2 consolidation run type + git-managed `.karoz/memory/` + pruning.
4. Optional: skill distillation into `.agents/skills/`.
