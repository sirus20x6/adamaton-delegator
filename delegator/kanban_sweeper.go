package delegator

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// Kanban stale-claim sweep.
//
// The kanban orchestration model (see docs/PROJECTS_KANBAN.md) lets agents
// atomically *claim* a card before working it: the apiserver flips
// evo.kanban_cards.claim_status to 'claimed', stamps claimed_by/claim_token,
// and sets claimed_at. A well-behaved worker later calls complete or release.
// A *crashed* worker — killed subagent, panicked Workflow, lost network —
// never does, leaving the card stuck in 'claimed' forever so no other agent
// can pick it up.
//
// Decision 3 in the design doc deliberately uses Postgres as the durable
// queue and a stale-claim sweep (not Temporal) as the crash-recovery
// mechanism. This sweeper is that recovery loop: it periodically flips
// 'claimed' rows whose claimed_at is older than a TTL back to 'unclaimed',
// clearing claimed_by/claim_token/claimed_at so the card returns to the Ready
// queue. The card's visual column is intentionally left untouched — the UI
// tracks column separately from claim_status (design doc §1), and an operator
// may want to see that a card was abandoned in "In Progress".
//
// It shares the delegator-mcp PgStore pool (same evo-schema DSN), so it adds
// no new connection. It is best-effort: a failed sweep is logged and retried
// on the next tick; nothing here is load-bearing for the agent loop.

const (
	// DefaultStaleClaimTTL is how long a card may sit 'claimed' without a
	// complete/release before the sweeper reclaims it. Matches the ~30m
	// figure in the design doc (§6) — long enough that a slow-but-live
	// worker isn't yanked out from under, short enough that a crashed
	// worker's card returns to the queue promptly.
	DefaultStaleClaimTTL = 30 * time.Minute

	// DefaultStaleClaimSweepInterval is how often the sweep runs. A few
	// minutes is well under the TTL so reclaim latency is bounded by the
	// interval, not the TTL.
	DefaultStaleClaimSweepInterval = 5 * time.Minute

	// staleSweepQueryTimeout bounds a single sweep UPDATE.
	staleSweepQueryTimeout = 30 * time.Second
)

// reclaimStaleClaimsSQL flips every stale 'claimed' card back to
// 'unclaimed', clearing the claim metadata. $1 is the TTL in seconds;
// NOW() - ($1 || ' seconds')::interval is the cutoff. The cards_claim_idx
// (board_id, claim_status, claimed_at) covers the predicate.
const reclaimStaleClaimsSQL = `
UPDATE evo.kanban_cards
   SET claim_status = 'unclaimed',
       claimed_by   = NULL,
       claim_token  = NULL,
       claimed_at   = NULL,
       updated_at   = NOW()
 WHERE claim_status = 'claimed'
   AND claimed_at IS NOT NULL
   AND claimed_at < NOW() - ($1 * INTERVAL '1 second')`

// StartKanbanSweeper launches the stale-claim sweep loop in a goroutine and
// returns immediately. The loop runs until ctx is cancelled. ttl and interval
// fall back to the package defaults when non-positive. Passing a nil pool is a
// no-op (logged once) so callers without a Postgres-backed store degrade
// gracefully rather than panic.
func StartKanbanSweeper(ctx context.Context, pool *pgxpool.Pool, ttl, interval time.Duration, logger *logrus.Logger) {
	if logger == nil {
		logger = logrus.New()
	}
	if pool == nil {
		logger.Warn("kanban sweeper: nil pool; stale-claim sweep disabled")
		return
	}
	if ttl <= 0 {
		ttl = DefaultStaleClaimTTL
	}
	if interval <= 0 {
		interval = DefaultStaleClaimSweepInterval
	}

	logger.WithFields(logrus.Fields{
		"ttl":      ttl.String(),
		"interval": interval.String(),
	}).Info("kanban stale-claim sweeper started")

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				logger.Debug("kanban sweeper: context cancelled, stopping")
				return
			case <-ticker.C:
				sweepStaleClaims(ctx, pool, ttl, logger)
			}
		}
	}()
}

// sweepStaleClaims runs one reclaim pass. Errors are logged and swallowed —
// the next tick retries. Returns the number of cards reclaimed (exported via
// the return value for tests / callers that want to act on it).
func sweepStaleClaims(ctx context.Context, pool *pgxpool.Pool, ttl time.Duration, logger *logrus.Logger) int64 {
	sweepCtx, cancel := context.WithTimeout(ctx, staleSweepQueryTimeout)
	defer cancel()

	tag, err := pool.Exec(sweepCtx, reclaimStaleClaimsSQL, int64(ttl.Seconds()))
	if err != nil {
		logger.WithError(err).Warn("kanban sweeper: reclaim query failed")
		return 0
	}
	n := tag.RowsAffected()
	if n > 0 {
		logger.WithField("reclaimed", n).Info("kanban sweeper: reclaimed stale card claims")
	}
	return n
}
