// Command skillrae-mcp exposes the evo SkillRAE service to Claude Code
// over MCP/stdio. Two tools:
//
//   - find_skill: Claude prepares a tech-stack + task description
//     (paper's q + the embedding signal the retriever wants), and gets
//     back a compiled context block C(q) ready to read — or an explicit
//     "no relevant skills found" signal if retrieval comes up empty.
//   - report_skill_feedback: closes the loop. Claude reports whether
//     the surfaced skill(s) actually helped, which flows into
//     evo.skill_usages.was_helpful for ranking + dashboard "used N
//     times" stats.
//
// Paper: arxiv 2605.10114v1, Meng/Wang/Fang, "SkillRAE: Agent
// Skill-Based Context Compilation for Retrieval-Augmented Execution".
// §3.1 defines the online stage as C(q) = Compile_B(q, K, H, A, O);
// this MCP is a thin Claude-side adapter to that.
//
// Build:
//
//	go build -o /thearray/git/evo/bin/skillrae-mcp ./mcp/cmd/skillrae-mcp
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamomaton-delegator/delegator/skillsclient"
)

const (
	defaultRAEBaseURL    = "http://localhost:7376"
	defaultSkillsBaseURL = "http://localhost:9123"
	defaultMinScore      = 0.2
	defaultBudgetTokens  = 1500
	defaultTopK          = 5

	// taskCacheTTL caps how long a (task_id → skill_ids) mapping
	// lives. A turn-of-Claude typically wraps in under an hour;
	// anything older is stale enough that feedback against it is
	// noise.
	taskCacheTTL = 1 * time.Hour

	// taskCacheGC fires the janitor goroutine. Cheap O(n) sweep of
	// the cache map.
	taskCacheGC = 10 * time.Minute
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "skillrae-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logger := logrus.New()
	// stdout is reserved for the JSON-RPC transport; any non-frame
	// bytes corrupt the stream.
	logger.SetOutput(os.Stderr)
	logger.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	client := buildClient(logger)
	if !client.RAEEnabled() {
		// We could refuse to start, but a useful failure mode is
		// "tools exist but every call returns a structured error
		// telling the user what env var to set." That makes the
		// problem visible inside Claude rather than as a startup
		// crash the user never sees.
		logger.Warn("SKILLS_RAE_URL is empty; find_skill will return an error until configured")
	}

	minScore := defaultMinScore
	if v := os.Getenv("SKILLRAE_MIN_SCORE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			minScore = f
		} else {
			logger.WithError(err).WithField("value", v).Warn("invalid SKILLRAE_MIN_SCORE; using default")
		}
	}

	cache := newTaskCache(taskCacheTTL)
	go cache.runJanitor(context.Background(), taskCacheGC)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "skillrae",
		Version: "0.1.0",
	}, nil)

	registerTools(server, client, cache, minScore, logger)

	logger.WithFields(logrus.Fields{
		"rae_url":    client.RAEBaseURL,
		"dash_url":   client.BaseURL,
		"min_score":  minScore,
	}).Info("skillrae-mcp ready (stdio)")
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp run: %w", err)
	}
	return nil
}

// buildClient constructs the HTTPClient directly (rather than calling
// skillsclient.New()) so we control both BaseURL and RAEBaseURL
// defaults independently. New() defaults BaseURL to the dashboard,
// which is also what we want here, but we re-state it explicitly
// because skillrae-mcp's failure modes depend on both URLs being
// known-good.
func buildClient(logger *logrus.Logger) *skillsclient.HTTPClient {
	rae := strings.TrimRight(os.Getenv("SKILLS_RAE_URL"), "/")
	if rae == "" {
		rae = defaultRAEBaseURL
	}
	dash := strings.TrimRight(os.Getenv("SKILLS_API_URL"), "/")
	if dash == "" {
		dash = defaultSkillsBaseURL
	}
	c := skillsclient.New()
	c.RAEBaseURL = rae
	c.BaseURL = dash
	logger.WithFields(logrus.Fields{"rae": rae, "dashboard": dash}).Debug("clients configured")
	return c
}

// --- task_id → skill_ids cache ---

type cacheEntry struct {
	skillIDs []string
	expires  time.Time
}

type taskCache struct {
	ttl  time.Duration
	mu   sync.Mutex
	data map[string]cacheEntry
}

func newTaskCache(ttl time.Duration) *taskCache {
	return &taskCache{ttl: ttl, data: make(map[string]cacheEntry)}
}

func (c *taskCache) put(taskID string, skillIDs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[taskID] = cacheEntry{
		skillIDs: append([]string(nil), skillIDs...),
		expires:  time.Now().Add(c.ttl),
	}
}

func (c *taskCache) get(taskID string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.data[taskID]
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	return append([]string(nil), entry.skillIDs...), true
}

func (c *taskCache) runJanitor(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.mu.Lock()
			now := time.Now()
			for k, v := range c.data {
				if now.After(v.expires) {
					delete(c.data, k)
				}
			}
			c.mu.Unlock()
		}
	}
}

// --- tool argument types ---

type findSkillArgs struct {
	Task           string `json:"task" jsonschema:"the task you're about to work on, in one paragraph of natural language. Subagent-prepared output of an Explore agent reads best here."`
	TechStack      string `json:"tech_stack,omitempty" jsonschema:"languages, frameworks, and key libraries of the code you're editing (e.g. 'Go 1.25, pgxpool, gorilla/mux'). Recommended for better retrieval."`
	OutputContract string `json:"output_contract,omitempty" jsonschema:"optional shape constraint on the output (e.g. 'return a JSON object with fields X, Y, Z'). Rendered into the compiled block as O(q)."`
	BudgetTokens   int    `json:"budget_tokens,omitempty" jsonschema:"context budget B for the compiled block (default 1500)"`
	TopK           int    `json:"top_k,omitempty" jsonschema:"max skills the compiler may consider (default 5)"`
}

type reportFeedbackArgs struct {
	TaskID     string   `json:"task_id" jsonschema:"the task_id returned by a prior find_skill call"`
	WasHelpful *bool    `json:"was_helpful" jsonschema:"true if the skill helped, false if it didn't"`
	SkillIDs   []string `json:"skill_ids,omitempty" jsonschema:"optional: which specific skills helped. Defaults to all skills surfaced by the original find_skill call."`
}

// --- tool registrations ---

func registerTools(server *mcp.Server, client *skillsclient.HTTPClient, cache *taskCache, minScore float64, logger *logrus.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "find_skill",
		Description: "Fetches a SkillRAE-compiled context block for the task you're about to work on. " +
			"BEFORE CALLING, dispatch an Explore subagent to identify (a) the tech stack of the file/module " +
			"you're editing — languages, frameworks, key libraries — and (b) a tight one-paragraph " +
			"statement of what you're trying to accomplish. Pass both as `tech_stack` and `task`. " +
			"The tool returns one of two shapes: status=\"ok\" with a compiled `context` markdown block " +
			"to read before you start work, OR status=\"no_relevant_skills_found\" if the library has " +
			"nothing useful — in which case just proceed without it. After you're done, call " +
			"`report_skill_feedback` with the `task_id` returned here and whether the skill helped.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args findSkillArgs) (*mcp.CallToolResult, any, error) {
		return handleFindSkill(ctx, client, cache, minScore, args, logger)
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "report_skill_feedback",
		Description: "Report whether the skill(s) surfaced by a prior find_skill call actually helped. " +
			"The library uses this to tune ranking and to surface 'used N times' stats. Call this " +
			"once you've finished the task. Pass the `task_id` from find_skill and `was_helpful` " +
			"(true|false). Skip this call when find_skill returned status=no_relevant_skills_found.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args reportFeedbackArgs) (*mcp.CallToolResult, any, error) {
		return handleReportFeedback(ctx, client, cache, args, logger)
	})
}

func handleFindSkill(ctx context.Context, client *skillsclient.HTTPClient, cache *taskCache, minScore float64, args findSkillArgs, logger *logrus.Logger) (*mcp.CallToolResult, any, error) {
	if !client.RAEEnabled() {
		return errResult("SKILLS_RAE_URL is not configured; set it in the MCP server env to enable skill retrieval"), nil, nil
	}
	task := strings.TrimSpace(args.Task)
	if task == "" {
		return errResult("task is required"), nil, nil
	}
	query := composeQuery(args.TechStack, task)
	topK := args.TopK
	if topK <= 0 {
		topK = defaultTopK
	}
	budget := args.BudgetTokens
	if budget <= 0 {
		budget = defaultBudgetTokens
	}

	taskID := uuid.NewString()

	rpcCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	compiled, err := client.CompileContext(rpcCtx, query, taskID, strings.TrimSpace(args.OutputContract), topK, budget)
	if err != nil {
		if errors.Is(err, skillsclient.ErrRAEDisabled) {
			return errResult("skills-rae not configured (SKILLS_RAE_URL empty)"), nil, nil
		}
		logger.WithError(err).Warn("CompileContext failed")
		return errResult(fmt.Sprintf("skills-rae call failed: %v", err)), nil, nil
	}

	topScore := 0.0
	for _, s := range compiled.SelectedSkills {
		if s.Score > topScore {
			topScore = s.Score
		}
	}

	if len(compiled.SelectedSkills) == 0 || topScore < minScore {
		logger.WithFields(logrus.Fields{
			"task_id":   taskID,
			"top_score": topScore,
			"min":       minScore,
			"selected":  len(compiled.SelectedSkills),
		}).Debug("no relevant skills found")
		return jsonResult(map[string]any{
			"status":      "no_relevant_skills_found",
			"task_id":     taskID,
			"top_score":   topScore,
			"min_score":   minScore,
			"diagnostics": compiled.Diagnostics,
		}), nil, nil
	}

	skillIDs := make([]string, 0, len(compiled.SelectedSkills))
	for _, s := range compiled.SelectedSkills {
		if s.SkillID != "" {
			skillIDs = append(skillIDs, s.SkillID)
		}
	}
	cache.put(taskID, skillIDs)

	logger.WithFields(logrus.Fields{
		"task_id":  taskID,
		"skills":   len(skillIDs),
		"top":      topScore,
		"stage_ms": compiled.Diagnostics.StageMs,
	}).Info("compiled context returned")

	return jsonResult(map[string]any{
		"status":          "ok",
		"task_id":         taskID,
		"context":         compiled.Context,
		"selected_skills": compiled.SelectedSkills,
		"rescue_attached": compiled.RescueAttached,
		"diagnostics":     compiled.Diagnostics,
	}), nil, nil
}

func handleReportFeedback(ctx context.Context, client *skillsclient.HTTPClient, cache *taskCache, args reportFeedbackArgs, logger *logrus.Logger) (*mcp.CallToolResult, any, error) {
	taskID := strings.TrimSpace(args.TaskID)
	if taskID == "" {
		return errResult("task_id is required"), nil, nil
	}
	if args.WasHelpful == nil {
		return errResult("was_helpful is required"), nil, nil
	}

	skillIDs := args.SkillIDs
	if len(skillIDs) == 0 {
		cached, ok := cache.get(taskID)
		if !ok {
			// No cached mapping. Could be a stale task_id from a
			// previous MCP process, or Claude inventing IDs. Surface
			// it so the caller can correct (rather than silently
			// firing zero POSTs and "succeeding").
			return errResult("unknown task_id: this MCP process has no cached skill_ids for it. Pass skill_ids explicitly, or call find_skill first."), nil, nil
		}
		skillIDs = cached
	}

	rpcCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var firstErr error
	successes := 0
	for _, sid := range skillIDs {
		if err := client.RecordUsageWithFeedback(rpcCtx, sid, taskID, args.WasHelpful); err != nil {
			logger.WithError(err).WithFields(logrus.Fields{
				"task_id":  taskID,
				"skill_id": sid,
			}).Warn("RecordUsageWithFeedback failed")
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		successes++
	}

	if successes == 0 && firstErr != nil {
		return errResult(fmt.Sprintf("all feedback posts failed; first error: %v", firstErr)), nil, nil
	}

	return jsonResult(map[string]any{
		"status":      "ok",
		"task_id":     taskID,
		"recorded":    successes,
		"requested":   len(skillIDs),
		"was_helpful": *args.WasHelpful,
	}), nil, nil
}

// composeQuery folds tech_stack into the natural-language query the
// retriever embeds with bge-m3. SkillRAE retrieval is purely
// text-embedding based (§3.2.1), so the way to surface stack context
// is to put it into q rather than as a separate retrieval channel.
func composeQuery(techStack, task string) string {
	techStack = strings.TrimSpace(techStack)
	task = strings.TrimSpace(task)
	if techStack == "" {
		return task
	}
	return "Tech stack: " + techStack + "\n\nTask: " + task
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
