---
status: resolved
nickname: icy
resolved_by: icy
resolved_at: 2026-07-18T17:00:48+08:00
---
# DECISION 0001: Gate resident bash by turn mode and user approval

Date: 2026-07-18
Author: icy
Nickname: icy

Context:
Resident agents originally exposed an unrestricted host bash tool. Runtime hardening removed it entirely and replaced repository inspection with bounded read-only tools. For a local AI studio, that removal is too restrictive, but restoring unconditional host-shell execution would also discard the safety boundary introduced by the hardening work.

Decision:
Every resident agent exposes a bash tool. Development turns may execute it directly. Ask and plan turns must stop and request explicit user approval for the exact command; only a matching, single-use approval may authorize execution on a later turn.

Implications:
The approval must be scoped to project, agent, and exact command, must not be reusable, and must not allow stale runs or unrelated commands to bypass the gate. Prompts, tool policy, persistence where needed, and regression tests must preserve this contract. Task worktrees remain the preferred location for coding changes even though approved resident shell execution is available.
