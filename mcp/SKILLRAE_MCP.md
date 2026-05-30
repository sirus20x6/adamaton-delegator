# skillrae-mcp tool reference

`skillrae-mcp` is a stdio MCP server (`mcp/cmd/skillrae-mcp/main.go`) that
exposes the evo SkillRAE service to Claude Code. It registers two tools:
`find_skill` and `report_skill_feedback`.

The implementation follows SkillRAE (arxiv 2605.10114v1, Meng/Wang/Fang):
the online stage compiles a context block C(q) = Compile_B(q, K, H, A, O)
and returns it ready to prepend to an agent prompt.

## Runtime requirements

| Env var | Default | Purpose |
|---|---|---|
| `SKILLS_RAE_URL` | `http://localhost:7376` | skills-rae service endpoint (`/v1/rae/compile`). Required for `find_skill` to work; tools register regardless, but calls return an error until this is set. |
| `SKILLS_API_URL` | `http://localhost:9123` | Dashboard endpoint for `report_skill_feedback` (`/api/v1/skills/usages`). |
| `SKILLS_API_TOKEN` | _(none)_ | Optional `X-API-Key` forwarded to both endpoints. |
| `SKILLRAE_MIN_SCORE` | `0.2` | Minimum retrieval score to consider a skill relevant. Below this the tool returns `status="no_relevant_skills_found"`. |

---

## Retrieve / compile flow

When `find_skill` is called:

1. The caller provides `tech_stack` (languages/frameworks) and `task` (one paragraph describing the work). The MCP folds them into a single retrieval query: `"Tech stack: <stack>\n\nTask: <task>"`.
2. The query is sent to the skills-rae service via `POST /v1/rae/compile` with `top_k`, `budget_tokens`, and an optional `output_contract` (the paper's O(q)).
3. skills-rae runs the two-stage retrieval (BM25 bottom-up + community top-down with BGE-M3 embeddings), compiles the selected skill chunks into a markdown context block, and returns `CompiledContext`.
4. The MCP checks the top score against `SKILLRAE_MIN_SCORE`. If no skill clears the threshold it returns `status="no_relevant_skills_found"` so the caller skips the block rather than prepending noise.
5. On success it caches `(task_id → skill_ids)` in memory (TTL 1 hour) and returns `status="ok"` with the compiled `context` block.

The `context` field is the block the agent reads before starting work.
`selected_skills` carries per-skill metadata (`skill_id`, `name`, `score`,
`l1`, `l0`, `name_score`, `boosted`, `community_id`).

After the task is complete, call `report_skill_feedback` to close the loop.

---

## Tools

### `find_skill`

Fetch a SkillRAE-compiled context block for the task you are about to work
on.

Before calling, identify (a) the tech stack of the file or module you are
editing and (b) a tight one-paragraph statement of what you are trying to
accomplish. Pass both as `tech_stack` and `task`.

The tool returns one of two shapes:

- `status="ok"` — a compiled `context` markdown block is ready to read
  before starting work.
- `status="no_relevant_skills_found"` — the library has nothing relevant;
  proceed without it.

After the task is done, call `report_skill_feedback` with the returned
`task_id`.

| Arg | Required | Description |
|---|---|---|
| `task` | yes | One-paragraph description of the task. Subagent-prepared "Explore" output reads best here. |
| `tech_stack` | no | Languages, frameworks, and key libraries (e.g. `Go 1.25, pgxpool, gorilla/mux`). Recommended — folded into the retrieval query for better embedding signal. |
| `output_contract` | no | Shape constraint on the expected output (the paper's O(q)), e.g. `return a JSON object with fields X, Y, Z`. Rendered into the compiled block. |
| `budget_tokens` | no | Context budget for the compiled block. Default 1500. |
| `top_k` | no | Max skills the compiler may consider. Default 5. |

On `status="ok"` the response includes:

```
{
  "status": "ok",
  "task_id": "<uuid>",           // pass to report_skill_feedback
  "context": "<markdown block>", // prepend to the agent prompt
  "selected_skills": [...],      // per-skill metadata
  "rescue_attached": [...],      // rescue cues attached to selected skills
  "diagnostics": {
    "stage_ms": {...},
    "top_down_labels": [...],
    "bottom_up_hits": N,
    "l1_hits": N
  }
}
```

On `status="no_relevant_skills_found"`:

```
{
  "status": "no_relevant_skills_found",
  "task_id": "<uuid>",
  "top_score": 0.07,
  "min_score": 0.2,
  "diagnostics": {...}
}
```

---

### `report_skill_feedback`

Report whether the skill(s) surfaced by a prior `find_skill` call actually
helped. The library uses this signal to tune ranking and to surface "used N
times" stats in the dashboard.

Call this once after finishing the task. Skip the call when `find_skill`
returned `status="no_relevant_skills_found"`.

The MCP caches `(task_id → skill_ids)` in memory for 1 hour. If the
`task_id` is not found in the cache (stale MCP process, or Claude invented
an ID), pass `skill_ids` explicitly.

| Arg | Required | Description |
|---|---|---|
| `task_id` | yes | The `task_id` returned by `find_skill`. |
| `was_helpful` | yes | `true` if the skill helped; `false` if it did not. |
| `skill_ids` | no | Specific skills to report on. Defaults to all skills from the `find_skill` call. |

Feedback is written to `budget.usage_records` via `POST /api/v1/skills/usages`
with `was_helpful` as a tri-state (`null` / `true` / `false`). The call
iterates all skill IDs; partial success is reported as `recorded < requested`.

Returns:

```
{
  "status": "ok",
  "task_id": "<uuid>",
  "recorded": 3,
  "requested": 3,
  "was_helpful": true
}
```

---

## Relationship to delegator-mcp skills RAG

`delegator-mcp` also does skills retrieval, but inline during
`delegate_task` dispatch — it calls `skillsclient.HTTPClient.CompileContext`
(the same `POST /v1/rae/compile` endpoint) and prepends the result to the
agent prompt automatically when `SKILLS_API_URL` is set. That path is
transparent to the caller; no MCP tool call is needed.

`skillrae-mcp` is the explicit surface. Use it when Claude Code itself (not
a delegated sub-agent) wants to look up skills before starting a complex
task, or when you want to close the feedback loop with `report_skill_feedback`
after work is done.
