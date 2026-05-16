package contextmode

import (
	"os"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-core/pgutil"
)

func newTestIndex(t *testing.T) *Index {
	t.Helper()
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
	return idx
}

func TestIndex_InsertAndSearch(t *testing.T) {
	idx := newTestIndex(t)

	chunks := []IndexedChunk{
		{Heading: "auth flow", Content: "func Login(u, p string) error {\n  return verifyPassword(u, p)\n}"},
		{Heading: "error handling", Content: "log.Errorf(\"login failed: %v\", err)"},
		{Heading: "helpers", Content: "func helper() int { return 42 }"},
	}
	if err := idx.Insert("src1", "execute", "find auth functions", chunks); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := idx.Search("login", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least one match for 'login'")
	}
	if !strings.Contains(strings.ToLower(got[0].Content), "login") {
		t.Errorf("top hit didn't contain 'login': %q", got[0].Content)
	}
}

func TestIndex_TrigramSubstring(t *testing.T) {
	idx := newTestIndex(t)
	chunks := []IndexedChunk{
		{Heading: "react hooks", Content: "useEffect(() => { fetchData(); }, [id])"},
	}
	if err := idx.Insert("src", "execute", "react", chunks); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Substring query — ngram(3,3) tokenizer should match "useEff" in "useEffect".
	got, err := idx.Search("useEff", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) == 0 {
		t.Error("ngram(3,3) tokenizer should match 'useEff' inside 'useEffect'")
	}
}

func TestIndex_Scoped(t *testing.T) {
	idx := newTestIndex(t)
	if err := idx.Insert("a", "execute", "first", []IndexedChunk{{Content: "alpha apple"}}); err != nil {
		t.Fatalf("Insert a: %v", err)
	}
	if err := idx.Insert("b", "execute", "second", []IndexedChunk{{Content: "alpha banana"}}); err != nil {
		t.Fatalf("Insert b: %v", err)
	}

	all, err := idx.Search("alpha", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 matches across all sources, got %d", len(all))
	}
	scoped, err := idx.SearchScoped("a", "alpha", 10)
	if err != nil {
		t.Fatalf("SearchScoped: %v", err)
	}
	if len(scoped) != 1 {
		t.Errorf("scoped search should return 1, got %d", len(scoped))
	}
	if len(scoped) > 0 && scoped[0].SourceID != "a" {
		t.Errorf("scoped result source_id mismatch: %q", scoped[0].SourceID)
	}
}

func TestIndex_Reinsert(t *testing.T) {
	idx := newTestIndex(t)
	if err := idx.Insert("src", "execute", "v1", []IndexedChunk{{Content: "old content"}}); err != nil {
		t.Fatalf("Insert v1: %v", err)
	}
	if err := idx.Insert("src", "execute", "v2", []IndexedChunk{{Content: "new content"}}); err != nil {
		t.Fatalf("Insert v2: %v", err)
	}

	old, err := idx.Search("old", 5)
	if err != nil {
		t.Fatalf("Search old: %v", err)
	}
	if len(old) != 0 {
		t.Errorf("expected old chunks to be evicted, got %d matches", len(old))
	}
	fresh, err := idx.Search("new", 5)
	if err != nil {
		t.Fatalf("Search new: %v", err)
	}
	if len(fresh) != 1 {
		t.Errorf("expected new chunks to be searchable, got %d matches", len(fresh))
	}
}

func TestIndex_QueryWithPunctuation(t *testing.T) {
	idx := newTestIndex(t)
	if err := idx.Insert("src", "execute", "code",
		[]IndexedChunk{{Content: "func main() { x.y = 1 }"}}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// pg_search's paradedb.match takes literal terms — the FTS5 parser
	// quoting hop is gone. Punctuation that used to need escaping just
	// flows through as part of the term to be tokenised against the
	// field's tokenizer.
	cases := []string{
		"main()",
		"x.y",
		`"quoted"`,
	}
	for _, q := range cases {
		if _, err := idx.Search(q, 5); err != nil {
			t.Errorf("query %q errored: %v", q, err)
		}
	}
}

func TestIndex_HeadingBoostRanksFirst(t *testing.T) {
	idx := newTestIndex(t)
	// Two rows: one with the term in heading only, one with the term
	// in content only. Heading 5× / content 1× boost should put the
	// heading match first.
	chunks := []IndexedChunk{
		{Heading: "filler heading", Content: "API endpoints are documented here"},
		{Heading: "API endpoints", Content: "filler content with no useful keyword"},
	}
	if err := idx.Insert("src", "execute", "boost-test", chunks); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := idx.Search("API", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("expected 2 matches, got %d", len(got))
	}
	if got[0].Heading != "API endpoints" {
		t.Errorf("heading-match row should rank first; got top heading %q with content %q",
			got[0].Heading, got[0].Content)
	}
}

func TestSearchAny_OR(t *testing.T) {
	idx := newTestIndex(t)
	// Three chunks each containing only ONE of three distinct terms —
	// AND search would miss all of them; OR search should hit all three.
	chunks := []IndexedChunk{
		{Heading: "alpha", Content: "the alpha section talks about alpha-only content"},
		{Heading: "beta", Content: "the beta section is here"},
		{Heading: "gamma", Content: "the gamma section closes things out"},
	}
	if err := idx.Insert("multi", "execute", "test", chunks); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	andHits, err := idx.Search("alpha beta gamma", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(andHits) != 0 {
		t.Errorf("AND search across 3 disjoint chunks should miss; got %d hits", len(andHits))
	}
	orHits, err := idx.SearchAny("alpha beta gamma", 10)
	if err != nil {
		t.Fatalf("SearchAny: %v", err)
	}
	if len(orHits) != 3 {
		t.Errorf("OR search should hit all 3 chunks; got %d", len(orHits))
	}
}

func TestBuildBoolQuery_SingleTerm(t *testing.T) {
	sql, args := buildBoolQuery([]string{"foo"}, false)
	if !strings.Contains(sql, "paradedb.match('heading', $1)") {
		t.Errorf("expected heading match clause; got %q", sql)
	}
	if !strings.Contains(sql, "paradedb.match('content', $1)") {
		t.Errorf("expected content match clause; got %q", sql)
	}
	if !strings.Contains(sql, "paradedb.boost(5.0,") {
		t.Errorf("expected 5.0 boost on heading; got %q", sql)
	}
	if !strings.Contains(sql, "paradedb.boost(1.0,") {
		t.Errorf("expected 1.0 boost on content; got %q", sql)
	}
	if strings.Contains(sql, "must =>") || strings.Contains(sql, "must=>") {
		t.Errorf("single-term query must NOT wrap in must/should: %q", sql)
	}
	if len(args) != 1 || args[0] != "foo" {
		t.Errorf("expected args=[\"foo\"], got %v", args)
	}
}

func TestBuildBoolQuery_MultiTermAND(t *testing.T) {
	sql, args := buildBoolQuery([]string{"foo", "bar"}, false)
	if !strings.Contains(sql, "must => ARRAY[") {
		t.Errorf("multi-term AND query should wrap in must => ARRAY; got %q", sql)
	}
	if !strings.Contains(sql, "$1") || !strings.Contains(sql, "$2") {
		t.Errorf("expected $1 and $2 placeholders; got %q", sql)
	}
	if len(args) != 2 || args[0] != "foo" || args[1] != "bar" {
		t.Errorf("expected args=[\"foo\",\"bar\"], got %v", args)
	}
}

func TestBuildBoolQuery_MultiTermOR(t *testing.T) {
	sql, args := buildBoolQuery([]string{"foo", "bar", "baz"}, true)
	if !strings.Contains(sql, "should => ARRAY[") {
		t.Errorf("multi-term OR query should wrap in should => ARRAY; got %q", sql)
	}
	if len(args) != 3 {
		t.Errorf("expected 3 args, got %d", len(args))
	}
}
