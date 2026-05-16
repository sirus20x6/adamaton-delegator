package contextmode

import (
	"strings"
)

// ChunkOpts controls splitting behaviour. Defaults aim at chunks small
// enough that a BM25 snippet() call (64 tokens) sits comfortably inside
// the chunk, and big enough that we don't blow up the index with one
// row per line.
type ChunkOpts struct {
	// MinChars merges adjacent chunks below this size. 0 → 256.
	MinChars int
	// MaxChars splits chunks above this size at line boundaries. 0 → 2048.
	MaxChars int
}

func (o ChunkOpts) withDefaults() ChunkOpts {
	if o.MinChars <= 0 {
		o.MinChars = 256
	}
	if o.MaxChars <= 0 {
		o.MaxChars = 2048
	}
	if o.MaxChars < o.MinChars {
		o.MaxChars = o.MinChars * 2
	}
	return o
}

// ChunkText splits raw output into IndexedChunks. The split rules:
//   - Strong split on lines starting with "## " or "### " or "==> "
//     (markdown headings, also git-grep file separators).
//   - Within a section, accumulate lines until we exceed MaxChars,
//     then flush.
//   - Sections smaller than MinChars are merged with the next sibling
//     so we don't over-fragment.
//
// The first non-empty line of each chunk becomes its heading (capped
// at 120 chars). Heading is what BM25 boosts 5× during search, so
// preserving structural markers there is what makes "find error
// handling" return the right block.
func ChunkText(s string, opts ChunkOpts) []IndexedChunk {
	opts = opts.withDefaults()
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")

	// Pass 1: split into sections by strong delimiters or by size.
	var sections [][]string
	cur := []string{}
	curLen := 0
	flush := func() {
		if len(cur) > 0 {
			sections = append(sections, cur)
			cur = nil
			curLen = 0
		}
	}
	for _, line := range lines {
		if isStrongHeading(line) {
			flush()
		}
		cur = append(cur, line)
		curLen += len(line) + 1
		if curLen >= opts.MaxChars {
			flush()
		}
	}
	flush()

	// Pass 2: merge undersized sections with the next.
	merged := make([][]string, 0, len(sections))
	for _, sec := range sections {
		if n := len(merged); n > 0 && joinedLen(merged[n-1]) < opts.MinChars {
			merged[n-1] = append(merged[n-1], sec...)
			continue
		}
		merged = append(merged, sec)
	}

	out := make([]IndexedChunk, 0, len(merged))
	for _, sec := range merged {
		text := strings.Join(sec, "\n")
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, IndexedChunk{
			Heading: firstNonEmptyTrim(sec, 120),
			Content: text,
		})
	}
	return out
}

func isStrongHeading(line string) bool {
	if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
		return true
	}
	// "==> " is git's filename separator in `git grep -p` output and a
	// common section divider in test runner output.
	if strings.HasPrefix(line, "==> ") {
		return true
	}
	return false
}

func joinedLen(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	return n
}

func firstNonEmptyTrim(lines []string, maxLen int) string {
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if len(t) > maxLen {
			t = t[:maxLen]
		}
		return t
	}
	return ""
}
