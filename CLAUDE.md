# Agent instructions — adamaton-delegator

> **🚫 BLOCKING — never commit to `main` directly.** Adamaton's pre-commit hook hard-rejects commits to `main`/`master` from the canonical checkout. Always start with:
> ```bash
> cd /thearray/git/Adamaton && bin/adam claim 'delegator'/<task>
> ```
> This creates a worktree on a feature branch where you can work freely. If you accidentally edited main, run `bin/adam rescue` from the umbrella to recover. See [umbrella CLAUDE.md](../CLAUDE.md) for the full agent workflow.

Multi-agent task router + MCP servers. Two Go modules: `delegator/` (orchestrator + budget-router) and `mcp/` (stdio MCP servers Claude Code launches).

## Before you edit

From the umbrella:

```bash
bin/adam claim delegator/<task>
```

## Build / test

```bash
cd delegator && go build ./... && go test ./...
cd mcp       && go build ./... && go test ./...
```

From the umbrella `bin/` directory is where the canonical `delegator-mcp` build target lands — global CLAUDE.md references `/thearray/git/evo/bin/delegator-mcp` (legacy path; the canonical home is now the Adamaton umbrella).

## Gotchas

- **MCP transport is stdio.** Don't add HTTP-only logic to anything imported by `mcp/cmd/delegator-mcp/main.go`; Claude Code launches the binary, talks JSON-RPC over stdin/stdout, kills it on session end.
- **Task store is Postgres, not SQLite** (memory: pre-Adamaton sqlite store at `~/.local/share/gogents/delegator/tasks.db` was retired). All status/result/list lookups go through `postgres_store.go` against `evo.tasks`.
- **Recurring schedules need Temporal** at `:7233`. If unreachable, `schedule_recurring_task`/`list_recurring_schedules`/`cancel_recurring_schedule` simply don't register — the rest of the MCP still works.
- **Rate limit tracking** is automatic via the CCSAVER proxy at `localhost:19456` — every Claude API call goes through it. Don't add a manual self-report path for Claude. Only Gemini needs `report_gemini_usage`.
- **Budget router contract**: callers always pass `difficulty` (trivial/easy/medium/hard/expert) and `priority` (immediate/normal/background). Default to OpenCode for background work — it's unlimited.
- **`platform.dashboard` imports this in-process.** Public API stability matters; breaking `orchestrator.go` types ripples into dashboard.
- **Per-agent CLI wrappers** in `delegator/llm/` must handle: missing CLI binary, auth-not-configured, rate-limited responses, and partial JSON output (some agents stream).

## Universal rules

See `../CLAUDE.md` in the umbrella.
