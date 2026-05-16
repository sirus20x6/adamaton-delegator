package delegator

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sirus20x6/adamomaton-delegator/delegator/budget"
	"github.com/sirus20x6/adamomaton-core/pgutil"
)

func newTestPgStore(t *testing.T) *PgStore {
	t.Helper()
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	store, err := NewPgStore(pgutil.TestDSN(t), 0, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestPgStore_PutGet(t *testing.T) {
	s := newTestPgStore(t)
	now := time.Now().UTC()
	task := &Task{
		ID:         "task-1",
		Agent:      "opencode",
		Provider:   budget.Provider("opencode"),
		Difficulty: Difficulty("easy"),
		Priority:   budget.Priority("normal"),
		Prompt:     "echo hi",
		WorkingDir: "/tmp",
		Model:      "vllm/qwen3.6-27b",
		Status:     TaskStatus("pending"),
		CreatedAt:  now,
	}
	s.Put(task)

	got, ok := s.Get("task-1")
	require.True(t, ok, "expected task to be persisted")
	assert.Equal(t, "task-1", got.ID)
	assert.Equal(t, "opencode", got.Agent)
	assert.Equal(t, budget.Provider("opencode"), got.Provider)
	assert.Equal(t, "echo hi", got.Prompt)
	assert.Equal(t, TaskStatus("pending"), got.Status)
	assert.WithinDuration(t, now, got.CreatedAt, time.Millisecond)
	assert.True(t, got.StartedAt.IsZero(), "StartedAt must be zero before set")
	assert.True(t, got.CompletedAt.IsZero(), "CompletedAt must be zero before set")
}

func TestPgStore_Get_Missing(t *testing.T) {
	s := newTestPgStore(t)
	_, ok := s.Get("nope")
	assert.False(t, ok)
}

func TestPgStore_Update_LifecycleFields(t *testing.T) {
	s := newTestPgStore(t)
	now := time.Now().UTC()
	s.Put(&Task{ID: "t2", Agent: "codex", Prompt: "x", Status: "pending", CreatedAt: now})

	startedAt := now.Add(50 * time.Millisecond)
	completedAt := startedAt.Add(200 * time.Millisecond)

	ok := s.Update("t2", func(t *Task) {
		t.Status = "completed"
		t.StartedAt = startedAt
		t.CompletedAt = completedAt
		t.ExitCode = 0
		t.Output = "done"
	})
	require.True(t, ok)

	got, ok := s.Get("t2")
	require.True(t, ok)
	assert.Equal(t, TaskStatus("completed"), got.Status)
	assert.WithinDuration(t, startedAt, got.StartedAt, time.Millisecond)
	assert.WithinDuration(t, completedAt, got.CompletedAt, time.Millisecond)
	assert.Equal(t, 0, got.ExitCode)
	assert.Equal(t, "done", got.Output)
}

func TestPgStore_Update_Missing(t *testing.T) {
	s := newTestPgStore(t)
	called := false
	ok := s.Update("never-existed", func(t *Task) { called = true })
	assert.False(t, ok)
	assert.False(t, called, "fn must not run when task is missing")
}

func TestPgStore_List_FiltersAndOrder(t *testing.T) {
	s := newTestPgStore(t)
	base := time.Now().UTC().Add(-1 * time.Hour)
	for i, status := range []TaskStatus{"pending", "running", "completed", "failed"} {
		s.Put(&Task{
			ID:        statusTaskID(status),
			Agent:     "opencode",
			Prompt:    "echo",
			Status:    status,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	s.Put(&Task{
		ID:        "other-agent",
		Agent:     "codex",
		Prompt:    "x",
		Status:    "completed",
		CreatedAt: base.Add(10 * time.Minute),
	})

	all := s.List("", "")
	require.Len(t, all, 5)
	// Newest first ordering.
	assert.Equal(t, "other-agent", all[0].ID)

	completed := s.List("completed", "")
	require.Len(t, completed, 2)
	for _, task := range completed {
		assert.Equal(t, TaskStatus("completed"), task.Status)
	}

	opencode := s.List("", "opencode")
	require.Len(t, opencode, 4)
	for _, task := range opencode {
		assert.Equal(t, "opencode", task.Agent)
	}

	combined := s.List("completed", "opencode")
	require.Len(t, combined, 1)
	assert.Equal(t, statusTaskID("completed"), combined[0].ID)
}

func TestPgStore_EvictsOldTerminalTasks(t *testing.T) {
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	s, err := NewPgStore(pgutil.TestDSN(t), 3, logger) // maxTasks=3
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	now := time.Now().UTC()
	// Insert 5 terminal tasks; expect the oldest two to be evicted.
	for i := 0; i < 5; i++ {
		s.Put(&Task{
			ID:        fmt.Sprintf("term-%d", i),
			Agent:     "opencode",
			Prompt:    "x",
			Status:    "completed",
			CreatedAt: now.Add(time.Duration(i) * time.Second),
		})
	}
	got := s.List("", "")
	require.Len(t, got, 3, "should evict oldest beyond maxTasks=3")
	// The three retained must be the newest three.
	for _, task := range got {
		assert.NotEqual(t, "term-0", task.ID)
		assert.NotEqual(t, "term-1", task.ID)
	}
}

func TestPgStore_DoesNotEvictInFlightTasks(t *testing.T) {
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	s, err := NewPgStore(pgutil.TestDSN(t), 2, logger) // maxTasks=2
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	now := time.Now().UTC()
	// Two pending tasks (must not be evicted).
	s.Put(&Task{ID: "pending-a", Agent: "x", Prompt: "p", Status: "pending", CreatedAt: now})
	s.Put(&Task{ID: "pending-b", Agent: "x", Prompt: "p", Status: "running", CreatedAt: now.Add(time.Second)})
	// Plus three terminal tasks; evictor should drop terminal only.
	for i := 0; i < 3; i++ {
		s.Put(&Task{
			ID:        fmt.Sprintf("done-%d", i),
			Agent:     "x",
			Prompt:    "p",
			Status:    "completed",
			CreatedAt: now.Add(time.Duration(2+i) * time.Second),
		})
	}
	got := s.List("", "")
	ids := map[string]bool{}
	for _, t := range got {
		ids[t.ID] = true
	}
	assert.True(t, ids["pending-a"], "pending must survive eviction")
	assert.True(t, ids["pending-b"], "running must survive eviction")
}

func TestPgStore_ConcurrentUpdatesSerialise(t *testing.T) {
	s := newTestPgStore(t)
	s.Put(&Task{
		ID:        "race",
		Agent:     "opencode",
		Prompt:    "x",
		Status:    "pending",
		CreatedAt: time.Now().UTC(),
	})

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			s.Update("race", func(t *Task) {
				t.ExitCode++
			})
		}()
	}
	wg.Wait()

	got, ok := s.Get("race")
	require.True(t, ok)
	assert.Equal(t, goroutines, got.ExitCode,
		"FOR UPDATE row lock must serialise increments to avoid lost updates")
}

// Helpers ----

func statusTaskID(s TaskStatus) string { return "task-" + string(s) }
