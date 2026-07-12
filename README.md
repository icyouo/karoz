# Karoz

[简体中文](README.zh-CN.md) | English

Local AI coding studio for project-scoped agents and tasks.

This repository is the local-first Karoz MVP. It runs as a browser UI backed by a local Go service. The first implementation focuses on structure and flow rather than final UI styling.

## Quick Start

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

## Environment

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

- `auto`: use Codex OAuth direct mode when `$HOME/.codex/auth.json` exists.
- `codex-direct`: read Codex CLI OAuth credentials and call the Codex upstream API directly.
- `stub`: no model call, useful for UI/task smoke tests.
- `cli2api`: call an external OpenAI-compatible CLIProxyAPI service at `KAROZ_CLI2API_BASE_URL`.
- `claude`: call host `claude` CLI directly.
- `codex`: call host `codex` CLI directly.

## Scope

The MVP implements:

- project parent directory scanning
- project-scoped default `karoz` agent
- development and deployment task records
- task run logs
- CLI diagnostics for `codex` and `claude`
- Codex OAuth direct adapter for Agent and Task execution
- optional external cli2api-compatible adapter
- worktree-based development task runner
- simplified browser UI

The MVP does not integrate a coding SDK. Agent turns and coding tasks reuse the host CLI login state where possible, then Karoz handles diff detection, optional verification, commit, and merge.

## Docker

Docker Compose exposes Karoz only on `127.0.0.1` and mounts only the project root,
Karoz data directory, and Codex configuration directory. It does not mount the
entire home directory.

The Compose project mount defaults to `./projects`. To use another directory:

```bash
KAROZ_PROJECTS_ROOT=$HOME/karoz-projects docker compose up --build
```

Karoz has no HTTP authentication layer. Keep it bound to a loopback address and
do not expose it directly to a LAN or the public internet. The `.karoz` directory
contains local agent messages, task logs, and other runtime state and must not be
committed or included in a Docker build context.

## License

MIT
