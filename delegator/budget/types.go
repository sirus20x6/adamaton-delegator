package budget

import (
	"fmt"
	"time"
)

// Provider identifies an LLM provider.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderOpenAI Provider = "openai"
	ProviderGemini Provider = "gemini"
	ProviderLocal  Provider = "local"
)

// ValidProviders is the set of recognized provider identifiers.
var ValidProviders = map[Provider]bool{
	ProviderClaude: true,
	ProviderOpenAI: true,
	ProviderGemini: true,
	ProviderLocal:  true,
}

// TaskComplexity categorizes the difficulty of a task.
type TaskComplexity string

const (
	ComplexityLow      TaskComplexity = "low"
	ComplexityMedium   TaskComplexity = "medium"
	ComplexityHigh     TaskComplexity = "high"
	ComplexityCritical TaskComplexity = "critical"
)

// ValidComplexities is the set of recognized complexity levels.
var ValidComplexities = map[TaskComplexity]bool{
	ComplexityLow:      true,
	ComplexityMedium:   true,
	ComplexityHigh:     true,
	ComplexityCritical: true,
}

// Priority signals how time-sensitive a task is. The router uses it to
// shift the cost weight: immediate tasks accept higher cost for headroom,
// background tasks bias toward cheap providers.
type Priority string

const (
	PriorityImmediate  Priority = "immediate"
	PriorityNormal     Priority = "normal"
	PriorityBackground Priority = "background"
)

// ValidPriorities is the set of recognized priority levels.
var ValidPriorities = map[Priority]bool{
	PriorityImmediate:  true,
	PriorityNormal:     true,
	PriorityBackground: true,
}

// ProviderTier classifies a provider's deployment model.
type ProviderTier string

const (
	TierCloud ProviderTier = "cloud"
	TierLocal ProviderTier = "local"
)

// --- API Request / Response Types ---

// RouteRequest asks the router to recommend a provider for a task.
type RouteRequest struct {
	TaskComplexity  TaskComplexity `json:"task_complexity"`
	EstimatedTokens int            `json:"estimated_tokens"`
	RequireCloud    bool           `json:"require_cloud,omitempty"`
	PreferProvider  Provider       `json:"prefer_provider,omitempty"`
	// Priority shifts the cost/headroom weighting. Empty defaults to PriorityNormal.
	Priority Priority `json:"priority,omitempty"`
}

// RouteResponse contains the router's recommendation.
type RouteResponse struct {
	Provider         Provider `json:"provider"`
	Model            string   `json:"model"`
	Reason           string   `json:"reason"`
	FallbackProvider Provider `json:"fallback_provider,omitempty"`
	BudgetWarning    string   `json:"budget_warning,omitempty"`
	Confidence       float64  `json:"confidence"`
}

// ReportRequest records token usage after a task completes.
type ReportRequest struct {
	Provider         Provider       `json:"provider"`
	Model            string         `json:"model,omitempty"`
	PromptTokens     int            `json:"prompt_tokens,omitempty"`
	CompletionTokens int            `json:"completion_tokens,omitempty"`
	TotalTokens      int            `json:"total_tokens"`
	TaskID           string         `json:"task_id,omitempty"`
	TaskComplexity   TaskComplexity `json:"task_complexity,omitempty"`
	Success          *bool          `json:"success,omitempty"`
}

// ReportResponse acknowledges a usage report and returns budget state.
type ReportResponse struct {
	Recorded        bool   `json:"recorded"`
	DailyRemaining  int    `json:"daily_remaining"`
	WeeklyRemaining int    `json:"weekly_remaining"`
	BudgetWarning   string `json:"budget_warning,omitempty"`
}

// BudgetStatus describes one provider's current budget state.
type BudgetStatus struct {
	Provider        Provider     `json:"provider"`
	Tier            ProviderTier `json:"tier"`
	DailyUsed       int          `json:"daily_used"`
	DailyLimit      int          `json:"daily_limit"`
	DailyRemaining  int          `json:"daily_remaining"`
	DailyPct        float64      `json:"daily_pct"`
	WeeklyUsed      int          `json:"weekly_used"`
	WeeklyLimit     int          `json:"weekly_limit"`
	WeeklyRemaining int          `json:"weekly_remaining"`
	WeeklyPct       float64      `json:"weekly_pct"`
	IsAvailable     bool         `json:"is_available"`
	ErrorCount      int          `json:"error_count"`
	DailyResetAt    time.Time    `json:"daily_reset_at"`
	WeeklyResetAt   time.Time    `json:"weekly_reset_at"`
}

// UsageRecord is an immutable ledger entry for history queries.
type UsageRecord struct {
	ID               int64          `json:"id"`
	Provider         Provider       `json:"provider"`
	Model            string         `json:"model"`
	PromptTokens     int            `json:"prompt_tokens"`
	CompletionTokens int            `json:"completion_tokens"`
	TotalTokens      int            `json:"total_tokens"`
	TaskID           string         `json:"task_id"`
	TaskComplexity   TaskComplexity `json:"task_complexity"`
	Success          bool           `json:"success"`
	RecordedAt       time.Time      `json:"recorded_at"`
}

// APIResponse is the standard response envelope.
type APIResponse struct {
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
	Success bool        `json:"success"`
}

// --- Configuration Types ---

// ServiceConfig is the top-level configuration for the budget router.
//
// DSN is the postgres connection string. The legacy DBPath field (sqlite
// file) was removed in the postgres migration — operators with an older
// budget.yaml must switch to a postgres DSN.
type ServiceConfig struct {
	ListenAddr     string           `mapstructure:"listen_addr"`
	DSN            string           `mapstructure:"dsn"`
	DailyResetHour int              `mapstructure:"daily_reset_hour"`
	WeeklyResetDay int              `mapstructure:"weekly_reset_day"` // 0=Sunday, 1=Monday, ...
	Timezone       string           `mapstructure:"timezone"`
	LogLevel       string           `mapstructure:"log_level"`
	APIToken       string           `mapstructure:"api_token"`
	Providers      []ProviderConfig `mapstructure:"providers"`
}

// Validate checks the service configuration for consistency. Returns the
// first encountered error, or nil if everything looks reasonable.
func (sc *ServiceConfig) Validate() error {
	if sc == nil {
		return fmt.Errorf("nil ServiceConfig")
	}
	if sc.DailyResetHour < 0 || sc.DailyResetHour > 23 {
		return fmt.Errorf("daily_reset_hour must be 0-23, got %d", sc.DailyResetHour)
	}
	if sc.WeeklyResetDay < 0 || sc.WeeklyResetDay > 6 {
		return fmt.Errorf("weekly_reset_day must be 0-6 (Sunday=0), got %d", sc.WeeklyResetDay)
	}
	if sc.Timezone == "" {
		return fmt.Errorf("timezone is required")
	}
	if _, err := time.LoadLocation(sc.Timezone); err != nil {
		return fmt.Errorf("timezone %q is not loadable: %w", sc.Timezone, err)
	}

	seen := make(map[Provider]bool, len(sc.Providers))
	for i := range sc.Providers {
		pc := &sc.Providers[i]
		if err := pc.Validate(); err != nil {
			return fmt.Errorf("providers[%d] (%s): %w", i, pc.Provider, err)
		}
		if seen[pc.Provider] {
			return fmt.Errorf("duplicate provider %q", pc.Provider)
		}
		seen[pc.Provider] = true
	}
	return nil
}

// ProviderConfig describes one LLM provider's limits and capabilities.
type ProviderConfig struct {
	Provider      Provider          `mapstructure:"provider"`
	Tier          ProviderTier      `mapstructure:"tier"`
	Strength      float64           `mapstructure:"strength"`     // 0.0-1.0
	DailyLimit    int               `mapstructure:"daily_limit"`  // 0 = unlimited (when Unlimited=true) or hard zero cap
	WeeklyLimit   int               `mapstructure:"weekly_limit"` // 0 = unlimited (when Unlimited=true) or hard zero cap
	DefaultModel  string            `mapstructure:"default_model"`
	Models        map[string]string `mapstructure:"models"` // complexity -> model
	CostPerMToken float64           `mapstructure:"cost_per_m_token"`
	// Unlimited explicitly marks a provider as having no budget cap. When true,
	// DailyLimit / WeeklyLimit are ignored by router and tracker logic.
	Unlimited bool `mapstructure:"unlimited"`
}

// Validate checks a provider config for sane values.
func (pc *ProviderConfig) Validate() error {
	if pc == nil {
		return fmt.Errorf("nil ProviderConfig")
	}
	if !ValidProviders[pc.Provider] {
		return fmt.Errorf("unknown provider %q", pc.Provider)
	}
	if pc.Tier != TierCloud && pc.Tier != TierLocal {
		return fmt.Errorf("tier must be %q or %q, got %q", TierCloud, TierLocal, pc.Tier)
	}
	if pc.Strength < 0.0 || pc.Strength > 1.0 {
		return fmt.Errorf("strength must be 0.0-1.0, got %f", pc.Strength)
	}
	if pc.CostPerMToken < 0 {
		return fmt.Errorf("cost_per_m_token must be >= 0, got %f", pc.CostPerMToken)
	}
	if pc.DailyLimit < 0 {
		return fmt.Errorf("daily_limit must be >= 0, got %d", pc.DailyLimit)
	}
	if pc.WeeklyLimit < 0 {
		return fmt.Errorf("weekly_limit must be >= 0, got %d", pc.WeeklyLimit)
	}
	return nil
}

// ModelForComplexity returns the best model for the given complexity,
// falling back to DefaultModel.
func (pc *ProviderConfig) ModelForComplexity(c TaskComplexity) string {
	if pc.Models != nil {
		if m, ok := pc.Models[string(c)]; ok {
			return m
		}
	}
	return pc.DefaultModel
}

// IsUnlimited returns true if the provider has no budget cap. A provider is
// considered unlimited if either the explicit Unlimited flag is set, or both
// DailyLimit and WeeklyLimit are zero (legacy semantics).
func (pc *ProviderConfig) IsUnlimited() bool {
	if pc.Unlimited {
		return true
	}
	return pc.DailyLimit == 0 && pc.WeeklyLimit == 0
}
