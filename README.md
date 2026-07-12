# Karoz

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8.svg)](go.mod)
[![Status](https://img.shields.io/badge/status-MVP-orange.svg)](#what-works-today)

[简体中文](README.zh-CN.md) | English

**Give every project a resident AI engineer — one that remembers your codebase, works from the browser, and ships reviewable commits inside isolated Git worktrees.**

AI coding tools answer the question in front of them, then forget everything. Every new session starts from zero: you re-explain the project, tasks scatter across terminals, and you still inspect diffs, run checks, and craft commits by hand.

Karoz closes that loop. Each project gets a persistent agent with its own memory and a traceable task workflow. Describe a goal in the browser, let the agent work with real project context, and watch development tasks run in isolated worktrees — with live logs, diffs, and commits you review before they touch your main branch. Context stays with the project, code stays on your machine, and every step stays visible and interruptible.

```
  ~/karoz-projects/
  ├── api-service   →  resident agent + memory + tasks + run history
  ├── web-app       →  resident agent + memory + tasks + run history
  └── data-pipeline →  resident agent + memory + tasks + run history

  task "add pagination"  →  isolated git worktree  →  agent edits
                         →  change detection  →  optional verify
                         →  commit  →  you review  →  merge
```

## Why Karoz

- **Built around projects, not chat windows.** Each project keeps its own agent, messages, memory, tasks, and run history — so context compounds instead of resetting every session.
- **Advice becomes delivery.** Karoz doesn't stop at code generation. It runs the work in isolation, detects changes, optionally verifies them, and produces a Git commit you can review.
- **Your main checkout stays clean.** Development tasks run in separate worktrees, so parallel work never collides or leaves surprise edits in your working tree.
- **Local-first, with clear boundaries.** You own the service, the project data, and the execution environment — and you can reuse the Codex or Claude auth you already have.
- **Transparent and interruptible.** Task state, live logs, and diffs stay visible. Inspect any run, or take over at any point.
- **No vendor lock-in.** Use Codex OAuth, a local CLI, or an OpenAI-compatible proxy — swap providers without rewriting your workflow.

## Who It Is For

Karoz is for individual developers and small teams juggling multiple repositories who want to hand repeatable engineering work to AI — without giving up their Git workflow, data control, or the ability to review what the agent did.

It shines on long-lived projects, parallel feature work, well-scoped implementation tasks, and any setup where you want a complete local record of what your coding agents actually changed.

## How It Works

1. Karoz scans your projects directory and gives each project its own workspace.
2. A project-scoped `karoz` agent keeps messages, memory, and context across sessions.
3. You create development or deployment tasks and follow their state and logs live in the browser.
4. Development tasks run in an isolated Git worktree — never directly in your main checkout.
5. Karoz detects changes, optionally runs your verification command, and commits the result for you to review and merge.

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

## What Works Today

- Local project discovery and management
- Persistent project agents with messages and memory
- Development and deployment task records
- Live task state and streaming execution logs
- `codex` and `claude` CLI diagnostics
- Direct Codex OAuth agent execution
- Optional OpenAI-compatible cli2api adapter
- Isolated worktree-based development task runner
- Change detection, optional verification, commit, and merge workflow
- Interrupted-task recovery on restart
- A local browser workspace

Karoz is an MVP. The focus right now is validating the core loop — a project-scoped agent plus traceable task execution. The interface, extension system, and team features are still evolving fast.

## Roadmap

### Near Term

- Richer project context, memory management, and retrieval
- Clearer task plans, run states, and human approval gates
- More reliable verification, recovery, and conflict handling
- In-product configuration for agents, models, and tools
- A more polished browser workspace

### Next

- Multi-agent delegation and collaboration
- Reusable skills, workflows, and project templates
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
KAROZ_VERIFY_COMMAND=
```

Providers:

- `auto`: use `codex-direct` when Codex OAuth credentials are available, otherwise fall back to an available local execution path.
- `codex-direct`: read Codex CLI OAuth credentials and call the Codex upstream API directly.
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
