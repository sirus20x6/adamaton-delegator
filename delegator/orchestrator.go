package delegator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-core/executor/cli"
	"github.com/sirus20x6/adamaton-delegator/delegator/budget"
	"github.com/sirus20x6/adamaton-delegator/delegator/skillsclient"
)

// BudgetClient is the orchestrator's narrow view of the budget router. The
// concrete budget.Router and an HTTP client both satisfy this — Phase 1
// uses the in-process Router; Phase 2 may swap in HTTP if the MCP server
// becomes a thin shell talking to a separate budget-router process.
type BudgetClient interface {
	Route(req budget.RouteRequest) (*budget.RouteResponse, error)
	Report(req budget.ReportRequest) (*budget.ReportResponse, error)
}

// Store is the narrow interface the orchestrator needs to persist task
// state. The in-memory TaskStore is the default; the sqlite-backed
// SQLiteStore lets cmd/api read tasks the MCP server has produced.
type Store interface {
	Put(t *Task)
	Get(id string) (*Task, bool)
	Update(id string, fn func(*Task)) bool
	List(status TaskStatus, agent string) []*Task
}

// Orchestrator is the entry point for the MCP delegate_task tool. It picks
// an agent based on declared difficulty + priority, runs the CLI via
// internal/cli.CLIExecutor, and reports usage back to the budget
// tracker.
type Orchestrator struct {
	Budget          BudgetClient
	CLI             *cli.CLIExecutor
	Store           Store
	Logger          *logrus.Logger
	ProviderToAgent map[budget.Provider]string

	// Skills is the optional retrieval-augmented-execution hook. When
	// non-nil, Delegate() asks for top-K skills matching the prompt and
	// prepends them as a RELEVANT SKILLS block before dispatching to the
	// CLI. After the run completes, run() fires-and-forgets a
	// RecordUsage call per surfaced hit so the dashboard can show
	// "used N times". A nil Skills disables the integration entirely —
	// the orchestrator stays viable when the dashboard isn't reachable.
	Skills skillsclient.Client

	// SkillsTopK caps how many hits we inject. Defaults to 5. The
	// dashboard's /search endpoint already dedupes by skill_id, so this
	// is effectively a token-budget knob.
	SkillsTopK int

	// Events, when non-nil, receives the running CLI's stdout/stderr
	// chunks so a UI can tail a delegation live (PgTaskEvents publishes
	// them over Postgres LISTEN/NOTIFY). Nil disables streaming; the
	// delegation behaves exactly as before.
	Events TaskEvents

	// runningCancels lets cancel_task interrupt an in-flight delegation.
	runningCancels sync.Map // taskID → context.CancelFunc

	// inflight counts this orchestrator's outstanding delegations per
	// provider, so chooseAgent can hand the budget router live
	// ProviderLoad. Without it the router's load-spreading penalty and
	// MaxConcurrency hard-filter never fire — the field stays nil and the
	// single cheapest provider absorbs every concurrent background task.
	// Guarded by inflightMu.
	inflightMu sync.Mutex
	inflight   map[budget.Provider]int
}

// DefaultProviderToAgent is the canonical mapping. The MCP server uses it
// when constructing the orchestrator unless the operator overrides.
var DefaultProviderToAgent = map[budget.Provider]string{
	budget.ProviderOpenAI: "codex",
	budget.ProviderGemini: "gemini",
	budget.ProviderLocal:  "opencode",
}

// New returns an orchestrator wired with a default agent map and a fresh
// in-memory task store.
func New(b BudgetClient, cli *cli.CLIExecutor, logger *logrus.Logger) *Orchestrator {
	if logger == nil {
		logger = logrus.New()
	}
	return &Orchestrator{
		Budget:          b,
		CLI:             cli,
		Store:           NewTaskStore(0),
		Logger:          logger,
		ProviderToAgent: DefaultProviderToAgent,
	}
}

// Delegate routes a request, kicks off the CLI in a goroutine, and returns
// immediately with a task ID. Callers poll Status / Result to follow up.
// Mirrors the TS delegator's "fire and forget, return ID" semantics.
func (o *Orchestrator) Delegate(ctx context.Context, req DelegateRequest) (*Task, error) {
	if req.Prompt == "" {
		return nil, errors.New("prompt is required")
	}

	agent, provider, err := o.chooseAgent(req)
	if err != nil {
		return nil, err
	}

	priority := req.Priority
	if priority == "" {
		priority = budget.PriorityNormal
	}

	// Phase 5 skills enrichment. Best-effort: a failed skills search is
	// logged but never blocks the task. The original prompt is preserved
	// so the budget reporter still meters the user's intent, not the
	// augmented context.
	skillHits, augmented := o.enrichPromptWithSkills(ctx, req.Prompt)
	enrichedReq := req
	if augmented != "" {
		enrichedReq.Prompt = augmented
	}

	task := &Task{
		ID:         newTaskID(),
		Agent:      agent,
		Provider:   provider,
		Difficulty: req.Difficulty,
		Priority:   priority,
		Prompt:     req.Prompt,
		WorkingDir: req.WorkingDir,
		Model:      req.Model,
		Status:     StatusPending,
		CreatedAt:  time.Now().UTC(),
	}
	o.Store.Put(task)

	// Detach from caller's context — caller's RPC may complete (returning
	// task_id) before the CLI finishes. Plumb our own cancellable context
	// so cancel_task can interrupt.
	bgCtx, cancel := context.WithCancel(context.Background())
	o.runningCancels.Store(task.ID, cancel)
	// Count this delegation against its provider until run() completes, so
	// concurrent Delegate calls see it as load. Balanced by the decrement
	// deferred in run().
	o.incrInflight(provider)

	go o.run(bgCtx, task, enrichedReq, skillHits)

	return task, nil
}

// enrichPromptWithSkills queries the skills library and returns the
// augmented prompt plus the hits that fed it. On any failure (no
// client, no hits, upstream error) it returns ("", nil) and the
// caller falls back to the original prompt.
//
// SkillRAE branch: when the underlying client has SKILLS_RAE_URL set,
// call /v1/rae/compile to get the multi-level-graph compiled context
// (top-down community boost + bottom-up subunit projection + rescue
// attachment + budgeted markdown render). Falls back to the legacy
// flat FormatSkillsBlock path on any error so a wedged skills-rae
// can't gate task dispatch.
func (o *Orchestrator) enrichPromptWithSkills(ctx context.Context, prompt string) ([]skillsclient.Hit, string) {
	if o.Skills == nil {
		return nil, ""
	}
	topK := o.SkillsTopK
	if topK <= 0 {
		topK = 5
	}

	// SkillRAE path (preferred when configured).
	if rae, ok := o.Skills.(*skillsclient.HTTPClient); ok && rae.RAEEnabled() {
		raeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		compiled, err := rae.CompileContext(raeCtx, prompt, "", "", topK, 1500)
		cancel()
		if err == nil && compiled != nil && len(compiled.SelectedSkills) > 0 {
			hits := skillsclient.HitsFromCompiled(compiled)
			o.Logger.WithFields(map[string]interface{}{
				"skills":   len(hits),
				"rescue":   len(compiled.RescueAttached),
				"stage_ms": compiled.Diagnostics.StageMs,
			}).Debug("skills-rae compiled context")
			return hits, compiled.Context + prompt
		}
		if err != nil {
			o.Logger.WithError(err).Warn("skills-rae compile failed; falling back to flat search")
		}
	}

	// Legacy flat path. Short timeout so a wedged skills service can't
	// gate every task.
	searchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	hits, err := o.Skills.Search(searchCtx, prompt, topK)
	if err != nil {
		o.Logger.WithError(err).Debug("skills search failed; running task without enrichment")
		return nil, ""
	}
	if len(hits) == 0 {
		return nil, ""
	}
	block := skillsclient.FormatSkillsBlock(hits)
	return hits, block + prompt
}

// run is the goroutine body for a single delegation. “surfacedSkills“
// captures the skill hits injected into the prompt; usage records are
// fired-and-forgotten after the task completes.
func (o *Orchestrator) run(ctx context.Context, task *Task, req DelegateRequest, surfacedSkills []skillsclient.Hit) {
	defer o.runningCancels.Delete(task.ID)
	defer o.decrInflight(task.Provider)

	o.Store.Update(task.ID, func(t *Task) {
		t.Status = StatusRunning
		t.StartedAt = time.Now().UTC()
	})

	in := cli.CLIInput{
		Prompt:     req.Prompt,
		WorkingDir: req.WorkingDir,
		Model:      req.Model,
		Timeout:    req.TimeoutSecs,
	}
	// Tail the subprocess live when a publisher is wired. The chunks are
	// also still captured into the stored Output by the executor, so this
	// only adds the live feed — it doesn't replace the final record.
	if o.Events != nil {
		id := task.ID
		in.OnStdout = func(b []byte) { o.Events.Publish(id, "stdout", b) }
		in.OnStderr = func(b []byte) { o.Events.Publish(id, "stderr", b) }
	}
	out, err := o.CLI.RunAgent(ctx, task.Agent, in)
	endedAt := time.Now().UTC()

	o.Store.Update(task.ID, func(t *Task) {
		t.CompletedAt = endedAt
		switch {
		case err != nil:
			t.Status = StatusFailed
			t.Error = err.Error()
		case out.Raw.TimedOut:
			t.Status = StatusTimedOut
			t.Output = out.Parsed
			t.Stderr = out.Raw.Stderr
			t.ExitCode = out.Raw.ExitCode
			t.Error = "timed out"
		case ctx.Err() != nil:
			t.Status = StatusCancelled
			t.Output = out.Parsed
			t.Stderr = out.Raw.Stderr
			t.ExitCode = out.Raw.ExitCode
		case out.Raw.ExitCode != 0:
			t.Status = StatusFailed
			t.Output = out.Parsed
			t.Stderr = out.Raw.Stderr
			t.ExitCode = out.Raw.ExitCode
			t.Error = fmt.Sprintf("exit %d", out.Raw.ExitCode)
		default:
			t.Status = StatusCompleted
			t.Output = out.Parsed
			t.Stderr = out.Raw.Stderr
			t.ExitCode = out.Raw.ExitCode
		}
	})

	o.report(task, out)
	o.recordSkillUsages(task.ID, surfacedSkills)
}

// recordSkillUsages fires off one POST per surfaced hit. Errors are
// logged but otherwise dropped — the dashboard's usage tally is a
// best-effort signal, never load-bearing for the agent loop. Calls run
// in their own goroutine so the orchestrator's run() doesn't block on
// dashboard I/O after the task is already complete.
func (o *Orchestrator) recordSkillUsages(taskID string, hits []skillsclient.Hit) {
	if o.Skills == nil || len(hits) == 0 || taskID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var failed int
		for _, h := range hits {
			if h.SkillID == "" {
				continue
			}
			if err := o.Skills.RecordUsage(ctx, h.SkillID, taskID); err != nil {
				failed++
				o.Logger.WithError(err).WithFields(logrus.Fields{
					"task_id": taskID, "skill_id": h.SkillID,
				}).Debug("record skill usage failed")
			}
		}
		if failed > 0 {
			o.Logger.WithFields(logrus.Fields{
				"task_id": taskID, "failed": strconv.Itoa(failed) + "/" + strconv.Itoa(len(hits)),
			}).Debug("some skill-usage records failed")
		}
	}()
}

// report logs token usage to the budget tracker. Best-effort: a report
// failure is logged and swallowed so orchestrator latency can't blow up
// when the tracker is briefly unavailable.
func (o *Orchestrator) report(task *Task, out *cli.CLIOutput) {
	if o.Budget == nil || task.Provider == "" {
		return
	}
	tokens := estimateTokens(task.Prompt, out)
	if tokens <= 0 {
		return
	}
	final, ok := o.Store.Get(task.ID)
	if !ok {
		return
	}
	success := final.Status == StatusCompleted
	rep := budget.ReportRequest{
		Provider:       task.Provider,
		Model:          task.Model,
		TotalTokens:    tokens,
		TaskID:         task.ID,
		TaskComplexity: task.Difficulty.ToComplexity(),
		Success:        &success,
	}
	if _, err := o.Budget.Report(rep); err != nil {
		o.Logger.WithError(err).WithField("task_id", task.ID).Warn("budget report failed")
	}
}

// estimateTokens is a coarse approximation when the CLI doesn't emit a
// real token count: ~4 characters per token, summed across prompt and
// captured output.
func estimateTokens(prompt string, out *cli.CLIOutput) int {
	chars := len(prompt)
	if out != nil {
		chars += out.Raw.StdoutBytes
	}
	if chars <= 0 {
		return 0
	}
	return chars / 4
}

// DelegateSync runs a delegation and blocks until it reaches a terminal
// status, returning the final task. Used by the Temporal DelegationActivity
// — the workflow needs to know whether the underlying CLI succeeded, which
// the async Delegate alone can't tell it.
//
// Polls the store every 500ms. The CLI subprocess itself enforces its own
// timeout via subprocess.Options; this method only honours the caller's
// context for cancellation. A cancelled context propagates into Cancel()
// so the in-flight subprocess gets SIGTERM'd cleanly.
func (o *Orchestrator) DelegateSync(ctx context.Context, req DelegateRequest) (*Task, error) {
	task, err := o.Delegate(ctx, req)
	if err != nil {
		return nil, err
	}
	const pollInterval = 500 * time.Millisecond
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if t, ok := o.Store.Get(task.ID); ok && isTerminalStatus(t.Status) {
			return t, nil
		}
		select {
		case <-ctx.Done():
			o.Cancel(task.ID)
			// Give the goroutine one tick to record the cancelled status,
			// then return whatever's in the store.
			time.Sleep(pollInterval)
			if t, ok := o.Store.Get(task.ID); ok {
				return t, ctx.Err()
			}
			return task, ctx.Err()
		case <-ticker.C:
		}
	}
}

func isTerminalStatus(s TaskStatus) bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusTimedOut:
		return true
	}
	return false
}

// Cancel aborts an in-flight delegation. Returns true if the task was
// running and the cancel was delivered; false if it was already terminal
// or unknown.
func (o *Orchestrator) Cancel(taskID string) bool {
	v, ok := o.runningCancels.Load(taskID)
	if !ok {
		return false
	}
	cancel, _ := v.(context.CancelFunc)
	if cancel != nil {
		cancel()
	}
	o.Store.Update(taskID, func(t *Task) {
		// run() will set the final status; we just record intent here in
		// case the goroutine has already exited between Load and Update.
		if t.Status == StatusPending || t.Status == StatusRunning {
			t.Status = StatusCancelled
		}
	})
	return true
}

// incrInflight records one more outstanding delegation for provider.
// Empty providers (an agent-hint with no provider mapping) are ignored so
// the load map only ever holds real, budget-tracked providers.
func (o *Orchestrator) incrInflight(p budget.Provider) {
	if p == "" {
		return
	}
	o.inflightMu.Lock()
	if o.inflight == nil {
		o.inflight = make(map[budget.Provider]int)
	}
	o.inflight[p]++
	o.inflightMu.Unlock()
}

// decrInflight releases one outstanding delegation for provider, deleting
// the key at zero so inflightSnapshot stays sparse.
func (o *Orchestrator) decrInflight(p budget.Provider) {
	if p == "" {
		return
	}
	o.inflightMu.Lock()
	if n := o.inflight[p]; n <= 1 {
		delete(o.inflight, p)
	} else {
		o.inflight[p] = n - 1
	}
	o.inflightMu.Unlock()
}

// inflightSnapshot returns a copy of the current per-provider in-flight
// counts for a RouteRequest, or nil when nothing is outstanding (which
// preserves the router's pre-load behaviour).
func (o *Orchestrator) inflightSnapshot() map[budget.Provider]int {
	o.inflightMu.Lock()
	defer o.inflightMu.Unlock()
	if len(o.inflight) == 0 {
		return nil
	}
	cp := make(map[budget.Provider]int, len(o.inflight))
	for k, v := range o.inflight {
		cp[k] = v
	}
	return cp
}

// chooseAgent returns (agent, provider). When AgentHint is set we honour
// it directly and look up the associated provider via reverse mapping;
// otherwise the budget router decides.
func (o *Orchestrator) chooseAgent(req DelegateRequest) (string, budget.Provider, error) {
	if req.AgentHint != "" {
		if _, ok := o.CLI.Specs[req.AgentHint]; !ok {
			return "", "", fmt.Errorf("unknown agent hint %q", req.AgentHint)
		}
		// Reverse-lookup provider. If the agent is unmapped, route with
		// nil provider — usage just won't be reported.
		var provider budget.Provider
		for p, a := range o.ProviderToAgent {
			if a == req.AgentHint {
				provider = p
				break
			}
		}
		return req.AgentHint, provider, nil
	}

	if o.Budget == nil {
		return "", "", errors.New("no budget client and no agent hint")
	}

	complexity := req.Difficulty.ToComplexity()
	priority := req.Priority
	if priority == "" {
		priority = budget.PriorityNormal
	}

	resp, err := o.Budget.Route(budget.RouteRequest{
		TaskComplexity:  complexity,
		EstimatedTokens: estimatedPromptTokens(req.Prompt),
		Priority:        priority,
		// Live in-flight counts so the router can spread concurrent work
		// and honour per-provider MaxConcurrency caps. QueueDepth stays 0:
		// Delegate dispatches immediately, so there is no routing backlog.
		ProviderLoad: o.inflightSnapshot(),
	})
	if err != nil {
		return "", "", fmt.Errorf("route: %w", err)
	}

	agent, ok := o.ProviderToAgent[resp.Provider]
	if !ok {
		return "", "", fmt.Errorf("router returned provider %q with no agent mapping", resp.Provider)
	}
	if _, ok := o.CLI.Specs[agent]; !ok {
		return "", "", fmt.Errorf("router selected agent %q has no CLI spec", agent)
	}
	return agent, resp.Provider, nil
}

// estimatedPromptTokens floors the request size at a baseline so a
// "trivial" mis-classified prompt can't starve good agents — even a tiny
// prompt costs at least 200 tokens once the model's response is in.
func estimatedPromptTokens(prompt string) int {
	t := len(prompt) / 4
	if t < 200 {
		t = 200
	}
	return t
}

// newTaskID returns a 16-byte hex ID. crypto/rand is overkill for an
// in-memory dev tool but the cost is one syscall.
func newTaskID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Fall back to a timestamp-based ID if rand is unavailable.
		return fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	return "task-" + hex.EncodeToString(buf[:])
}
