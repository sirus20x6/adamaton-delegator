package contextmode

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamomaton-core/pgutil"
)

// TestService_Cascade_RerankerPreemptsCompress: when a BGE reranker
// is configured and stages 1+2 miss, stage 3 reranks the source's
// chunks against the intent and returns top-K verbatim. The
// fakeCompressor's Compress hook should NOT fire — verbatim bytes
// beat paraphrased ones.
func TestService_Cascade_RerankerPreemptsCompress(t *testing.T) {
	skipUnlessBash(t)
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	idx, err := NewIndex(pgutil.TestDSN(t), logger)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	fc := &fakeCompressor{
		translateOut: "ALSO_NOT_PRESENT",
		compressOut:  "should-not-run",
	}
	svc := NewService(idx, fc, logger)
	svc.Threshold = 256

	f := newFakeReranker(t)
	svc.SetReranker(NewRerankerClient(f.server.URL, "fake", logger))

	// Output has 50 filler lines plus one line carrying the rerank
	// hit marker. Intent uses a string whose trigrams do not appear
	// in any chunk's content (so BM25 misses), AND translation also
	// resolves to ALSO_NOT_PRESENT (also misses). Stage 3 with the
	// reranker MUST surface the marker chunk because the fake reranker
	// short-circuits marker-tagged docs to score 1.0 regardless of
	// the intent → content trigram overlap.
	script := `for i in $(seq 1 50); do
		if [ $i -eq 25 ]; then echo "this chunk wins via ` + rerankHitMarker + `"; else echo "filler line $i"; fi
	done`
	res, err := svc.Execute(context.Background(), ExecuteRequest{
		Script: script,
		Intent: "xkcd-nonsense-zzz",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if res.Mode != ModeCompressed {
		t.Fatalf("expected ModeCompressed (rerank-rescue), got %q", res.Mode)
	}
	if len(res.Snippets) == 0 {
		t.Fatal("expected rerank to surface at least one snippet")
	}
	if fc.compressCalls != 0 {
		t.Errorf("LLM compress must NOT fire when the reranker is configured; got %d calls",
			fc.compressCalls)
	}
	if f.calls != 1 {
		t.Errorf("reranker should have been called exactly once; got %d", f.calls)
	}
	if res.Snippets[0].Score < 0.99 {
		t.Errorf("top-ranked snippet should be the marker chunk with score ~1.0; got %v",
			res.Snippets[0].Score)
	}
	if !contains(res.Snippets[0].Content, rerankHitMarker) {
		t.Errorf("top-ranked snippet should contain rerankHitMarker; got %q",
			res.Snippets[0].Content)
	}
}

// TestService_Cascade_RerankerUnavailableFallsBackToCompress: when
// no reranker is configured, stage 3 still uses the LLM compressor
// (pre-Phase-4 behaviour). This is what every gogents deployment
// without a BGE_SIDECAR_URL sees.
func TestService_Cascade_RerankerUnavailableFallsBackToCompress(t *testing.T) {
	skipUnlessBash(t)
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	idx, _ := NewIndex(pgutil.TestDSN(t), logger)
	t.Cleanup(func() { _ = idx.Close() })

	fc := &fakeCompressor{
		translateOut: "ALSO_NOT_PRESENT",
		compressOut:  "compressed summary",
	}
	svc := NewService(idx, fc, logger)
	svc.Threshold = 256
	// No SetReranker — reranker stays nil.

	script := `for i in $(seq 1 50); do echo "filler line $i"; done`
	res, err := svc.Execute(context.Background(), ExecuteRequest{
		Script: script,
		Intent: "absent_thing",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Mode != ModeCompressed {
		t.Errorf("expected ModeCompressed, got %q", res.Mode)
	}
	if fc.compressCalls != 1 {
		t.Errorf("compressor should fire when reranker isn't configured; got %d calls",
			fc.compressCalls)
	}
}

// contains is a tiny inline helper to avoid importing strings into
// the test for one use.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
