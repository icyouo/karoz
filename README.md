# Karoz

[简体中文](README.zh-CN.md) | English

**Turn one-off AI coding chats into a local studio that understands your projects and gets work done.**

AI coding tools are good at answering the question in front of them, but they often lose the bigger picture. Every new session needs another explanation, tasks live in disconnected terminals, and you still have to inspect changes, run checks, and organize commits by hand.

Karoz gives every project a persistent agent and a traceable task workflow. Describe a goal in the browser, let the agent work with project context, and run development tasks in isolated Git worktrees with logs, diffs, and commits you can review. Context stays with the project, code stays on your machine, and the process remains visible and interruptible.

## Why Karoz

- **Built around projects, not chat windows:** each project gets its own agent, messages, memory, tasks, and run history.
- **Moves from advice to delivery:** Karoz handles isolated execution, change detection, optional verification, and Git commits—not just code generation.
- **Keeps parallel work out of your main checkout:** development tasks run in separate worktrees to reduce collisions and accidental changes.
- **Local-first with explicit boundaries:** you control the service, project data, and execution environment while reusing existing Codex or Claude authentication.
- **Transparent and reviewable:** task state, logs, and code changes remain visible so you can inspect or take over at any time.
- **Provider-flexible:** use Codex OAuth, local CLIs, or an OpenAI-compatible proxy without tying the workflow to one model vendor.

## Who It Is For

Karoz is for individual developers and small teams maintaining multiple repositories who want to delegate repeatable engineering work to AI without giving up Git workflows, data control, or reviewability.

It is especially useful for long-lived projects, parallel feature work, well-scoped implementation tasks, and teams that want a complete local record of what their coding agents did.

## How It Works

1. Karoz scans your projects directory and creates an independent workspace for each project.
2. A project-scoped `karoz` agent retains messages, memory, and project context over time.
3. You create development or deployment tasks and follow their state and logs from the browser.
4. Development tasks run in isolated Git worktrees instead of modifying your main checkout directly.
5. Karoz detects changes, optionally runs your verification command, and commits the result for review and merge.

## Quick Start

You need Go and a parent directory containing the projects you want Karoz to scan.

```bash
go run ./cmd/karoz
```

Open:

```text
http://127.0.0.1:8088
```

Defaults:

- project parent directory: `$HOME/karoz-projects`
- data directory: `.karoz`
- default project agent: `karoz`

When `$HOME/.codex/auth.json` exists, `auto` mode reuses the Codex CLI OAuth credentials directly.

## What Works Today

- local project discovery and management
- persistent project agents, messages, and memory
- development and deployment task records
- live task state and execution logs
- `codex` and `claude` CLI diagnostics
- direct Codex OAuth agent execution
- optional OpenAI-compatible cli2api adapter
- isolated worktree-based development task runner
- change detection, optional verification, commit, and merge workflow
- local browser workspace

Karoz is currently an MVP. The priority is validating the core project-agent and traceable task-execution workflow; the interface, extension system, and team features are still evolving.

## Roadmap

### Near Term

- richer project context, memory management, and retrieval
- clearer task plans, run states, and human approval gates
- more reliable verification, recovery, and conflict handling
- in-product configuration for agents, models, and tools
- a more polished browser workspace

### Next

- multi-agent delegation and collaboration
- reusable skills, workflows, and project templates
- Pull Request, Issue, and CI integrations
- scheduled jobs, background queues, and notifications
- team sharing, permissions, and audit history

### Long-Term Direction

- give every project a persistent engineering team that can learn, execute independently, and remain under human supervision
- provide one consistent agent workflow across local machines, private infrastructure, and cloud environments
- build an open model, tool, and skill ecosystem without vendor lock-in

The roadmap communicates direction, not committed dates. Priorities will change with real-world feedback.

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

Docker Compose exposes Karoz only on `127.0.0.1` and mounts only the project root, Karoz data directory, and Codex configuration directory. It does not mount the entire home directory.

The Compose project mount defaults to `./projects`. To use another directory:

```bash
KAROZ_PROJECTS_ROOT=$HOME/karoz-projects docker compose up --build
```

## Security

Karoz currently has no HTTP authentication layer. Keep it bound to a loopback address and do not expose it directly to a LAN or the public internet.

The `.karoz` directory contains local agent messages, task logs, and other runtime state. Do not commit it or include it in a Docker build context. Review project permissions and verification commands before running agents, just as you would with any local development tool.

## License

MIT
