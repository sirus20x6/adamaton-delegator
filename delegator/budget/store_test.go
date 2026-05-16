package budget

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sirus20x6/adamaton-core/pgutil"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	dsn := pgutil.TestDSN(t)
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	store, err := NewStore(dsn, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestStore_InitAndGetProvider(t *testing.T) {
	store := newTestStore(t)

	daily := time.Now().Add(24 * time.Hour)
	weekly := time.Now().Add(7 * 24 * time.Hour)

	err := store.InitProvider(ProviderClaude, 200000, 1000000, daily, weekly)
	require.NoError(t, err)

	status, err := store.GetProviderStatus(ProviderClaude)
	require.NoError(t, err)

	assert.Equal(t, ProviderClaude, status.Provider)
	assert.Equal(t, 200000, status.DailyLimit)
	assert.Equal(t, 1000000, status.WeeklyLimit)
	assert.Equal(t, 0, status.DailyUsed)
	assert.Equal(t, 0, status.WeeklyUsed)
	assert.True(t, status.IsAvailable)
}

func TestStore_RecordUsage(t *testing.T) {
	store := newTestStore(t)

	daily := time.Now().Add(24 * time.Hour)
	weekly := time.Now().Add(7 * 24 * time.Hour)
	require.NoError(t, store.InitProvider(ProviderOpenAI, 300000, 1500000, daily, weekly))

	rec := UsageRecord{
		Provider:         ProviderOpenAI,
		Model:            "gpt-4o",
		PromptTokens:     2000,
		CompletionTokens: 3000,
		TotalTokens:      5000,
		TaskID:           "task-1",
		TaskComplexity:   ComplexityHigh,
		Success:          true,
		RecordedAt:       time.Now().UTC(),
	}
	require.NoError(t, store.RecordUsage(rec))

	status, err := store.GetProviderStatus(ProviderOpenAI)
	require.NoError(t, err)
	assert.Equal(t, 5000, status.DailyUsed)
	assert.Equal(t, 5000, status.WeeklyUsed)
	assert.Equal(t, 295000, status.DailyRemaining)
}

func TestStore_RecordUsage_ErrorTracking(t *testing.T) {
	store := newTestStore(t)

	daily := time.Now().Add(24 * time.Hour)
	weekly := time.Now().Add(7 * 24 * time.Hour)
	require.NoError(t, store.InitProvider(ProviderClaude, 200000, 1000000, daily, weekly))

	for i := 0; i < 6; i++ {
		rec := UsageRecord{
			Provider:    ProviderClaude,
			TotalTokens: 100,
			Success:     false,
			RecordedAt:  time.Now().UTC(),
		}
		require.NoError(t, store.RecordUsage(rec))
	}

	status, err := store.GetProviderStatus(ProviderClaude)
	require.NoError(t, err)
	assert.False(t, status.IsAvailable)
	assert.Equal(t, 6, status.ErrorCount)
}

func TestStore_ResetDaily(t *testing.T) {
	store := newTestStore(t)

	daily := time.Now().Add(24 * time.Hour)
	weekly := time.Now().Add(7 * 24 * time.Hour)
	require.NoError(t, store.InitProvider(ProviderGemini, 500000, 2000000, daily, weekly))

	rec := UsageRecord{
		Provider:    ProviderGemini,
		TotalTokens: 100000,
		Success:     true,
		RecordedAt:  time.Now().UTC(),
	}
	require.NoError(t, store.RecordUsage(rec))

	nextReset := time.Now().Add(48 * time.Hour)
	require.NoError(t, store.ResetDaily(ProviderGemini, nextReset))

	status, err := store.GetProviderStatus(ProviderGemini)
	require.NoError(t, err)
	assert.Equal(t, 0, status.DailyUsed)
	assert.Equal(t, 100000, status.WeeklyUsed) // weekly not reset
	assert.True(t, status.IsAvailable)
	assert.Equal(t, 0, status.ErrorCount)
}

func TestStore_ResetWeekly(t *testing.T) {
	store := newTestStore(t)

	daily := time.Now().Add(24 * time.Hour)
	weekly := time.Now().Add(7 * 24 * time.Hour)
	require.NoError(t, store.InitProvider(ProviderLocal, 0, 0, daily, weekly))

	rec := UsageRecord{
		Provider:    ProviderLocal,
		TotalTokens: 50000,
		Success:     true,
		RecordedAt:  time.Now().UTC(),
	}
	require.NoError(t, store.RecordUsage(rec))

	nextReset := time.Now().Add(14 * 24 * time.Hour)
	require.NoError(t, store.ResetWeekly(ProviderLocal, nextReset))

	status, err := store.GetProviderStatus(ProviderLocal)
	require.NoError(t, err)
	assert.Equal(t, 50000, status.DailyUsed) // daily not reset
	assert.Equal(t, 0, status.WeeklyUsed)
}

func TestStore_GetAllStatuses(t *testing.T) {
	store := newTestStore(t)

	daily := time.Now().Add(24 * time.Hour)
	weekly := time.Now().Add(7 * 24 * time.Hour)
	require.NoError(t, store.InitProvider(ProviderClaude, 200000, 1000000, daily, weekly))
	require.NoError(t, store.InitProvider(ProviderOpenAI, 300000, 1500000, daily, weekly))

	statuses, err := store.GetAllStatuses()
	require.NoError(t, err)
	assert.Len(t, statuses, 2)
}

func TestStore_QueryHistory(t *testing.T) {
	store := newTestStore(t)

	daily := time.Now().Add(24 * time.Hour)
	weekly := time.Now().Add(7 * 24 * time.Hour)
	require.NoError(t, store.InitProvider(ProviderClaude, 200000, 1000000, daily, weekly))

	for i := 0; i < 5; i++ {
		rec := UsageRecord{
			Provider:    ProviderClaude,
			Model:       "claude-sonnet",
			TotalTokens: 1000 * (i + 1),
			Success:     true,
			RecordedAt:  time.Now().UTC(),
		}
		require.NoError(t, store.RecordUsage(rec))
	}

	records, err := store.QueryHistory("", time.Now().Add(-1*time.Hour), 10)
	require.NoError(t, err)
	assert.Len(t, records, 5)

	records, err = store.QueryHistory(ProviderClaude, time.Now().Add(-1*time.Hour), 3)
	require.NoError(t, err)
	assert.Len(t, records, 3)
}

func TestStore_RecordUsage_DecrementsErrorCountOnSuccess(t *testing.T) {
	store := newTestStore(t)

	daily := time.Now().Add(24 * time.Hour)
	weekly := time.Now().Add(7 * 24 * time.Hour)
	require.NoError(t, store.InitProvider(ProviderClaude, 200000, 1000000, daily, weekly))

	for i := 0; i < 3; i++ {
		require.NoError(t, store.RecordUsage(UsageRecord{
			Provider:    ProviderClaude,
			TotalTokens: 100,
			Success:     false,
			RecordedAt:  time.Now().UTC(),
		}))
	}
	status, err := store.GetProviderStatus(ProviderClaude)
	require.NoError(t, err)
	assert.Equal(t, 3, status.ErrorCount)

	require.NoError(t, store.RecordUsage(UsageRecord{
		Provider:    ProviderClaude,
		TotalTokens: 100,
		Success:     true,
		RecordedAt:  time.Now().UTC(),
	}))
	status, err = store.GetProviderStatus(ProviderClaude)
	require.NoError(t, err)
	assert.Equal(t, 2, status.ErrorCount)

	// More successes than failures: clamps at zero, never goes negative.
	for i := 0; i < 5; i++ {
		require.NoError(t, store.RecordUsage(UsageRecord{
			Provider:    ProviderClaude,
			TotalTokens: 100,
			Success:     true,
			RecordedAt:  time.Now().UTC(),
		}))
	}
	status, err = store.GetProviderStatus(ProviderClaude)
	require.NoError(t, err)
	assert.Equal(t, 0, status.ErrorCount)
}

// TestStore_RecordUsage_MissingRowAutoCreated: RecordUsage without a
// prior InitProvider must still land — the ensure-row INSERT inside
// the transaction creates a minimal status row first.
func TestStore_RecordUsage_MissingRowAutoCreated(t *testing.T) {
	store := newTestStore(t)

	require.NoError(t, store.RecordUsage(UsageRecord{
		Provider:    ProviderOpenAI,
		TotalTokens: 1234,
		Success:     true,
		RecordedAt:  time.Now().UTC(),
	}))

	status, err := store.GetProviderStatus(ProviderOpenAI)
	require.NoError(t, err)
	assert.Equal(t, 1234, status.DailyUsed)
	assert.Equal(t, 1234, status.WeeklyUsed)
}

func TestStore_DailyPctClampedToOne(t *testing.T) {
	store := newTestStore(t)

	daily := time.Now().Add(24 * time.Hour)
	weekly := time.Now().Add(7 * 24 * time.Hour)
	require.NoError(t, store.InitProvider(ProviderGemini, 1000, 10000, daily, weekly))

	require.NoError(t, store.RecordUsage(UsageRecord{
		Provider:    ProviderGemini,
		TotalTokens: 5000,
		Success:     true,
		RecordedAt:  time.Now().UTC(),
	}))

	status, err := store.GetProviderStatus(ProviderGemini)
	require.NoError(t, err)
	assert.LessOrEqual(t, status.DailyPct, 1.0)
	assert.Equal(t, 0, status.DailyRemaining)
}

func TestStore_ConcurrentWrites(t *testing.T) {
	store := newTestStore(t)

	daily := time.Now().Add(24 * time.Hour)
	weekly := time.Now().Add(7 * 24 * time.Hour)
	require.NoError(t, store.InitProvider(ProviderClaude, 200000, 1000000, daily, weekly))

	done := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			rec := UsageRecord{
				Provider:    ProviderClaude,
				TotalTokens: 1000,
				Success:     true,
				RecordedAt:  time.Now().UTC(),
			}
			assert.NoError(t, store.RecordUsage(rec))
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	status, err := store.GetProviderStatus(ProviderClaude)
	require.NoError(t, err)
	assert.Equal(t, 10000, status.DailyUsed)
}

// TestStore_HighConcurrencyNoLockErrors stresses the pool: 100 concurrent
// RecordUsage calls. Under sqlite+WAL this used to require serialisation
// via MaxOpenConns=1 to avoid "database is locked"; postgres MVCC handles
// the interleave naturally. We still keep the test because the row-level
// counter math has to land monotonically — the sum must equal
// goroutines*100, with no lost updates from concurrent transactions.
func TestStore_HighConcurrencyNoLockErrors(t *testing.T) {
	store := newTestStore(t)

	daily := time.Now().Add(24 * time.Hour)
	weekly := time.Now().Add(7 * 24 * time.Hour)
	require.NoError(t, store.InitProvider(ProviderClaude, 200000, 1000000, daily, weekly))

	const goroutines = 100
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := UsageRecord{
				Provider:    ProviderClaude,
				TotalTokens: 100,
				Success:     true,
				RecordedAt:  time.Now().UTC(),
			}
			if err := store.RecordUsage(rec); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent RecordUsage error: %v", err)
	}

	status, err := store.GetProviderStatus(ProviderClaude)
	require.NoError(t, err)
	assert.Equal(t, 100*goroutines, status.DailyUsed)
}
