// Command delegator-mcp is the Go MCP server that absorbs the TS delegator.
// It exposes delegate_task / get_task_* / cancel_task / list_tasks over
// stdio, with budget-router-driven agent selection. Phase 1 runs the CLI
// directly in-process — Phase 2 will wrap each delegation in a Temporal
// workflow.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/sirus20x6/adamaton-platform/temporal/activities"
	"github.com/sirus20x6/adamaton-delegator/delegator/budget"
	"github.com/sirus20x6/adamaton-delegator/delegator"
	"github.com/sirus20x6/adamaton-delegator/delegator/skillsclient"
	"github.com/sirus20x6/adamaton-core/executor/cli"
	"github.com/sirus20x6/adamaton-delegator/delegator/quota"
	"github.com/sirus20x6/adamaton-platform/temporal/workflows"
)

// maxPromptBytes caps prompt size accepted by delegate_task /
// schedule_recurring_task. 1 MiB is far beyond any legitimate
// agent prompt and prevents a misbehaving client from pushing
// the orchestrator into a multi-GB memory state.
const maxPromptBytes = 1024 * 1024

// maxTimeoutSeconds caps the per-task wall-clock budget. 30 minutes
// is more than enough for any in-process CLI run; anything longer
// should be a workflow, not a single delegate call.
const maxTimeoutSeconds = 1800

// defaultTimeoutSeconds is applied when the caller doesn't supply
// a positive timeout_seconds value.
const defaultTimeoutSeconds = 300

// temporalDialTimeout caps the lazy gate's first-call latency.
// The Temporal SDK ignores grpc.WithTimeout / WithBlock, so this
// is enforced by a watchdog goroutine in ensureUp().
const temporalDialTimeout = 5 * time.Second

// validateWorkingDir cleans and verifies a caller-supplied working
// directory. Empty input returns nil (use operator default). Otherwise:
//
//  1. filepath.Clean normalises ".."/"." segments.
//  2. The cleaned path must be absolute.
//  3. The path must live under DELEGATOR_WORKING_DIR_ROOT (default
//     "/thearray"). This blocks the obvious path-traversal vector
//     of "/etc/..." or "../../etc/...".
//  4. filepath.EvalSymlinks resolves the path and the prefix is
//     re-checked, so a symlink inside the allowlist that points
//     outside cannot smuggle the working dir past the gate.
//
// The function is a no-op when the path doesn't exist on disk yet
// only for the EvalSymlinks step — the prefix check is still
// enforced on the cleaned absolute path so non-existent escape
// attempts are rejected.
func validateWorkingDir(dir string) error {
	if dir == "" {
		return nil
	}
	cleaned := filepath.Clean(dir)
	if !filepath.IsAbs(cleaned) {
		return fmt.Errorf("working_dir must be an absolute path after cleaning, got %q", dir)
	}
	root := os.Getenv("DELEGATOR_WORKING_DIR_ROOT")
	if root == "" {
		root = "/thearray"
	}
	root = filepath.Clean(root)
	if !pathHasPrefix(cleaned, root) {
		return fmt.Errorf("working_dir %q is outside allowlist root %q", cleaned, root)
	}
	// EvalSymlinks fails on non-existent paths; treat that as a
	// post-fix-cleaned prefix check pass since exec.Command.Dir
	// will fail loudly anyway if the directory is missing.
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("resolve symlinks in working_dir: %w", err)
	}
	if !pathHasPrefix(resolved, root) {
		return fmt.Errorf("working_dir %q resolves to %q which escapes allowlist root %q", cleaned, resolved, root)
	}
	return nil
}

// pathHasPrefix is filepath.HasPrefix-style but path-segment-aware,
// so "/thearrayother" doesn't satisfy a "/thearray" prefix.
func pathHasPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return path[len(prefix)] == filepath.Separator
}

// clampTimeout normalises caller-supplied timeout_seconds: zero or
// negative falls back to defaultTimeoutSeconds, anything above the
// cap is reduced to maxTimeoutSeconds.
func clampTimeout(secs int) int {
	if secs <= 0 {
		return defaultTimeoutSeconds
	}
	if secs > maxTimeoutSeconds {
		return maxTimeoutSeconds
	}
	return secs
}

// clampTimeoutCapOnly enforces the upper bound but preserves zero
// so downstream callers that have their own default can keep it.
// Used by execute()'s 60s default in contextmode.
func clampTimeoutCapOnly(secs int) int {
	if secs <= 0 {
		return 0
	}
	if secs > maxTimeoutSeconds {
		return maxTimeoutSeconds
	}
	return secs
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "delegator-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logger := logrus.New()
	// Logs go to STDERR — the MCP server uses stdout for the JSON-RPC
	// transport. Anything we write to stdout that isn't a frame breaks
	// the transport for the client.
	logger.SetOutput(os.Stderr)
	logger.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if level, err := logrus.ParseLevel(cfg.LogLevel); err == nil {
		logger.SetLevel(level)
	}

	if cfg.DSN == "" {
		return fmt.Errorf("budget_router.dsn (or POSTGRES_DSN) is required")
	}

	store, err := budget.NewStore(cfg.DSN, logger)
	if err != nil {
		return fmt.Errorf("open budget store: %w", err)
	}
	defer store.Close()

	tracker, err := budget.NewTracker(store, cfg, logger)
	if err != nil {
		return fmt.Errorf("create tracker: %w", err)
	}
	defer tracker.Stop()

	providerConfigs := make(map[budget.Provider]*budget.ProviderConfig, len(cfg.Providers))
	for i := range cfg.Providers {
		pc := &cfg.Providers[i]
		providerConfigs[pc.Provider] = pc
	}
	router := budget.NewRouter(tracker, providerConfigs, logger)
	bc := &localBudget{router: router, tracker: tracker}

	cliExec := cli.NewDefaultCLIExecutor()

	// Boot the long-lived `opencode serve` process so delegations to
	// opencode skip the ~3s Node cold-start tax. Boot runs in a
	// goroutine so the MCP transport comes up immediately — Claude
	// Code's `initialize` handshake doesn't have to wait. Calls that
	// land during the ~10s boot window fall back to CLI spawn
	// transparently (CLIExecutor.RunAgent checks Ready() first).
	if os.Getenv("GOGENTS_DISABLE_OPENCODE_SERVE") == "" {
		ocServe := cli.NewOpencodeServer(cli.OpencodeServerOpts{
			Provider: opencodeProviderHint(),
			Model:    opencodeModelHint(),
			Logger:   logger,
		})
		cliExec.OpencodeServe = ocServe
		defer ocServe.Stop()
		go func() {
			bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer bootCancel()
			if err := ocServe.Start(bootCtx, 20*time.Second); err != nil {
				logger.WithError(err).Warn("opencode serve failed to boot; delegations will fall back to CLI spawn")
			} else {
				logger.Info("opencode serve ready; opencode delegations will take the fast path")
			}
		}()
	}

	orch := delegator.New(bc, cliExec, logger)

	// Skills RAG: when the dashboard is reachable, every delegate_task
	// asks /api/v1/skills/search for top-K relevant skills and prepends
	// them as context. Set SKILLS_API_URL="" (literal empty string) to
	// disable. Default is http://localhost:9123.
	if v, ok := os.LookupEnv("SKILLS_API_URL"); !ok || v != "" {
		orch.Skills = skillsclient.New()
		orch.SkillsTopK = 5
		logger.WithField("base_url", skillsBaseURLFor(logger)).Info("skills RAG enabled")
	} else {
		logger.Info("SKILLS_API_URL is empty; skills RAG disabled")
	}

	// Replace the in-memory task store with postgres so cmd/api can read
	// the same task list and surface it in the gogents UI. Both sides
	// open their own pool against the same DSN; postgres MVCC handles
	// the writer/reader concurrency the sqlite open-per-request dance
	// used to avoid.
	tasksStore, err := delegator.NewPgStore(cfg.DSN, 0, logger)
	if err != nil {
		return fmt.Errorf("open tasks store: %w", err)
	}
	defer tasksStore.Close()
	orch.Store = tasksStore

	// Kanban stale-claim sweep. Reuses the tasks-store pool (same evo-schema
	// DSN) to periodically flip crashed-worker card claims back to unclaimed
	// — the crash-recovery mechanism for the kanban orchestration model
	// (docs/PROJECTS_KANBAN.md decision 3). Best-effort; tied to the server
	// lifetime via sweepCtx so it stops cleanly on shutdown. Set
	// KANBAN_STALE_SWEEP=off to disable.
	sweepCtx, sweepCancel := context.WithCancel(context.Background())
	defer sweepCancel()
	if os.Getenv("KANBAN_STALE_SWEEP") != "off" {
		delegator.StartKanbanSweeper(sweepCtx, tasksStore.Pool(),
			delegator.DefaultStaleClaimTTL, delegator.DefaultStaleClaimSweepInterval, logger)
	} else {
		logger.Info("KANBAN_STALE_SWEEP=off; kanban stale-claim sweep disabled")
	}

	// Lazy Temporal gate — dials + spins up the worker on first schedule
	// tool call, not at startup. This means starting Temporal AFTER
	// delegator-mcp doesn't require a Claude Code restart; the next
	// schedule_recurring_task call brings everything online.
	gate := newTemporalGate(orch, logger)
	defer gate.close()

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "delegator",
		Version: "0.5.0",
	}, nil)

	registerTools(server, orch, gate, cfg.DSN, logger)

	logger.Info("delegator-mcp ready (stdio)")
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp run: %w", err)
	}
	return nil
}

// skillsBaseURLFor returns the SKILLS_API_URL value the integration
// will use, with the same default the client falls back to. Used only
// for the startup log line.
func skillsBaseURLFor(_ *logrus.Logger) string {
	if v := os.Getenv("SKILLS_API_URL"); v != "" {
		return v
	}
	return "http://localhost:9123"
}

// delegationTaskQueue is the Temporal task queue for recurring delegations.
// Multiple delegator-mcp processes (one per Claude Code session) all listen
// on this queue — Temporal load-balances fired schedules across whoever's
// available, so as long as one delegator-mcp is running, recurring tasks
// fire.
const delegationTaskQueue = "delegator-recurring"

// temporalGate lazily dials Temporal on the first schedule tool call.
// Avoids the chicken-and-egg of "delegator-mcp must restart whenever
// Temporal restarts" — the connection (and the worker that handles
// scheduled-fire workflows) come up on demand and stay up.
type temporalGate struct {
	addr   string
	orch   *delegator.Orchestrator
	logger *logrus.Logger

	mu     sync.Mutex
	client client.Client
	worker worker.Worker
	sched  *delegator.Scheduler
}

func newTemporalGate(orch *delegator.Orchestrator, logger *logrus.Logger) *temporalGate {
	addr := os.Getenv("TEMPORAL_ADDRESS")
	if addr == "" {
		addr = "127.0.0.1:7233"
	}
	return &temporalGate{addr: addr, orch: orch, logger: logger}
}

// ensureUp dials Temporal (if not already), registers the workflow + activity,
// starts the worker, and returns a Scheduler. Subsequent calls return the
// cached scheduler. On dial failure the gate stays uninitialised so the
// next tool call can retry.
func (g *temporalGate) ensureUp() (*delegator.Scheduler, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sched != nil {
		return g.sched, nil
	}
	// The Temporal SDK ignores grpc dial timeouts (see Options.DialOptions
	// docs), so wrap Dial in a watchdog. Without this, a stopped Temporal
	// server can hang the MCP tool call for grpc.NewClient's full backoff
	// budget — much longer than the operator-friendly "down right now,
	// retry once it's back" message we want.
	type dialResult struct {
		c   client.Client
		err error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		c, err := client.Dial(client.Options{HostPort: g.addr})
		resultCh <- dialResult{c: c, err: err}
	}()
	var c client.Client
	select {
	case r := <-resultCh:
		if r.err != nil {
			return nil, fmt.Errorf("dial temporal at %s: %w", g.addr, r.err)
		}
		c = r.c
	case <-time.After(temporalDialTimeout):
		// The goroutine keeps running and will close the client if/when
		// the dial finally succeeds; that's the cheapest way to avoid
		// leaking the connection while still returning quickly.
		go func() {
			if r := <-resultCh; r.err == nil && r.c != nil {
				r.c.Close()
			}
		}()
		return nil, fmt.Errorf("dial temporal at %s timed out after %s", g.addr, temporalDialTimeout)
	}
	w := worker.New(c, delegationTaskQueue, worker.Options{})
	w.RegisterWorkflow(workflows.DelegationWorkflow)
	w.RegisterActivity(&activities.DelegationActivities{
		Orchestrator: delegator.NewActivityAdapter(g.orch),
	})
	if err := w.Start(); err != nil {
		c.Close()
		return nil, fmt.Errorf("start worker: %w", err)
	}
	g.client = c
	g.worker = w
	g.sched = delegator.NewScheduler(c, delegationTaskQueue, "DelegationWorkflow", g.logger)
	g.logger.WithField("addr", g.addr).Info("Temporal wired on first schedule tool call")
	return g.sched, nil
}

// close releases worker + client if they were ever brought up.
func (g *temporalGate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.worker != nil {
		g.worker.Stop()
		g.worker = nil
	}
	if g.client != nil {
		g.client.Close()
		g.client = nil
	}
	g.sched = nil
}

// localBudget adapts budget.Router + budget.Tracker into the orchestrator's
// BudgetClient interface. Routing reads through Router; reporting writes
// through Tracker. No HTTP hop — this is a single-binary MCP server.
type localBudget struct {
	router  *budget.Router
	tracker *budget.Tracker
}

func (b *localBudget) Route(req budget.RouteRequest) (*budget.RouteResponse, error) {
	return b.router.Route(req)
}

func (b *localBudget) Report(req budget.ReportRequest) (*budget.ReportResponse, error) {
	return b.tracker.Report(req)
}

// --- tool argument types ---

type delegateArgs struct {
	Prompt     string `json:"prompt" jsonschema:"the task instructions for the agent (required)"`
	Difficulty string `json:"difficulty,omitempty" jsonschema:"estimated task difficulty before calling: trivial=one-line edit, easy=small change, medium=multi-line change with light reasoning, hard=multi-file change with reasoning, expert=multi-file refactor or systems design. Routing uses this to pick a provider whose capability matches."`
	Priority   string `json:"priority,omitempty" jsonschema:"how time-sensitive: immediate (we'll pay for speed), normal (default), background (cheap > fast)"`
	Agent      string `json:"agent,omitempty" jsonschema:"optional: skip the budget router and use a specific agent (codex, gemini, opencode)"`
	WorkingDir string `json:"working_dir,omitempty" jsonschema:"working directory for the spawned CLI (defaults to caller's cwd)"`
	Model      string `json:"model,omitempty" jsonschema:"optional model override; honoured by codex/gemini/opencode if their CLI accepts -m"`
	Timeout    int    `json:"timeout_seconds,omitempty" jsonschema:"timeout in seconds (default 300)"`
}

type taskIDArg struct {
	TaskID string `json:"task_id" jsonschema:"the task ID returned from delegate_task"`
}

type listArgs struct {
	StatusFilter string `json:"status_filter,omitempty" jsonschema:"only show tasks in this status: pending, running, completed, failed, cancelled, timed_out"`
	Agent        string `json:"agent,omitempty" jsonschema:"only show tasks routed to this agent"`
}

type quotaArgs struct {
	Agent string `json:"agent,omitempty" jsonschema:"filter by agent name (claude, codex, gemini, opencode). Omit for all."`
	Days  int    `json:"days,omitempty" jsonschema:"how many days back to aggregate (default 1)"`
}

type geminiRateLimitsArgs struct {
	UtilizationDaily *float64                                  `json:"utilization_daily,omitempty" jsonschema:"daily utilization as a decimal (0.11 = 11%)"`
	ResetDaily       string                                    `json:"reset_daily,omitempty" jsonschema:"daily reset time as ISO string"`
	Models           map[string]quota.GeminiRateLimitModelInfo `json:"models,omitempty" jsonschema:"per-model usage breakdown"`
}

// --- tool registrations ---

// scheduleArgs is the union of fields delegate_task supports plus the
// recurring-only cron expression. Same naming so models that already
// know delegate_task can fill in schedule_recurring_task naturally.
type scheduleCreateArgs struct {
	ID         string `json:"id" jsonschema:"unique schedule ID. Use a slug like 'nightly-test-suite'."`
	Cron       string `json:"cron" jsonschema:"5-field cron expression in operator timezone, e.g. '*/5 * * * *' or '0 9 * * 1-5'"`
	Note       string `json:"note,omitempty" jsonschema:"free-text label, surfaced in list_recurring_schedules"`
	Prompt     string `json:"prompt" jsonschema:"the task instructions for the agent"`
	Difficulty string `json:"difficulty,omitempty" jsonschema:"trivial|easy|medium|hard|expert"`
	Priority   string `json:"priority,omitempty" jsonschema:"immediate|normal|background"`
	Agent      string `json:"agent,omitempty" jsonschema:"optional: pin a specific agent (codex|gemini|opencode)"`
	WorkingDir string `json:"working_dir,omitempty" jsonschema:"working directory for the spawned CLI"`
	Model      string `json:"model,omitempty" jsonschema:"optional model override"`
	Timeout    int    `json:"timeout_seconds,omitempty" jsonschema:"timeout in seconds (default 300)"`
}

type scheduleIDArgs struct {
	ID string `json:"id" jsonschema:"the schedule ID"`
}

type listSchedulesArgs struct{}

func registerTools(server *mcp.Server, orch *delegator.Orchestrator, gate *temporalGate, dsn string, logger *logrus.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "delegate_task",
		Description: "Hand a coding task off to another AI agent. Returns immediately with a task_id; poll with get_task_status. Before calling, estimate difficulty (trivial/easy/medium/hard/expert) and priority (immediate/normal/background) — the budget router uses both to pick the cheapest available agent that can handle the work.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args delegateArgs) (*mcp.CallToolResult, any, error) {
		if len(args.Prompt) > maxPromptBytes {
			return errResult(fmt.Sprintf("prompt exceeds %d byte limit (got %d)", maxPromptBytes, len(args.Prompt))), nil, nil
		}
		if err := validateWorkingDir(args.WorkingDir); err != nil {
			return errResult(err.Error()), nil, nil
		}
		req := delegator.DelegateRequest{
			Prompt:      args.Prompt,
			Difficulty:  delegator.Difficulty(strings.ToLower(args.Difficulty)),
			Priority:    budget.Priority(strings.ToLower(args.Priority)),
			AgentHint:   strings.ToLower(args.Agent),
			WorkingDir:  args.WorkingDir,
			Model:       args.Model,
			TimeoutSecs: clampTimeout(args.Timeout),
		}
		if req.Difficulty != "" && !delegator.ValidDifficulties[req.Difficulty] {
			return errResult(fmt.Sprintf("invalid difficulty %q", args.Difficulty)), nil, nil
		}
		if req.Priority != "" && !budget.ValidPriorities[req.Priority] {
			return errResult(fmt.Sprintf("invalid priority %q", args.Priority)), nil, nil
		}

		task, err := orch.Delegate(context.Background(), req)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(map[string]any{
			"task_id":  task.ID,
			"agent":    task.Agent,
			"provider": task.Provider,
			"status":   string(task.Status),
		}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_task_status",
		Description: "Get the current status of a delegated task without the full output.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args taskIDArg) (*mcp.CallToolResult, any, error) {
		task, ok := orch.Store.Get(args.TaskID)
		if !ok {
			return errResult("task not found"), nil, nil
		}
		var elapsed int
		if !task.StartedAt.IsZero() {
			until := time.Now().UTC()
			if !task.CompletedAt.IsZero() {
				until = task.CompletedAt
			}
			elapsed = int(until.Sub(task.StartedAt).Seconds())
		}
		return jsonResult(map[string]any{
			"task_id":         task.ID,
			"agent":           task.Agent,
			"provider":        task.Provider,
			"status":          string(task.Status),
			"elapsed_seconds": elapsed,
			"output_preview":  truncate(task.Output, 500),
			"stderr_preview":  truncate(task.Stderr, 200),
		}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_task_result",
		Description: "Get the full result of a completed task, including the parsed output and exit code.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args taskIDArg) (*mcp.CallToolResult, any, error) {
		task, ok := orch.Store.Get(args.TaskID)
		if !ok {
			return errResult("task not found"), nil, nil
		}
		out := map[string]any{
			"task_id":   task.ID,
			"agent":     task.Agent,
			"provider":  task.Provider,
			"status":    string(task.Status),
			"exit_code": task.ExitCode,
			"output":    task.Output,
		}
		if task.Status == delegator.StatusFailed || task.Status == delegator.StatusTimedOut {
			out["error"] = task.Error
			out["stderr"] = truncate(task.Stderr, 1000)
		}
		return jsonResult(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_task",
		Description: "Cancel a running task. Sends SIGTERM-then-SIGKILL to the underlying CLI.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args taskIDArg) (*mcp.CallToolResult, any, error) {
		ok := orch.Cancel(args.TaskID)
		status := "cancelled"
		if !ok {
			status = "not_found_or_not_running"
		}
		return jsonResult(map[string]any{
			"task_id": args.TaskID,
			"status":  status,
		}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_quota_usage",
		Description: "Get current quota utilization for all agents, or filter to one. Reads session JSONL files and CCSAVER rate-limit headers — values reflect actual usage, not the budget router's bookkeeping.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args quotaArgs) (*mcp.CallToolResult, any, error) {
		days := args.Days
		if days <= 0 {
			days = 1
		}
		usage, err := quota.GetAllAgentUsage(ctx, days, quota.AggregateConfig{Logger: logger})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		if args.Agent != "" {
			filtered := usage[:0]
			for _, u := range usage {
				if strings.EqualFold(u.Agent, args.Agent) {
					filtered = append(filtered, u)
				}
			}
			usage = filtered
		}
		return jsonResult(map[string]any{"agents": usage}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "report_gemini_usage",
		Description: "Manually report Gemini's daily usage limits (run after checking /stats in Gemini CLI). The values are cached on disk so subsequent get_quota_usage calls reflect them.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args geminiRateLimitsArgs) (*mcp.CallToolResult, any, error) {
		report := quota.GeminiRateLimitsReport{
			UtilizationDaily: args.UtilizationDaily,
			ResetTimeDaily:   args.ResetDaily,
			Models:           args.Models,
		}
		if err := quota.ReportGeminiRateLimits(report, quota.GeminiConfig{Logger: logger}); err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(map[string]string{"status": "ok"}), nil, nil
	})

	// Recurring schedules — always register so the tool surface is
	// stable. The gate lazy-dials Temporal on first call so a delegator-
	// mcp launched before Temporal can still serve schedules once
	// Temporal comes up; no Claude Code restart needed.
	registerScheduleTools(server, gate, logger)

	// Context-mode tools: execute / fetch_and_index / search. Go runs
	// the script or fetch directly, big outputs go into the pg_search
	// (Tantivy-backed BM25) index and the model gets cropped exact
	// bytes back. Opencode/qwen is a last-resort compressor when
	// search returns nothing useful.
	registerContextTools(server, orch, dsn, logger)

	// Kanban tools: thin REST clients of the apiserver kanban API
	// (board/column/card store shared with the dashboard). Base URL
	// from KANBAN_API_URL (default http://localhost:9123).
	registerKanbanTools(server, logger)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_tasks",
		Description: "List recently delegated tasks, optionally filtered by status or agent.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args listArgs) (*mcp.CallToolResult, any, error) {
		tasks := orch.Store.List(delegator.TaskStatus(strings.ToLower(args.StatusFilter)), strings.ToLower(args.Agent))
		summary := make([]map[string]any, 0, len(tasks))
		for _, t := range tasks {
			elapsed := 0
			if !t.StartedAt.IsZero() {
				until := time.Now().UTC()
				if !t.CompletedAt.IsZero() {
					until = t.CompletedAt
				}
				elapsed = int(until.Sub(t.StartedAt).Seconds())
			}
			summary = append(summary, map[string]any{
				"task_id":         t.ID,
				"agent":           t.Agent,
				"provider":        t.Provider,
				"status":          string(t.Status),
				"prompt_preview":  truncate(t.Prompt, 100),
				"elapsed_seconds": elapsed,
				"exit_code":       t.ExitCode,
			})
		}
		return jsonResult(summary), nil, nil
	})
}

// temporalUnreachable wraps a gate dial error into the user-facing tool
// response. Keeps the gate's internal error visible (which port, which
// underlying gRPC failure) instead of swallowing it.
func temporalUnreachable(err error) string {
	return fmt.Sprintf("Temporal is not reachable: %v. Start it with `temporal server start-dev` (or set TEMPORAL_ADDRESS).", err)
}

func registerScheduleTools(server *mcp.Server, gate *temporalGate, logger *logrus.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "schedule_recurring_task",
		Description: "Create a recurring delegation that fires on a cron schedule. Each fire spawns a fresh delegate_task-equivalent run. Use when the user asks to repeat a task on a cadence ('every 5 minutes', 'weekdays at 9am').",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args scheduleCreateArgs) (*mcp.CallToolResult, any, error) {
		if len(args.Prompt) > maxPromptBytes {
			return errResult(fmt.Sprintf("prompt exceeds %d byte limit (got %d)", maxPromptBytes, len(args.Prompt))), nil, nil
		}
		if err := validateWorkingDir(args.WorkingDir); err != nil {
			return errResult(err.Error()), nil, nil
		}
		sched, err := gate.ensureUp()
		if err != nil {
			return errResult(temporalUnreachable(err)), nil, nil
		}
		spec := delegator.ScheduleSpec{
			ID:          args.ID,
			Cron:        args.Cron,
			Note:        args.Note,
			Prompt:      args.Prompt,
			Difficulty:  delegator.Difficulty(strings.ToLower(args.Difficulty)),
			Priority:    budget.Priority(strings.ToLower(args.Priority)),
			AgentHint:   strings.ToLower(args.Agent),
			WorkingDir:  args.WorkingDir,
			Model:       args.Model,
			TimeoutSecs: clampTimeout(args.Timeout),
		}
		if spec.Difficulty != "" && !delegator.ValidDifficulties[spec.Difficulty] {
			return errResult(fmt.Sprintf("invalid difficulty %q", args.Difficulty)), nil, nil
		}
		if spec.Priority != "" && !budget.ValidPriorities[spec.Priority] {
			return errResult(fmt.Sprintf("invalid priority %q", args.Priority)), nil, nil
		}
		summary, scErr := sched.Create(ctx, spec)
		if scErr != nil {
			return errResult(scErr.Error()), nil, nil
		}
		return jsonResult(summary), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_recurring_schedules",
		Description: "List all active recurring delegation schedules.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listSchedulesArgs) (*mcp.CallToolResult, any, error) {
		sched, err := gate.ensureUp()
		if err != nil {
			return errResult(temporalUnreachable(err)), nil, nil
		}
		summaries, lErr := sched.List(ctx)
		if lErr != nil {
			return errResult(lErr.Error()), nil, nil
		}
		return jsonResult(summaries), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_recurring_schedule",
		Description: "Delete a recurring delegation schedule. Idempotent — deleting a missing schedule returns ok.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args scheduleIDArgs) (*mcp.CallToolResult, any, error) {
		sched, err := gate.ensureUp()
		if err != nil {
			return errResult(temporalUnreachable(err)), nil, nil
		}
		if err := sched.Cancel(ctx, args.ID); err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(map[string]string{"id": args.ID, "status": "cancelled"}), nil, nil
	})

	_ = logger
}

// --- helpers ---

func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: jsonString(map[string]any{"error": msg})}},
	}
}

func jsonResult(v any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: jsonString(v)}},
	}
}

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"error":"marshal: %v"}`, err)
	}
	return string(b)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// --- config (mirrors cmd/budget-router) ---

func loadConfig() (*budget.ServiceConfig, error) {
	v := viper.New()
	v.SetConfigName("budget")
	v.SetConfigType("yaml")
	// Search paths cover both repo-local development (cwd-relative) and
	// MCP-launched-from-arbitrary-cwd. The hardcoded gogents path is a
	// pragmatic fallback for the single-user dev tool — change via
	// GOGENTS_DELEGATOR_CONFIG_DIR if the repo lives elsewhere.
	v.AddConfigPath("configs/")
	v.AddConfigPath(".")
	if extra := os.Getenv("GOGENTS_DELEGATOR_CONFIG_DIR"); extra != "" {
		v.AddConfigPath(extra)
	}
	v.AddConfigPath("/thearray/gogents/configs")
	v.SetEnvPrefix("BUDGET")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	v.SetDefault("budget_router.listen_addr", "127.0.0.1:8070")
	v.SetDefault("budget_router.dsn", os.Getenv("POSTGRES_DSN"))
	v.SetDefault("budget_router.daily_reset_hour", 0)
	v.SetDefault("budget_router.weekly_reset_day", 1)
	v.SetDefault("budget_router.timezone", "UTC")
	v.SetDefault("budget_router.log_level", "info")
	v.SetDefault("budget_router.api_token", "")

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		// Use errors.As-style check; viper still returns this concrete type.
		if !asConfigNotFound(err, &notFound) {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	var wrapper struct {
		BudgetRouter budget.ServiceConfig `mapstructure:"budget_router"`
	}
	if err := v.Unmarshal(&wrapper); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	cfg := &wrapper.BudgetRouter

	if len(cfg.Providers) == 0 {
		cfg.Providers = defaultProviders()
	}
	return cfg, nil
}

// opencodeProviderHint and opencodeModelHint resolve the {provider, model}
// pair the serve API needs by re-using the same probes that the CLI
// path uses (auto-discovered local model + opencode-config provider key).
// Empty returns let the server skip without erroring; the CLI fallback
// will do its own probing on every spawn.
func opencodeProviderHint() string {
	return cli.FindOpencodeProvider(opencodeProbeURL())
}

func opencodeModelHint() string {
	models, err := cli.ProbeLocalModels(context.Background(), opencodeProbeURL())
	if err != nil || len(models) == 0 {
		return ""
	}
	return models[0]
}

// opencodeProbeURL is the vLLM /v1/models endpoint we probe both for
// model discovery (here) and for opencode provider matching. The MCP
// host typically sets GEMINI_API_BASE for the proxy; that variable is
// the right starting point for finding the local model server.
func opencodeProbeURL() string {
	if env := os.Getenv("GEMINI_API_BASE"); env != "" {
		return strings.TrimRight(env, "/") + "/vllm/v1/models"
	}
	return "http://127.0.0.1:19456/vllm/v1/models"
}

func asConfigNotFound(err error, target *viper.ConfigFileNotFoundError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(viper.ConfigFileNotFoundError); ok {
		*target = e
		return true
	}
	return false
}

func defaultProviders() []budget.ProviderConfig {
	return []budget.ProviderConfig{
		{
			Provider:      budget.ProviderOpenAI,
			Tier:          budget.TierCloud,
			Strength:      0.85,
			DailyLimit:    300000,
			WeeklyLimit:   1500000,
			DefaultModel:  "gpt-4o",
			Models:        map[string]string{"critical": "gpt-4o", "high": "gpt-4o", "medium": "gpt-4o-mini", "low": "gpt-4o-mini"},
			CostPerMToken: 2.50,
		},
		{
			Provider:      budget.ProviderGemini,
			Tier:          budget.TierCloud,
			Strength:      0.80,
			DailyLimit:    500000,
			WeeklyLimit:   2000000,
			DefaultModel:  "gemini-2.5-flash",
			Models:        map[string]string{"critical": "gemini-2.5-pro", "high": "gemini-2.5-flash", "medium": "gemini-2.5-flash", "low": "gemini-2.5-flash-lite"},
			CostPerMToken: 1.25,
		},
		{
			Provider:      budget.ProviderLocal,
			Tier:          budget.TierLocal,
			Strength:      0.45,
			DailyLimit:    0,
			WeeklyLimit:   0,
			DefaultModel:  "qwen-coder",
			CostPerMToken: 0.0,
		},
	}
}
