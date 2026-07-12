# Karoz

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8.svg)](go.mod)
[![Status](https://img.shields.io/badge/status-MVP-orange.svg)](#当前能力)

简体中文 | [English](README.md)

**为每个项目配一名常驻 AI 工程师——它记得你的代码库，在浏览器里工作，并在隔离的 Git worktree 中交付可审查的提交。**

AI 编程工具擅长回答眼前的问题，然后把一切忘光。每个新会话都从零开始：你重新解释项目、任务散落在各个终端，最后仍要手动检查差异、跑验证、整理提交。

Karoz 闭合了这个回路。每个项目拥有一个带独立记忆的常驻 Agent 和一套可追踪的任务工作流。你在浏览器里描述目标，让 Agent 结合真实项目上下文工作；开发任务在隔离的 worktree 里由成熟的原生编码 Agent 执行——实时日志、差异、提交都留在专属分支上，在你审查并推送之前不会到达远程。上下文留在项目里，代码留在你的机器上，每一步都可见、可中断。

```
  ~/karoz-projects/
  ├── api-service   →  常驻 Agent + 记忆 + 任务 + 运行记录
  ├── web-app       →  常驻 Agent + 记忆 + 任务 + 运行记录
  └── data-pipeline →  常驻 Agent + 记忆 + 任务 + 运行记录

  任务「加分页」
     → 隔离 git worktree（分支 karoz/task-…）
     → 原生 codex / claude Agent 实现   ← Agent 自己的循环
     → 差异检测 → 可选验证命令
     → 在任务分支提交 → no-ff 合并进本地 base 分支
     → 你审查结果，就绪后再 push
```

## 为什么选择 Karoz

- **围绕项目，而非聊天窗口。** 每个项目保留自己的 Agent、消息、记忆、任务和运行记录——上下文持续累积，而不是每个会话都清零。
- **从建议走到交付。** Karoz 不止于生成代码。它把编码任务交给隔离 worktree 里的原生 `codex`/`claude` Agent，检测改动、按需验证，并产出一个可供你审查的 Git 提交。
- **是一个团队，而不只是一个 Agent。** 一个项目可以容纳多个常驻 Agent——架构、构建、评审——它们通过可追踪的协议互相交接工作，由 Karoz 协调。
- **主工作区始终干净。** 开发任务在独立 worktree 中运行，并行工作永不冲突，也不会在工作区里留下意外改动。
- **本地优先，边界清晰。** 服务、项目数据和执行环境都由你掌控——还能复用你已有的 Codex 或 Claude 登录。
- **过程透明，可随时接管。** 任务状态、实时日志和差异全程可见。任何一次运行都能检查，也能随时接手。
- **不锁定供应商。** Codex OAuth、本地 CLI、OpenAI 兼容代理任选——切换 Provider 无需重写工作流。
- **天生可扩展。** 常驻 Agent 自动识别本地 Skill（可复用的指令包）和 MCP 服务器——stdio 或 SSE——你能接入自己的工作手册和工具，而无需改动 Karoz。

## 适合谁

Karoz 适合同时维护多个代码库、想把重复开发工作交给 AI，又不愿放弃 Git 工作流、数据控制权和「能审查 Agent 到底改了什么」的个人开发者与小团队。

它尤其适合这些场景：持续维护长期项目、并行推进多个需求、把明确任务交给 Agent 执行，或在采用 AI 编程时保留一份完整的本地改动记录。

## Karoz 如何工作

1. Karoz 扫描你的项目目录，为每个项目建立独立工作空间。
2. 项目级 `karoz` Agent 跨会话持续保留消息、记忆和上下文——并可扩展成一支互相交接工作的 Agent 团队。
3. 你创建开发或部署任务，并在浏览器里实时跟踪状态与日志。
4. 每个开发任务在隔离的 Git worktree 中运行，最终以专属分支上一个可审查的提交返回——绝不直接改动你的主工作区（[详见下文](#任务如何执行)）。

## 快速开始

需要 Go 环境，以及一个可供 Karoz 扫描的项目父目录。

```bash
go run ./cmd/karoz
```

浏览器打开：

```text
http://127.0.0.1:8088
```

默认配置：

- 项目目录：`$HOME/karoz-projects`
- 数据目录：`.karoz`
- 默认项目 Agent：`karoz`

已经在用 Codex CLI？当 `$HOME/.codex/auth.json` 存在时，`auto` 模式会直接复用它的 OAuth 凭证——无需额外配置。

## 任务如何执行

聊天和任务执行是两条独立路径。常驻 Agent 负责对话与协调；当真正需要改代码时，Karoz 把活交给一个**原生编码 Agent**——也就是你机器上已安装的 `codex` 或 `claude` CLI——在隔离的 worktree 里完成：

1. **隔离** — Karoz 在专属的 `karoz/task-…` 分支上创建 Git worktree（若仓库尚无提交，会先建一个 base 快照）。
2. **开发** — 调用 `codex exec` 或 `claude`，授予完整工作区权限。该 Agent 运行自己的内部循环——读文件、改代码、跑命令、迭代——直到任务完成。Karoz 委托这个编码循环，而不是自己重造一个。
3. **检测** — 如果 Agent 没产生任何仓库改动，任务直接判失败，而不是谎报成功。
4. **验证** — 设置了 `KAROZ_VERIFY_COMMAND` 时，Karoz 在 worktree 里运行它；失败则把任务标记为 failed 并附上捕获的输出。
5. **提交与合并** — 改动在任务分支提交，然后以 `--no-ff` 合并进你的本地 base 分支。不会 push 到任何远程——你审查后自行 push。

每一步都流式写入实时任务日志，每个终态都会通知常驻 Agent 以决定下一步。被中断的任务会在重启后恢复。

worktree 就是安全边界：因为代码位于边界之后的独立分支上，原生 Agent 可以在里面以完整权限运行，而你的主工作区和远程始终不受影响。

## 多 Agent 协作

项目不局限于单个 Agent。你可以组建一支由不同角色常驻 Agent 构成的团队——例如架构、构建、评审——它们通过可追踪的异步交接（handoff）协议协作，而不是靠零散的闲聊。

- **直接交接。** Agent 用 `send_to` 把工作交给队友，用 `reply_to` 返回一个具体结果，或用 `decline_handoff` 拒绝——每个都以队友的唯一昵称寻址。
- **真正的状态机。** 每次交接都经过校验的状态流转——`queued → delivered → claimed → working → replied / declined / failed / closed`——所以不会有工作悄悄卡住或被重复执行。
- **串行、可恢复的执行。** 每个 Agent 逐个排空自己的队列，带去重、自动重试，以及重启后对进行中交接的恢复。
- **共享黑板。** Agent 把进度、阻塞、决策作为信号发布，整个项目都能看到；项目静默时 Karoz 会对账并处理 backlog。
- **Karoz 负责协调，你保持掌控。** Karoz 路由工作、对账状态；它以上报为主而非擅自行动，整个交互都在运行时时间线里可见。

## Skill 与 MCP

常驻 Agent 是一个开放运行时——你可以扩展它「懂什么」和「能做什么」，而不必改动 Karoz 本身。

**Skill** 是本地指令包（一个带 `name`/`description` frontmatter 的 `SKILL.md` 文件）。Karoz 会从项目级和用户级目录发现它们——`.agents/skills`、`.codex/skills`、`~/.agents/skills`、`~/.codex/skills`——复用你可能已经在用的 Codex 约定。每一轮都会按名字列出可用 Skill；Agent 用 `read_skill` 按需读取，或者你在消息里 `$SkillName` 提及以内联注入整份 Skill。

**MCP 服务器** 通过 [Model Context Protocol](https://modelcontextprotocol.io) 把外部工具接入 Agent。可在项目目录放 `.mcp.json` 做全局或项目级配置，走 `stdio` 或 `SSE` transport。Karoz 会实时发现每个服务器的工具，并以 `mcp__<server>__<tool>` 暴露给 Agent——于是 Figma、数据库或自研 MCP 服务器都能在对话中途被调用，无需改一行代码。

## 当前能力

**项目与 Agent**
- 扫描并管理本地项目
- 项目级常驻 Agent，带消息与记忆
- 本地浏览器工作台，运行时状态实时更新

**多 Agent 团队**
- 基于角色的团队，通过 `send_to` / `reply_to` / `decline_handoff` 交接工作
- 经校验的交接状态机 + 每 Agent 串行、可恢复队列
- 共享黑板，以及 Karoz 在空闲时的 backlog 对账

**任务执行**
- 隔离 worktree 运行器：开发 → 检测 → 验证 → 提交 → 合并
- 编码委托给 worktree 内的原生 `codex`/`claude` Agent
- 实时任务状态、流式日志，以及重启后中断任务恢复

**可扩展性与 Provider**
- 本地 Skill 发现、列举与 `$提及` 注入
- MCP 工具支持，stdio 与 SSE，项目级或全局
- 常驻聊天 Agent：Codex OAuth 直连、本地 CLI，或 OpenAI 兼容 cli2api 适配器
- `codex` 和 `claude` CLI 诊断

Karoz 目前处于 MVP 阶段。核心回路——项目级 Agent、多 Agent 交接、可追踪任务执行——今天已能端到端跑通。浏览器界面、配置面和可靠性护栏仍在快速演进。

## 路线图

### 近期

- 更完整的项目上下文、记忆管理与检索
- 更清晰的任务计划、运行状态和人工审批节点
- 更可靠的验证、失败恢复与冲突处理
- Agent、模型和工具的产品内配置
- 更成熟的浏览器工作台体验

### 下一阶段

- 可复用的工作流和项目模板
- Pull Request、Issue 与 CI 平台集成
- 定时任务、后台队列和通知
- 团队共享、权限控制与审计记录

### 长期方向

- 让每个项目拥有可持续学习、可独立执行、可被人类监督的常驻工程团队
- 在本地、私有基础设施和云端之间提供一致的 Agent 工作流
- 建立开放的模型、工具和技能生态，避免供应商锁定

路线图代表方向而非发布日期，优先级会随真实使用反馈调整。

## 配置

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

Provider：

- `auto`：检测到 Codex OAuth 凭证时使用 `codex-direct`，否则回退到可用的本地执行方式。
- `codex-direct`：读取 Codex CLI OAuth 凭证并直接调用 Codex 上游 API。
- `codex`：直接调用宿主机上的 `codex` CLI。
- `claude`：直接调用宿主机上的 `claude` CLI。
- `cli2api`：调用 `KAROZ_CLI2API_BASE_URL` 指定的 OpenAI 兼容服务。
- `stub`：不调用模型，适合界面和任务冒烟测试。

## Docker

Docker Compose 仅在 `127.0.0.1` 暴露 Karoz，并且只挂载项目根目录、Karoz 数据目录和 Codex 配置目录——绝不挂载整个主目录。

Compose 默认将 `./projects` 作为项目挂载目录。如需使用其他目录：

```bash
KAROZ_PROJECTS_ROOT=$HOME/karoz-projects docker compose up --build
```

## 安全说明

Karoz 目前还没有 HTTP 身份验证层。请始终绑定回环地址，不要暴露到局域网或公网。

`.karoz` 目录包含本地 Agent 消息、任务日志和其他运行状态，不应提交到 Git，也不应包含在 Docker 构建上下文中。运行 Agent 前，请像审查其他本地开发工具一样审查其项目权限和验证命令。

## 许可证

MIT
