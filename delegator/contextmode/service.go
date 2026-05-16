package contextmode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// Service is the public face of the contextmode package, wrapping the
// index, the script executor, the URL fetcher, and the optional
// fallbacks (BGE reranker + LLM compressor). One Service per
// delegator-mcp process.
type Service struct {
	Index      *Index
	Compressor Compressor
	Reranker   *RerankerClient
	Logger     *logrus.Logger

	// Threshold is the "small enough to return raw" cutoff in BYTES.
	// Outputs smaller than this skip indexing entirely.
	// 0 → 5 KB (matches context-mode's default).
	Threshold int

	// MaxOutputBytes is the cap on raw output we'll capture from a
	// script. 0 → 10 MB.
	MaxOutputBytes int
}

// NewService validates inputs and applies defaults.
func NewService(idx *Index, comp Compressor, logger *logrus.Logger) *Service {
	if logger == nil {
		logger = logrus.New()
	}
	if comp == nil {
		comp = NoopCompressor{}
	}
	return &Service{
		Index:          idx,
		Compressor:     comp,
		Logger:         logger,
		Threshold:      5 * 1024,
		MaxOutputBytes: 10 * 1024 * 1024,
	}
}

// SetReranker attaches an optional BGE reranker. When attached, the
// cascade's stage 3 reranks every chunk of the source against the
// intent and returns the top-K verbatim instead of asking an LLM to
// compress/paraphrase. Pass nil (or a disabled client) to keep the
// pre-Phase-4 LLM-compress behaviour.
func (s *Service) SetReranker(r *RerankerClient) {
	s.Reranker = r
}

// ExecuteRequest is the input to Execute().
type ExecuteRequest struct {
	Script     string
	Language   string
	Intent     string
	WorkingDir string
	TimeoutSec int
}

// ExecuteMode tags how the output reached the caller — useful for the
// MCP tool to surface in its response so the model knows whether what
// it sees is raw bytes or LLM-cropped.
type ExecuteMode string

const (
	ModeRaw           ExecuteMode = "raw"            // small output, full bytes
	ModeBM25          ExecuteMode = "bm25"           // big output + intent + BM25 hit on literal intent
	ModeBM25Translated ExecuteMode = "bm25_translated" // hit only after qwen rewrote intent → BM25 terms
	ModeCompressed    ExecuteMode = "compressed"     // big output + intent + LLM fallback
	ModeTruncated     ExecuteMode = "truncated"      // big output + no intent → indexed but only head returned
)

// ExecuteResult is the output of Execute(). Output is always populated;
// Snippets is set in ModeBM25/ModeBM25Translated; SourceID is set
// whenever indexing happened.
type ExecuteResult struct {
	Mode       ExecuteMode `json:"mode"`
	SourceID   string      `json:"source_id,omitempty"`
	Output     string      `json:"output"`
	Snippets   []Snippet   `json:"snippets,omitempty"`
	BM25Query  string      `json:"bm25_query,omitempty"` // surfaced when translation kicked in
	ExitCode   int         `json:"exit_code"`
	TimedOut   bool        `json:"timed_out,omitempty"`
	Truncated  bool        `json:"truncated,omitempty"`
	Stderr     string      `json:"stderr,omitempty"`
	DurationMs int64       `json:"duration_ms"`
}

// Execute runs a script in the chosen language. Output paths:
//
//	output ≤ threshold              → ModeRaw (full bytes)
//	output > threshold + intent     → index → BM25 search
//	  ≥1 snippet                    → ModeBM25
//	  0 snippets + Compressor       → ModeCompressed
//	  0 snippets + NoopCompressor   → ModeTruncated (raw head + index)
//	output > threshold + no intent  → ModeTruncated (raw head + index)
func (s *Service) Execute(ctx context.Context, req ExecuteRequest) (*ExecuteResult, error) {
	if req.Script == "" {
		return nil, errors.New("script is required")
	}
	lang := Detect(req.Language, req.Script)
	if !ValidLanguages[lang] {
		return nil, fmt.Errorf("unsupported language %q", req.Language)
	}
	timeout := time.Duration(req.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	exec, err := Run(ctx, ExecOptions{
		Language:   lang,
		Script:     req.Script,
		WorkingDir: req.WorkingDir,
		Env:        os.Environ(),
		Timeout:    timeout,
		MaxBytes:   s.MaxOutputBytes,
	})
	if err != nil {
		return nil, err
	}

	out := combineOutput(exec)
	res := &ExecuteResult{
		ExitCode:   exec.ExitCode,
		TimedOut:   exec.TimedOut,
		Truncated:  exec.Truncated,
		Stderr:     trimStderr(exec.Stderr, 4*1024),
		DurationMs: exec.Duration.Milliseconds(),
	}

	// Small output: skip indexing, return raw bytes.
	if len(out) <= s.Threshold {
		res.Mode = ModeRaw
		res.Output = out
		return res, nil
	}

	// Big output: index it. Source ID is a hash of (script, time) so
	// the same script run later gets a fresh ID and doesn't clobber
	// older indexed runs (handy for compare-two-runs flows).
	sourceID := hashSource("execute", req.Script, time.Now().UnixNano())
	chunks := ChunkText(out, ChunkOpts{})
	meta := scriptPreview(req.Script, 200)
	if err := s.Index.Insert(sourceID, "execute", meta, chunks); err != nil {
		s.Logger.WithError(err).Warn("contextmode: indexing execute output failed; returning truncated raw")
		res.Mode = ModeTruncated
		res.SourceID = sourceID
		res.Output = headTailTrim(out, s.Threshold)
		return res, nil
	}
	res.SourceID = sourceID

	// No intent → return head-trimmed raw + the source_id so the
	// model can call search() to drill in if it wants more.
	if strings.TrimSpace(req.Intent) == "" {
		res.Mode = ModeTruncated
		res.Output = headTailTrim(out, s.Threshold) +
			"\n\n[source_id=" + sourceID + " — call `search(query, ...)` for more]"
		return res, nil
	}

	// Have intent: cascade through three stages (literal BM25 →
	// translated BM25 → full LLM compress) and project the outcome
	// onto the result.
	stage := s.runIntentCascade(ctx, sourceID, out, req.Intent)
	res.Mode = stage.mode
	res.Snippets = stage.snippets
	res.BM25Query = stage.query
	if stage.output != "" {
		res.Output = stage.output
	} else {
		res.Output = headTailTrim(out, s.Threshold) +
			"\n\n[source_id=" + sourceID + " — search returned no matches]"
	}
	return res, nil
}

// intentCascadeResult is the shared output of the three-stage retry.
// One of (snippets, output) is set per the chosen mode.
type intentCascadeResult struct {
	mode     ExecuteMode
	snippets []Snippet
	output   string
	query    string // BM25 query actually executed when mode is BM25/BM25Translated
}

// runIntentCascade is the heart of intent-driven retrieval. Stages:
//   1. BM25 against the literal intent. Cheapest path; if the user's
//      vocabulary already matches indexed content we skip every LLM
//      hop entirely.
//   2. Have qwen rewrite the intent into BM25 terms (using chunk
//      headings as a vocabulary hint); retry BM25 with OR semantics.
//      Rescues natural-language intents whose words don't appear in
//      the content (e.g. "walk the DOM" vs "Descendants iterator").
//   3. Full LLM compression on the raw bytes. Last resort — most
//      expensive path, but catches cases where neither stage 1 nor 2
//      can find anything searchable.
//
// Returns ModeTruncated as a fallback only if all three stages fail
// (compressor unavailable AND BM25 found nothing).
func (s *Service) runIntentCascade(ctx context.Context, sourceID, raw, intent string) intentCascadeResult {
	// Stage 1: literal intent.
	snippets, err := s.Index.SearchScoped(sourceID, intent, 10)
	if err != nil {
		s.Logger.WithError(err).Warn("contextmode: BM25 literal search failed")
	}
	if len(snippets) > 0 {
		return intentCascadeResult{
			mode:     ModeBM25,
			snippets: snippets,
			output:   formatSnippets(snippets, sourceID),
			query:    intent,
		}
	}

	// Stage 2: dense retrieval. Embed the literal intent via the octen
	// sidecar and run a cosine-similarity ANN search against per-chunk
	// embeddings. When octen is unattached, SearchScopedDense returns
	// nil + nil and we degrade to the qwen-translate retry below
	// (today's pre-Phase-3 behaviour). When octen is up but returns
	// no above-threshold matches, we still try the qwen path before
	// surrendering to stage 3.
	if dense, dErr := s.Index.SearchScopedDense(sourceID, intent, 10); dErr != nil {
		s.Logger.WithError(dErr).Warn("contextmode: dense search failed")
	} else if len(dense) > 0 {
		return intentCascadeResult{
			mode:     ModeBM25Translated,
			snippets: dense,
			output:   formatSnippetsWithQuery(dense, sourceID, intent, "(dense rescue)"),
			query:    intent,
		}
	}

	// Stage 2 (fallback): ask the compressor to translate the intent
	// into BM25 terms and retry. Vocab hint = chunk headings, anchors
	// the translation to tokens known to exist in the content.
	// Skipped when no translator is wired (NoopCompressor returns "").
	vocab := s.collectVocabHints(sourceID)
	translated, tErr := s.Compressor.TranslateIntent(ctx, intent, vocab)
	if tErr != nil {
		s.Logger.WithError(tErr).Warn("contextmode: intent translation failed")
	}
	if translated != "" && translated != intent {
		// Translated terms use OR semantics — qwen's terms often span
		// different topical chunks, and AND would require one chunk
		// to hold every term.
		snippets2, err2 := s.Index.SearchScopedAny(sourceID, translated, 10)
		if err2 != nil {
			s.Logger.WithError(err2).Warn("contextmode: BM25 translated search failed")
		}
		if len(snippets2) > 0 {
			return intentCascadeResult{
				mode:     ModeBM25Translated,
				snippets: snippets2,
				output:   formatSnippetsWithQuery(snippets2, sourceID, intent, translated),
				query:    translated,
			}
		}
	}

	// Stage 3 (preferred): BGE reranker over the source's full chunk
	// inventory. The reranker is a cross-encoder, so unlike BM25 and
	// dense it can pick up matches both lexical and semantic
	// retrieval missed. Cost is wallclock-bounded (~1.8s for 50 pairs
	// on CPU per deepresearch benches) and crucially the response is
	// verbatim chunk content — no LLM paraphrasing of the raw bytes.
	if s.Reranker != nil && s.Reranker.Enabled() {
		if snippets, err := s.rerankChunks(ctx, sourceID, intent); err != nil {
			s.Logger.WithError(err).Warn("contextmode: reranker failed; falling back to LLM compress")
		} else if len(snippets) > 0 {
			return intentCascadeResult{
				mode:     ModeCompressed,
				snippets: snippets,
				output:   formatSnippetsWithQuery(snippets, sourceID, intent, "(reranker)"),
				query:    intent,
			}
		}
	}

	// Stage 3 (fallback): full LLM compression. Only fires when the
	// reranker is unconfigured or returned nothing AND a compressor
	// is wired. Paraphrases the raw bytes — violates the "exact
	// bytes only" rule, hence the demotion to fallback in Phase 4.
	compressed, cErr := s.Compressor.Compress(ctx, raw, intent)
	if cErr != nil {
		s.Logger.WithError(cErr).Warn("contextmode: LLM fallback failed")
	}
	if compressed != "" {
		return intentCascadeResult{
			mode:   ModeCompressed,
			output: compressed,
		}
	}
	return intentCascadeResult{mode: ModeTruncated}
}

// rerankChunks pulls every indexed chunk for a source (bounded at 50
// to keep cross-encoder cost finite) and asks the reranker which 5
// best answer the intent. Returns the matching Snippets in score
// order with their original bytes untouched.
func (s *Service) rerankChunks(ctx context.Context, sourceID, intent string) ([]Snippet, error) {
	const maxCandidates = 50
	const topK = 5

	chunks, err := s.Index.ListChunksForSource(sourceID, maxCandidates)
	if err != nil {
		return nil, fmt.Errorf("list chunks: %w", err)
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	docs := make([]string, len(chunks))
	for i, c := range chunks {
		docs[i] = c.Content
	}
	ranked, err := s.Reranker.Rerank(ctx, intent, docs, topK)
	if err != nil {
		return nil, err
	}
	out := make([]Snippet, 0, len(ranked))
	for _, r := range ranked {
		c := chunks[r.Index]
		c.Score = r.Score
		out = append(out, c)
	}
	return out, nil
}

// FetchAndIndexRequest is the input to FetchAndIndex().
type FetchAndIndexRequest struct {
	URL    string
	Intent string
}

// FetchAndIndexResult is the same shape as ExecuteResult but with a
// title field for human readability.
type FetchAndIndexResult struct {
	Mode      ExecuteMode `json:"mode"`
	SourceID  string      `json:"source_id"`
	URL       string      `json:"url"`
	Title     string      `json:"title,omitempty"`
	Output    string      `json:"output"`
	Snippets  []Snippet   `json:"snippets,omitempty"`
	BM25Query string      `json:"bm25_query,omitempty"`
}

// FetchAndIndex downloads a URL, converts the page body to plaintext-ish
// markdown, indexes the chunks, and returns either the full text (if
// small) or BM25 snippets (if big + intent given).
func (s *Service) FetchAndIndex(ctx context.Context, req FetchAndIndexRequest) (*FetchAndIndexResult, error) {
	if req.URL == "" {
		return nil, errors.New("url is required")
	}
	title, body, err := FetchTextFromURL(ctx, req.URL, s.MaxOutputBytes)
	if err != nil {
		return nil, err
	}

	res := &FetchAndIndexResult{URL: req.URL, Title: title}

	if len(body) <= s.Threshold {
		res.Mode = ModeRaw
		res.Output = body
		return res, nil
	}

	sourceID := hashSource("fetch", req.URL, time.Now().UnixNano())
	chunks := ChunkText(body, ChunkOpts{})
	meta := req.URL
	if title != "" {
		meta = title + " — " + req.URL
	}
	if err := s.Index.Insert(sourceID, "fetch", meta, chunks); err != nil {
		return nil, fmt.Errorf("index fetch: %w", err)
	}
	res.SourceID = sourceID

	if strings.TrimSpace(req.Intent) == "" {
		res.Mode = ModeTruncated
		res.Output = headTailTrim(body, s.Threshold) +
			"\n\n[source_id=" + sourceID + " — call `search(query, ...)` for more]"
		return res, nil
	}
	stage := s.runIntentCascade(ctx, sourceID, body, req.Intent)
	res.Mode = stage.mode
	res.Snippets = stage.snippets
	res.BM25Query = stage.query
	if stage.output != "" {
		res.Output = stage.output
	} else {
		res.Output = headTailTrim(body, s.Threshold) +
			"\n\n[source_id=" + sourceID + " — search returned no matches]"
	}
	return res, nil
}

// Search is the standalone search tool — queries across every indexed
// source. Useful when the model has done several execute/fetch calls
// and wants to find a term across all of them.
func (s *Service) Search(query string, topK int) ([]Snippet, error) {
	return s.Index.Search(query, topK)
}

// --- helpers ---

func combineOutput(r *ExecResult) string {
	stdout := string(r.Stdout)
	stderr := string(r.Stderr)
	if stderr == "" {
		return appendExitFooter(stdout, r)
	}
	// Surface stderr for non-zero exits — that's where the failure
	// message lives. For zero exits keep stderr small (it's often
	// noise like "warnings").
	if r.ExitCode != 0 {
		return stdout + "\n=== stderr ===\n" + stderr + appendExitFooter("", r)
	}
	return appendExitFooter(stdout, r)
}

func appendExitFooter(out string, r *ExecResult) string {
	footer := fmt.Sprintf("\n[exit=%d duration=%s", r.ExitCode, r.Duration.Round(time.Millisecond))
	if r.TimedOut {
		footer += " timed_out=true"
	}
	if r.Truncated {
		footer += " truncated=true"
	}
	footer += "]"
	return out + footer
}

func trimStderr(b []byte, max int) string {
	s := string(b)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…[truncated]"
}

func scriptPreview(script string, max int) string {
	first := script
	if i := strings.IndexByte(script, '\n'); i > 0 {
		first = script[:i]
	}
	if len(first) > max {
		first = first[:max] + "…"
	}
	return first
}

func formatSnippets(snippets []Snippet, sourceID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d matches in source_id=%s.\n\n", len(snippets), sourceID)
	for idx, s := range snippets {
		fmt.Fprintf(&b, "--- Match %d (chunk %d", idx+1, s.ChunkIdx)
		if s.Heading != "" {
			fmt.Fprintf(&b, " · %s", s.Heading)
		}
		fmt.Fprintf(&b, " · score=%.2f) ---\n", s.Score)
		b.WriteString(s.Content)
		b.WriteString("\n\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatSnippetsWithQuery is the formatter when the translated-intent
// path was used — surfaces both the original intent and the rewritten
// BM25 query so the model knows which words actually matched. The
// model can then refine future searches if it wants.
func formatSnippetsWithQuery(snippets []Snippet, sourceID, originalIntent, translatedQuery string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d matches in source_id=%s (literal intent missed; ran translated query).\n", len(snippets), sourceID)
	fmt.Fprintf(&b, "  intent:    %s\n", originalIntent)
	fmt.Fprintf(&b, "  bm25:      %s\n\n", translatedQuery)
	for idx, s := range snippets {
		fmt.Fprintf(&b, "--- Match %d (chunk %d", idx+1, s.ChunkIdx)
		if s.Heading != "" {
			fmt.Fprintf(&b, " · %s", s.Heading)
		}
		fmt.Fprintf(&b, " · score=%.2f) ---\n", s.Score)
		b.WriteString(s.Content)
		b.WriteString("\n\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// hashSource produces a stable-but-unique source id. Inputs include a
// nanosecond timestamp so re-running the same script gets a fresh id.
func hashSource(kind, body string, nonce int64) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00", kind, nonce)
	h.Write([]byte(body))
	return kind + "-" + hex.EncodeToString(h.Sum(nil))[:16]
}

// collectVocabHints returns up to 32 non-empty headings for a source.
// These are the chunk-header lines (e.g. "## section name", "==> file
// path <==") and form a content-grounded vocabulary that the translate
// stage uses to anchor qwen's output to terms that actually exist in
// the indexed text.
func (s *Service) collectVocabHints(sourceID string) []string {
	if s.Index == nil {
		return nil
	}
	headings, err := s.Index.HeadingsForSource(sourceID, 32)
	if err != nil {
		s.Logger.WithError(err).Debug("contextmode: failed to collect vocab hints")
		return nil
	}
	return headings
}
