package contextmode

import (
	"os"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-core/octen"
	"github.com/sirus20x6/adamaton-core/pgutil"
)

// TestSearchScopedDense_RoundTrip verifies the full Phase-3 pathway:
// Insert with an octen client attached writes embeddings, then
// SearchScopedDense returns rows ranked by cosine similarity. The
// fake sidecar emits deterministic-per-input embeddings so the
// "closer-content scores higher" assertion is reproducible.
func TestSearchScopedDense_RoundTrip(t *testing.T) {
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	idx, err := NewIndex(pgutil.TestDSN(t), logger)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	f := newFakeOcten(t)
	idx.SetOcten(octen.NewClient(f.server.URL, "octen-test", logger))

	chunks := []IndexedChunk{
		{Heading: "alpha", Content: "alpha is the first one"},
		{Heading: "beta", Content: "beta is somewhere different"},
		{Heading: "gamma", Content: "gamma is yet another"},
	}
	if err := idx.Insert("dense-src", "execute", "dense-test", chunks); err != nil {
		t.Fatalf("Insert with embeddings: %v", err)
	}

	// fake batched 3 inputs in one call.
	if len(f.docInputs) != 1 {
		t.Errorf("expected 1 batched doc call from Insert, got %d", len(f.docInputs))
	}

	got, err := idx.SearchScopedDense("dense-src", "alpha is the first one", 5)
	if err != nil {
		t.Fatalf("SearchScopedDense: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least one dense hit")
	}
	// The exact-match chunk should rank first because its embedding
	// is identical to the query's (deterministic embedder).
	if got[0].ChunkIdx != 0 {
		t.Errorf("expected chunk 0 (alpha) to rank first; got idx=%d content=%q",
			got[0].ChunkIdx, got[0].Content)
	}
	if got[0].Score < 0.99 {
		t.Errorf("self-match cosine should be ~1.0; got %v", got[0].Score)
	}
}

func TestSearchScopedDense_DisabledReturnsNil(t *testing.T) {
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	idx, err := NewIndex(pgutil.TestDSN(t), logger)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	// Don't attach octen.

	got, err := idx.SearchScopedDense("any", "query text", 10)
	if err != nil {
		t.Errorf("disabled SearchScopedDense should not error, got %v", err)
	}
	if got != nil {
		t.Errorf("disabled SearchScopedDense should return nil, got %d", len(got))
	}
}

func TestInsert_OctenFailureGraceful(t *testing.T) {
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	idx, err := NewIndex(pgutil.TestDSN(t), logger)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	f := newFakeOcten(t)
	idx.SetOcten(octen.NewClient(f.server.URL, "octen-test", logger))

	// "boom" in the content triggers a synthetic 500 inside the fake.
	chunks := []IndexedChunk{
		{Heading: "h", Content: "normal text"},
		{Heading: "h2", Content: "this has boom in it"},
	}
	// Insert should NOT fail just because the sidecar errored; the
	// chunks land with NULL embeddings, BM25 keeps working.
	if err := idx.Insert("failgraceful", "execute", "test", chunks); err != nil {
		t.Fatalf("Insert should be best-effort on sidecar failure; got %v", err)
	}

	// Confirm BM25 still finds the chunks despite NULL embeddings.
	hits, err := idx.SearchScoped("failgraceful", "boom", 5)
	if err != nil {
		t.Fatalf("BM25 SearchScoped: %v", err)
	}
	if len(hits) == 0 {
		t.Error("BM25 should still match after octen failure")
	}

	// Dense search using a non-boom query — must return zero rows
	// because every embedding in this source is NULL, and the partial
	// HNSW index skips NULLs. We use a query that doesn't trigger the
	// fake's 500 path so the failure mode under test is "rows have no
	// embedding", not "query embedding itself failed".
	dense, err := idx.SearchScopedDense("failgraceful", "harmless query", 5)
	if err != nil {
		t.Fatalf("SearchScopedDense: %v", err)
	}
	if len(dense) != 0 {
		t.Errorf("dense search should return empty for NULL-embedding rows; got %d hits",
			len(dense))
	}

	// Recovery: a non-failing insert after this should produce a row
	// with a real embedding that dense search can find.
	if err := idx.Insert("recovers", "execute", "test", []IndexedChunk{
		{Heading: "ok", Content: "all good here"},
	}); err != nil {
		t.Fatalf("recovery Insert: %v", err)
	}
	dense2, err := idx.SearchScopedDense("recovers", "all good here", 5)
	if err != nil {
		t.Fatalf("recovery SearchScopedDense: %v", err)
	}
	if len(dense2) == 0 {
		t.Error("post-recovery dense search should hit")
	}
}
