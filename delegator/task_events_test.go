package delegator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sirus20x6/adamaton-core/pgutil"
)

func TestTaskChannel(t *testing.T) {
	// Hyphens (task ids look like "task-<hex>") and other non-alphanumerics
	// collapse to '_' so the result is a legal unquoted Postgres identifier.
	assert.Equal(t, "delegator_task_task_ab12_cd", TaskChannel("task-ab12-cd"))
	assert.Equal(t, "delegator_task_abc", TaskChannel("ABC")) // lowercased
	for _, r := range TaskChannel("weird/.:id space") {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
		assert.Truef(t, ok, "channel must be [a-z0-9_], got %q", r)
	}
}

func TestSplitBytes(t *testing.T) {
	// Short input is returned unsplit.
	assert.Equal(t, [][]byte{[]byte("abc")}, splitBytes([]byte("abc"), 5))
	// max<=0 disables splitting.
	assert.Equal(t, [][]byte{[]byte("xyz")}, splitBytes([]byte("xyz"), 0))
	// Exact multiples and a trailing remainder.
	got := splitBytes([]byte("abcdefg"), 3)
	require.Len(t, got, 3)
	assert.Equal(t, "abc", string(got[0]))
	assert.Equal(t, "def", string(got[1]))
	assert.Equal(t, "g", string(got[2]))
}

func TestPgTaskEvents_NilAndEmptyAreNoops(t *testing.T) {
	// Must not panic with a nil receiver, nil pool, or empty chunk.
	var p *PgTaskEvents
	p.Publish("task-1", "stdout", []byte("x"))
	NewPgTaskEvents(nil, nil).Publish("task-1", "stdout", []byte("x"))
	NewPgTaskEvents(nil, nil).Publish("task-1", "stdout", nil)
}

func TestPgTaskEvents_PublishNotifies(t *testing.T) {
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	pool, err := pgxpool.New(context.Background(), pgutil.TestDSN(t))
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Dedicated connection held in LISTEN mode for the duration.
	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	t.Cleanup(conn.Release)

	const taskID = "task-abc-123"
	channel := TaskChannel(taskID)
	_, err = conn.Exec(context.Background(), "LISTEN "+channel) // channel is sanitized → safe identifier
	require.NoError(t, err)

	NewPgTaskEvents(pool, nil).Publish(taskID, "stderr", []byte("hello world"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(ctx)
	require.NoError(t, err)
	assert.Equal(t, channel, n.Channel)

	var env taskChunkEnvelope
	require.NoError(t, json.Unmarshal([]byte(n.Payload), &env))
	assert.Equal(t, "stderr", env.Stream)
	data, err := base64.StdEncoding.DecodeString(env.Data)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(data))
}

func TestPgTaskEvents_SplitsLargeChunk(t *testing.T) {
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	pool, err := pgxpool.New(context.Background(), pgutil.TestDSN(t))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	t.Cleanup(conn.Release)

	const taskID = "task-big"
	channel := TaskChannel(taskID)
	_, err = conn.Exec(context.Background(), "LISTEN "+channel)
	require.NoError(t, err)

	// 12_000 raw bytes → 3 notifications at taskEventMaxRaw=5000.
	big := make([]byte, 12_000)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	NewPgTaskEvents(pool, nil).Publish(taskID, "stdout", big)

	var reassembled []byte
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		n, err := conn.Conn().WaitForNotification(ctx)
		cancel()
		require.NoError(t, err, "expected 3 notifications for a 12KB chunk")
		var env taskChunkEnvelope
		require.NoError(t, json.Unmarshal([]byte(n.Payload), &env))
		require.LessOrEqual(t, len(n.Payload), 8000, "payload must stay under the pg_notify cap")
		piece, err := base64.StdEncoding.DecodeString(env.Data)
		require.NoError(t, err)
		reassembled = append(reassembled, piece...)
	}
	assert.Equal(t, big, reassembled, "split pieces must reassemble to the original")
}
