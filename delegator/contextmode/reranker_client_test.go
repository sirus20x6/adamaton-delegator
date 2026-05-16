package contextmode

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

// rerankHitMarker is the literal token the fake reranker scores
// highest. Tests inject it into the chunk they want the reranker to
// surface; queries / content trigrams don't have to overlap. This
// lets a test build a corpus where BM25 misses (no trigram overlap
// between intent and content) but the reranker still rescues, which
// is the design's whole point.
const rerankHitMarker = "RERANK_HIT_MARKER"

// fakeReranker stands in for the deepresearch sidecar's /v1/rerank.
// Two scoring modes coexist:
//   - Documents containing rerankHitMarker get score 1.0 (lets tests
//     simulate "cross-encoder found a match BM25 / dense couldn't").
//   - Other documents get a 3-byte-overlap-based fallback score
//     (deterministic ranking for plain bag-of-words tests).
// Documents containing "boom" trigger a 500.
type fakeReranker struct {
	t      *testing.T
	server *httptest.Server
	calls  int
}

func newFakeReranker(t *testing.T) *fakeReranker {
	t.Helper()
	f := &fakeReranker{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rerank", func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		var req struct {
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
			TopK      int      `json:"top_k"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad req", http.StatusBadRequest)
			return
		}
		for _, d := range req.Documents {
			if strings.Contains(d, "boom") {
				http.Error(w, "synthetic failure", http.StatusInternalServerError)
				return
			}
		}
		// Score each doc; emit unsorted so the client's sort step is
		// exercised. Marker-tagged docs short-circuit to 1.0 so tests
		// can decouple the rerank rescue from query/content overlap.
		results := make([]map[string]any, len(req.Documents))
		for i, d := range req.Documents {
			score := fakeOverlap(req.Query, d)
			if strings.Contains(d, rerankHitMarker) {
				score = 1.0
			}
			results[i] = map[string]any{
				"index": i,
				"score": score,
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": results,
			"model":   "fake-reranker",
		})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// fakeOverlap is a tiny deterministic similarity stand-in: count
// 3-byte windows shared between query and document, normalised.
func fakeOverlap(q, d string) float64 {
	if len(q) < 3 || len(d) < 3 {
		return 0
	}
	seen := map[uint32]bool{}
	for i := 0; i+3 <= len(q); i++ {
		h := fnv.New32a()
		_, _ = h.Write([]byte(q[i : i+3]))
		seen[h.Sum32()] = true
	}
	overlap := 0
	total := 0
	for i := 0; i+3 <= len(d); i++ {
		h := fnv.New32a()
		_, _ = h.Write([]byte(d[i : i+3]))
		if seen[h.Sum32()] {
			overlap++
		}
		total++
	}
	if total == 0 {
		return 0
	}
	return float64(overlap) / float64(total)
}

func TestRerankerClient_Disabled(t *testing.T) {
	c := NewRerankerClient("", "", quietLogger())
	if c.Enabled() {
		t.Error("empty URL should be disabled")
	}
	out, err := c.Rerank(context.Background(), "q", []string{"a"}, 5)
	if err != nil {
		t.Errorf("disabled Rerank should not error, got %v", err)
	}
	if out != nil {
		t.Errorf("disabled Rerank should return nil, got %d", len(out))
	}
}

func TestRerankerClient_SortsAndTopK(t *testing.T) {
	f := newFakeReranker(t)
	c := NewRerankerClient(f.server.URL, "fake", quietLogger())

	docs := []string{
		"this document is completely unrelated to widgets",
		"widgets widgets widgets",
		"a single mention of widgets here",
	}
	out, err := c.Rerank(context.Background(), "widgets", docs, 2)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected top_k=2 results, got %d", len(out))
	}
	if out[0].Index != 1 {
		t.Errorf("expected highest score on doc[1] (widgets x3); got idx=%d score=%v",
			out[0].Index, out[0].Score)
	}
	if out[0].Score < out[1].Score {
		t.Errorf("results must be descending by score; got %v then %v", out[0].Score, out[1].Score)
	}
}

func TestRerankerClient_HTTPError(t *testing.T) {
	f := newFakeReranker(t)
	c := NewRerankerClient(f.server.URL, "fake", quietLogger())
	_, err := c.Rerank(context.Background(), "q", []string{"this has boom in it"}, 5)
	if err == nil {
		t.Fatal("expected error for boom trigger")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("expected HTTP 500 in error, got %v", err)
	}
}

func TestRerankerClient_EmptyDocs(t *testing.T) {
	f := newFakeReranker(t)
	c := NewRerankerClient(f.server.URL, "fake", quietLogger())
	out, err := c.Rerank(context.Background(), "q", nil, 5)
	if err != nil {
		t.Errorf("empty docs should not error, got %v", err)
	}
	if out != nil {
		t.Errorf("empty docs should return nil, got %d", len(out))
	}
	if f.calls != 0 {
		t.Errorf("empty docs should short-circuit without hitting server; got %d calls", f.calls)
	}
}

func quietLogger() *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.WarnLevel)
	l.SetOutput(io.Discard)
	return l
}
