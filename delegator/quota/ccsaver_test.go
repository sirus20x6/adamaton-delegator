package quota

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// buildCCSaverDB creates a synthetic CCSAVER schema-compatible DB at the
// given path and seeds it with the provided rows.
func buildCCSaverDB(t *testing.T, path string, rows []ccsRow) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	schema := `
CREATE TABLE interactions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  api_type TEXT,
  model TEXT,
  input_tokens INTEGER DEFAULT 0,
  output_tokens INTEGER DEFAULT 0,
  estimated_cost_usd REAL DEFAULT 0,
  duration_ms INTEGER DEFAULT 0,
  response_headers TEXT,
  timestamp TEXT
);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	stmt, err := db.Prepare(`INSERT INTO interactions
		(api_type, model, input_tokens, output_tokens, estimated_cost_usd, duration_ms, response_headers, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()

	for _, r := range rows {
		_, err := stmt.Exec(r.APIType, r.Model, r.InputTokens, r.OutputTokens,
			r.CostUSD, r.DurationMs, r.HeadersJSON, r.Timestamp)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}

type ccsRow struct {
	APIType      string
	Model        string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	DurationMs   int64
	HeadersJSON  string
	Timestamp    string
}

func TestCCSaver_GetClaudeRateLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ccs.db")
	resetEpoch := time.Now().Add(2 * time.Hour).Unix()
	headers := map[string][]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.42"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.18"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {sprintInt64(resetEpoch)},
		"Anthropic-Ratelimit-Unified-5h-Status":      {"allowed"},
	}
	hdrJSON, _ := json.Marshal(headers)
	now := time.Now().UTC().Format(time.RFC3339)
	buildCCSaverDB(t, path, []ccsRow{
		{APIType: "anthropic", HeadersJSON: string(hdrJSON), Timestamp: now},
	})

	cs, err := OpenCCSaver(CCSaverConfig{Path: path})
	if err != nil {
		t.Fatalf("OpenCCSaver: %v", err)
	}
	defer cs.Close()

	rl := cs.GetClaudeRateLimits()
	if rl == nil {
		t.Fatalf("nil rate limits")
	}
	if rl.Utilization5h != 0.42 {
		t.Errorf("Utilization5h = %v, want 0.42", rl.Utilization5h)
	}
	if rl.Utilization7d != 0.18 {
		t.Errorf("Utilization7d = %v, want 0.18", rl.Utilization7d)
	}
	if rl.ResetTime5h == "" {
		t.Errorf("ResetTime5h empty")
	}
	if rl.Status5h != "allowed" {
		t.Errorf("Status5h = %q, want allowed", rl.Status5h)
	}
}

func TestCCSaver_GetLatestQuota_Anthropic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ccs.db")
	headers := map[string][]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.5"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.25"},
		"Anthropic-Ratelimit-Unified-5h-Status":      {"allowed"},
	}
	hdrJSON, _ := json.Marshal(headers)
	now := time.Now().UTC().Format(time.RFC3339)
	buildCCSaverDB(t, path, []ccsRow{
		{APIType: "anthropic", Model: "claude-opus", InputTokens: 100, OutputTokens: 50,
			CostUSD: 0.01, HeadersJSON: string(hdrJSON), Timestamp: now},
	})

	cs, err := OpenCCSaver(CCSaverConfig{Path: path})
	if err != nil {
		t.Fatalf("OpenCCSaver: %v", err)
	}
	defer cs.Close()

	q, err := cs.GetLatestQuota("anthropic")
	if err != nil {
		t.Fatalf("GetLatestQuota: %v", err)
	}
	if q.Agent != "claude" {
		t.Errorf("Agent = %q, want claude", q.Agent)
	}
	if q.Utilization5h == nil || *q.Utilization5h != 0.5 {
		t.Errorf("Utilization5h = %v, want 0.5", q.Utilization5h)
	}
	if q.Status5h != "allowed" {
		t.Errorf("Status5h = %q, want allowed", q.Status5h)
	}
	if q.TotalCalls == nil || *q.TotalCalls != 1 {
		t.Errorf("TotalCalls = %v, want 1", q.TotalCalls)
	}
}

func TestCCSaver_GetLatestQuota_OpenAI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ccs.db")
	headers := map[string]string{
		"x-ratelimit-remaining-tokens": "200",
		"x-ratelimit-limit-tokens":     "1000",
		"x-ratelimit-reset-tokens":     "10s",
	}
	hdrJSON, _ := json.Marshal(headers)
	now := time.Now().UTC().Format(time.RFC3339)
	buildCCSaverDB(t, path, []ccsRow{
		{APIType: "openai", Model: "gpt-5", HeadersJSON: string(hdrJSON), Timestamp: now},
	})

	cs, err := OpenCCSaver(CCSaverConfig{Path: path})
	if err != nil {
		t.Fatalf("OpenCCSaver: %v", err)
	}
	defer cs.Close()

	q, err := cs.GetLatestQuota("openai")
	if err != nil {
		t.Fatalf("GetLatestQuota: %v", err)
	}
	if q.Agent != "codex" {
		t.Errorf("Agent = %q, want codex", q.Agent)
	}
	if q.Utilization5h == nil || *q.Utilization5h != 0.8 {
		t.Errorf("Utilization5h = %v, want 0.8", q.Utilization5h)
	}
	if q.ResetTime5h != "10s" {
		t.Errorf("ResetTime5h = %q, want 10s", q.ResetTime5h)
	}
}

func TestCCSaver_GetUsageStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ccs.db")
	now := time.Now().UTC().Format(time.RFC3339)
	buildCCSaverDB(t, path, []ccsRow{
		{APIType: "anthropic", Model: "claude-opus", InputTokens: 100, OutputTokens: 50, CostUSD: 0.01, DurationMs: 1000, Timestamp: now},
		{APIType: "anthropic", Model: "claude-opus", InputTokens: 200, OutputTokens: 80, CostUSD: 0.02, DurationMs: 2000, Timestamp: now},
		{APIType: "anthropic", Model: "claude-haiku", InputTokens: 50, OutputTokens: 25, CostUSD: 0.001, DurationMs: 500, Timestamp: now},
	})

	cs, err := OpenCCSaver(CCSaverConfig{Path: path})
	if err != nil {
		t.Fatalf("OpenCCSaver: %v", err)
	}
	defer cs.Close()

	stats, err := cs.GetUsageStats("anthropic", 7)
	if err != nil {
		t.Fatalf("GetUsageStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("got %d models, want 2", len(stats))
	}

	byModel := map[string]UsageStats{}
	for _, s := range stats {
		byModel[s.Model] = s
	}
	if byModel["claude-opus"].Calls != 2 {
		t.Errorf("opus.Calls = %d, want 2", byModel["claude-opus"].Calls)
	}
	if byModel["claude-opus"].InputTokens != 300 {
		t.Errorf("opus.InputTokens = %d, want 300", byModel["claude-opus"].InputTokens)
	}
	if byModel["claude-haiku"].Calls != 1 {
		t.Errorf("haiku.Calls = %d, want 1", byModel["claude-haiku"].Calls)
	}
}

func TestCCSaver_GetCostBreakdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ccs.db")
	now := time.Now().UTC().Format(time.RFC3339)
	buildCCSaverDB(t, path, []ccsRow{
		{APIType: "anthropic", Model: "claude", CostUSD: 0.05, InputTokens: 10, OutputTokens: 5, Timestamp: now},
		{APIType: "openai", Model: "gpt-5", CostUSD: 0.10, InputTokens: 20, OutputTokens: 10, Timestamp: now},
	})

	cs, err := OpenCCSaver(CCSaverConfig{Path: path})
	if err != nil {
		t.Fatalf("OpenCCSaver: %v", err)
	}
	defer cs.Close()

	rows, err := cs.GetCostBreakdown(7, "api_type")
	if err != nil {
		t.Fatalf("GetCostBreakdown: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	totalCost := 0.0
	for _, r := range rows {
		totalCost += r.TotalCost
	}
	if totalCost < 0.149 || totalCost > 0.151 {
		t.Errorf("totalCost = %v, want ~0.15", totalCost)
	}
}

func TestCCSaver_GetGeminiUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ccs.db")
	now := time.Now().UTC().Format(time.RFC3339)
	buildCCSaverDB(t, path, []ccsRow{
		{APIType: "gemini-code-assist", Model: "gemini-2.5-pro", InputTokens: 500, OutputTokens: 200, Timestamp: now},
		{APIType: "gemini-code-assist", Model: "gemini-2.5-flash", InputTokens: 100, OutputTokens: 50, Timestamp: now},
		{APIType: "anthropic", InputTokens: 999, Timestamp: now}, // must be ignored
	})

	cs, err := OpenCCSaver(CCSaverConfig{Path: path})
	if err != nil {
		t.Fatalf("OpenCCSaver: %v", err)
	}
	defer cs.Close()

	totals := cs.GetGeminiUsage(1)
	if totals == nil {
		t.Fatalf("nil totals")
	}
	if totals.Calls != 2 {
		t.Errorf("Calls = %d, want 2", totals.Calls)
	}
	if totals.Input != 600 {
		t.Errorf("Input = %d, want 600", totals.Input)
	}
	if totals.Output != 250 {
		t.Errorf("Output = %d, want 250", totals.Output)
	}
	if len(totals.Models) != 2 {
		t.Errorf("Models = %v, want 2", totals.Models)
	}
}

func TestCCSaver_MissingDB(t *testing.T) {
	_, err := OpenCCSaver(CCSaverConfig{Path: filepath.Join(t.TempDir(), "does-not-exist.db")})
	if err == nil {
		t.Fatal("expected error for missing db")
	}
}

func TestCCSaver_GetLatestQuota_NoRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ccs.db")
	buildCCSaverDB(t, path, nil)

	cs, err := OpenCCSaver(CCSaverConfig{Path: path})
	if err != nil {
		t.Fatalf("OpenCCSaver: %v", err)
	}
	defer cs.Close()

	q, err := cs.GetLatestQuota("anthropic")
	if err != nil {
		t.Fatalf("GetLatestQuota: %v", err)
	}
	if q.Agent != "claude" || q.APIType != "anthropic" {
		t.Errorf("expected stub QuotaInfo, got %+v", q)
	}
	if q.Utilization5h != nil {
		t.Errorf("Utilization5h should be nil for empty DB")
	}
}

func TestCCSaver_GetAllQuotas_IncludesOpenCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ccs.db")
	now := time.Now().UTC().Format(time.RFC3339)
	buildCCSaverDB(t, path, []ccsRow{
		{APIType: "anthropic", Timestamp: now},
		{APIType: "openai", Timestamp: now},
	})

	cs, err := OpenCCSaver(CCSaverConfig{Path: path})
	if err != nil {
		t.Fatalf("OpenCCSaver: %v", err)
	}
	defer cs.Close()

	all, err := cs.GetAllQuotas()
	if err != nil {
		t.Fatalf("GetAllQuotas: %v", err)
	}
	hasOpenCode := false
	apiTypes := map[string]bool{}
	for _, q := range all {
		apiTypes[q.APIType] = true
		if q.Agent == "opencode" {
			hasOpenCode = true
		}
	}
	if !hasOpenCode {
		t.Errorf("opencode missing from GetAllQuotas")
	}
	if !apiTypes["anthropic"] || !apiTypes["openai"] {
		t.Errorf("missing api types: got %v", apiTypes)
	}
}

func sprintInt64(v int64) string {
	b := make([]byte, 0, 20)
	if v < 0 {
		b = append(b, '-')
		v = -v
	}
	if v == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	b = append(b, digits[i:]...)
	return string(b)
}
