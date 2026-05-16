package contextmode

import (
	"strings"
	"testing"
)

func TestChunkText_Empty(t *testing.T) {
	if got := ChunkText("", ChunkOpts{}); len(got) != 0 {
		t.Errorf("empty input should produce 0 chunks, got %d", len(got))
	}
}

func TestChunkText_SingleSmallSection(t *testing.T) {
	in := "line one\nline two\nline three"
	got := ChunkText(in, ChunkOpts{})
	if len(got) != 1 {
		t.Fatalf("small input should be 1 chunk, got %d", len(got))
	}
	if got[0].Content != in {
		t.Errorf("content mismatch: got %q", got[0].Content)
	}
	if got[0].Heading != "line one" {
		t.Errorf("first non-empty line should be heading: got %q", got[0].Heading)
	}
}

func TestChunkText_SplitOnHeadings(t *testing.T) {
	in := "## section one\nfoo bar\n## section two\nbaz qux\n### section three\nquux"
	got := ChunkText(in, ChunkOpts{MinChars: 1, MaxChars: 4096})
	if len(got) != 3 {
		t.Fatalf("expected 3 chunks (3 headings), got %d", len(got))
	}
	for i, want := range []string{"## section one", "## section two", "### section three"} {
		if got[i].Heading != want {
			t.Errorf("chunk %d heading: got %q want %q", i, got[i].Heading, want)
		}
	}
}

func TestChunkText_MergeSmall(t *testing.T) {
	// Three tiny sections; with min=200, all three should merge.
	in := "## a\nx\n## b\ny\n## c\nz"
	got := ChunkText(in, ChunkOpts{MinChars: 200, MaxChars: 4096})
	if len(got) != 1 {
		t.Fatalf("tiny sections should merge into 1 chunk, got %d", len(got))
	}
	if !strings.Contains(got[0].Content, "## c") {
		t.Errorf("merged chunk should include all three sections; got %q", got[0].Content)
	}
}

func TestChunkText_SizeBasedSplit(t *testing.T) {
	// One big block with no headings; should split by size.
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString("the quick brown fox jumps over the lazy dog\n")
	}
	got := ChunkText(b.String(), ChunkOpts{MinChars: 256, MaxChars: 1024})
	if len(got) < 2 {
		t.Errorf("big block should split into multiple chunks, got %d", len(got))
	}
}

func TestIsStrongHeading(t *testing.T) {
	yes := []string{"## hi", "### hi", "==> file.go <=="}
	for _, s := range yes {
		if !isStrongHeading(s) {
			t.Errorf("expected strong heading: %q", s)
		}
	}
	no := []string{"#hi", "regular line", "## "[:0]}
	for _, s := range no {
		if isStrongHeading(s) {
			t.Errorf("not a strong heading: %q", s)
		}
	}
}
