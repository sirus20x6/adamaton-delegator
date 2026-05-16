package contextmode

import (
	"context"
	"fmt"
	"strings"

	"github.com/sirus20x6/adamomaton-delegator/delegator/budget"
	"github.com/sirus20x6/adamomaton-delegator/delegator"
)

// Compressor is the last-resort fallback when output exceeds the
// threshold AND BM25 search returns no usable matches. We send the
// raw bytes plus the user's intent to a local model with strict
// "preserve exact strings, drop only repetition" rules.
//
// TranslateIntent is the lighter-touch path: rewrite a natural-language
// intent into BM25 search terms BEFORE any compression call. The
// `vocab` hints are tokens (typically chunk headings) known to appear
// in the indexed content — passing them constrains the model to pick
// terms that actually have a chance of matching, instead of inventing
// synonyms blind. Returning "" tells the caller to skip the retry.
type Compressor interface {
	Compress(ctx context.Context, raw, intent string) (string, error)
	TranslateIntent(ctx context.Context, intent string, vocab []string) (string, error)
}

// NoopCompressor returns the raw input unchanged. Used when no agent
// is available — better to return truncated raw than to fail the call.
type NoopCompressor struct{}

func (NoopCompressor) Compress(_ context.Context, raw, _ string) (string, error) {
	return raw, nil
}

// TranslateIntent on NoopCompressor returns "" so the service skips
// the translated retry — there's no model available to do the work.
func (NoopCompressor) TranslateIntent(_ context.Context, _ string, _ []string) (string, error) {
	return "", nil
}

// OpencodeCompressor sends the raw output to opencode/qwen via the
// delegator orchestrator. Pinned to the local agent — the whole point
// of compression-via-LLM is that it should be cheap to run on every
// missed BM25 match.
type OpencodeCompressor struct {
	Orch        *delegator.Orchestrator
	TimeoutSecs int
	// MaxInputBytes caps the size of the raw input before we send it
	// to the model. Anything bigger gets head/tail trimmed. 0 → 64KB.
	MaxInputBytes int
}

// TranslateIntent asks qwen for a small bag of BM25-friendly terms
// covering the user's natural-language intent. Cheap (short in, short
// out) — used to rescue queries where the literal intent vocabulary
// doesn't appear in the indexed content (e.g. intent uses "walk the
// DOM" but the content uses "Descendants iterator").
//
// `vocab` is a hint list of tokens known to appear in the content
// (typically chunk headings). Passing it constrains qwen to pick terms
// that have a chance of matching instead of guessing at synonyms.
//
// On any failure (timeout, agent error, empty result) returns "" so
// the caller falls back to either the literal intent or full compression.
func (c *OpencodeCompressor) TranslateIntent(ctx context.Context, intent string, vocab []string) (string, error) {
	if c.Orch == nil || strings.TrimSpace(intent) == "" {
		return "", nil
	}
	prompt := buildTranslateIntentPrompt(intent, vocab)
	task, err := c.Orch.DelegateSync(ctx, delegator.DelegateRequest{
		Prompt:      prompt,
		AgentHint:   "opencode",
		Difficulty:  delegator.DifficultyTrivial,
		Priority:    budget.PriorityImmediate,
		TimeoutSecs: 30,
	})
	if err != nil {
		return "", fmt.Errorf("translate intent dispatch: %w", err)
	}
	if task.Status != delegator.StatusCompleted {
		return "", fmt.Errorf("translate intent %s: %s", task.Status, task.Error)
	}
	return cleanTranslatedQuery(task.Output), nil
}

// Compress runs the synchronous delegate, returning the agent's text.
// On failure (no opencode in budget map, agent error, timeout) we
// return the raw input head-trimmed to MaxInputBytes — the model is
// better off seeing partial bytes than an error.
func (c *OpencodeCompressor) Compress(ctx context.Context, raw, intent string) (string, error) {
	if c.Orch == nil {
		return raw, nil
	}
	maxIn := c.MaxInputBytes
	if maxIn <= 0 {
		maxIn = 64 * 1024
	}
	timeout := c.TimeoutSecs
	if timeout <= 0 {
		timeout = 60
	}
	prompt := buildCompressPrompt(headTailTrim(raw, maxIn), intent)
	task, err := c.Orch.DelegateSync(ctx, delegator.DelegateRequest{
		Prompt:      prompt,
		AgentHint:   "opencode",
		Difficulty:  delegator.DifficultyTrivial,
		Priority:    budget.PriorityImmediate,
		TimeoutSecs: timeout,
	})
	if err != nil {
		return "", fmt.Errorf("opencode compress dispatch: %w", err)
	}
	if task.Status != delegator.StatusCompleted {
		errMsg := task.Error
		if errMsg == "" {
			errMsg = string(task.Status)
		}
		return "", fmt.Errorf("opencode compress %s: %s", task.Status, errMsg)
	}
	return strings.TrimSpace(task.Output), nil
}

// buildCompressPrompt produces a strict, terse compression prompt. The
// tone matches context-mode's: drop redundancy, never paraphrase, keep
// exact bytes for code/paths/identifiers. Intent narrows what to keep.
func buildCompressPrompt(raw, intent string) string {
	var b strings.Builder
	b.WriteString("Compress the following text for an AI coding assistant's context.\n\n")
	if intent != "" {
		b.WriteString("Intent (what the assistant cares about): " + intent + "\n\n")
	}
	b.WriteString("Strict rules:\n")
	b.WriteString("- PRESERVE exact strings: code snippets, file paths, error messages, identifiers, version numbers, regex patterns, command lines, URLs.\n")
	b.WriteString("- DROP repetition: identical lines, progress bars, retry messages.\n")
	b.WriteString("- DROP boilerplate: copyright headers, navigation prose, marketing language.\n")
	b.WriteString("- NEVER paraphrase code. Quote it byte-for-byte inside fenced blocks.\n")
	b.WriteString("- NEVER summarise into your own prose. Bullet points are fine; English summaries of what the text 'is about' are not.\n")
	b.WriteString("- If intent is given, prioritise content matching the intent and prune the rest.\n")
	b.WriteString("- Output the compressed result directly. No preamble, no surrounding markdown.\n\n")
	b.WriteString("--- BEGIN TEXT ---\n")
	b.WriteString(raw)
	if !strings.HasSuffix(raw, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("--- END TEXT ---\n")
	return b.String()
}

// buildTranslateIntentPrompt is the rewrite prompt for qwen. The
// output format is intentionally rigid (single line of space-separated
// terms, no punctuation/preamble) so post-processing in
// cleanTranslatedQuery can trust it.
//
// When vocab is non-empty, the prompt anchors qwen to that vocabulary —
// it's the chunk headings of the indexed content, which are content-
// specific tokens known to exist. Without this, qwen tends to echo the
// intent words verbatim when it has no domain knowledge.
func buildTranslateIntentPrompt(intent string, vocab []string) string {
	var b strings.Builder
	b.WriteString("Pick 2-5 BM25 search terms that will retrieve content matching the user's intent.\n\n")
	b.WriteString("Background: terms are queried against a SQLite FTS5 trigram index. The retry stage uses OR semantics, so each term you pick is a SEPARATE candidate match — they don't all have to appear in the same chunk.\n\n")
	if len(vocab) > 0 {
		b.WriteString("Vocabulary in the indexed content (PREFER terms from this list, or close variations of them):\n")
		for _, v := range vocab {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			b.WriteString("  - " + v + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Output rules — STRICT:\n")
	b.WriteString("- Reply with ONLY the search terms separated by single spaces.\n")
	b.WriteString("- ONE LINE. No newlines, no repetition, no markdown, no preamble.\n")
	b.WriteString("- No quotes, no commas, no punctuation around terms.\n")
	b.WriteString("- DO NOT echo the intent words verbatim. Pick DIFFERENT, content-specific tokens that someone reading the content would actually use.\n")
	b.WriteString("- Prefer identifier-style names (camelCase, snake_case, dotted paths) and technical terms.\n")
	b.WriteString("- 2-5 terms. Fewer is better when each is highly specific.\n\n")
	b.WriteString("Examples:\n")
	b.WriteString("  intent: \"how does ctx_execute handle stdin?\" → ctx_execute stdin\n")
	b.WriteString("  intent: \"files that call workflow.GetVersion\" → workflow.GetVersion\n")
	b.WriteString("  intent: \"how to walk the DOM and find headings\" → Descendants ChildNodes ElementNode atom\n")
	b.WriteString("  intent: \"where is the rate limiter implemented\" → rateLimiter throttle ratelimit\n\n")
	b.WriteString("Now translate this intent:\n\n  ")
	b.WriteString(intent)
	b.WriteString("\n\nOutput:")
	return b.String()
}

// cleanTranslatedQuery sanitises the model's response into something
// safe to feed FTS5. We strip code fences, leading/trailing
// punctuation, squash whitespace, and dedupe terms. Models sometimes
// emit the same term twice (especially when they output a thinking
// step plus the answer); duplication wastes BM25 budget and inflates
// the bm25_query field shown back to the caller.
func cleanTranslatedQuery(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Drop code fences if the model wrapped them anyway.
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	// Collapse newlines/tabs to spaces.
	s = strings.NewReplacer("\n", " ", "\t", " ", "\r", " ").Replace(s)
	// Squash runs of spaces.
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	s = strings.TrimSpace(s)
	s = strings.Trim(s, ",;-• ")
	if s == "" {
		return ""
	}
	// Dedupe terms preserving first-seen order. Case-insensitive
	// comparison so "DOM" and "dom" don't both make it through, but we
	// keep the original casing of the first occurrence (FTS5 index is
	// case-insensitive too, but readers see the model's chosen form).
	seen := make(map[string]bool, 8)
	out := make([]string, 0, 8)
	for _, term := range strings.Fields(s) {
		key := strings.ToLower(term)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, term)
	}
	// Cap at 6 terms — beyond that BM25's signal/noise tilts toward
	// noise, especially with OR semantics.
	if len(out) > 6 {
		out = out[:6]
	}
	return strings.Join(out, " ")
}

// headTailTrim keeps the first and last halves of `raw` when it
// exceeds `max`, separated by a marker. Tail-only or head-only would
// drop important context (errors near the end, signatures near the
// top); split is safer for ad-hoc command output.
func headTailTrim(s string, max int) string {
	if len(s) <= max {
		return s
	}
	half := (max - 64) / 2
	if half < 256 {
		half = 256
	}
	if half*2 >= len(s) {
		return s
	}
	return s[:half] + "\n... [truncated " + fmt.Sprintf("%d bytes", len(s)-2*half) + "] ...\n" + s[len(s)-half:]
}
