package delegator

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-delegator/delegator/budget"
	"github.com/sirus20x6/adamaton-core/pgutil"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// pgStoreQueryTimeout bounds every store call. Tasks records can be
// long (output/stderr blobs), but a healthy postgres should still
// respond well inside this budget.
const pgStoreQueryTimeout = 30 * time.Second

// PgStore is the postgres-backed Task store. Implements the Store
// interface the orchestrator consumes — the in-memory TaskStore is
// still available for fast/local-only mode.
//
// Concurrency is delegated to postgres MVCC; no Go-side mutex needed.
type PgStore struct {
	pool     *pgxpool.Pool
	maxTasks int
	logger   *logrus.Logger
}

// NewPgStore dials Postgres, runs the delegator migrations, and
// returns a store ready for cross-process task visibility (MCP writer
// + apiserver reader).
func NewPgStore(dsn string, maxTasks int, logger *logrus.Logger) (*PgStore, error) {
	if dsn == "" {
		return nil, errors.New("delegator.NewPgStore: DSN required")
	}
	if maxTasks <= 0 {
		maxTasks = 1000
	}
	if logger == nil {
		logger = logrus.New()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgutil.Open(ctx, pgutil.Config{DSN: dsn, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("delegator.NewPgStore: open pool: %w", err)
	}
	if err := pgutil.MigrateAll(dsn, "delegator", "migrations", migrationsFS, logger); err != nil {
		pool.Close()
		return nil, fmt.Errorf("delegator.NewPgStore: migrate: %w", err)
	}

	return &PgStore{pool: pool, maxTasks: maxTasks, logger: logger}, nil
}

// Close releases the underlying pool.
func (s *PgStore) Close() error {
	if s.pool != nil {
		s.pool.Close()
	}
	return nil
}

// Put inserts or updates a task by id. Logs and silently returns on
// failure to match the Store interface contract (callers can't act on
// a persistence failure mid-task; the in-memory cancellation map is
// still authoritative for runtime state).
func (s *PgStore) Put(t *Task) {
	ctx, cancel := context.WithTimeout(context.Background(), pgStoreQueryTimeout)
	defer cancel()

	const upsert = `
INSERT INTO delegator.tasks
	(id, agent, provider, difficulty, priority, prompt, working_dir, model,
	 status, created_at, started_at, completed_at, exit_code, output, stderr, error)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8,
	 $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (id) DO UPDATE SET
	status       = EXCLUDED.status,
	started_at   = EXCLUDED.started_at,
	completed_at = EXCLUDED.completed_at,
	exit_code    = EXCLUDED.exit_code,
	output       = EXCLUDED.output,
	stderr       = EXCLUDED.stderr,
	error        = EXCLUDED.error
`
	if _, err := s.pool.Exec(ctx, upsert,
		t.ID, t.Agent, string(t.Provider), string(t.Difficulty), string(t.Priority),
		t.Prompt, t.WorkingDir, t.Model,
		string(t.Status), t.CreatedAt.UTC(),
		timeOrNil(t.StartedAt), timeOrNil(t.CompletedAt),
		t.ExitCode, t.Output, t.Stderr, t.Error,
	); err != nil {
		s.logger.WithError(err).WithField("task_id", t.ID).Warn("pg Put failed")
		return
	}
	s.evictIfFull(ctx)
}

// Get returns a copy of the task or false.
func (s *PgStore) Get(id string) (*Task, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), pgStoreQueryTimeout)
	defer cancel()

	row := s.pool.QueryRow(ctx, selectColumns+` FROM delegator.tasks WHERE id = $1`, id)
	t, err := scanTaskRow(row)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.logger.WithError(err).WithField("task_id", id).Warn("pg Get failed")
		}
		return nil, false
	}
	return t, true
}

// Update applies fn to a task in a read-modify-write transaction.
// Returns false if the task doesn't exist. The whole transaction runs
// at READ COMMITTED with explicit row locking — two concurrent
// Update() calls against the same task serialise instead of clobbering.
func (s *PgStore) Update(id string, fn func(*Task)) bool {
	ctx, cancel := context.WithTimeout(context.Background(), pgStoreQueryTimeout)
	defer cancel()

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		s.logger.WithError(err).WithField("task_id", id).Warn("pg Update begin failed")
		return false
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, selectColumns+` FROM delegator.tasks WHERE id = $1 FOR UPDATE`, id)
	t, err := scanTaskRow(row)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.logger.WithError(err).WithField("task_id", id).Warn("pg Update select failed")
		}
		return false
	}
	fn(t)

	if _, err := tx.Exec(ctx, `
		UPDATE delegator.tasks SET
			status       = $1,
			started_at   = $2,
			completed_at = $3,
			exit_code    = $4,
			output       = $5,
			stderr       = $6,
			error        = $7
		WHERE id = $8`,
		string(t.Status), timeOrNil(t.StartedAt), timeOrNil(t.CompletedAt),
		t.ExitCode, t.Output, t.Stderr, t.Error,
		id,
	); err != nil {
		s.logger.WithError(err).WithField("task_id", id).Warn("pg Update exec failed")
		return false
	}
	if err := tx.Commit(ctx); err != nil {
		s.logger.WithError(err).WithField("task_id", id).Warn("pg Update commit failed")
		return false
	}
	return true
}

// List returns up to maxTasks copies, newest first. Filters by status
// / agent when those args are non-empty.
func (s *PgStore) List(status TaskStatus, agent string) []*Task {
	ctx, cancel := context.WithTimeout(context.Background(), pgStoreQueryTimeout)
	defer cancel()

	var (
		q       strings.Builder
		args    []any
		clauses []string
	)
	q.WriteString(selectColumns)
	q.WriteString(` FROM delegator.tasks`)

	if status != "" {
		args = append(args, string(status))
		clauses = append(clauses, fmt.Sprintf("status = $%d", len(args)))
	}
	if agent != "" {
		args = append(args, agent)
		clauses = append(clauses, fmt.Sprintf("agent = $%d", len(args)))
	}
	if len(clauses) > 0 {
		q.WriteString(" WHERE ")
		q.WriteString(strings.Join(clauses, " AND "))
	}
	args = append(args, s.maxTasks)
	fmt.Fprintf(&q, " ORDER BY created_at DESC LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, q.String(), args...)
	if err != nil {
		s.logger.WithError(err).Warn("pg List failed")
		return nil
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTaskRow(rows)
		if err != nil {
			s.logger.WithError(err).Warn("pg scan failed")
			continue
		}
		out = append(out, t)
	}
	return out
}

// evictIfFull drops oldest TERMINAL tasks beyond maxTasks. Pending
// and running tasks are never evicted — that would orphan the caller's
// task_id. Run as a separate transaction so a failed eviction doesn't
// break the Put that triggered it.
func (s *PgStore) evictIfFull(ctx context.Context) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM delegator.tasks`).Scan(&n); err != nil {
		s.logger.WithError(err).Warn("pg evict count failed")
		return
	}
	if n <= s.maxTasks {
		return
	}
	excess := n - s.maxTasks
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM delegator.tasks
		 WHERE id IN (
		     SELECT id FROM delegator.tasks
		      WHERE status NOT IN ('pending','running')
		      ORDER BY created_at ASC
		      LIMIT $1
		 )`, excess); err != nil {
		s.logger.WithError(err).Warn("pg evict failed")
	}
}

// --- helpers ---

// selectColumns lists every column in canonical order so Get/Update/
// List share a single scan implementation.
const selectColumns = `
SELECT id, agent, provider, difficulty, priority, prompt, working_dir, model,
       status, created_at, started_at, completed_at,
       exit_code, output, stderr, error`

type pgRowScanner interface {
	Scan(dest ...any) error
}

func scanTaskRow(r pgRowScanner) (*Task, error) {
	var (
		t                  Task
		provider, diff     string
		priority, working  string
		model              string
		started, completed *time.Time
	)
	err := r.Scan(
		&t.ID, &t.Agent, &provider, &diff, &priority, &t.Prompt, &working, &model,
		&t.Status, &t.CreatedAt, &started, &completed,
		&t.ExitCode, &t.Output, &t.Stderr, &t.Error,
	)
	if err != nil {
		return nil, err
	}
	t.Provider = budget.Provider(provider)
	t.Difficulty = Difficulty(diff)
	t.Priority = budget.Priority(priority)
	t.WorkingDir = working
	t.Model = model
	t.CreatedAt = t.CreatedAt.UTC()
	if started != nil {
		t.StartedAt = started.UTC()
	}
	if completed != nil {
		t.CompletedAt = completed.UTC()
	}
	return &t, nil
}

// timeOrNil returns nil for a zero time so the column lands NULL
// instead of '0001-01-01T00:00:00Z'.
func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
