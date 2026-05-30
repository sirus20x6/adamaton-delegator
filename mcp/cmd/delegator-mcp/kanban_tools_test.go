package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestKanbanClient points a kanbanClient at the given base URL with tight
// timeouts so retry/backoff tests don't add real wall-clock latency.
func newTestKanbanClient(base string) *kanbanClient {
	return &kanbanClient{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 5 * time.Second},
	}
}

// TestKanbanClient_GetSuccess verifies a 2xx GET returns the raw JSON body.
func TestKanbanClient_GetSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/p1/kanban/boards" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %q", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"b1","name":"Board One"}]`))
	}))
	defer srv.Close()

	c := newTestKanbanClient(srv.URL)
	raw, err := c.do(context.Background(), http.MethodGet, "/projects/p1/kanban/boards", nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	var boards []map[string]any
	if err := json.Unmarshal(raw, &boards); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(boards) != 1 || boards[0]["id"] != "b1" {
		t.Fatalf("unexpected body: %s", raw)
	}
}

// TestKanbanClient_PostBodyEncoded verifies a POST JSON-encodes the body and
// returns the created resource on 2xx.
func TestKanbanClient_PostBodyEncoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %q", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("missing json content-type, got %q", ct)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body["agent_id"] != "agent-7" {
			t.Errorf("agent_id not threaded: %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"card":{"id":"c1"},"claim_token":"tok-1"}`))
	}))
	defer srv.Close()

	c := newTestKanbanClient(srv.URL)
	raw, err := c.do(context.Background(), http.MethodPost, "/kanban/cards/c1/claim",
		map[string]any{"agent_id": "agent-7"})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["claim_token"] != "tok-1" {
		t.Fatalf("claim_token missing: %s", raw)
	}
}

// TestKanbanClient_ClaimConflict409 verifies a 409 (card already claimed) is
// returned as an error with the apiserver's message verbatim and is NOT retried
// (it's a definitive answer, not a transient blip).
func TestKanbanClient_ClaimConflict409(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"card already claimed by agent-3"}`))
	}))
	defer srv.Close()

	c := newTestKanbanClient(srv.URL)
	_, err := c.do(context.Background(), http.MethodPost, "/kanban/cards/c1/claim",
		map[string]any{"agent_id": "agent-7"})
	if err == nil {
		t.Fatal("expected error on 409")
	}
	if !strings.Contains(err.Error(), "already claimed by agent-3") {
		t.Fatalf("409 message not surfaced: %v", err)
	}
	if !strings.Contains(err.Error(), "409") {
		t.Fatalf("status code not in error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("409 must not be retried, got %d calls", got)
	}
}

// TestKanbanClient_4xxNotRetried verifies a generic 4xx on a GET is not retried.
func TestKanbanClient_4xxNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestKanbanClient(srv.URL)
	_, err := c.do(context.Background(), http.MethodGet, "/projects/nope", nil)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("4xx must not be retried, got %d calls", got)
	}
}

// TestKanbanClient_GetRetriesOn5xx verifies an idempotent GET retries on 5xx and
// succeeds once the apiserver recovers.
func TestKanbanClient_GetRetriesOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			http.Error(w, "boom", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := newTestKanbanClient(srv.URL)
	raw, err := c.do(context.Background(), http.MethodGet, "/projects", nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("unexpected body: %s", raw)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 attempts (2 fail + 1 ok), got %d", got)
	}
}

// TestKanbanClient_PostNotRetriedOn5xx verifies a non-idempotent POST is NOT
// retried on 5xx — replaying a mutation could double-apply it.
func TestKanbanClient_PostNotRetriedOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestKanbanClient(srv.URL)
	_, err := c.do(context.Background(), http.MethodPost, "/kanban/cards/c1/move",
		map[string]any{"target_column_id": "col-2"})
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("POST must not be retried, got %d calls", got)
	}
}

// TestKanbanClient_GetExhaustsRetries verifies a GET that 5xxes on every attempt
// eventually returns the last error after the retry budget is spent.
func TestKanbanClient_GetExhaustsRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "still down", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := newTestKanbanClient(srv.URL)
	_, err := c.do(context.Background(), http.MethodGet, "/projects", nil)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got := atomic.LoadInt32(&calls); got != int32(maxRetries) {
		t.Fatalf("expected %d attempts, got %d", maxRetries, got)
	}
}

// TestKanbanClient_NetworkErrorRetriedThenFails verifies a connection-refused
// (server closed) GET is treated as transient and retried up to the budget.
func TestKanbanClient_NetworkErrorRetriedThenFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close() // nothing is listening now → connection refused

	c := newTestKanbanClient(base)
	_, err := c.do(context.Background(), http.MethodGet, "/projects", nil)
	if err == nil {
		t.Fatal("expected error against a closed server")
	}
	if !strings.Contains(err.Error(), "call GET /projects") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestKanbanClient_ContextCancelledNotRetried verifies that an already-cancelled
// caller context short-circuits without spending the retry budget.
func TestKanbanClient_ContextCancelledNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := newTestKanbanClient(srv.URL)
	_, err := c.do(ctx, http.MethodGet, "/projects", nil)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	// Either zero calls (cancelled before dial) or one call whose error wraps
	// the context cancellation — never the full retry budget.
	if got := atomic.LoadInt32(&calls); got > 1 {
		t.Fatalf("cancelled ctx must not exhaust retries, got %d calls", got)
	}
}

// TestIsIdempotent locks the retry-eligibility contract: only GET/HEAD.
func TestIsIdempotent(t *testing.T) {
	cases := map[string]bool{
		http.MethodGet:    true,
		http.MethodHead:   true,
		http.MethodPost:   false,
		http.MethodPut:    false,
		http.MethodDelete: false,
		http.MethodPatch:  false,
	}
	for method, want := range cases {
		if got := isIdempotent(method); got != want {
			t.Errorf("isIdempotent(%s) = %v, want %v", method, got, want)
		}
	}
}

// TestBackoffFor verifies the backoff is jittered within [0, clamp] and never
// exceeds maxBackoff.
func TestBackoffFor(t *testing.T) {
	for attempt := 1; attempt <= 6; attempt++ {
		for i := 0; i < 50; i++ {
			d := backoffFor(attempt)
			if d < 0 {
				t.Fatalf("attempt %d: negative backoff %v", attempt, d)
			}
			if d > maxBackoff {
				t.Fatalf("attempt %d: backoff %v exceeds max %v", attempt, d, maxBackoff)
			}
		}
	}
}
