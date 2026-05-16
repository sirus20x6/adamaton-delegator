# adamaton-delegator

Multi-agent delegation orchestrator. Routes coding tasks across Codex, Gemini, and OpenCode via a budget-aware router, tracks quota, and exposes the whole thing as two MCP servers for Claude Code to consume.

## Layout

| Dir | Purpose |
|---|---|
| `delegator/` | Orchestrator library + `budget-router` binary |
| `delegator/orchestrator.go` | Task dispatch loop |
| `delegator/scheduler.go` | Temporal-backed recurring schedules |
| `delegator/budget/` | Budget router — picks the cheapest agent that matches `difficulty` × `priority` |
| `delegator/quota/` | Quota aggregation (CCSAVER headers + Gemini self-report) |
| `delegator/llm/` | Per-agent CLI wrappers (claude, codex, gemini, opencode) |
| `delegator/contextmode/` | Context-mode helpers for handing partial conversations to agents |
| `delegator/skillsclient/` | Thin HTTP client for `knowledge.skills-rae` |
| `delegator/postgres_store.go` | Postgres persistence — `evo.tasks` schema |
| `delegator/migrations/` | golang-migrate SQL files |
| `delegator/cmd/budget-router/` | Standalone HTTP router binary |
| `mcp/cmd/delegator-mcp/` | MCP server exposing `delegate_task`, `get_task_status`, `get_task_result`, `cancel_task`, `list_tasks`, `get_quota_usage`, `report_gemini_usage`, `schedule_recurring_task`, etc. |
| `mcp/cmd/skillrae-mcp/` | MCP server bridging skills-rae endpoints to Claude Code |

## Build

```bash
cd delegator && go build ./...
cd mcp       && go build ./...
```

From the umbrella, `go build ./...` covers both via `go.work`.

The `delegator-mcp` binary is the one wired into Claude Code's MCP config — built target at the umbrella's `bin/delegator-mcp`.

## Test

```bash
cd delegator && go test ./...
cd mcp       && go test ./...
```

The orchestrator tests use a testcontainer-postgres.

## Dev DSN / endpoints

- Postgres: `postgres://evo:evo@localhost:5432/evo` (writes to `evo.tasks` schema)
- Temporal: `localhost:7233` (required for recurring schedules; if unreachable the schedule tools simply don't register)
- MCP transport: **stdio only** — Claude Code launches the binary as a child process

## Where this fits

`platform.dashboard` imports `delegator` **in-process** (Go import, not HTTP) so the `/delegator` page can read live orchestrator state. The MCP servers are called by Claude Code over stdio, not by other Adamaton components. Depends on `adamaton-core` and `adamaton-knowledge` (skills-rae client). See `docs/ARCHITECTURE.md` in the umbrella.
