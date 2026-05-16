package budget

import (
	"os"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sirus20x6/adamaton-core/pgutil"
)

func newTestRouter(t *testing.T) *Router {
	t.Helper()
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	store, err := NewStore(pgutil.TestDSN(t), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	config := &ServiceConfig{
		DailyResetHour: 0,
		WeeklyResetDay: 1,
		Timezone:       "UTC",
		Providers: []ProviderConfig{
			{
				Provider:      ProviderClaude,
				Tier:          TierCloud,
				Strength:      0.95,
				DailyLimit:    200000,
				WeeklyLimit:   1000000,
				DefaultModel:  "claude-sonnet",
				Models:        map[string]string{"critical": "claude-opus", "high": "claude-sonnet", "medium": "claude-haiku", "low": "claude-haiku"},
				CostPerMToken: 3.00,
			},
			{
				Provider:      ProviderOpenAI,
				Tier:          TierCloud,
				Strength:      0.85,
				DailyLimit:    300000,
				WeeklyLimit:   1500000,
				DefaultModel:  "gpt-4o",
				Models:        map[string]string{"critical": "gpt-4o", "high": "gpt-4o", "medium": "gpt-4o-mini", "low": "gpt-4o-mini"},
				CostPerMToken: 2.50,
			},
			{
				Provider:      ProviderGemini,
				Tier:          TierCloud,
				Strength:      0.80,
				DailyLimit:    500000,
				WeeklyLimit:   2000000,
				DefaultModel:  "gemini-flash",
				Models:        map[string]string{"critical": "gemini-pro", "high": "gemini-flash", "medium": "gemini-flash", "low": "gemini-flash-lite"},
				CostPerMToken: 1.25,
			},
			{
				Provider:      ProviderLocal,
				Tier:          TierLocal,
				Strength:      0.45,
				DailyLimit:    0,
				WeeklyLimit:   0,
				DefaultModel:  "qwen-coder",
				CostPerMToken: 0.0,
			},
		},
	}

	tracker, err := NewTracker(store, config, logger)
	require.NoError(t, err)
	t.Cleanup(func() { tracker.Stop() })

	providerConfigs := make(map[Provider]*ProviderConfig)
	for i := range config.Providers {
		pc := &config.Providers[i]
		providerConfigs[pc.Provider] = pc
	}

	return NewRouter(tracker, providerConfigs, logger)
}

func TestRouter_CriticalTask(t *testing.T) {
	router := newTestRouter(t)

	resp, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityCritical,
		EstimatedTokens: 5000,
	})
	require.NoError(t, err)

	// Critical tasks should prefer the strongest provider (claude, strength=0.95)
	assert.Equal(t, ProviderClaude, resp.Provider)
	assert.Equal(t, "claude-opus", resp.Model)
	assert.NotEmpty(t, resp.Reason)
}

func TestRouter_LowTask(t *testing.T) {
	router := newTestRouter(t)

	resp, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityLow,
		EstimatedTokens: 500,
	})
	require.NoError(t, err)

	// Low tasks should prefer cost-efficient (local, cost=0)
	assert.Equal(t, ProviderLocal, resp.Provider)
	assert.Equal(t, "qwen-coder", resp.Model)
}

func TestRouter_RequireCloud(t *testing.T) {
	router := newTestRouter(t)

	resp, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityLow,
		EstimatedTokens: 500,
		RequireCloud:    true,
	})
	require.NoError(t, err)

	// Should not pick local
	assert.NotEqual(t, ProviderLocal, resp.Provider)
}

func TestRouter_PreferProvider(t *testing.T) {
	router := newTestRouter(t)

	resp, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityMedium,
		EstimatedTokens: 2000,
		PreferProvider:  ProviderGemini,
	})
	require.NoError(t, err)

	// Gemini gets +0.1 bonus on medium where cost matters a lot
	// With cost weight=0.4, gemini (cheapest cloud) plus preference bonus should win
	assert.Equal(t, ProviderGemini, resp.Provider)
}

func TestRouter_BudgetExhausted(t *testing.T) {
	router := newTestRouter(t)

	// Request more tokens than any single cloud provider has daily
	resp, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityHigh,
		EstimatedTokens: 600000, // exceeds all cloud daily limits
	})
	require.NoError(t, err)

	// Should fall back to local (unlimited)
	assert.Equal(t, ProviderLocal, resp.Provider)
}

func TestRouter_FallbackProvider(t *testing.T) {
	router := newTestRouter(t)

	resp, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityHigh,
		EstimatedTokens: 5000,
	})
	require.NoError(t, err)

	// Should have a fallback
	assert.NotEmpty(t, resp.FallbackProvider)
	assert.NotEqual(t, resp.Provider, resp.FallbackProvider)
}

func TestRouter_MediumTask(t *testing.T) {
	router := newTestRouter(t)

	resp, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityMedium,
		EstimatedTokens: 2000,
	})
	require.NoError(t, err)

	// Medium tasks: cost weight=0.4, so local (cost=0, costScore=1.0) should dominate
	assert.Equal(t, ProviderLocal, resp.Provider)
}

func TestRouter_LocalFallbackRespectsAvailability(t *testing.T) {
	// Build a router where local is the only configured provider AND its
	// status row is forcibly marked unavailable. Routing a cloud-required
	// task must NOT silently recommend local.
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	store, err := NewStore(pgutil.TestDSN(t), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	cfg := &ServiceConfig{
		DailyResetHour: 0,
		WeeklyResetDay: 1,
		Timezone:       "UTC",
		Providers: []ProviderConfig{
			{
				Provider:      ProviderLocal,
				Tier:          TierLocal,
				Strength:      0.45,
				DailyLimit:    0,
				WeeklyLimit:   0,
				DefaultModel:  "qwen-coder",
				CostPerMToken: 0.0,
			},
		},
	}
	tracker, err := NewTracker(store, cfg, logger)
	require.NoError(t, err)
	t.Cleanup(func() { tracker.Stop() })

	// Force local unavailable by recording 6 failures.
	for i := 0; i < 6; i++ {
		require.NoError(t, store.RecordUsage(UsageRecord{
			Provider:    ProviderLocal,
			TotalTokens: 100,
			Success:     false,
			RecordedAt:  time.Now().UTC(),
		}))
	}

	configs := map[Provider]*ProviderConfig{ProviderLocal: &cfg.Providers[0]}
	router := NewRouter(tracker, configs, logger)

	// RequireCloud forces the candidates list to be empty (local is the
	// only provider and it's not cloud). With local unavailable, the
	// fallback must refuse.
	_, err = router.Route(RouteRequest{
		TaskComplexity:  ComplexityHigh,
		EstimatedTokens: 100,
		RequireCloud:    true,
	})
	require.Error(t, err)
}

// TestRouter_DeterministicOnTiedScores constructs a router with two cloud
// providers tuned to score identically (same strength, same cost, same
// limits, no usage history → same headroom) and asserts that 10 successive
// Route() calls all return the same Provider/FallbackProvider. Before the
// sort.SliceStable + sorted-keys fix, Go's randomized map iteration would
// flap the result across calls on identical input.
func TestRouter_DeterministicOnTiedScores(t *testing.T) {
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	store, err := NewStore(pgutil.TestDSN(t), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// Two providers with identical scoring inputs — capability, cost, and
	// limits are all equal, so headroom is also equal at zero usage. They
	// MUST tie on score.
	cfg := &ServiceConfig{
		DailyResetHour: 0,
		WeeklyResetDay: 1,
		Timezone:       "UTC",
		Providers: []ProviderConfig{
			{
				Provider:      ProviderClaude,
				Tier:          TierCloud,
				Strength:      0.85,
				DailyLimit:    200000,
				WeeklyLimit:   1000000,
				DefaultModel:  "claude-sonnet",
				CostPerMToken: 2.50,
			},
			{
				Provider:      ProviderOpenAI,
				Tier:          TierCloud,
				Strength:      0.85,
				DailyLimit:    200000,
				WeeklyLimit:   1000000,
				DefaultModel:  "gpt-4o",
				CostPerMToken: 2.50,
			},
		},
	}

	tracker, err := NewTracker(store, cfg, logger)
	require.NoError(t, err)
	t.Cleanup(func() { tracker.Stop() })

	configs := make(map[Provider]*ProviderConfig)
	for i := range cfg.Providers {
		pc := &cfg.Providers[i]
		configs[pc.Provider] = pc
	}
	router := NewRouter(tracker, configs, logger)

	req := RouteRequest{
		TaskComplexity:  ComplexityHigh,
		EstimatedTokens: 1000,
	}

	first, err := router.Route(req)
	require.NoError(t, err)
	require.NotNil(t, first)

	for i := 0; i < 10; i++ {
		resp, err := router.Route(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, first.Provider, resp.Provider,
			"provider must be deterministic across calls on tied scores (call %d)", i+1)
		assert.Equal(t, first.FallbackProvider, resp.FallbackProvider,
			"fallback must be deterministic across calls on tied scores (call %d)", i+1)
	}

	// Stronger guarantee: with two tied cloud providers, lex order picks
	// "claude" over "openai".
	assert.Equal(t, ProviderClaude, first.Provider, "lex tiebreaker must pick claude over openai")
	assert.Equal(t, ProviderOpenAI, first.FallbackProvider, "lex tiebreaker must pick openai as fallback")
}

// TestRouter_PriorityShiftsCostWeight verifies the priority axis bends the
// router's answer for high-complexity tasks where local is penalized by
// minimumStrength. At normal/background priority, the cheap-but-weaker cloud
// wins; at immediate, the cost weight is small enough that the strongest
// provider takes the slot.
func TestRouter_PriorityShiftsCostWeight(t *testing.T) {
	router := newTestRouter(t)

	cases := []struct {
		name     string
		priority Priority
		want     Provider
	}{
		// Immediate: cost weight 0.2 → 0.06; claude (str 0.95) wins despite cost.
		{"immediate picks strongest", PriorityImmediate, ProviderClaude},
		// Normal: gemini (cheap-ish, 0.80) edges out claude on cost contribution.
		{"normal favors cost-efficient cloud", PriorityNormal, ProviderGemini},
		// Background: cost weight 0.2 → 0.3; gemini wins by even more.
		{"background doubles down on cheap", PriorityBackground, ProviderGemini},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := router.Route(RouteRequest{
				TaskComplexity:  ComplexityHigh,
				EstimatedTokens: 5000,
				Priority:        tc.priority,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, resp.Provider, "priority %s should select %s", tc.priority, tc.want)
		})
	}
}

// TestRouter_PriorityConfidenceMonotonic asserts that for the same task,
// confidence in the recommendation moves consistently with cost-weight shifts.
// The recommended provider's score should be highest under background (most
// extreme cost emphasis) when the winning provider is the cheapest.
func TestRouter_PriorityConfidenceMonotonic(t *testing.T) {
	router := newTestRouter(t)
	req := RouteRequest{TaskComplexity: ComplexityHigh, EstimatedTokens: 5000}

	req.Priority = PriorityNormal
	normal, err := router.Route(req)
	require.NoError(t, err)
	require.Equal(t, ProviderGemini, normal.Provider)

	req.Priority = PriorityBackground
	bg, err := router.Route(req)
	require.NoError(t, err)
	require.Equal(t, ProviderGemini, bg.Provider)

	assert.Greater(t, bg.Confidence, normal.Confidence,
		"background priority should boost confidence in the cheap winner")
}

// TestRouter_PriorityEmptyDefaultsToNormal asserts that an empty Priority
// behaves identically to PriorityNormal — callers shouldn't have to populate
// the field for the existing default behavior to hold.
func TestRouter_PriorityEmptyDefaultsToNormal(t *testing.T) {
	router := newTestRouter(t)

	req := RouteRequest{
		TaskComplexity:  ComplexityMedium,
		EstimatedTokens: 2000,
	}

	withEmpty, err := router.Route(req)
	require.NoError(t, err)

	req.Priority = PriorityNormal
	withNormal, err := router.Route(req)
	require.NoError(t, err)

	assert.Equal(t, withEmpty.Provider, withNormal.Provider)
	assert.InDelta(t, withEmpty.Confidence, withNormal.Confidence, 1e-9)
}

func TestRouter_Confidence(t *testing.T) {
	router := newTestRouter(t)

	resp, err := router.Route(RouteRequest{
		TaskComplexity:  ComplexityHigh,
		EstimatedTokens: 5000,
	})
	require.NoError(t, err)

	assert.Greater(t, resp.Confidence, 0.0)
	assert.LessOrEqual(t, resp.Confidence, 1.0)
}
