package budget

import (
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStatus is an in-memory statusProvider so the load-aware routing tests run
// without a Postgres-backed Tracker. It returns a fixed set of BudgetStatus
// rows — enough to exercise the scoring, concurrency hard-filter, and fallback
// paths deterministically.
type fakeStatus struct {
	rows []BudgetStatus
	err  error
}

func (f fakeStatus) Status() ([]BudgetStatus, error) { return f.rows, f.err }

// availableStatus builds an "everything available, nothing used" status row for
// a provider so headroom is full and budget filters never trip — isolating the
// load axis under test.
func availableStatus(p Provider, tier ProviderTier, dailyLimit, weeklyLimit int) BudgetStatus {
	return BudgetStatus{
		Provider:        p,
		Tier:            tier,
		DailyLimit:      dailyLimit,
		DailyRemaining:  dailyLimit,
		WeeklyLimit:     weeklyLimit,
		WeeklyRemaining: weeklyLimit,
		IsAvailable:     true,
	}
}

func quietLogger() *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.PanicLevel)
	return l
}

// loadTestRouter wires a Router over a fakeStatus with two cloud providers (one
// strong, one cheap) plus an uncapped local. Each cloud provider has a
// MaxConcurrency cap so the concurrency filter is exercisable.
func loadTestRouter(t *testing.T) (*Router, map[Provider]*ProviderConfig) {
	t.Helper()
	cfg := &ServiceConfig{
		Providers: []ProviderConfig{
			{
				Provider:       ProviderClaude,
				Tier:           TierCloud,
				Strength:       0.95,
				DailyLimit:     200000,
				WeeklyLimit:    1000000,
				DefaultModel:   "claude-sonnet",
				Models:         map[string]string{"high": "claude-sonnet", "critical": "claude-opus"},
				CostPerMToken:  3.0,
				MaxConcurrency: 2,
			},
			{
				Provider:       ProviderGemini,
				Tier:           TierCloud,
				Strength:       0.80,
				DailyLimit:     500000,
				WeeklyLimit:    2000000,
				DefaultModel:   "gemini-flash",
				CostPerMToken:  1.25,
				MaxConcurrency: 2,
			},
			{
				Provider:      ProviderLocal,
				Tier:          TierLocal,
				Strength:      0.45,
				DefaultModel:  "qwen-coder",
				CostPerMToken: 0.0,
			},
		},
	}
	configs := make(map[Provider]*ProviderConfig)
	for i := range cfg.Providers {
		pc := &cfg.Providers[i]
		configs[pc.Provider] = pc
	}
	fs := fakeStatus{rows: []BudgetStatus{
		availableStatus(ProviderClaude, TierCloud, 200000, 1000000),
		availableStatus(ProviderGemini, TierCloud, 500000, 2000000),
		availableStatus(ProviderLocal, TierLocal, 0, 0),
	}}
	return newRouterWithStatus(fs, configs, quietLogger()), configs
}

// TestRouter_ConcurrencyCapHardFilters verifies a provider at its concurrency
// cap is dropped from candidates even when it would otherwise win on score.
func TestRouter_ConcurrencyCapHardFilters(t *testing.T) {
	router, _ := loadTestRouter(t)

	// Without load, an immediate critical task picks claude (strongest).
	base, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityCritical,
		EstimatedTokens: 5000,
		Priority:        PriorityImmediate,
	})
	require.NoError(t, err)
	require.Equal(t, ProviderClaude, base.Provider)

	// With claude at its cap (2/2 in flight), it must be filtered out and the
	// next-best cloud provider (gemini) wins instead.
	resp, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityCritical,
		EstimatedTokens: 5000,
		Priority:        PriorityImmediate,
		ProviderLoad:    map[Provider]int{ProviderClaude: 2},
	})
	require.NoError(t, err)
	assert.NotEqual(t, ProviderClaude, resp.Provider, "claude at cap must be excluded")
	assert.Contains(t, []Provider{ProviderGemini, ProviderLocal}, resp.Provider)
}

// TestRouter_AllCloudAtCapSelectsLocal verifies that when every cloud provider
// is at its concurrency cap, the router still returns the uncapped local
// provider (which remains a normal candidate) rather than the false "all
// exhausted" error. Local is selected through the ordinary scoring path here —
// it's never been filtered out — so no special fallback warning is expected.
func TestRouter_AllCloudAtCapSelectsLocal(t *testing.T) {
	router, _ := loadTestRouter(t)

	resp, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityHigh,
		EstimatedTokens: 5000,
		ProviderLoad: map[Provider]int{
			ProviderClaude: 2,
			ProviderGemini: 2,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, ProviderLocal, resp.Provider)
	// Cloud providers were filtered, so local has no cloud fallback peer.
	assert.Empty(t, resp.FallbackProvider)
}

// TestRouter_AllCloudAtCapFallbackReasonWhenLocalNotCandidate verifies the
// distinct "at concurrency cap" fallback reason fires when local exists but
// can't be a normal candidate (require_cloud filters it from scoring), so the
// router reaches the empty-candidates branch and must choose the right message.
func TestRouter_AllCloudAtCapFallbackReasonWhenLocalNotCandidate(t *testing.T) {
	router, _ := loadTestRouter(t)

	resp, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityHigh,
		EstimatedTokens: 5000,
		RequireCloud:    true,
		ProviderLoad: map[Provider]int{
			ProviderClaude: 2,
			ProviderGemini: 2,
		},
	})
	// require_cloud removes local from candidates, every cloud provider is
	// capped → empty candidates. Local fallback in the empty branch ignores the
	// require_cloud constraint by design (it's a last resort), so we still get
	// local but with the concurrency-cap reason/warning.
	require.NoError(t, err)
	assert.Equal(t, ProviderLocal, resp.Provider)
	assert.Contains(t, resp.Reason, "concurrency cap")
	assert.Contains(t, resp.BudgetWarning, "concurrency cap")
}

// TestRouter_AllCloudAtCapNoLocalRejects verifies that when all cloud providers
// are capped AND no local fallback exists, the router rejects with a
// concurrency-specific error (not the generic exhausted message).
func TestRouter_AllCloudAtCapNoLocalRejects(t *testing.T) {
	cfg := &ServiceConfig{
		Providers: []ProviderConfig{
			{Provider: ProviderClaude, Tier: TierCloud, Strength: 0.9, DailyLimit: 1, WeeklyLimit: 1, DefaultModel: "c", MaxConcurrency: 1},
			{Provider: ProviderGemini, Tier: TierCloud, Strength: 0.8, DailyLimit: 1, WeeklyLimit: 1, DefaultModel: "g", MaxConcurrency: 1},
		},
	}
	configs := make(map[Provider]*ProviderConfig)
	for i := range cfg.Providers {
		configs[cfg.Providers[i].Provider] = &cfg.Providers[i]
	}
	fs := fakeStatus{rows: []BudgetStatus{
		availableStatus(ProviderClaude, TierCloud, 1, 1),
		availableStatus(ProviderGemini, TierCloud, 1, 1),
	}}
	router := newRouterWithStatus(fs, configs, quietLogger())

	_, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityHigh,
		EstimatedTokens: 1,
		ProviderLoad:    map[Provider]int{ProviderClaude: 1, ProviderGemini: 1},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "concurrency cap")
}

// TestRouter_LoadPenaltyBreaksTies verifies that between two otherwise-tied
// providers, the less-loaded one wins once a queue backlog raises the load
// emphasis.
func TestRouter_LoadPenaltyBreaksTies(t *testing.T) {
	cfg := &ServiceConfig{
		Providers: []ProviderConfig{
			{Provider: ProviderClaude, Tier: TierCloud, Strength: 0.85, DailyLimit: 200000, WeeklyLimit: 1000000, DefaultModel: "c", CostPerMToken: 2.5, MaxConcurrency: 10},
			{Provider: ProviderOpenAI, Tier: TierCloud, Strength: 0.85, DailyLimit: 200000, WeeklyLimit: 1000000, DefaultModel: "o", CostPerMToken: 2.5, MaxConcurrency: 10},
		},
	}
	configs := make(map[Provider]*ProviderConfig)
	for i := range cfg.Providers {
		configs[cfg.Providers[i].Provider] = &cfg.Providers[i]
	}
	fs := fakeStatus{rows: []BudgetStatus{
		availableStatus(ProviderClaude, TierCloud, 200000, 1000000),
		availableStatus(ProviderOpenAI, TierCloud, 200000, 1000000),
	}}
	router := newRouterWithStatus(fs, configs, quietLogger())

	// No load: lex tiebreaker picks claude.
	tie, err := router.Route(RouteRequest{TaskComplexity: ComplexityHigh, EstimatedTokens: 1000})
	require.NoError(t, err)
	require.Equal(t, ProviderClaude, tie.Provider)

	// Claude heavily loaded + a deep queue: openai (idle) must overtake it.
	resp, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityHigh,
		EstimatedTokens: 1000,
		QueueDepth:      20,
		ProviderLoad:    map[Provider]int{ProviderClaude: 9},
	})
	require.NoError(t, err)
	assert.Equal(t, ProviderOpenAI, resp.Provider, "less-loaded provider should win under backlog")
}

// TestRouter_NoLoadInfoPreservesBehavior verifies that omitting ProviderLoad and
// QueueDepth reproduces the pre-load routing decision exactly.
func TestRouter_NoLoadInfoPreservesBehavior(t *testing.T) {
	router, _ := loadTestRouter(t)

	withLoad, err := router.Route(RouteRequest{TaskComplexity: ComplexityHigh, EstimatedTokens: 5000})
	require.NoError(t, err)

	// Same request, explicit empty load — must be identical.
	withEmpty, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityHigh,
		EstimatedTokens: 5000,
		ProviderLoad:    map[Provider]int{},
		QueueDepth:      0,
	})
	require.NoError(t, err)
	assert.Equal(t, withLoad.Provider, withEmpty.Provider)
	assert.InDelta(t, withLoad.Confidence, withEmpty.Confidence, 1e-9)
}

// TestRouter_ConcurrencyWarningWhenWinnerNearCap verifies the winner emits a
// concurrency warning once it crosses 80% of its cap, when no budget warning
// takes the slot.
func TestRouter_ConcurrencyWarningWhenWinnerNearCap(t *testing.T) {
	cfg := &ServiceConfig{
		Providers: []ProviderConfig{
			{Provider: ProviderLocal, Tier: TierLocal, Strength: 0.45, DefaultModel: "qwen", CostPerMToken: 0, MaxConcurrency: 5},
		},
	}
	configs := map[Provider]*ProviderConfig{ProviderLocal: &cfg.Providers[0]}
	fs := fakeStatus{rows: []BudgetStatus{availableStatus(ProviderLocal, TierLocal, 0, 0)}}
	router := newRouterWithStatus(fs, configs, quietLogger())

	// 4/5 in flight = 80%, warning fires; local still selected (below cap).
	resp, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityLow,
		EstimatedTokens: 100,
		ProviderLoad:    map[Provider]int{ProviderLocal: 4},
	})
	require.NoError(t, err)
	assert.Equal(t, ProviderLocal, resp.Provider)
	assert.Contains(t, resp.BudgetWarning, "4/5 concurrent")
}

// TestRouter_StatusErrorPropagates verifies a Status() failure surfaces as an
// error rather than a silent empty recommendation.
func TestRouter_StatusErrorPropagates(t *testing.T) {
	fs := fakeStatus{err: assertErr("boom")}
	router := newRouterWithStatus(fs, map[Provider]*ProviderConfig{}, quietLogger())
	_, err := router.Route(RouteRequest{TaskComplexity: ComplexityHigh, EstimatedTokens: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

// TestLoadHelpers exercises the pure load-scoring helpers directly.
func TestLoadHelpers(t *testing.T) {
	// loadScoreFor
	assert.Equal(t, 1.0, loadScoreFor(0, 0), "uncapped is always free")
	assert.Equal(t, 1.0, loadScoreFor(5, 0), "uncapped is always free even when loaded")
	assert.Equal(t, 1.0, loadScoreFor(0, 4), "empty cap is fully free")
	assert.Equal(t, 0.5, loadScoreFor(2, 4), "half full")
	assert.Equal(t, 0.0, loadScoreFor(4, 4), "at cap is zero free")
	assert.Equal(t, 0.0, loadScoreFor(9, 4), "over cap clamps to zero")
	assert.Equal(t, 1.0, loadScoreFor(-3, 4), "negative load clamps to empty")

	// loadEmphasisFor rises with queue depth, then clamps.
	assert.Equal(t, baseLoadEmphasis, loadEmphasisFor(0))
	assert.Equal(t, baseLoadEmphasis, loadEmphasisFor(-5))
	assert.Greater(t, loadEmphasisFor(10), loadEmphasisFor(0))
	assert.Equal(t, maxLoadEmphasis, loadEmphasisFor(20))
	assert.Equal(t, maxLoadEmphasis, loadEmphasisFor(1000), "clamps at full scale")
}

// assertErr is a tiny error type for the Status() failure test.
type assertErr string

func (e assertErr) Error() string { return string(e) }
