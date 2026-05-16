package budget

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sirus20x6/adamomaton-core/pgutil"
)

func newTestTracker(t *testing.T) (*Tracker, *Store) {
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
				CostPerMToken: 3.00,
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

	return tracker, store
}

func TestTracker_Report(t *testing.T) {
	tracker, _ := newTestTracker(t)

	boolTrue := true
	resp, err := tracker.Report(ReportRequest{
		Provider:    ProviderClaude,
		Model:       "claude-sonnet",
		TotalTokens: 5000,
		Success:     &boolTrue,
	})
	require.NoError(t, err)
	assert.True(t, resp.Recorded)
	assert.Equal(t, 195000, resp.DailyRemaining)
	assert.Equal(t, 995000, resp.WeeklyRemaining)
}

func TestTracker_Report_Unlimited(t *testing.T) {
	tracker, _ := newTestTracker(t)

	boolTrue := true
	resp, err := tracker.Report(ReportRequest{
		Provider:    ProviderLocal,
		TotalTokens: 50000,
		Success:     &boolTrue,
	})
	require.NoError(t, err)
	assert.True(t, resp.Recorded)
	assert.Equal(t, -1, resp.DailyRemaining)  // unlimited
	assert.Equal(t, -1, resp.WeeklyRemaining) // unlimited
}

func TestTracker_Report_BudgetWarning(t *testing.T) {
	tracker, _ := newTestTracker(t)

	boolTrue := true
	// Use 81% of daily budget (162000 out of 200000)
	resp, err := tracker.Report(ReportRequest{
		Provider:    ProviderClaude,
		TotalTokens: 162000,
		Success:     &boolTrue,
	})
	require.NoError(t, err)
	assert.Contains(t, resp.BudgetWarning, "claude")
}

func TestTracker_Report_UnknownProvider(t *testing.T) {
	tracker, _ := newTestTracker(t)

	_, err := tracker.Report(ReportRequest{
		Provider:    Provider("unknown"),
		TotalTokens: 100,
	})
	assert.Error(t, err)
}

func TestTracker_Status(t *testing.T) {
	tracker, _ := newTestTracker(t)

	statuses, err := tracker.Status()
	require.NoError(t, err)
	assert.Len(t, statuses, 2)

	// Find claude status
	for _, s := range statuses {
		if s.Provider == ProviderClaude {
			assert.Equal(t, TierCloud, s.Tier)
			assert.Equal(t, 200000, s.DailyLimit)
		}
		if s.Provider == ProviderLocal {
			assert.Equal(t, TierLocal, s.Tier)
		}
	}
}

func TestTracker_ProviderStatus(t *testing.T) {
	tracker, _ := newTestTracker(t)

	status, err := tracker.ProviderStatus(ProviderClaude)
	require.NoError(t, err)
	assert.Equal(t, ProviderClaude, status.Provider)
	assert.Equal(t, TierCloud, status.Tier)
}

func TestTracker_ResetProvider(t *testing.T) {
	tracker, _ := newTestTracker(t)

	boolTrue := true
	_, err := tracker.Report(ReportRequest{
		Provider:    ProviderClaude,
		TotalTokens: 50000,
		Success:     &boolTrue,
	})
	require.NoError(t, err)

	require.NoError(t, tracker.ResetProvider(ProviderClaude))

	status, err := tracker.ProviderStatus(ProviderClaude)
	require.NoError(t, err)
	assert.Equal(t, 0, status.DailyUsed)
	assert.Equal(t, 0, status.WeeklyUsed)
}

func TestTracker_ResetProvider_Unknown(t *testing.T) {
	tracker, _ := newTestTracker(t)

	err := tracker.ResetProvider(Provider("unknown"))
	assert.Error(t, err)
}

func TestTracker_NextDailyReset(t *testing.T) {
	tracker, _ := newTestTracker(t)

	// At 23:00, next reset (hour=0) should be tomorrow at 00:00
	now := time.Date(2026, 2, 5, 23, 0, 0, 0, time.UTC)
	next := tracker.nextDailyReset(now)
	assert.Equal(t, 2026, next.Year())
	assert.Equal(t, time.February, next.Month())
	assert.Equal(t, 6, next.Day())
}

func TestTracker_NextWeeklyReset(t *testing.T) {
	tracker, _ := newTestTracker(t)

	// Wednesday Feb 5 2026 — next Monday should be Feb 9
	now := time.Date(2026, 2, 5, 12, 0, 0, 0, time.UTC)
	next := tracker.nextWeeklyReset(now)
	assert.Equal(t, time.Monday, next.Weekday())
	assert.True(t, next.After(now))
}

// TestTracker_NextDailyReset_DSTSpringForwardWarns constructs a tracker
// configured for America/New_York with daily_reset_hour=2 and computes the
// next daily reset on a date where 02:00 doesn't exist (US spring-forward,
// 2026-03-08). Go's time.Date normalizes the missing wall clock — the reset
// still fires correctly, but the wall-clock hour shifts by one. The fix
// emits a warn-level log so operators can correlate the off-by-one with
// the DST shift instead of debugging "why did the reset fire at the wrong
// hour today?".
func TestTracker_NextDailyReset_DSTSpringForwardWarns(t *testing.T) {
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	var buf bytes.Buffer
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	logger.SetOutput(&buf)

	store, err := NewStore(pgutil.TestDSN(t), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// daily_reset_hour=2 in America/New_York. On 2026-03-08, 02:00 EST does
	// not exist; the clock jumps 02:00 → 03:00.
	cfg := &ServiceConfig{
		DailyResetHour: 2,
		WeeklyResetDay: 1,
		Timezone:       "America/New_York",
		Providers: []ProviderConfig{
			{Provider: ProviderClaude, Tier: TierCloud, Strength: 0.5, DailyLimit: 1000, WeeklyLimit: 5000, DefaultModel: "x", CostPerMToken: 1.0},
		},
	}
	tracker, err := NewTracker(store, cfg, logger)
	require.NoError(t, err)
	t.Cleanup(func() { tracker.Stop() })

	// Drain any startup log noise (e.g. "Initialized provider") before we
	// inspect the next call's output.
	buf.Reset()

	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	// Pick "now" at 01:00 on spring-forward day. The next reset would be
	// "today at 02:00" — except 02:00 doesn't exist. time.Date normalizes,
	// and the warning must fire.
	now := time.Date(2026, 3, 8, 1, 0, 0, 0, loc)
	reset := tracker.nextDailyReset(now)

	// Sanity: reset is a real time and is in the future.
	require.False(t, reset.IsZero())
	require.True(t, reset.After(now), "reset must be in the future")

	// The warning must mention DST and the requested hour.
	out := buf.String()
	assert.True(t, strings.Contains(out, "DST shift"), "expected DST warning in log, got: %q", out)
	assert.True(t, strings.Contains(out, "requested_hour=2"),
		"expected requested_hour=2 in log, got: %q", out)

	// Negative case: outside the spring-forward day, the warning must NOT fire.
	buf.Reset()
	normal := time.Date(2026, 3, 10, 1, 0, 0, 0, loc) // two days later — no DST gap
	_ = tracker.nextDailyReset(normal)
	assert.False(t, strings.Contains(buf.String(), "DST shift"),
		"DST warning must not fire on a normal day, got: %q", buf.String())
}

func TestTracker_StopIsIdempotent(t *testing.T) {
	tracker, _ := newTestTracker(t)

	// Two explicit Stops plus the t.Cleanup-scheduled one — all must be safe.
	tracker.Stop()
	tracker.Stop()
}

func TestNewTracker_RejectsInvalidConfig(t *testing.T) {
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	store, err := NewStore(pgutil.TestDSN(t), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	bad := &ServiceConfig{
		DailyResetHour: 25, // out of range
		WeeklyResetDay: 1,
		Timezone:       "UTC",
	}
	_, err = NewTracker(store, bad, logger)
	require.Error(t, err)

	bad2 := &ServiceConfig{
		DailyResetHour: 0,
		WeeklyResetDay: 8, // out of range
		Timezone:       "UTC",
	}
	_, err = NewTracker(store, bad2, logger)
	require.Error(t, err)

	bad3 := &ServiceConfig{
		DailyResetHour: 0,
		WeeklyResetDay: 1,
		Timezone:       "Mars/Olympus_Mons", // not a real tz
	}
	_, err = NewTracker(store, bad3, logger)
	require.Error(t, err)

	bad4 := &ServiceConfig{
		DailyResetHour: 0,
		WeeklyResetDay: 1,
		Timezone:       "UTC",
		Providers: []ProviderConfig{
			{Provider: ProviderClaude, Tier: TierCloud, Strength: 1.5}, // out of range
		},
	}
	_, err = NewTracker(store, bad4, logger)
	require.Error(t, err)

	bad5 := &ServiceConfig{
		DailyResetHour: 0,
		WeeklyResetDay: 1,
		Timezone:       "UTC",
		Providers: []ProviderConfig{
			{Provider: ProviderClaude, Tier: TierCloud, Strength: 0.5, CostPerMToken: -1.0},
		},
	}
	_, err = NewTracker(store, bad5, logger)
	require.Error(t, err)
}

func TestProviderConfig_UnlimitedFlagWins(t *testing.T) {
	pc := &ProviderConfig{Unlimited: true, DailyLimit: 999, WeeklyLimit: 999}
	if !pc.IsUnlimited() {
		t.Fatal("Unlimited=true should override non-zero limits")
	}
	pc2 := &ProviderConfig{DailyLimit: 0, WeeklyLimit: 0}
	if !pc2.IsUnlimited() {
		t.Fatal("legacy semantics: both limits zero implies unlimited")
	}
	pc3 := &ProviderConfig{DailyLimit: 0, WeeklyLimit: 100}
	if pc3.IsUnlimited() {
		t.Fatal("partial-zero limits should NOT be considered unlimited (avoids router asymmetry)")
	}
}

func TestTracker_History(t *testing.T) {
	tracker, _ := newTestTracker(t)

	boolTrue := true
	for i := 0; i < 3; i++ {
		_, err := tracker.Report(ReportRequest{
			Provider:    ProviderClaude,
			TotalTokens: 1000,
			Success:     &boolTrue,
		})
		require.NoError(t, err)
	}

	records, err := tracker.History(ProviderClaude, time.Now().Add(-1*time.Hour), 10)
	require.NoError(t, err)
	assert.Len(t, records, 3)
}
