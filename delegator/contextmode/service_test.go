package contextmode

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-core/pgutil"
)

func skipUnlessBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping")
	}
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	idx, err := NewIndex(pgutil.TestDSN(t), logger)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return NewService(idx, NoopCompressor{}, logger)
}

func TestService_Execute_RawSmallOutput(t *testing.T) {
	skipUnlessBash(t)
	svc := newTestService(t)
	res, err := svc.Execute(context.Background(), ExecuteRequest{
		Script: "echo small",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Mode != ModeRaw {
		t.Errorf("expected ModeRaw, got %q", res.Mode)
	}
	if !strings.Contains(res.Output, "small") {
		t.Errorf("output missing: %q", res.Output)
	}
	if res.SourceID != "" {
		t.Errorf("small outputs shouldn't be indexed; got source_id=%q", res.SourceID)
	}
}

func TestService_Execute_BigOutputBM25(t *testing.T) {
	skipUnlessBash(t)
	svc := newTestService(t)
	svc.Threshold = 256 // shrink threshold so the test stays fast

	// Generate 50 lines, with a single distinctive marker on line 25.
	script := `for i in $(seq 1 50); do
  if [ $i -eq 25 ]; then echo "the special MARKER_XYZ phrase"; else echo "filler line $i"; fi
done`
	res, err := svc.Execute(context.Background(), ExecuteRequest{
		Script: script,
		Intent: "MARKER_XYZ",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Mode != ModeBM25 {
		t.Fatalf("expected ModeBM25, got %q (output: %q)", res.Mode, res.Output)
	}
	if len(res.Snippets) == 0 {
		t.Error("expected at least one snippet")
	}
	if !strings.Contains(res.Output, "MARKER_XYZ") {
		t.Errorf("output should include the marker line: %q", res.Output)
	}
}

func TestService_Execute_BigOutputNoIntent(t *testing.T) {
	skipUnlessBash(t)
	svc := newTestService(t)
	svc.Threshold = 256

	script := `for i in $(seq 1 50); do echo "line $i"; done`
	res, err := svc.Execute(context.Background(), ExecuteRequest{
		Script: script,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Mode != ModeTruncated {
		t.Errorf("expected ModeTruncated when no intent, got %q", res.Mode)
	}
	if res.SourceID == "" {
		t.Error("expected source_id even when truncated, so search() can be used later")
	}
	if !strings.Contains(res.Output, res.SourceID) {
		t.Error("output should include the source_id hint")
	}
}

// fakeCompressor lets us exercise the three-stage cascade without
// spinning up opencode.
type fakeCompressor struct {
	translateOut string
	translateErr error
	compressOut  string
	compressErr  error

	translateCalls int
	compressCalls  int
}

func (f *fakeCompressor) TranslateIntent(_ context.Context, _ string, _ []string) (string, error) {
	f.translateCalls++
	return f.translateOut, f.translateErr
}

func (f *fakeCompressor) Compress(_ context.Context, _, _ string) (string, error) {
	f.compressCalls++
	return f.compressOut, f.compressErr
}

func TestService_Cascade_LiteralBM25Hits_SkipsBoth(t *testing.T) {
	skipUnlessBash(t)
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	idx, _ := NewIndex(pgutil.TestDSN(t), logger)
	t.Cleanup(func() { _ = idx.Close() })

	fc := &fakeCompressor{translateOut: "shouldnt run", compressOut: "shouldnt run"}
	svc := NewService(idx, fc, logger)
	svc.Threshold = 256

	// Literal intent term IS in the output → BM25 hits → translation
	// and compression must NOT fire.
	script := `for i in $(seq 1 50); do
  if [ $i -eq 10 ]; then echo "the LITERAL_HIT phrase"; else echo "filler $i"; fi
done`
	res, err := svc.Execute(context.Background(), ExecuteRequest{
		Script: script,
		Intent: "LITERAL_HIT",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Mode != ModeBM25 {
		t.Errorf("expected ModeBM25, got %q", res.Mode)
	}
	if fc.translateCalls != 0 {
		t.Errorf("translate should not have been called on literal-hit path; got %d", fc.translateCalls)
	}
	if fc.compressCalls != 0 {
		t.Errorf("compress should not have been called on literal-hit path; got %d", fc.compressCalls)
	}
	if res.BM25Query != "LITERAL_HIT" {
		t.Errorf("BM25Query should echo literal intent; got %q", res.BM25Query)
	}
}

func TestService_Cascade_TranslateRescuesMiss(t *testing.T) {
	skipUnlessBash(t)
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	idx, _ := NewIndex(pgutil.TestDSN(t), logger)
	t.Cleanup(func() { _ = idx.Close() })

	fc := &fakeCompressor{
		translateOut: "RESCUE_TERM",
		compressOut:  "shouldnt run",
	}
	svc := NewService(idx, fc, logger)
	svc.Threshold = 256

	// Output contains RESCUE_TERM but the user's intent uses different
	// vocabulary ("vague language") that has no match. Translation
	// should rewrite the query and BM25 should hit on retry.
	script := `for i in $(seq 1 50); do
  if [ $i -eq 10 ]; then echo "the RESCUE_TERM is here"; else echo "filler $i"; fi
done`
	res, err := svc.Execute(context.Background(), ExecuteRequest{
		Script: script,
		Intent: "vague natural language query that wont match anything literally",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Mode != ModeBM25Translated {
		t.Errorf("expected ModeBM25Translated, got %q (output: %q)", res.Mode, res.Output)
	}
	if fc.translateCalls != 1 {
		t.Errorf("translate should have been called once; got %d", fc.translateCalls)
	}
	if fc.compressCalls != 0 {
		t.Errorf("compress must not fire when translated BM25 hits; got %d", fc.compressCalls)
	}
	if res.BM25Query != "RESCUE_TERM" {
		t.Errorf("BM25Query should be the translated terms; got %q", res.BM25Query)
	}
}

func TestService_Cascade_CompressLastResort(t *testing.T) {
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

	// Neither literal nor translated terms appear in the content.
	// Both BM25 stages miss → full compress fires.
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
	if fc.translateCalls != 1 {
		t.Errorf("translate should have run once; got %d", fc.translateCalls)
	}
	if fc.compressCalls != 1 {
		t.Errorf("compress should have run once; got %d", fc.compressCalls)
	}
}

func TestService_Cascade_NoopCompressorSkipsTranslate(t *testing.T) {
	skipUnlessBash(t)
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	idx, _ := NewIndex(pgutil.TestDSN(t), logger)
	t.Cleanup(func() { _ = idx.Close() })

	svc := NewService(idx, NoopCompressor{}, logger)
	svc.Threshold = 256

	// Noop translator returns "" — service should not retry BM25 on
	// an empty query, just go straight to compress (which also noops),
	// and end up Truncated.
	script := `for i in $(seq 1 50); do echo "filler $i"; done`
	res, err := svc.Execute(context.Background(), ExecuteRequest{
		Script: script,
		Intent: "no_match_anywhere",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Mode != ModeCompressed && res.Mode != ModeTruncated {
		// NoopCompressor.Compress returns the raw bytes with no error,
		// so the service treats that as a successful compression.
		t.Errorf("expected ModeCompressed or ModeTruncated for noop path; got %q", res.Mode)
	}
}

func TestCleanTranslatedQuery(t *testing.T) {
	cases := map[string]string{
		"":                            "",
		"hello world":                 "hello world",
		"  spaced   out  ":            "spaced out",
		"```\nfoo bar\n```":           "foo bar",
		"foo\nbar\tbaz":               "foo bar baz",
		"-foo bar-":                   "foo bar",
		// dedup: same term repeated on a separate line
		"walk DOM h1 h2\nwalk DOM h1 h2":          "walk DOM h1 h2",
		"alpha alpha beta":                       "alpha beta",
		"DOM dom Dom":                            "DOM",
		// cap at 6
		"a b c d e f g h i j":                    "a b c d e f",
	}
	for in, want := range cases {
		got := cleanTranslatedQuery(in)
		if got != want {
			t.Errorf("cleanTranslatedQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeadingsForSource(t *testing.T) {
	skipUnlessBash(t)
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	idx, err := NewIndex(pgutil.TestDSN(t), logger)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	chunks := []IndexedChunk{
		{Heading: "## introduction", Content: "..."},
		{Heading: "## main API", Content: "..."},
		{Heading: "", Content: "no heading"},
		{Heading: "## conclusion", Content: "..."},
	}
	if err := idx.Insert("hsrc", "execute", "test", chunks); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := idx.HeadingsForSource("hsrc", 10)
	if err != nil {
		t.Fatalf("HeadingsForSource: %v", err)
	}
	want := []string{"## introduction", "## main API", "## conclusion"}
	if len(got) != len(want) {
		t.Fatalf("got %d headings, want %d (got=%v)", len(got), len(want), got)
	}
	for i, h := range got {
		if h != want[i] {
			t.Errorf("heading[%d] = %q, want %q", i, h, want[i])
		}
	}
}

func TestService_Search_AcrossSources(t *testing.T) {
	skipUnlessBash(t)
	svc := newTestService(t)
	svc.Threshold = 64

	_, _ = svc.Execute(context.Background(), ExecuteRequest{
		Script: `echo "this is the first run with TOKEN_ABC in it"; for i in $(seq 1 30); do echo "filler $i"; done`,
		Intent: "no match here",
	})
	_, _ = svc.Execute(context.Background(), ExecuteRequest{
		Script: `echo "second run mentions TOKEN_ABC too"; for i in $(seq 1 30); do echo "filler $i"; done`,
		Intent: "no match here",
	})
	got, err := svc.Search("TOKEN_ABC", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) < 2 {
		t.Errorf("expected matches across both sources, got %d", len(got))
	}
}
