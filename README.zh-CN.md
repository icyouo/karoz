# Karoz

简体中文 | [English](README.md)

面向项目级 Agent 和任务的本地 AI 编程工作室。

本仓库是 Karoz 的本地优先 MVP。它由浏览器界面和本地 Go 服务组成，首个版本主要聚焦产品结构与工作流程，而非最终的界面样式。

## 快速开始

```bash
go run ./cmd/karoz
```

打开：

```text
http://127.0.0.1:8088
```

默认配置：

- 项目父目录：`$HOME/karoz-projects`
- 数据目录：`.karoz`
- 默认项目 Agent：`karoz`

## 环境变量

```bash
KAROZ_ADDR=127.0.0.1:8088
KAROZ_DATA_DIR=.karoz
KAROZ_PROJECTS_ROOT=$HOME/karoz-projects
KAROZ_AGENT_PROVIDER=auto
KAROZ_TASK_PROVIDER=auto
KAROZ_CODEX_AUTH_PATH=$HOME/.codex/auth.json
KAROZ_CODEX_BASE_URL=https://chatgpt.com/backend-api/codex
KAROZ_CODEX_MODEL=gpt-5.3-codex-spark
KAROZ_CLI2API_BASE_URL=http://127.0.0.1:8317
KAROZ_CLI2API_MODEL=claude
KAROZ_CLI2API_API_KEY=
KAROZ_VERIFY_COMMAND=
```

Provider：

- `auto`：当 `$HOME/.codex/auth.json` 存在时，使用 Codex OAuth 直连模式。
- `codex-direct`：读取 Codex CLI OAuth 凭证并直接调用 Codex 上游 API。
- `stub`：不调用模型，适合界面和任务冒烟测试。
- `cli2api`：调用 `KAROZ_CLI2API_BASE_URL` 指定的外部 OpenAI 兼容 CLIProxyAPI 服务。
- `claude`：直接调用宿主机上的 `claude` CLI。
- `codex`：直接调用宿主机上的 `codex` CLI。

## 功能范围

MVP 已实现：

- 扫描项目父目录
- 项目级默认 `karoz` Agent
- 开发和部署任务记录
- 任务运行日志
- `codex` 和 `claude` CLI 诊断
- 用于 Agent 和任务执行的 Codex OAuth 直连适配器
- 可选的外部 cli2api 兼容适配器
- 基于 worktree 的开发任务运行器
- 简化的浏览器界面

MVP 尚未集成编程 SDK。Agent 对话和编程任务会尽可能复用宿主机 CLI 的登录状态，随后由 Karoz 负责差异检测、可选验证、提交和合并。

## Docker

Docker Compose 仅在 `127.0.0.1` 暴露 Karoz，并且只挂载项目根目录、Karoz 数据目录和 Codex 配置目录，不会挂载整个主目录。

Compose 默认将 `./projects` 作为项目挂载目录。如需使用其他目录：

```bash
KAROZ_PROJECTS_ROOT=$HOME/karoz-projects docker compose up --build
```

Karoz 没有 HTTP 身份验证层。请始终绑定回环地址，不要直接暴露到局域网或公网。`.karoz` 目录包含本地 Agent 消息、任务日志和其他运行状态，不应提交到 Git，也不应包含在 Docker 构建上下文中。

## 许可证

MIT
