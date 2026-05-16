// Package quota ports the delegator's agent-usage and quota-tracker readers
// from TypeScript to Go. It scrapes per-agent JSONL session logs and the
// CCSAVER SQLite database to surface usage and rate-limit information.
//
// The package intentionally mirrors the TS data shapes — the goal is parity,
// not redesign. JSON tags match the TS field names so existing dashboard
// consumers can decode either implementation interchangeably.
package quota

// QuotaInfo mirrors the TypeScript QuotaInfo shape used by quota-tracker.ts.
// Optional numeric fields use *float64 so that omitempty + nil pointers
// correctly distinguish "no data" from "zero".
type QuotaInfo struct {
	Agent              string   `json:"agent"`
	APIType            string   `json:"apiType"`
	Utilization5h      *float64 `json:"utilization5h,omitempty"`
	Utilization7d     *float64 `json:"utilization7d,omitempty"`
	ResetTime5h        string   `json:"resetTime5h,omitempty"`
	ResetTime7d        string   `json:"resetTime7d,omitempty"`
	Status5h           string   `json:"status5h,omitempty"`
	Status7d           string   `json:"status7d,omitempty"`
	EstimatedCostToday *float64 `json:"estimatedCostToday,omitempty"`
	TotalCalls         *int64   `json:"totalCalls,omitempty"`
}

// UsageStats mirrors the TS UsageStats shape from types.ts.
type UsageStats struct {
	Model            string  `json:"model"`
	Calls            int64   `json:"calls"`
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	EstimatedCostUSD float64 `json:"estimatedCostUsd"`
	AvgDurationMs    float64 `json:"avgDurationMs"`
}

// AgentUsage is the unified per-agent summary returned by GetAllAgentUsage,
// matching TS AgentUsageSummary. Pointer fields stand in for TS optional
// fields so the JSON shape lines up.
type AgentUsage struct {
	Agent         string   `json:"agent"`
	APIType       string   `json:"apiType"`
	Sessions      int64    `json:"sessions"`
	InputTokens   int64    `json:"inputTokens"`
	OutputTokens  int64    `json:"outputTokens"`
	Model         string   `json:"model"`
	Utilization5h *float64 `json:"utilization5h,omitempty"`
	Utilization7d *float64 `json:"utilization7d,omitempty"`
	ResetTime5h   string   `json:"resetTime5h,omitempty"`
	ResetTime7d   string   `json:"resetTime7d,omitempty"`
}

// ClaudeUsage matches the TS ClaudeUsage interface from agent-usage.ts.
type ClaudeUsage struct {
	Sessions                  int64    `json:"sessions"`
	TotalInputTokens          int64    `json:"totalInputTokens"`
	TotalOutputTokens         int64    `json:"totalOutputTokens"`
	TotalCacheCreationTokens  int64    `json:"totalCacheCreationTokens"`
	TotalCacheReadTokens      int64    `json:"totalCacheReadTokens"`
	MessageCount              int64    `json:"messageCount"`
	ToolCallCount             int64    `json:"toolCallCount"`
	Utilization5h             *float64 `json:"utilization5h,omitempty"`
	Utilization7d             *float64 `json:"utilization7d,omitempty"`
	ResetTime5h               string   `json:"resetTime5h,omitempty"`
	ResetTime7d               string   `json:"resetTime7d,omitempty"`
}

// CodexUsage matches the TS CodexUsage interface.
type CodexUsage struct {
	Sessions             int64    `json:"sessions"`
	TotalInputTokens     int64    `json:"totalInputTokens"`
	TotalOutputTokens    int64    `json:"totalOutputTokens"`
	TotalReasoningTokens int64    `json:"totalReasoningTokens"`
	Model                string   `json:"model"`
	Utilization5h        *float64 `json:"utilization5h,omitempty"`
	Utilization7d        *float64 `json:"utilization7d,omitempty"`
	ResetTime5h          string   `json:"resetTime5h,omitempty"`
	ResetTime7d          string   `json:"resetTime7d,omitempty"`
}

// GeminiUsage matches the TS GeminiUsage interface.
type GeminiUsage struct {
	Sessions            int64    `json:"sessions"`
	TotalInputTokens    int64    `json:"totalInputTokens"`
	TotalOutputTokens   int64    `json:"totalOutputTokens"`
	TotalThoughtTokens  int64    `json:"totalThoughtTokens"`
	TotalCachedTokens   int64    `json:"totalCachedTokens"`
	Models              []string `json:"models"`
}

// OpenCodeUsage is a stub — opencode is local + unlimited.
type OpenCodeUsage struct {
	Sessions     int64  `json:"sessions"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	Model        string `json:"model"`
}

// GeminiQuotaInfo is the live-quota result from the Cloud Code OAuth API.
type GeminiQuotaInfo struct {
	Models          []GeminiQuotaModel `json:"models"`
	LowestRemaining float64            `json:"lowestRemaining"`
	ResetTime       string             `json:"resetTime,omitempty"`
}

// GeminiQuotaModel is one row inside GeminiQuotaInfo.Models.
type GeminiQuotaModel struct {
	ModelID            string  `json:"modelId"`
	RemainingFraction  float64 `json:"remainingFraction"`
	ResetTime          string  `json:"resetTime,omitempty"`
}

// GeminiRateLimitsReport is the input to ReportGeminiRateLimits — manual
// reports from Gemini-CLI stdout that complement the live OAuth quota path.
type GeminiRateLimitsReport struct {
	UtilizationDaily *float64                            `json:"utilizationDaily,omitempty"`
	ResetTimeDaily   string                              `json:"resetTimeDaily,omitempty"`
	Models           map[string]GeminiRateLimitModelInfo `json:"models,omitempty"`
}

// GeminiRateLimitModelInfo mirrors the per-model bucket in the TS report.
type GeminiRateLimitModelInfo struct {
	Requests  int64 `json:"requests"`
	UsageLeft int64 `json:"usageLeft"`
}

// CCSaverRateLimits is the parsed Anthropic rate-limit headers extracted from
// the most recent CCSAVER interactions row.
type CCSaverRateLimits struct {
	Utilization5h float64 `json:"utilization5h"`
	Utilization7d float64 `json:"utilization7d"`
	ResetTime5h   string  `json:"resetTime5h"`
	ResetTime7d   string  `json:"resetTime7d"`
	Status5h      string  `json:"status5h"`
	Status7d      string  `json:"status7d"`
	Timestamp     string  `json:"timestamp"`
}

// GeminiCCSaverTotals aggregates gemini-code-assist interactions.
type GeminiCCSaverTotals struct {
	Input  int64
	Output int64
	Calls  int64
	Models []string
}

// CostBreakdownRow is one bucket from GetCostBreakdown — mirrors the TS
// generic Record<string, unknown>.
type CostBreakdownRow struct {
	Period       string  `json:"period"`
	Calls        int64   `json:"calls"`
	TotalCost    float64 `json:"total_cost"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

// ptrFloat returns a pointer to the given float64. Helper for building optional
// fields. Cannot be a generic helper because Go doesn't like &literal.
func ptrFloat(v float64) *float64 { return &v }

// ptrInt64 returns a pointer to the given int64.
func ptrInt64(v int64) *int64 { return &v }
