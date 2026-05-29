package quota

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

// apiTypeToAgent maps the api_type column in the interactions table to the
// agent name surfaced by GetLatestQuota. Mirrors API_TYPE_TO_AGENT in the TS.
var apiTypeToAgent = map[string]string{
	"anthropic":          "claude",
	"openai":             "codex",
	"openai-codex":       "codex",
	"gemini":             "gemini",
	"gemini-code-assist": "gemini",
}

// CCSaver wraps the read-only SQLite handle for the CCSAVER interactions DB.
//
// We don't reuse internal/budget/store here because the schema is different
// (CCSAVER's interactions table is owned by an external Node service) and we
// need read-only access. Open via OpenCCSaver.
type CCSaver struct {
	db     *sql.DB
	path   string
	mu     sync.RWMutex
	logger *logrus.Logger
}

// CCSaverConfig configures the CCSAVER reader. Tests inject a synthetic DB
// path here; production callers pass DefaultCCSaverPath().
type CCSaverConfig struct {
	// Path is the absolute path to the CCSAVER SQLite file. If empty,
	// DefaultCCSaverPath() is used.
	Path string
	// Logger receives warnings; nil falls back to logrus.StandardLogger().
	Logger *logrus.Logger
}

// DefaultCCSaverPath resolves the CCSAVER DB path: $CCSAVER_DB if set,
// otherwise ~/.local/share/ccsaver/data.db. HomeDir is honored via the HOME
// env var so tests can override it with t.Setenv.
func DefaultCCSaverPath() string {
	if env := strings.TrimSpace(os.Getenv("CCSAVER_DB")); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "ccsaver", "data.db")
}

// OpenCCSaver opens the CCSAVER SQLite database read-only. Returns an error if
// the file does not exist (mirroring the TS `fileMustExist: true`).
func OpenCCSaver(cfg CCSaverConfig) (*CCSaver, error) {
	path := cfg.Path
	if path == "" {
		path = DefaultCCSaverPath()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = logrus.StandardLogger()
	}

	if path != ":memory:" {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("ccsaver db: %w", err)
		}
	}

	// modernc.org/sqlite uses URI-style query parameters for read-only mode.
	// In-memory databases skip the mode override so tests can populate them.
	dsn := path
	if path != ":memory:" {
		dsn = fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", path)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open ccsaver: %w", err)
	}
	// The CCSAVER DB is read-only and shared with another process — pin a
	// single connection so we don't fan out connections for nothing.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping ccsaver: %w", err)
	}

	return &CCSaver{db: db, path: path, logger: logger}, nil
}

// Close closes the underlying database handle.
func (c *CCSaver) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

// rawHeaders is the parsed JSON of the response_headers column.
// Anthropic's proxy stringifies the headers map with []string values, but
// some imported rows store scalar strings, so we tolerate both shapes.
type rawHeaders map[string]any

func parseHeaders(headersJSON string) rawHeaders {
	if headersJSON == "" {
		return rawHeaders{}
	}
	var h rawHeaders
	if err := json.Unmarshal([]byte(headersJSON), &h); err != nil {
		return rawHeaders{}
	}
	return h
}

// getHeaderValue is the case-insensitive header lookup that mirrors the TS
// helper. It returns the first element of an array-valued header or the raw
// string when the value is scalar.
func getHeaderValue(h rawHeaders, key string) string {
	if v, ok := h[key]; ok {
		if s := firstHeaderString(v); s != "" {
			return s
		}
	}
	lowerKey := strings.ToLower(key)
	for k, v := range h {
		if strings.ToLower(k) == lowerKey {
			if s := firstHeaderString(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func firstHeaderString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		if len(t) == 0 {
			return ""
		}
		if s, ok := t[0].(string); ok {
			return s
		}
	case []string:
		if len(t) == 0 {
			return ""
		}
		return t[0]
	}
	return ""
}

// GetLatestQuota returns the QuotaInfo derived from the most recent
// interaction row for the given api_type. Mirrors quota-tracker.ts
// getLatestQuota.
func (c *CCSaver) GetLatestQuota(apiType string) (QuotaInfo, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	agent, ok := apiTypeToAgent[apiType]
	if !ok {
		agent = apiType
	}
	info := QuotaInfo{Agent: agent, APIType: apiType}

	var headersJSON sql.NullString
	var costNullable sql.NullFloat64
	row := c.db.QueryRow(
		`SELECT response_headers, estimated_cost_usd
		 FROM interactions
		 WHERE api_type = ?
		 ORDER BY id DESC
		 LIMIT 1`,
		apiType,
	)
	if err := row.Scan(&headersJSON, &costNullable); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return info, nil
		}
		return info, fmt.Errorf("query latest interaction: %w", err)
	}

	headers := parseHeaders(headersJSON.String)

	switch apiType {
	case "anthropic":
		if u := getHeaderValue(headers, "Anthropic-Ratelimit-Unified-5h-Utilization"); u != "" {
			if v, err := strconv.ParseFloat(u, 64); err == nil {
				info.Utilization5h = ptrFloat(v)
			}
		}
		if u := getHeaderValue(headers, "Anthropic-Ratelimit-Unified-7d-Utilization"); u != "" {
			if v, err := strconv.ParseFloat(u, 64); err == nil {
				info.Utilization7d = ptrFloat(v)
			}
		}
		if r := getHeaderValue(headers, "Anthropic-Ratelimit-Unified-5h-Reset"); r != "" {
			info.ResetTime5h = epochSecsToISO(r)
		}
		if r := getHeaderValue(headers, "Anthropic-Ratelimit-Unified-7d-Reset"); r != "" {
			info.ResetTime7d = epochSecsToISO(r)
		}
		if s := getHeaderValue(headers, "Anthropic-Ratelimit-Unified-5h-Status"); s != "" {
			info.Status5h = s
		}
		if s := getHeaderValue(headers, "Anthropic-Ratelimit-Unified-7d-Status"); s != "" {
			info.Status7d = s
		}
	case "openai", "openai-codex":
		remaining := getHeaderValue(headers, "x-ratelimit-remaining-tokens")
		limit := getHeaderValue(headers, "x-ratelimit-limit-tokens")
		reset := getHeaderValue(headers, "x-ratelimit-reset-tokens")
		if remaining != "" && limit != "" {
			rem, errR := strconv.ParseInt(remaining, 10, 64)
			lim, errL := strconv.ParseInt(limit, 10, 64)
			if errR == nil && errL == nil && lim > 0 {
				info.Utilization5h = ptrFloat(1 - float64(rem)/float64(lim))
			}
		}
		if reset != "" {
			info.ResetTime5h = reset
		}
	case "gemini", "gemini-code-assist":
		remaining := getHeaderValue(headers, "x-ratelimit-remaining")
		limit := getHeaderValue(headers, "x-ratelimit-limit")
		if remaining != "" && limit != "" {
			rem, errR := strconv.ParseInt(remaining, 10, 64)
			lim, errL := strconv.ParseInt(limit, 10, 64)
			if errR == nil && errL == nil && lim > 0 {
				info.Utilization5h = ptrFloat(1 - float64(rem)/float64(lim))
			}
		}
	}

	// Today's cost + call count
	var totalCost sql.NullFloat64
	var totalCalls sql.NullInt64
	costRow := c.db.QueryRow(
		`SELECT COALESCE(SUM(estimated_cost_usd), 0), COUNT(*)
		 FROM interactions
		 WHERE api_type = ? AND timestamp >= date('now')`,
		apiType,
	)
	if err := costRow.Scan(&totalCost, &totalCalls); err == nil {
		info.EstimatedCostToday = ptrFloat(totalCost.Float64)
		info.TotalCalls = ptrInt64(totalCalls.Int64)
	}
	// costNullable from the first SELECT is intentionally unused — the
	// breakdown query above is authoritative for today's cost. We only
	// scanned it to keep the SELECT shape parallel to the TS source.
	_ = costNullable

	return info, nil
}

// GetAllQuotas returns one QuotaInfo per distinct api_type plus an opencode
// stub. Mirrors quota-tracker.ts getAllQuotas.
func (c *CCSaver) GetAllQuotas() ([]QuotaInfo, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	rows, err := c.db.Query(
		`SELECT DISTINCT api_type FROM interactions WHERE api_type IS NOT NULL`,
	)
	if err != nil {
		return nil, fmt.Errorf("query api_types: %w", err)
	}
	defer rows.Close()

	var apiTypes []string
	for rows.Next() {
		var t sql.NullString
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan api_type: %w", err)
		}
		if t.Valid && t.String != "" {
			apiTypes = append(apiTypes, t.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate api_types: %w", err)
	}

	// Releasing the read lock here is safe because GetLatestQuota re-acquires
	// it. We must release before the recursive call to avoid sql/database
	// connection-pool starvation when MaxOpenConns=1.
	out := make([]QuotaInfo, 0, len(apiTypes)+1)
	c.mu.RUnlock()
	for _, t := range apiTypes {
		q, err := c.GetLatestQuota(t)
		if err != nil {
			c.mu.RLock() // re-acquire so deferred RUnlock balances
			return nil, err
		}
		out = append(out, q)
	}
	c.mu.RLock() // re-acquire so deferred RUnlock balances

	out = append(out, QuotaInfo{
		Agent:              "opencode",
		APIType:            "local",
		Status5h:           "unlimited",
		Status7d:           "unlimited",
		EstimatedCostToday: ptrFloat(0),
		TotalCalls:         ptrInt64(0),
	})
	return out, nil
}

// GetUsageStats returns per-model rolled-up stats over the last `days`.
// Mirrors quota-tracker.ts getUsageStats.
func (c *CCSaver) GetUsageStats(apiType string, days int) ([]UsageStats, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if days <= 0 {
		days = 7
	}

	rows, err := c.db.Query(
		`SELECT model,
		        COUNT(*) AS calls,
		        COALESCE(SUM(input_tokens), 0) AS input_tokens,
		        COALESCE(SUM(output_tokens), 0) AS output_tokens,
		        COALESCE(SUM(estimated_cost_usd), 0) AS estimated_cost_usd,
		        COALESCE(AVG(duration_ms), 0) AS avg_duration_ms
		 FROM interactions
		 WHERE api_type = ? AND timestamp >= datetime('now', '-' || ? || ' days')
		 GROUP BY model`,
		apiType, days,
	)
	if err != nil {
		return nil, fmt.Errorf("query usage stats: %w", err)
	}
	defer rows.Close()

	var out []UsageStats
	for rows.Next() {
		var s UsageStats
		var modelNull sql.NullString
		if err := rows.Scan(&modelNull, &s.Calls, &s.InputTokens, &s.OutputTokens,
			&s.EstimatedCostUSD, &s.AvgDurationMs); err != nil {
			return nil, fmt.Errorf("scan usage stats: %w", err)
		}
		s.Model = modelNull.String
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetCostBreakdown returns aggregated cost rows grouped by `groupBy`, which
// must be "day", "model", or "api_type". Mirrors getCostBreakdown.
func (c *CCSaver) GetCostBreakdown(days int, groupBy string) ([]CostBreakdownRow, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if days <= 0 {
		days = 7
	}

	var groupCol string
	switch groupBy {
	case "day", "":
		groupCol = "date(timestamp)"
	case "model":
		groupCol = "model"
	case "api_type":
		groupCol = "api_type"
	default:
		return nil, fmt.Errorf("unsupported groupBy %q (want day|model|api_type)", groupBy)
	}

	// SQL injection guard: groupCol is from the switch above, never user input.
	query := fmt.Sprintf(
		`SELECT %s AS period,
		        COUNT(*) AS calls,
		        COALESCE(SUM(estimated_cost_usd), 0) AS total_cost,
		        COALESCE(SUM(input_tokens), 0) AS input_tokens,
		        COALESCE(SUM(output_tokens), 0) AS output_tokens
		 FROM interactions
		 WHERE timestamp >= datetime('now', '-' || ? || ' days')
		 GROUP BY period
		 ORDER BY period`, groupCol)

	rows, err := c.db.Query(query, days)
	if err != nil {
		return nil, fmt.Errorf("query cost breakdown: %w", err)
	}
	defer rows.Close()

	var out []CostBreakdownRow
	for rows.Next() {
		var row CostBreakdownRow
		var periodNull sql.NullString
		if err := rows.Scan(&periodNull, &row.Calls, &row.TotalCost,
			&row.InputTokens, &row.OutputTokens); err != nil {
			return nil, fmt.Errorf("scan cost breakdown: %w", err)
		}
		row.Period = periodNull.String
		out = append(out, row)
	}
	return out, rows.Err()
}

// GetClaudeRateLimits extracts the Anthropic rate-limit headers from the
// most recent qualifying interactions row. Returns nil if no row has real
// headers (the TS code excludes "null" imports via length(response_headers)>10).
// Mirrors getClaudeRateLimitsFromCCSaver.
func (c *CCSaver) GetClaudeRateLimits() *CCSaverRateLimits {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var headersJSON, ts sql.NullString
	row := c.db.QueryRow(
		`SELECT response_headers, timestamp
		 FROM interactions
		 WHERE api_type = 'anthropic' AND length(response_headers) > 10
		 ORDER BY id DESC
		 LIMIT 1`,
	)
	if err := row.Scan(&headersJSON, &ts); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			c.logger.WithError(err).Warn("ccsaver: claude rate limits query failed")
		}
		return nil
	}

	headers := parseHeaders(headersJSON.String)

	u5hStr := getHeaderValue(headers, "Anthropic-Ratelimit-Unified-5h-Utilization")
	u7dStr := getHeaderValue(headers, "Anthropic-Ratelimit-Unified-7d-Utilization")
	if u5hStr == "" && u7dStr == "" {
		return nil
	}

	u5h, _ := strconv.ParseFloat(u5hStr, 64)
	u7d, _ := strconv.ParseFloat(u7dStr, 64)

	r5h := getHeaderValue(headers, "Anthropic-Ratelimit-Unified-5h-Reset")
	r7d := getHeaderValue(headers, "Anthropic-Ratelimit-Unified-7d-Reset")
	s5h := getHeaderValue(headers, "Anthropic-Ratelimit-Unified-5h-Status")
	s7d := getHeaderValue(headers, "Anthropic-Ratelimit-Unified-7d-Status")
	if s5h == "" {
		s5h = "unknown"
	}
	if s7d == "" {
		s7d = "unknown"
	}

	return &CCSaverRateLimits{
		Utilization5h: u5h,
		Utilization7d: u7d,
		ResetTime5h:   epochSecsToISO(r5h),
		ResetTime7d:   epochSecsToISO(r7d),
		Status5h:      s5h,
		Status7d:      s7d,
		Timestamp:     ts.String,
	}
}

// GetGeminiUsage rolls up gemini-code-assist interactions over the last
// `days` window. Returns nil on failure (matches TS undefined return).
func (c *CCSaver) GetGeminiUsage(days int) *GeminiCCSaverTotals {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if days <= 0 {
		days = 1
	}
	offset := fmt.Sprintf("-%d days", days)

	rows, err := c.db.Query(
		`SELECT model,
		        COALESCE(input_tokens, 0)  AS in_tokens,
		        COALESCE(output_tokens, 0) AS out_tokens
		 FROM interactions
		 WHERE api_type = 'gemini-code-assist'
		   AND datetime(timestamp) >= datetime('now', ?)`,
		offset,
	)
	if err != nil {
		c.logger.WithError(err).Warn("ccsaver: gemini usage query failed")
		return nil
	}
	defer rows.Close()

	totals := &GeminiCCSaverTotals{}
	modelSet := make(map[string]struct{})
	for rows.Next() {
		var modelNull sql.NullString
		var inT, outT int64
		if err := rows.Scan(&modelNull, &inT, &outT); err != nil {
			c.logger.WithError(err).Warn("ccsaver: gemini scan failed")
			continue
		}
		totals.Calls++
		totals.Input += inT
		totals.Output += outT
		if modelNull.Valid && modelNull.String != "" {
			modelSet[modelNull.String] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		c.logger.WithError(err).Warn("ccsaver: gemini rows iteration failed")
	}
	for m := range modelSet {
		totals.Models = append(totals.Models, m)
	}
	return totals
}

// GetTokenTotalsByAPIType rolls up input/output tokens, call counts, and
// models per api_type over the last `days` window. This is the single source
// of agent token usage for GetAllAgentUsage — the per-agent CLI session-file
// scanners no longer feed token counts. Returns one entry per api_type that
// has at least one row in the window; api_types with no rows are simply
// absent from the map.
func (c *CCSaver) GetTokenTotalsByAPIType(days int) (map[string]AgentTokenTotals, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if days <= 0 {
		days = 1
	}
	// Compare the BARE timestamp column against a Go-computed cutoff so the
	// idx_timestamp index is usable. Wrapping the column in datetime(...) — as
	// the older GetGeminiUsage/GetUsageStats queries do — forces a full scan,
	// which is ~70s on the multi-GB workstation DB. The cutoff is formatted in
	// local time to match the proxy's stored RFC3339 timestamps (numeric
	// offset), so the lexicographic range scan lines up with the wall clock.
	cutoff := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	out := make(map[string]AgentTokenTotals)

	// Pass 1: per-api_type sums, call count, and the id of the most recent
	// row (resolved to a model below).
	maxID := make(map[string]int64)
	// INDEXED BY idx_timestamp forces the timestamp range scan; without it the
	// planner picks idx_api_type for the GROUP BY and walks every api_type's
	// full history (~33s on the workstation DB vs ~0.07s with the hint). Both
	// the live DB and the ccsaver-mirror snapshot define idx_timestamp.
	aggRows, err := c.db.Query(
		`SELECT api_type,
		        COALESCE(SUM(input_tokens), 0)  AS in_tokens,
		        COALESCE(SUM(output_tokens), 0) AS out_tokens,
		        COUNT(*)                        AS calls,
		        MAX(id)                         AS max_id
		 FROM interactions INDEXED BY idx_timestamp
		 WHERE timestamp >= ? AND api_type IS NOT NULL
		 GROUP BY api_type`,
		cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("query token totals: %w", err)
	}
	defer aggRows.Close()
	for aggRows.Next() {
		var apiType sql.NullString
		var maxIDNull sql.NullInt64
		var t AgentTokenTotals
		if err := aggRows.Scan(&apiType, &t.InputTokens, &t.OutputTokens, &t.Calls, &maxIDNull); err != nil {
			return nil, fmt.Errorf("scan token totals: %w", err)
		}
		if !apiType.Valid || apiType.String == "" {
			continue
		}
		out[apiType.String] = t
		if maxIDNull.Valid {
			maxID[apiType.String] = maxIDNull.Int64
		}
	}
	if err := aggRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate token totals: %w", err)
	}

	// Resolve the latest model per api_type from the MAX(id) rows in one
	// id-indexed lookup (≤ one row per api_type).
	for at, id := range maxID {
		var model sql.NullString
		err := c.db.QueryRow(`SELECT model FROM interactions WHERE id = ?`, id).Scan(&model)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, fmt.Errorf("query latest model: %w", err)
		}
		if model.Valid && model.String != "" {
			t := out[at]
			t.LatestModel = model.String
			out[at] = t
		}
	}

	// Pass 2: distinct models per api_type. Pre-sorted by the query so the
	// appended Models slices are stable.
	modRows, err := c.db.Query(
		`SELECT DISTINCT api_type, model FROM interactions INDEXED BY idx_timestamp
		 WHERE timestamp >= ?
		   AND api_type IS NOT NULL AND model IS NOT NULL AND model <> ''
		 ORDER BY api_type, model`,
		cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("query token models: %w", err)
	}
	defer modRows.Close()
	for modRows.Next() {
		var apiType, model sql.NullString
		if err := modRows.Scan(&apiType, &model); err != nil {
			return nil, fmt.Errorf("scan token models: %w", err)
		}
		if !apiType.Valid || apiType.String == "" {
			continue
		}
		// Only attach models to api_types that produced a totals row.
		if t, ok := out[apiType.String]; ok {
			t.Models = append(t.Models, model.String)
			out[apiType.String] = t
		}
	}
	if err := modRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate token models: %w", err)
	}

	return out, nil
}

// epochSecsToISO converts a Unix-epoch-seconds string to an ISO-8601 UTC
// timestamp, returning "" if the input cannot be parsed. The TS source
// `new Date(parseInt(reset) * 1000).toISOString()` is permissive — any
// numeric prefix wins. We replicate that with strconv.ParseInt on the trimmed
// value.
func epochSecsToISO(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// Fall back to ParseFloat so values like "1714521600.5" still resolve.
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return ""
		}
		v = int64(f)
	}
	// JS toISOString uses milliseconds with .sssZ suffix; Go's RFC3339Nano
	// is the closest match. Tests assert prefix only.
	return time.Unix(v, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}
