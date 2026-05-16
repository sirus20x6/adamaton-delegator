package skillsclient

// SkillRAE client: a separate base URL because the new service runs
// at :7376 alongside the existing :9123 dashboard. When SKILLS_RAE_URL
// is unset, HTTPClient.RAEEnabled() reports false and callers fall
// back to the flat Search + FormatSkillsBlock path.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// CompiledContext is what skills-rae returns from POST /v1/rae/compile.
// The Context field is the rendered markdown block ready to prepend to
// the agent prompt; SelectedSkills mirror the structured payload so
// the orchestrator can still RecordUsage per skill.
type CompiledContext struct {
	Context        string                 `json:"context"`
	SelectedSkills []SelectedSkillSummary `json:"selected_skills"`
	RescueAttached []RescueAttachedRow    `json:"rescue_attached"`
	Skipped        []SkippedCueRow        `json:"skipped_cues"`
	Diagnostics    CompiledDiagnostics    `json:"diagnostics"`
}

// SelectedSkillSummary mirrors skills-rae/internal/retrieval.SelectedSkill
// minus the Postgres-specific UUID encodings — we use string here so
// JSON marshalling is symmetric.
type SelectedSkillSummary struct {
	SkillID     string  `json:"skill_id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Score       float64 `json:"score"`
	L1          float64 `json:"l1"`
	L0          float64 `json:"l0"`
	NameScore   float64 `json:"name_score"`
	Boosted     bool    `json:"boosted"`
	CommunityID string  `json:"community_id,omitempty"`
}

// RescueAttachedRow is one rescue cue that landed on a selected skill.
type RescueAttachedRow struct {
	HostSkillID   string  `json:"host_skill_id"`
	SourceSkillID string  `json:"source_skill_id"`
	Subunit       string  `json:"subunit"`
	Kind          string  `json:"kind"`
	AffScore      float64 `json:"aff_score"`
}

// SkippedCueRow is a rescue cue that didn't make the final block. We
// carry it for telemetry only; agents don't consume this.
type SkippedCueRow struct {
	SubunitID string `json:"subunit_id"`
	Reason    string `json:"reason"`
}

// CompiledDiagnostics carries per-stage timings + the matched
// community labels — useful for the dashboard's debug view.
type CompiledDiagnostics struct {
	StageMs       map[string]int64 `json:"stage_ms"`
	TopDownLabels []string         `json:"top_down_labels"`
	BottomUpHits  int              `json:"bottom_up_hits"`
	L1Hits        int              `json:"l1_hits"`
}

// RAEEnabled reports whether SKILLS_RAE_URL was set. New() picks it
// up from the env; callers should branch on this before invoking
// CompileContext so the fallback path stays cheap.
func (c *HTTPClient) RAEEnabled() bool {
	return c != nil && c.RAEBaseURL != ""
}

type compilePayload struct {
	Query          string `json:"query"`
	TaskID         string `json:"task_id,omitempty"`
	TopK           int    `json:"top_k,omitempty"`
	BudgetTokens   int    `json:"budget_tokens,omitempty"`
	OutputContract string `json:"output_contract,omitempty"`
}

// CompileContext calls POST /v1/rae/compile on the skills-rae service
// and returns the compiled context block + structured metadata. The
// returned CompiledContext is the source of truth for what gets
// prepended to the agent prompt; the SelectedSkills slice is what the
// orchestrator turns into RecordUsage calls.
//
// outputContract is the paper's O(q) — task-output constraints rendered
// into the compiled block. Pass "" when the caller has no shape
// requirement.
//
// Errors when the client is not RAE-enabled, when the query is empty,
// or when skills-rae returns 4xx/5xx — callers should fall back to
// the flat Search path on any error.
func (c *HTTPClient) CompileContext(ctx context.Context, query, taskID, outputContract string, topK, budget int) (*CompiledContext, error) {
	if c == nil {
		return nil, ErrRAEDisabled
	}
	if !c.RAEEnabled() {
		return nil, ErrRAEDisabled
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, errors.New("skillsclient: empty query")
	}
	if topK <= 0 {
		topK = 5
	}
	if budget <= 0 {
		budget = 1500
	}
	body, err := json.Marshal(compilePayload{
		Query:          q,
		TaskID:         taskID,
		TopK:           topK,
		BudgetTokens:   budget,
		OutputContract: outputContract,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.RAEBaseURL+"/v1/rae/compile", bytes.NewReader(body))
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
	var out CompiledContext
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return &out, nil
}

// HitsFromCompiled mirrors the SelectedSkills into the legacy Hit
// shape so the orchestrator's existing post-task RecordUsage path
// works unchanged. The Hit.Text field is left empty — the legacy
// dashboard /api/v1/skills/usages endpoint only needs (skill_id,
// task_id), and the field is informational only.
func HitsFromCompiled(c *CompiledContext) []Hit {
	if c == nil {
		return nil
	}
	out := make([]Hit, 0, len(c.SelectedSkills))
	for _, s := range c.SelectedSkills {
		out = append(out, Hit{
			SkillID:   s.SkillID,
			SkillName: s.Name,
			Score:     s.Score,
		})
	}
	return out
}

// ErrRAEDisabled is returned by CompileContext when RAE_URL is not
// configured. Callers detect this and route to the flat Search path.
var ErrRAEDisabled = errors.New("skillsclient: skills-rae not configured (SKILLS_RAE_URL empty)")
