// Package skillsclient is the delegator's HTTP client for the evo
// dashboard's /api/v1/skills/{search,usages} endpoints. It exists as a
// separate package so the orchestrator can depend on a small surface
// without pulling in the full apiserver — and so the MCP server's
// main.go can swap the real client for a fake in tests.
//
// The contract is intentionally narrow:
//
//	type Client interface {
//	    Search(ctx, query, limit) ([]Hit, error)
//	    RecordUsage(ctx, skillID, taskID) error
//	}
//
// Two top-level deliverables:
//
//   - Pre-task RAG retrieval: orchestrator calls Search() with the
//     user's prompt before dispatching to the agent CLI, then prepends
//     a "RELEVANT SKILLS" block.
//   - Post-task usage tracking: after the run completes, orchestrator
//     fires-and-forgets one RecordUsage per surfaced hit so the
//     dashboard can show "used N times".
//
// Both endpoints are best-effort: any error is logged but never blocks
// the task from running. The skill library is an augmentation, not a
// dependency.
package skillsclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Hit is one search result. Mirrors the dashboard's
// ``skillsSearchHit`` shape but kept independent so the orchestrator
// doesn't depend on apiserver internals.
type Hit struct {
	SkillID   string   `json:"skill_id"`
	SkillName string   `json:"skill_name"`
	ChunkKind string   `json:"chunk_kind"`
	Score     float64  `json:"score"`
	Text      string   `json:"text"`
	Community string   `json:"community"`
	Tags      []string `json:"tags"`
}

// Client is the interface the orchestrator depends on. The HTTP
// implementation below is the production wiring; tests can use a
// simple struct of channels or canned responses.
type Client interface {
	Search(ctx context.Context, query string, limit int) ([]Hit, error)
	RecordUsage(ctx context.Context, skillID, taskID string) error
}

// HTTPClient talks to the evo dashboard. Construct with New() and
// pass to delegator.Orchestrator.Skills. Safe for concurrent use.
type HTTPClient struct {
	BaseURL    string        // e.g. "http://localhost:9123" — no trailing slash
	RAEBaseURL string        // e.g. "http://localhost:7376" — empty when RAE not deployed
	Token      string        // optional API token forwarded as X-API-Key
	Timeout    time.Duration // per-request cap (default 15s)

	httpClient *http.Client
}

// New builds an HTTPClient from environment variables:
//
//	SKILLS_API_URL       base URL (default http://localhost:9123)
//	SKILLS_API_TOKEN     optional X-API-Key value
//	SKILLS_R2R_INSECURE  if "1"/"true", skip TLS verification (DEV ONLY)
//
// The insecure flag uses the same name as skills/dashboard so a single
// env var disables TLS verification consistently across the stack. It
// MUST stay off in production — InsecureSkipVerify defeats the entire
// purpose of TLS. Default is secure (verify certs).
//
// Returns nil when SKILLS_API_URL is explicitly set to the empty string
// — that's the operator's way of disabling the integration.
func New() *HTTPClient {
	base := strings.TrimRight(os.Getenv("SKILLS_API_URL"), "/")
	if base == "" {
		base = "http://localhost:9123"
	}
	tr := &http.Transport{}
	if v := os.Getenv("SKILLS_R2R_INSECURE"); v == "1" || strings.EqualFold(v, "true") {
		// nolint:gosec — opt-in via SKILLS_R2R_INSECURE for local dev
		// only. Self-signed certs on the dashboard are common in dev;
		// this flag bypasses cert verification so the delegator can
		// still call /api/v1/skills/* without a CA roll-out.
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &HTTPClient{
		BaseURL:    base,
		RAEBaseURL: strings.TrimRight(os.Getenv("SKILLS_RAE_URL"), "/"),
		Token:      os.Getenv("SKILLS_API_TOKEN"),
		Timeout:    15 * time.Second,
		httpClient: &http.Client{
			Transport: tr,
			Timeout:   15 * time.Second,
		},
	}
}

type searchPayload struct {
	Query    string `json:"query"`
	Limit    int    `json:"limit,omitempty"`
	CorpusID string `json:"corpus_id,omitempty"`
}

type searchResponse struct {
	Hits  []Hit  `json:"hits"`
	Error string `json:"error,omitempty"`
}

// Search calls /api/v1/skills/search and returns up to `limit` hits.
// An empty `query` returns nil with no error — the orchestrator
// shouldn't ask for an empty-prompt enrichment.
func (c *HTTPClient) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	if c == nil {
		return nil, nil
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	body, err := json.Marshal(searchPayload{Query: q, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/api/v1/skills/search", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("X-API-Key", c.Token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed searchResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if parsed.Error != "" {
		return nil, errors.New(parsed.Error)
	}
	return parsed.Hits, nil
}

type usagePayload struct {
	SkillID    string `json:"skill_id"`
	TaskID     string `json:"task_id"`
	WasHelpful *bool  `json:"was_helpful,omitempty"`
}

// RecordUsage POSTs /api/v1/skills/usages. Idempotent server-side
// (ON CONFLICT DO UPDATE), so repeated calls with the same (skill_id,
// task_id) are harmless. Use RecordUsageWithFeedback when you have
// a was_helpful signal from the caller — this method leaves the
// column NULL.
func (c *HTTPClient) RecordUsage(ctx context.Context, skillID, taskID string) error {
	return c.recordUsage(ctx, skillID, taskID, nil)
}

// RecordUsageWithFeedback is RecordUsage plus the was_helpful flag. The
// dashboard endpoint stores nil/true/false as a tri-state — pass nil
// when the caller has no opinion. Used by skillrae-mcp to surface
// Claude's "did this skill help" feedback.
func (c *HTTPClient) RecordUsageWithFeedback(ctx context.Context, skillID, taskID string, wasHelpful *bool) error {
	return c.recordUsage(ctx, skillID, taskID, wasHelpful)
}

func (c *HTTPClient) recordUsage(ctx context.Context, skillID, taskID string, wasHelpful *bool) error {
	if c == nil {
		return nil
	}
	if skillID == "" || taskID == "" {
		return nil
	}
	body, err := json.Marshal(usagePayload{SkillID: skillID, TaskID: taskID, WasHelpful: wasHelpful})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/api/v1/skills/usages", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("X-API-Key", c.Token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// FormatSkillsBlock renders a list of hits as the prompt-prefix block
// that the orchestrator injects ahead of the user's task. Each hit gets
// a heading + the matched text; duplicates by SkillID are dropped (the
// dashboard already dedupes, but the helper stays defensive).
//
// Returns the empty string when ``hits`` is empty so callers can simply
// concatenate the result with the original prompt.
func FormatSkillsBlock(hits []Hit) string {
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# RELEVANT SKILLS\n\n")
	b.WriteString("The following skills from the evo skill library look relevant to this task. Use them where they apply.\n\n")
	seen := map[string]bool{}
	for _, h := range hits {
		key := h.SkillID
		if key == "" {
			key = h.SkillName + "|" + h.Text[:min(40, len(h.Text))]
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		name := h.SkillName
		if name == "" {
			name = "(unnamed skill)"
		}
		b.WriteString("## ")
		b.WriteString(name)
		if h.Community != "" {
			b.WriteString("  _[")
			b.WriteString(h.Community)
			b.WriteString("]_")
		}
		b.WriteString("\n\n")
		text := strings.TrimSpace(h.Text)
		if text != "" {
			b.WriteString(text)
			b.WriteString("\n\n")
		}
	}
	b.WriteString("---\n\n")
	return b.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
