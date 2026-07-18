# Karoz

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8.svg)](go.mod)
[![Status](https://img.shields.io/badge/status-MVP-orange.svg)](#what-works-today)

[简体中文](README.zh-CN.md) | English

**Give every project a resident AI engineer — one that remembers your codebase, works from the browser, and ships reviewable commits inside isolated Git worktrees.**

AI coding tools answer the question in front of them, then forget everything. Every new session starts from zero: you re-explain the project, tasks scatter across terminals, and you still inspect diffs, run checks, and craft commits by hand.

Karoz closes that loop. Each project gets a persistent agent with its own memory and a traceable task workflow. Describe a goal in the browser, let the agent work with real project context, and watch development tasks run in isolated worktrees driven by battle-tested coding agents — with live logs, diffs, and commits kept on a dedicated branch so nothing reaches your remote until you review and push. Context stays with the project, code stays on your machine, and every step stays visible and interruptible.

```
  ~/karoz-projects/
  ├── api-service   →  resident agent + memory + tasks + run history
  ├── web-app       →  resident agent + memory + tasks + run history
  └── data-pipeline →  resident agent + memory + tasks + run history

  task "add pagination"
     → isolated git worktree (branch karoz/task-…)
     → native codex / claude agent implements it   ← the agent's own loop
     → change detection → optional verify command
     → commit on task branch → no-ff merge into local base
     → you review the result and push when ready
```

## Why Karoz

- **Built around projects, not chat windows.** Each project keeps its own agent, messages, memory, tasks, and run history — so context compounds instead of resetting every session.
- **Advice becomes delivery.** Karoz doesn't stop at code generation. It hands coding tasks to a native `codex`/`claude` agent inside an isolated worktree, detects the changes, optionally verifies them, and produces a reviewable Git commit.
- **A team, not just one agent.** A project can host multiple resident agents — architect, builder, reviewer — that hand off work to each other through a tracked protocol, coordinated by Karoz.
- **Your main checkout stays clean.** Development tasks run in separate worktrees, so parallel work never collides or leaves surprise edits in your working tree.
- **Local-first, with clear boundaries.** You own the service, the project data, and the execution environment — and you can reuse the Codex or Claude auth you already have.
- **Transparent and interruptible.** Task state, live logs, and diffs stay visible. Inspect any run, or take over at any point.
- **No vendor lock-in.** Use Codex OAuth, a local CLI, or an OpenAI-compatible proxy — swap providers without rewriting your workflow.
- **Extensible by design.** The resident agent picks up local Skills (reusable instruction packs) and explicitly trusted MCP servers — stdio or SSE — so you can plug in your own playbooks and tools without changing Karoz.

## Who It Is For

Karoz is for individual developers and small teams juggling multiple repositories who want to hand repeatable engineering work to AI — without giving up their Git workflow, data control, or the ability to review what the agent did.

It shines on long-lived projects, parallel feature work, well-scoped implementation tasks, and any setup where you want a complete local record of what your coding agents actually changed.

## How It Works

1. Karoz scans your projects directory and gives each project its own workspace.
2. A project-scoped `karoz` agent keeps messages, memory, and context across sessions — and can grow into a team of agents that hand off work to each other.
3. You create development or deployment tasks and follow their state and logs live in the browser.
4. Each development task runs in an isolated Git worktree and comes back as a reviewable commit on its own branch — never directly in your main checkout ([details below](#how-tasks-run)).

## Quick Start

You need Go and a parent directory holding the projects you want Karoz to scan.

```bash
go run ./cmd/karoz
```

Then open:

```text
http://127.0.0.1:8088
```

Defaults:

- projects directory: `$HOME/karoz-projects`
- data directory: `.karoz`
- default project agent: `karoz`

Already using the Codex CLI? When `$HOME/.codex/auth.json` exists, `auto` mode reuses its OAuth credentials directly — no extra setup.

## How Tasks Run

Chat and task execution are two separate paths. The resident agent handles conversation and coordination. When real code needs to change, Karoz hands the work to a **native coding agent** — the `codex` or `claude` CLI already installed on your machine — inside an isolated worktree:

1. **Isolate** — Karoz creates a Git worktree on a dedicated `karoz/task-…` branch (initializing a base snapshot first if the repo has no commits yet).
2. **Develop** — it invokes `codex exec` or `claude` with full workspace access. That agent runs its own internal loop — reading files, editing, running commands, iterating — until the task is done. Karoz delegates the coding loop rather than reimplementing it.
3. **Detect** — if the agent produced no repository changes, the task fails fast instead of reporting false success.
4. **Verify** — when `KAROZ_VERIFY_COMMAND` is set, Karoz runs it in the worktree; a failure marks the task failed with the captured output.
5. **Commit & merge** — changes are committed on the task branch, then merged `--no-ff` into your local base branch. Nothing is pushed to a remote — you review and push when ready.

Every step streams to a live task log, and each terminal state notifies the resident agent so it can decide the next move. Interrupted tasks are recovered on restart.

The worktree is the safety boundary: because the code lives on its own branch behind that boundary, the native agent can run with full permissions inside it while your main checkout and remote stay untouched.

## Multi-Agent Collaboration

A project isn't limited to one agent. You can stand up a team of resident agents with distinct roles — for example architect, builder, and reviewer — that coordinate through a tracked, asynchronous handoff protocol rather than ad-hoc chatter.

- **Direct handoffs.** An agent hands work to a teammate with `send_to`, returns a concrete result with `reply_to`, or turns work down with `decline_handoff` — each addressed by a unique teammate nickname.
- **A real state machine.** Every handoff moves through validated states — `queued → delivered → claimed → working → replied / declined / failed / closed` — so nothing silently stalls or gets worked twice.
- **Serialized, recoverable execution.** Each agent drains its own queue one job at a time, with de-duplication and restart recovery. Automatic retries stop once a run has begun external or persistent side effects, preventing duplicate actions.
- **A shared blackboard.** Agents post progress, blockers, and decisions as signals the whole project can see, and Karoz reconciles the backlog when the project goes idle.
- **Karoz coordinates, you stay in control.** Karoz routes work and reconciles state; it reports rather than silently acting, and the whole exchange is visible in the runtime timeline.

## Skills & MCP

The resident agent is an open runtime — you extend what it knows and what it can do without touching Karoz itself.

**Skills** are local instruction packs (a `SKILL.md` file with `name`/`description` frontmatter). Karoz discovers them from project and user directories — `.agents/skills`, `.codex/skills`, `~/.agents/skills`, and `~/.codex/skills` — reusing the conventions you may already have from Codex. Every turn lists the available skills by name; the agent reads one on demand with `read_skill`, or you pull a full skill inline by mentioning `$SkillName` in your message.

**MCP servers** connect external tools to the agent over the [Model Context Protocol](https://modelcontextprotocol.io). Configure trusted servers globally over `stdio` or `SSE`; repository-owned `.mcp.json` files are ignored unless `KAROZ_TRUST_PROJECT_MCP=1` explicitly grants them host-level trust. MCP tools are exposed only to `plan` and `dev` turns as `mcp__<server>__<tool>`.

Every resident agent has a host Bash tool that starts in the selected project directory. It is not filesystem-sandboxed and can access anything available to the Karoz process. Development turns execute commands directly; ask and plan turns require explicit, single-use approval for the exact command. Path-bounded repository tools remain available for read-only inspection, artifacts are stored under the project's `.karoz/artifacts/` directory with metadata in `.karoz/artifacts.json`, and tracked worktree tasks remain the preferred path for substantial source changes. Karoz adds `/.karoz/` to `.gitignore` only when the project is first created or imported; later user changes are respected.

## What Works Today

**Projects & agents**
- Local project discovery and management
- Persistent project agents with messages and memory
- A local browser workspace with live runtime updates

**Multi-agent teams**
- Role-based teams that hand off work through `send_to` / `reply_to` / `decline_handoff`
- A validated handoff state machine with serialized, recoverable per-agent queues
- A shared blackboard plus idle-time backlog reconciliation by Karoz

**Task execution**
- Isolated worktree runner: develop → detect → verify → commit → merge
- Coding delegated to the native `codex`/`claude` agent inside the worktree
- Live task state, streaming logs, and interrupted-task recovery on restart

**Extensibility & providers**
- Local Skills discovery, listing, and `$mention` injection
- Trusted MCP tool support over stdio and SSE; project MCP requires explicit opt-in
- Streaming resident chat agent via Codex OAuth with tools and live interrupts
- `codex` and `claude` CLI diagnostics

Karoz is an MVP. The core loop — project-scoped agents, multi-agent handoffs, and traceable task execution — works end to end today. The browser interface, configuration surface, and reliability guardrails are still evolving fast.

## Roadmap

### Near Term

- Richer project context, memory management, and retrieval
- Clearer task plans, run states, and human approval gates
- More reliable verification, recovery, and conflict handling
- In-product configuration for agents, models, and tools
- A more polished browser workspace

### Next

- Reusable workflows and project templates
- Pull Request, Issue, and CI integrations
- Scheduled jobs, background queues, and notifications
- Team sharing, permissions, and audit history

### Long-Term Direction

- Give every project a persistent engineering team that learns, executes independently, and stays under human supervision
- Offer one consistent agent workflow across local machines, private infrastructure, and the cloud
- Build an open model, tool, and skill ecosystem — free of vendor lock-in

The roadmap communicates direction, not committed dates. Priorities will shift with real-world feedback.

## Configuration

```bash
KAROZ_ADDR=127.0.0.1:8088
KAROZ_DATA_DIR=.karoz
KAROZ_PROJECTS_ROOT=$HOME/karoz-projects
KAROZ_AGENT_PROVIDER=auto
KAROZ_TASK_PROVIDER=auto
KAROZ_CODEX_AUTH_PATH=$HOME/.codex/auth.json
KAROZ_CODEX_BASE_URL=https://chatgpt.com/backend-api/codex
KAROZ_CODEX_MODEL=gpt-5.6-luna
KAROZ_CLI2API_BASE_URL=http://127.0.0.1:8317
KAROZ_CLI2API_MODEL=claude
KAROZ_CLI2API_API_KEY=
KAROZ_TRUST_PROJECT_MCP=0
KAROZ_VERIFY_COMMAND=
```

Resident provider (`KAROZ_AGENT_PROVIDER`):

- `auto`: use `codex-direct` when Codex OAuth credentials are available; otherwise fail explicitly because a capability-complete resident provider is unavailable.
- `codex-direct`: read Codex CLI OAuth credentials and call the Codex upstream API directly.
- `codex-oauth` / `codex-api`: compatibility aliases for the same streaming resident path.

Task and diagnostics providers (`KAROZ_TASK_PROVIDER` and `/api/cli2api`):

- `codex`: call the host `codex` CLI directly.
- `claude`: call the host `claude` CLI directly.
- `cli2api`: call the OpenAI-compatible service configured by `KAROZ_CLI2API_BASE_URL`.
- `stub`: skip model calls for UI and task smoke testing.

## Docker

Docker Compose exposes Karoz only on `127.0.0.1` and mounts only the project root, the Karoz data directory, and the Codex configuration directory — never your entire home directory.

The Compose project mount defaults to `./projects`. To use another directory:

```bash
KAROZ_PROJECTS_ROOT=$HOME/karoz-projects docker compose up --build
```

## Security

Karoz has no HTTP authentication layer yet. Keep it bound to a loopback address and do not expose it to a LAN or the public internet.

The `.karoz` directory holds local agent messages, task logs, and other runtime state. Don't commit it or include it in a Docker build context. Review project permissions and verification commands before running agents, just as you would with any local development tool.

## License

MIT
