package budget

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamomaton-core/pgutil"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// queryTimeout bounds every store call so a misbehaving postgres cannot
// indefinitely hold a budget-router goroutine.
const queryTimeout = 30 * time.Second

// Store manages Postgres persistence for budget data.
//
// The store is concurrency-safe at the database layer (postgres MVCC).
// We deliberately do not lock around it in Go — every method opens
// short-lived transactions or single statements, and the budget-router
// is read-heavy with infrequent writes from the tracker reset loop.
type Store struct {
	pool   *pgxpool.Pool
	logger *logrus.Logger
}

// NewStore dials Postgres at dsn, runs any pending migrations, and
// returns a Store ready to serve queries. The caller owns the lifecycle
// via Store.Close().
func NewStore(dsn string, logger *logrus.Logger) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("budget.NewStore: DSN required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgutil.Open(ctx, pgutil.Config{DSN: dsn, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("budget.NewStore: open pool: %w", err)
	}

	if err := pgutil.MigrateAll(dsn, "budget", "migrations", migrationsFS, logger); err != nil {
		pool.Close()
		return nil, fmt.Errorf("budget.NewStore: migrate: %w", err)
	}

	return &Store{pool: pool, logger: logger}, nil
}

// InitProvider upserts a provider's status row with the given limits
// and reset times.
func (s *Store) InitProvider(provider Provider, dailyLimit, weeklyLimit int, dailyReset, weeklyReset time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO budget.provider_status
			(provider, daily_limit, weekly_limit, daily_reset_at, weekly_reset_at, last_updated)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (provider) DO UPDATE SET
			daily_limit  = EXCLUDED.daily_limit,
			weekly_limit = EXCLUDED.weekly_limit,
			last_updated = EXCLUDED.last_updated`,
		string(provider), dailyLimit, weeklyLimit,
		dailyReset.UTC(), weeklyReset.UTC(),
		time.Now().UTC(),
	)
	return err
}

// RecordUsage inserts a ledger record and atomically increments the
// provider's counters. On success the provider's error_count is
// decremented (clamped at zero) so transient failures eventually clear
// instead of holding the provider unavailable for ~24h until the daily
// reset.
func (s *Store) RecordUsage(rec UsageRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO budget.usage_records
			(provider, model, prompt_tokens, completion_tokens, total_tokens,
			 task_id, task_complexity, success, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		string(rec.Provider), rec.Model, rec.PromptTokens, rec.CompletionTokens,
		rec.TotalTokens, rec.TaskID, string(rec.TaskComplexity),
		rec.Success, rec.RecordedAt.UTC(),
	); err != nil {
		return fmt.Errorf("insert record: %w", err)
	}

	// Ensure provider_status row exists. A caller might RecordUsage before
	// InitProvider; without this an UPDATE alone matches zero rows.
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO budget.provider_status
			(provider, daily_used, daily_limit, weekly_used, weekly_limit,
			 daily_reset_at, weekly_reset_at, is_available, error_count, last_updated)
		VALUES ($1, 0, 0, 0, 0, $2, $2, TRUE, 0, $2)
		ON CONFLICT (provider) DO NOTHING`,
		string(rec.Provider), now,
	); err != nil {
		return fmt.Errorf("ensure provider row: %w", err)
	}

	if rec.Success {
		ct, err := tx.Exec(ctx, `
			UPDATE budget.provider_status SET
				daily_used   = daily_used + $1,
				weekly_used  = weekly_used + $1,
				error_count  = GREATEST(0, error_count - 1),
				last_updated = $2
			WHERE provider = $3`,
			rec.TotalTokens, now, string(rec.Provider),
		)
		if err != nil {
			return fmt.Errorf("update counters: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("update counters: no provider_status row for %q", rec.Provider)
		}
	} else {
		ct, err := tx.Exec(ctx, `
			UPDATE budget.provider_status SET
				daily_used   = daily_used + $1,
				weekly_used  = weekly_used + $1,
				error_count  = error_count + 1,
				last_updated = $2
			WHERE provider = $3`,
			rec.TotalTokens, now, string(rec.Provider),
		)
		if err != nil {
			return fmt.Errorf("update counters: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("update counters: no provider_status row for %q", rec.Provider)
		}

		// Mark unavailable once the error_count exceeds the threshold.
		if _, err := tx.Exec(ctx, `
			UPDATE budget.provider_status SET is_available = FALSE
			WHERE provider = $1 AND error_count > 5`,
			string(rec.Provider),
		); err != nil {
			return fmt.Errorf("update availability: %w", err)
		}
	}

	return tx.Commit(ctx)
}

// GetProviderStatus returns the current budget state for one provider.
func (s *Store) GetProviderStatus(provider Provider) (*BudgetStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	row := s.pool.QueryRow(ctx, `
		SELECT provider, daily_used, daily_limit, weekly_used, weekly_limit,
		       daily_reset_at, weekly_reset_at, is_available, error_count
		FROM budget.provider_status WHERE provider = $1`, string(provider))
	return scanProviderStatus(row)
}

// GetAllStatuses returns the current budget state for all providers,
// ordered by provider name.
func (s *Store) GetAllStatuses() ([]BudgetStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	rows, err := s.pool.Query(ctx, `
		SELECT provider, daily_used, daily_limit, weekly_used, weekly_limit,
		       daily_reset_at, weekly_reset_at, is_available, error_count
		FROM budget.provider_status ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var statuses []BudgetStatus
	for rows.Next() {
		bs, err := scanProviderStatus(rows)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, *bs)
	}
	return statuses, rows.Err()
}

// ResetDaily zeroes the daily counters for one provider and sets the
// next reset time.
func (s *Store) ResetDaily(provider Provider, nextReset time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	_, err := s.pool.Exec(ctx, `
		UPDATE budget.provider_status SET
			daily_used     = 0,
			error_count    = 0,
			is_available   = TRUE,
			daily_reset_at = $1,
			last_updated   = $2
		WHERE provider = $3`,
		nextReset.UTC(), time.Now().UTC(), string(provider),
	)
	return err
}

// ResetWeekly zeroes the weekly counters for one provider and sets the
// next reset time.
func (s *Store) ResetWeekly(provider Provider, nextReset time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	_, err := s.pool.Exec(ctx, `
		UPDATE budget.provider_status SET
			weekly_used     = 0,
			weekly_reset_at = $1,
			last_updated    = $2
		WHERE provider = $3`,
		nextReset.UTC(), time.Now().UTC(), string(provider),
	)
	return err
}

// QueryHistory returns recent usage records, optionally filtered by
// provider. limit is clamped to (0, 1000]; values outside that range
// default to 100.
func (s *Store) QueryHistory(provider Provider, since time.Time, limit int) ([]UsageRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	var (
		rows pgx.Rows
		err  error
	)
	if provider != "" {
		rows, err = s.pool.Query(ctx, `
			SELECT id, provider, model, prompt_tokens, completion_tokens, total_tokens,
			       task_id, task_complexity, success, recorded_at
			FROM budget.usage_records
			WHERE provider = $1 AND recorded_at >= $2
			ORDER BY recorded_at DESC
			LIMIT $3`,
			string(provider), since.UTC(), limit)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT id, provider, model, prompt_tokens, completion_tokens, total_tokens,
			       task_id, task_complexity, success, recorded_at
			FROM budget.usage_records
			WHERE recorded_at >= $1
			ORDER BY recorded_at DESC
			LIMIT $2`,
			since.UTC(), limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []UsageRecord
	for rows.Next() {
		var (
			rec      UsageRecord
			provStr  string
			compStr  string
		)
		if err := rows.Scan(
			&rec.ID, &provStr, &rec.Model, &rec.PromptTokens,
			&rec.CompletionTokens, &rec.TotalTokens, &rec.TaskID,
			&compStr, &rec.Success, &rec.RecordedAt,
		); err != nil {
			return nil, err
		}
		rec.Provider = Provider(provStr)
		rec.TaskComplexity = TaskComplexity(compStr)
		records = append(records, rec)
	}
	return records, rows.Err()
}

// Close releases the underlying pool.
func (s *Store) Close() error {
	if s.pool != nil {
		s.pool.Close()
	}
	return nil
}

// scannable lets scanProviderStatus accept both a QueryRow Row and an
// iterated Rows.
type scannable interface {
	Scan(dest ...any) error
}

// clampPct ensures a usage percentage stays in [0.0, 1.0]. Used so we
// never emit a "120%" gauge to API consumers when daily_used briefly
// outpaces daily_limit (e.g. a single oversized recording).
func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1.0 {
		return 1.0
	}
	return v
}

func scanProviderStatus(row scannable) (*BudgetStatus, error) {
	var (
		bs      BudgetStatus
		provStr string
	)
	err := row.Scan(
		&provStr, &bs.DailyUsed, &bs.DailyLimit,
		&bs.WeeklyUsed, &bs.WeeklyLimit,
		&bs.DailyResetAt, &bs.WeeklyResetAt,
		&bs.IsAvailable, &bs.ErrorCount,
	)
	if err != nil {
		return nil, err
	}
	bs.Provider = Provider(provStr)

	if bs.DailyLimit > 0 {
		bs.DailyRemaining = bs.DailyLimit - bs.DailyUsed
		if bs.DailyRemaining < 0 {
			bs.DailyRemaining = 0
		}
		bs.DailyPct = clampPct(float64(bs.DailyUsed) / float64(bs.DailyLimit))
	}
	if bs.WeeklyLimit > 0 {
		bs.WeeklyRemaining = bs.WeeklyLimit - bs.WeeklyUsed
		if bs.WeeklyRemaining < 0 {
			bs.WeeklyRemaining = 0
		}
		bs.WeeklyPct = clampPct(float64(bs.WeeklyUsed) / float64(bs.WeeklyLimit))
	}
	return &bs, nil
}
