package delegator

import (
	"sort"
	"sync"
)

// TaskStore is an in-memory ring of recent delegations. Phase 1 keeps state
// in process — Phase 2 will replace this with workflow lookups against
// Temporal. The ring caps at maxTasks so a long-running session can't grow
// unbounded; oldest completed tasks evict first.
type TaskStore struct {
	mu       sync.RWMutex
	tasks    map[string]*Task
	order    []string
	maxTasks int
}

// NewTaskStore returns a store that retains the most recent maxTasks tasks.
// Pass 0 for the default of 1000.
func NewTaskStore(maxTasks int) *TaskStore {
	if maxTasks <= 0 {
		maxTasks = 1000
	}
	return &TaskStore{
		tasks:    make(map[string]*Task),
		order:    make([]string, 0, maxTasks),
		maxTasks: maxTasks,
	}
}

// Put inserts or updates a task.
func (s *TaskStore) Put(t *Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.tasks[t.ID]
	// Set tasks first so the just-added entry is visible to evictIfFull —
	// otherwise the evictor sees an "orphan" in order and treats it as
	// evictable, dropping the new task we just inserted.
	s.tasks[t.ID] = t
	if !exists {
		s.order = append(s.order, t.ID)
		s.evictIfFull()
	}
}

// Get returns a copy of the task or false.
func (s *TaskStore) Get(id string) (*Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// Update applies fn to a task under the lock. Returns false if the task is
// gone (already evicted or never existed). fn must not block; it runs with
// the store mutex held.
func (s *TaskStore) Update(id string, fn func(*Task)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false
	}
	fn(t)
	return true
}

// List returns copies of all tasks, newest first. Optional filters by
// status and agent — empty values mean no filter on that field.
func (s *TaskStore) List(status TaskStatus, agent string) []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Task, 0, len(s.order))
	for _, id := range s.order {
		t, ok := s.tasks[id]
		if !ok {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		if agent != "" && t.Agent != agent {
			continue
		}
		cp := *t
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// evictIfFull drops the oldest *terminal* task when the store is over cap.
// We never evict in-flight tasks (status pending/running) — that would
// strand the caller's task_id. Caller holds s.mu.
func (s *TaskStore) evictIfFull() {
	for len(s.order) > s.maxTasks {
		var removed bool
		for i, id := range s.order {
			t, ok := s.tasks[id]
			if !ok {
				// Already gone (shouldn't happen) — drop the order entry.
				s.order = append(s.order[:i], s.order[i+1:]...)
				removed = true
				break
			}
			if t.Status == StatusPending || t.Status == StatusRunning {
				continue
			}
			s.order = append(s.order[:i], s.order[i+1:]...)
			delete(s.tasks, id)
			removed = true
			break
		}
		if !removed {
			// Everything in flight — let the store grow until tasks settle.
			return
		}
	}
}
