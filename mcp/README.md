# delegator-mcp tool reference

`delegator-mcp` is a stdio MCP server. Claude Code launches the binary,
talks JSON-RPC over stdin/stdout, and kills it on session end. All tools
are registered in `mcp/cmd/delegator-mcp/`.

## Runtime requirements

| Requirement | Details |
|---|---|
| `POSTGRES_DSN` | Required. Budget store + task store. |
| Temporal at `:7233` | Optional. Only the three `schedule_*` tools need it; the rest work without it. If unreachable, the gate returns an error on first schedule call. |
| `KANBAN_API_URL` | Optional. Default `http://localhost:9123`. Used by all `kanban_*` and `project_*` tools. |
| `SKILLS_API_URL` | Optional. Default `http://localhost:9123`. Skills RAG on `delegate_task`. Set to `""` to disable. |

---

## Delegation tools

### `delegate_task`

Hand a coding task off to another AI agent. Returns immediately with a
`task_id`; poll with `get_task_status`.

Before calling, estimate `difficulty` and `priority` — the budget router
uses both to pick the cheapest agent that can handle the work. Default to
`opencode` for background work (unlimited, no quota risk).

| Arg | Required | Description |
|---|---|---|
| `prompt` | yes | Task instructions for the agent. Max 1 MiB. |
| `difficulty` | no | `trivial` / `easy` / `medium` / `hard` / `expert` |
| `priority` | no | `immediate` / `normal` / `background` |
| `agent` | no | Skip the router; pin a specific agent: `codex`, `gemini`, `opencode`. |
| `working_dir` | no | Absolute path under `DELEGATOR_WORKING_DIR_ROOT` (default `/thearray`). |
| `model` | no | Model override forwarded to the agent CLI via `-m`. |
| `timeout_seconds` | no | Default 300, max 1800. |

Returns `{task_id, agent, provider, status}`.

---

### `get_task_status`

Get the current status of a delegated task without the full output.

| Arg | Required | Description |
|---|---|---|
| `task_id` | yes | ID returned by `delegate_task`. |

Returns `{task_id, agent, provider, status, elapsed_seconds, output_preview, stderr_preview}`.
`output_preview` is capped at 500 bytes; `stderr_preview` at 200 bytes.

---

### `get_task_result`

Get the full result of a completed task, including parsed output and exit code.

| Arg | Required | Description |
|---|---|---|
| `task_id` | yes | ID returned by `delegate_task`. |

Returns `{task_id, agent, provider, status, exit_code, output}`. On failure also includes `error` and `stderr` (capped at 1000 bytes).

---

### `cancel_task`

Cancel a running task. Sends SIGTERM then SIGKILL to the underlying CLI.

| Arg | Required | Description |
|---|---|---|
| `task_id` | yes | ID of the task to cancel. |

Returns `{task_id, status}` where status is `cancelled` or `not_found_or_not_running`.

---

### `list_tasks`

List recently delegated tasks, optionally filtered by status or agent.

| Arg | Required | Description |
|---|---|---|
| `status_filter` | no | `pending` / `running` / `completed` / `failed` / `cancelled` / `timed_out` |
| `agent` | no | Filter to tasks routed to a specific agent. |

Returns an array of task summaries with `task_id`, `agent`, `provider`, `status`, `prompt_preview` (100 bytes), `elapsed_seconds`, `exit_code`.

---

## Quota tools

### `get_quota_usage`

Get current quota utilization for all agents, or filter to one. Reads
CCSAVER rate-limit headers (Claude) and cached on-disk state (Gemini) —
values reflect actual usage, not the budget router's bookkeeping.

| Arg | Required | Description |
|---|---|---|
| `agent` | no | `claude` / `codex` / `gemini` / `opencode`. Omit for all. |
| `days` | no | Days back to aggregate. Default 1. |

Returns `{agents: [...]}`.

---

### `report_gemini_usage`

Manually report Gemini's daily usage limits (run after checking `/stats`
in the Gemini CLI). The values are cached on disk so subsequent
`get_quota_usage` calls reflect them.

| Arg | Required | Description |
|---|---|---|
| `utilization_daily` | no | Daily utilization as a decimal (e.g. `0.11` = 11%). |
| `reset_daily` | no | Daily reset time as an ISO string. |
| `models` | no | Per-model breakdown: `{model: {requests, usageLeft}}`. |

Returns `{status: "ok"}`.

---

## Recurring schedule tools

These three tools require Temporal at `:7233` (or `TEMPORAL_ADDRESS`).
The gate dials lazily on the first call — starting Temporal after
`delegator-mcp` does not require a Claude Code restart.

### `schedule_recurring_task`

Create a recurring delegation that fires on a cron schedule. Each fire
spawns a fresh `delegate_task`-equivalent run.

| Arg | Required | Description |
|---|---|---|
| `id` | yes | Unique schedule slug, e.g. `nightly-test-suite`. |
| `cron` | yes | 5-field cron in operator timezone, e.g. `*/5 * * * *`. |
| `prompt` | yes | Task instructions. Max 1 MiB. |
| `note` | no | Free-text label surfaced in `list_recurring_schedules`. |
| `difficulty` | no | Same values as `delegate_task`. |
| `priority` | no | Same values as `delegate_task`. |
| `agent` | no | Pin a specific agent. |
| `working_dir` | no | Absolute path under the allowlist root. |
| `model` | no | Model override. |
| `timeout_seconds` | no | Default 300, max 1800. |

Returns the Temporal schedule summary.

---

### `list_recurring_schedules`

List all active recurring delegation schedules.

No arguments. Returns an array of schedule summaries.

---

### `cancel_recurring_schedule`

Delete a recurring delegation schedule. Idempotent — deleting a missing
schedule returns ok.

| Arg | Required | Description |
|---|---|---|
| `id` | yes | The schedule ID to delete. |

Returns `{id, status: "cancelled"}`.

---

## Context-mode tools

These tools require `POSTGRES_DSN`. They run scripts or fetch URLs
directly in Go, index large outputs into a Tantivy-backed BM25 store
(pg_search inside Postgres), and return exact bytes cropped around the
caller's stated intent. They replace chains of shell Bash + Read calls
for analysis-of-many-files workflows.

Optional sidecar upgrades:
- `OCTEN_SIDECAR_URL` — enables dense retrieval (BGE-M3 embeddings) on top of BM25.
- `BGE_SIDECAR_URL` — enables BGE-Reranker stage-3; reranks chunks against intent instead of LLM compression.

### `execute`

Execute a script (bash/python/node/go) in a subprocess and return its
output. Outputs under ~5 KB return raw; bigger outputs are indexed and,
when `intent` is set, return BM25-ranked snippets cropped around matches
(exact bytes, no LLM paraphrasing).

| Arg | Required | Description |
|---|---|---|
| `script` | yes | Script body. Bash by default; override with `language`. |
| `language` | no | `bash` / `python` / `node` / `go`. Detected from shebang if omitted. |
| `intent` | no | What to look for. When set + output exceeds the threshold, BM25 crops the result. |
| `working_dir` | no | Absolute path under the allowlist root. |
| `timeout_seconds` | no | Hard cap. Default 60, max 1800. |

---

### `fetch_and_index`

Fetch a URL, strip nav/footer/ads, convert to clean markdown-ish text,
and index it. Returns the full text when small; returns BM25-ranked
snippets when big and `intent` is set. The model always gets exact indexed
bytes — never paraphrased.

| Arg | Required | Description |
|---|---|---|
| `url` | yes | Absolute URL (http/https only). |
| `intent` | no | What to extract. Triggers BM25 cropping on large pages. |

---

### `search`

Query the BM25 index built by previous `execute()` and `fetch_and_index()`
calls. Returns ranked snippets across all indexed sources with exact bytes
cropped around matches. Useful for follow-up exploration after a big script
ran without an `intent`.

| Arg | Required | Description |
|---|---|---|
| `query` | yes | Whitespace-separated terms. The ngram(3,3) tokenizer means substring matches work (`useEff` hits `useEffect`). |
| `top_k` | no | Max snippets. Default 10. |

---

## Kanban tools

Thin REST clients of the apiserver kanban API (`/api/v1/kanban/...`). The
MCP server never touches Postgres directly for kanban — atomicity (advisory
locks, claim-token matching) lives in the apiserver. Base URL from
`KANBAN_API_URL` (default `http://localhost:9123`).

### `kanban_create_board`

Create a kanban board under a project. Seeds 5 columns: Backlog, Ready,
In Progress, Review, Done. Returns `{board, columns}`.

| Arg | Required | Description |
|---|---|---|
| `project_id` | yes | Project to create the board under. |
| `name` | yes | Board name. |

---

### `kanban_list_boards`

List all kanban boards for a project. Returns `Board[]`.

| Arg | Required | Description |
|---|---|---|
| `project_id` | yes | Project whose boards to list. |

---

### `kanban_add_card`

Add a card to a kanban board. Defaults to Backlog column, priority
`normal`, difficulty `medium`. Use priority `low|normal|high|urgent` and
difficulty `trivial|easy|medium|hard|expert`. For coding cards, write a
self-contained body with acceptance criteria and the explicit requirement to
work in a fresh worktree, commit, open a PR, merge into `main`/`master`, verify
trunk, then complete the card. Returns the created Card.

| Arg | Required | Description |
|---|---|---|
| `board_id` | yes | Board to add the card to. |
| `title` | yes | Card title. |
| `body` | no | Card description. |
| `column_id` | no | Target column. Defaults to Backlog. |
| `priority` | no | `low`, `normal`, `high`, or `urgent`. Default `normal`. |
| `difficulty` | no | `trivial`, `easy`, `medium`, `hard`, or `expert`. Default `medium`. |

---

### `kanban_get_board`

Fetch a board in full. Returns `{board, columns, cards, comments, links}`.
Archived cards are hidden by default; pass `include_archived=true` to inspect
archived Done work and its comments/dependency links.

| Arg | Required | Description |
|---|---|---|
| `board_id` | yes | Board to fetch. |
| `include_archived` | no | Include archived cards/comments/links when true. |

---

### `kanban_list_ready_cards`

List unclaimed, unarchived, dependency-unblocked cards in the board's Ready
column. This is the queue agents should pull from. Cards blocked by incomplete
dependencies are hidden here and also fail claim with 409. Returns `Card[]`.

| Arg | Required | Description |
|---|---|---|
| `board_id` | yes | Board whose ready cards to list. |

---

### `kanban_claim_card`

Atomically claim a ready card for an agent. Returns `{card, claim_token}` or a
409 error if the card is already claimed, archived, or blocked by incomplete
dependencies. The `claim_token` is required for later move/complete/release
calls.

| Arg | Required | Description |
|---|---|---|
| `card_id` | yes | Card to claim. |
| `agent_id` | yes | Agent claiming the card. |

---

### `kanban_move_card`

Move a card to another column. If the card is claimed, `claim_token` must
match. Returns the updated Card.

| Arg | Required | Description |
|---|---|---|
| `card_id` | yes | Card to move. |
| `target_column_id` | yes | Destination column. |
| `claim_token` | no | Required when the card is claimed. |
| `position` | no | Optional position within the target column. |

---

### `kanban_complete_card`

Mark a claimed card done and move it to the Done column. For coding work, call
this only after the agent worked in a worktree, committed the intended changes,
opened a PR targeting `main`/`master`, merged that PR into `main`/`master` (not
another branch), and verified trunk contains the result. `claim_token` must
match. Returns the updated Card.

| Arg | Required | Description |
|---|---|---|
| `card_id` | yes | Card to complete. |
| `claim_token` | yes | Token from `kanban_claim_card`. |
| `result_summary` | no | Short summary of the completed work. |
| `result_task_id` | no | `delegate_task` ID that produced the result. |
| `result_pr_url` | no | PR URL for the completed work. |

---

### `kanban_release_card`

Release a claimed card back to unclaimed (clears `claimed_by`, `claim_token`,
`claimed_at`). Use this for failed, blocked, or abandoned attempts only after
adding a comment explaining the failure and any worktree/branch state. Do not
release a successfully completed coding card as a substitute for commit/PR/merge
to trunk. `claim_token` must match. Returns the updated Card.

| Arg | Required | Description |
|---|---|---|
| `card_id` | yes | Card to release. |
| `claim_token` | yes | Token from `kanban_claim_card`. |

---

### `kanban_add_comment`

Add a comment to a card. Returns the created Comment.

| Arg | Required | Description |
|---|---|---|
| `card_id` | yes | Card to comment on. |
| `author` | yes | Comment author identifier. |
| `text` | yes | Comment body. |

---

### `kanban_add_dependency`

Make `card_id` depend on `depends_on_card_id`. Both cards must be on the same
board. A dependent card does not appear in `kanban_list_ready_cards` and cannot
be claimed until every dependency card is completed. Returns the dependency link.

| Arg | Required | Description |
|---|---|---|
| `card_id` | yes | Blocked card. |
| `depends_on_card_id` | yes | Prerequisite card that must complete first. |

---

### `kanban_delete_dependency`

Remove a dependency edge so `card_id` no longer depends on
`depends_on_card_id`. Returns `{deleted, card_id, depends_on_card_id}`.

| Arg | Required | Description |
|---|---|---|
| `card_id` | yes | Blocked card. |
| `depends_on_card_id` | yes | Dependency card to remove. |

---

### `kanban_reopen_card`

Reopen a completed or failed, unarchived card into Ready. Returns the updated
Card.

| Arg | Required | Description |
|---|---|---|
| `card_id` | yes | Completed or failed card to reopen. |

---

### `kanban_archive_card`

Soft-archive a completed card so busy boards hide old Done work without
deleting history. Returns the updated Card.

| Arg | Required | Description |
|---|---|---|
| `card_id` | yes | Completed card to archive. |

---

### `kanban_restore_card`

Restore an archived card to the visible board while preserving status, result
metadata, comments, and dependency links. Returns the updated Card.

| Arg | Required | Description |
|---|---|---|
| `card_id` | yes | Archived card to restore. |

---

### `kanban_archive_done_cards`

Bulk soft-archive completed Done cards on a board. Omit `older_than_days` to
archive all Done cards, or pass `0+` to keep newer completions visible. Returns
`{archived, board_id}`.

| Arg | Required | Description |
|---|---|---|
| `board_id` | yes | Board whose Done cards should be archived. |
| `older_than_days` | no | Only archive Done cards older than this many days. |

---

### `kanban_release_stale_cards`

Operator bulk action to clear claims older than `older_than_minutes` so
abandoned agent work returns to the queue. Defaults to 30 minutes. Use with care:
a running agent may still be editing in its worktree. Returns `{released,
board_id}`.

| Arg | Required | Description |
|---|---|---|
| `board_id` | yes | Board whose stale claims should be released. |
| `older_than_minutes` | no | Claim age threshold in minutes. Default 30. |

---

## Project tools

Thin REST clients of the apiserver projects API (`/api/v1/projects/...`).
Reuses `KANBAN_API_URL` (default `http://localhost:9123`). File and tree
requests for projects on remote hosts are proxied transparently through the
apiserver to that host's deploy-agent.

### `project_list`

List registered projects (the dashboard's project registry). Returns
`Project[]` with `id`, `path`, `display_name`, `type`, `host`, `git_remote`.
Use a project's `id` with the `kanban_*` and other `project_*` tools.

No arguments.

---

### `project_get`

Get a single registered project by id. Returns the Project
(`id`, `path`, `display_name`, `type`, `host`, `git_remote`).

| Arg | Required | Description |
|---|---|---|
| `project_id` | yes | Project ID to fetch. |

---

### `project_hosts`

List the hosts a project can be registered on (the local host plus every
host with a deploy-agent, deduped by physical machine). Returns `string[]`.

No arguments.

---

### `project_register`

Register a folder as a project. `path` is an absolute host path; `host`
selects which machine it lives on (see `project_hosts`). Folder type and
git remote are auto-detected. Returns the created Project.

| Arg | Required | Description |
|---|---|---|
| `path` | yes | Absolute path on the target host (git repo, worktree, submodule, or folder). |
| `host` | no | Target host. Defaults to the apiserver's local host. |
| `display_name` | no | Human-readable name. Defaults to the folder basename. |

---

### `project_tree`

List a directory in a project (lazy, one level by default). Works across
hosts. Returns `FileNode[]` with `{name, path, is_dir, size}`.

| Arg | Required | Description |
|---|---|---|
| `project_id` | yes | Project to browse. |
| `path` | no | Path relative to the project root. Empty = root. |
| `depth` | no | Directory depth to return. Default 1, max 3. |

---

### `project_file`

Read a file from a project (up to 1 MiB; binary files returned base64).
Works across hosts. Returns `{path, contents, encoding, size, truncated}`.

| Arg | Required | Description |
|---|---|---|
| `project_id` | yes | Project the file belongs to. |
| `path` | yes | File path relative to the project root. |
