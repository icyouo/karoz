# Karoz

简体中文 | [English](README.md)

**把一次性的 AI 编程对话，变成持续理解项目、真正完成任务的本地工作室。**

AI 编程工具很擅长回答眼前的问题，却常常缺少项目级上下文：换一个会话就要重新解释，多个任务彼此割裂，执行过程散落在终端里，最后仍需要你手动检查改动、运行验证、整理提交。

Karoz 为每个项目提供一个长期驻留的 Agent 和一套可追踪的任务工作流。你可以在浏览器里描述目标，让 Agent 结合项目上下文工作；开发任务则在隔离的 Git worktree 中执行，并留下日志、差异和提交结果。上下文留在项目里，代码留在你的机器上，过程始终可见、可检查、可接管。

## 为什么选择 Karoz

- **围绕项目，而不是围绕聊天窗口**：每个项目拥有独立的 Agent、消息、记忆、任务和运行记录。
- **从建议走到交付**：不止生成代码，还负责隔离执行、差异检测、可选验证和 Git 提交。
- **并行工作不污染主目录**：开发任务运行在独立 worktree，降低多任务并行时的冲突和误操作风险。
- **本地优先，边界清晰**：服务、项目数据和执行环境均由你掌控，并可复用现有 Codex 或 Claude 登录状态。
- **过程透明，可随时接管**：任务状态、运行日志和代码变化都可追踪，不把关键过程藏在黑盒里。
- **模型与执行器可替换**：支持 Codex OAuth 直连、本地 CLI 以及 OpenAI 兼容代理，不把工作流锁死在单一供应商上。

## 适合谁

Karoz 适合同时维护多个代码库、希望把重复开发工作交给 AI，又不愿牺牲 Git 工作流、数据控制权和可审查性的个人开发者与小团队。

它尤其适合这些场景：持续维护一个长期项目、并行推进多个需求、把明确任务交给 Agent 执行，或在采用 AI 编程时保留完整的本地执行记录。

## Karoz 如何工作

1. Karoz 扫描你的项目目录，为每个项目建立独立工作空间。
2. 项目级 `karoz` Agent 持续保留消息、记忆和项目上下文。
3. 你创建开发或部署任务，并在界面中跟踪执行状态与日志。
4. 开发任务在独立 Git worktree 中运行，避免直接扰动主工作区。
5. Karoz 检测代码变化，按需执行验证，然后提交结果供你审查和合并。

## 快速开始

需要 Go 环境和一个可供 Karoz 扫描的项目父目录。

```bash
go run ./cmd/karoz
```

浏览器打开：

```text
http://127.0.0.1:8088
```

默认配置：

- 项目父目录：`$HOME/karoz-projects`
- 数据目录：`.karoz`
- 默认项目 Agent：`karoz`

如果 `$HOME/.codex/auth.json` 存在，`auto` 模式会直接复用 Codex CLI 的 OAuth 凭证。

## 当前能力

- 扫描并管理本地项目
- 项目级常驻 Agent、消息与记忆
- 开发和部署任务记录
- 实时任务状态与运行日志
- `codex` 和 `claude` CLI 诊断
- Codex OAuth 直连 Agent
- 可选的 OpenAI 兼容 cli2api 适配器
- 基于 worktree 的隔离开发任务运行器
- 差异检测、可选验证、提交与合并工作流
- 本地浏览器工作台

Karoz 目前仍处于 MVP 阶段。我们优先验证项目级 Agent 与可追踪任务执行的核心工作流，界面、扩展机制和团队协作能力仍在持续完善。

## 路线图

### 近期

- 更完整的项目上下文、记忆管理与检索
- 更清晰的任务计划、运行状态和人工审批节点
- 更可靠的验证、失败恢复与冲突处理
- Agent、模型和工具配置界面
- 更成熟的浏览器工作台体验

### 下一阶段

- 多 Agent 分工与协作
- 可复用的技能、工作流和项目模板
- Pull Request、Issue 与 CI 平台集成
- 定时任务、后台队列和通知
- 团队共享、权限控制与审计记录

### 长期方向

- 让项目拥有可持续学习、可独立执行、可被人类监督的常驻工程团队
- 在本地、私有基础设施和云端之间提供一致的 Agent 工作流
- 建立开放的模型、工具和技能生态，避免供应商锁定

路线图代表方向而非发布日期，优先级会根据真实使用反馈调整。

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

Docker Compose 仅在 `127.0.0.1` 暴露 Karoz，并且只挂载项目根目录、Karoz 数据目录和 Codex 配置目录，不会挂载整个主目录。

Compose 默认将 `./projects` 作为项目挂载目录。如需使用其他目录：

```bash
KAROZ_PROJECTS_ROOT=$HOME/karoz-projects docker compose up --build
```

## 安全说明

Karoz 当前没有 HTTP 身份验证层。请始终绑定回环地址，不要直接暴露到局域网或公网。

`.karoz` 目录包含本地 Agent 消息、任务日志和其他运行状态，不应提交到 Git，也不应包含在 Docker 构建上下文中。运行 Agent 前，请像审查其他本地开发工具一样审查其项目权限和验证命令。

## 许可证

MIT
