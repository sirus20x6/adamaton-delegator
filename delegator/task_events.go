package delegator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// TaskEvents publishes live output chunks from a running delegation so a
// UI can tail it as it happens instead of polling get_task_status for a
// finished blob. A nil TaskEvents on the Orchestrator disables streaming
// entirely — the delegation runs exactly as before.
type TaskEvents interface {
	Publish(taskID, stream string, chunk []byte)
}

const (
	// taskEventMaxRaw bounds the raw bytes carried per NOTIFY. Postgres
	// caps a pg_notify payload at 8000 bytes; base64 inflates ~4/3 and the
	// JSON envelope adds a little, so 5000 raw → ~6800 encoded stays clear
	// of the limit. Larger chunks are split across several notifications.
	taskEventMaxRaw = 5000

	// taskEventPublishTimeout bounds a single NOTIFY so a wedged pool can
	// never stall the subprocess output pump.
	taskEventPublishTimeout = 5 * time.Second
)

// PgTaskEvents publishes chunks over Postgres LISTEN/NOTIFY on a per-task
// channel, reusing the tasks-store pool. It is fire-and-forget: a dropped
// notification (no listener attached, oversize, transient error) never
// affects the delegation — streaming is observability, not control.
//
// Only the subprocess execution path emits chunks; the opencode `serve`
// fast-path reads a full response body and does not stream, so opencode
// delegations taking that path produce no live events (a known v1 gap).
type PgTaskEvents struct {
	pool   *pgxpool.Pool
	logger *logrus.Logger
}

// NewPgTaskEvents builds a publisher over an existing pool (typically
// PgStore.Pool()).
func NewPgTaskEvents(pool *pgxpool.Pool, logger *logrus.Logger) *PgTaskEvents {
	if logger == nil {
		logger = logrus.New()
	}
	return &PgTaskEvents{pool: pool, logger: logger}
}

// TaskChannel derives the NOTIFY channel for a task id. Postgres unquoted
// identifiers disallow hyphens (task ids look like "task-<hex>"), so
// non-alphanumerics collapse to '_'. Deterministic from the id, so the SSE
// consumer derives the same name with no lookup table (mirrors
// deepresearch's ChannelFor).
func TaskChannel(taskID string) string {
	return "delegator_task_" + sanitizeChannel(taskID)
}

func sanitizeChannel(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// taskChunkEnvelope is the JSON written into each NOTIFY payload. Data is
// base64 so arbitrary subprocess bytes (partial UTF-8, control chars)
// survive the text channel intact.
type taskChunkEnvelope struct {
	Stream string `json:"stream"` // "stdout" | "stderr"
	Data   string `json:"data"`   // base64 of the raw chunk
}

// Publish splits chunk into payload-sized pieces and NOTIFYs each on the
// task's channel. Best-effort; errors are logged at debug and swallowed.
func (p *PgTaskEvents) Publish(taskID, stream string, chunk []byte) {
	if p == nil || p.pool == nil || len(chunk) == 0 {
		return
	}
	channel := TaskChannel(taskID)
	for _, piece := range splitBytes(chunk, taskEventMaxRaw) {
		env := taskChunkEnvelope{Stream: stream, Data: base64.StdEncoding.EncodeToString(piece)}
		payload, err := json.Marshal(env)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), taskEventPublishTimeout)
		// pg_notify(text, text): channel and payload are both string args,
		// so the hyphen-free channel rule and quoting are handled for us.
		_, err = p.pool.Exec(ctx, "SELECT pg_notify($1, $2)", channel, string(payload))
		cancel()
		if err != nil {
			p.logger.WithError(err).WithField("task_id", taskID).Debug("task event publish failed")
		}
	}
}

// splitBytes chops b into <=max-byte slices (the last may be smaller).
// max<=0 or a short input returns b unsplit.
func splitBytes(b []byte, max int) [][]byte {
	if max <= 0 || len(b) <= max {
		return [][]byte{b}
	}
	out := make([][]byte, 0, (len(b)+max-1)/max)
	for len(b) > max {
		out = append(out, b[:max])
		b = b[max:]
	}
	if len(b) > 0 {
		out = append(out, b)
	}
	return out
}
