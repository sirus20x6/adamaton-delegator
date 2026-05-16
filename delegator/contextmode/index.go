// Package contextmode implements the script-execute / index-search /
// fetch-and-index trio of MCP tools modelled after mksglu/context-mode.
//
// The core design choice — adopted from context-mode — is that big tool
// outputs are NEVER summarised by an LLM. Output is chunked, indexed
// into pg_search (Tantivy-backed BM25) with the ngram(3,3) tokenizer,
// and retrieved via BM25 ranking. The model gets back exact bytes
// cropped around relevant terms. Editorialising risk = zero.
//
// Opencode/qwen is wired in only as a last-resort compressor when an
// intent is given but BM25 returns no usable matches.
package contextmode

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	pgvectorpgx "github.com/pgvector/pgvector-go/pgx"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamomaton-core/octen"
	"github.com/sirus20x6/adamomaton-core/pgutil"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// queryTimeout bounds every Index call so a misbehaving postgres can't
// hold a caller indefinitely.
const queryTimeout = 30 * time.Second

// Index wraps a pg_search BM25 index plus a sources metadata table.
// pg_search permits one BM25 index per table, so heading and content
// share `chunks_bm25` with per-field tokenizers (default word-level on
// heading, ngram(3,3) on content). The trigram-style substring matching
// behaviour the FTS5 version had is preserved by the content
// tokenizer.
//
// When an octen.Client is attached, Insert also writes a dense
// embedding per chunk and SearchScopedDense becomes available. If the
// client is nil or disabled, contextmode falls back to BM25-only
// retrieval — embeddings remain NULL and the partial HNSW index
// ignores them.
type Index struct {
	pool  *pgxpool.Pool
	octen *octen.Client
	log   *logrus.Logger
}

// NewIndex dials Postgres at dsn, runs the contextmode migrations, and
// returns an Index ready to insert / search. Caller owns the lifecycle
// via Close().
func NewIndex(dsn string, logger *logrus.Logger) (*Index, error) {
	if dsn == "" {
		return nil, errors.New("contextmode.NewIndex: DSN required")
	}
	if logger == nil {
		logger = logrus.New()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgutil.Open(ctx, pgutil.Config{
		DSN:    dsn,
		Logger: logger,
		// pgvector ships a pgx codec that handles vector(N) for both
		// binary and CopyFrom paths. Without this hook pgx falls back
		// to text-protocol encoding which corrupts the dim header
		// (postgres then rejects it as "vector cannot have more than
		// 16000 dimensions").
		AfterConnect: func(ctx context.Context, conn *pgx.Conn) error {
			return pgvectorpgx.RegisterTypes(ctx, conn)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("contextmode.NewIndex: open pool: %w", err)
	}
	if err := pgutil.MigrateAll(dsn, "contextmode", "migrations", migrationsFS, logger); err != nil {
		pool.Close()
		return nil, fmt.Errorf("contextmode.NewIndex: migrate: %w", err)
	}
	return &Index{pool: pool, log: logger}, nil
}

// SetOcten attaches a dense-embedding client. Pass a disabled client
// (or just nil) to opt out — Insert and SearchScopedDense become
// no-ops in that case and the cascade falls back to BM25.
func (i *Index) SetOcten(c *octen.Client) {
	i.octen = c
}

// Close releases the underlying pool.
func (i *Index) Close() error {
	if i.pool != nil {
		i.pool.Close()
	}
	return nil
}

// IndexedChunk is the shape callers pass to Insert — we let the chunker
// decide what counts as a chunk and supply an optional heading for
// better BM25 ranking on multi-term queries.
type IndexedChunk struct {
	Heading string
	Content string
}

// Insert writes a new source plus its chunks atomically. Re-inserting
// the same source_id overwrites any prior chunks (re-running the same
// script should not duplicate-index the output).
//
// When an octen.Client is attached, Insert also batches every chunk's
// content through /v1/embeddings and stores the result in the
// embedding column. A sidecar failure or transient HTTP error doesn't
// fail the Insert — embeddings come back nil, the chunks land
// anyway, and SearchScopedDense will see NULL rows (which the partial
// HNSW index skips). This makes the dense-retrieval path opt-in
// without coupling the BM25 path's reliability to a sidecar.
func (i *Index) Insert(sourceID, sourceType, sourceMeta string, chunks []IndexedChunk) error {
	if sourceID == "" {
		return errors.New("source id required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	totalBytes := 0
	for _, c := range chunks {
		totalBytes += len(c.Content)
	}

	// Best-effort embedding. We compute outside the transaction so a
	// slow sidecar doesn't hold a postgres tx open. nil embeddings
	// just mean "this chunk lands without a dense vector"; partial
	// failures get logged.
	embeddings := i.embedChunks(ctx, chunks)

	tx, err := i.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO context.sources
			(id, source_type, source_meta, created_at, total_chunks, total_bytes)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			source_type  = EXCLUDED.source_type,
			source_meta  = EXCLUDED.source_meta,
			created_at   = EXCLUDED.created_at,
			total_chunks = EXCLUDED.total_chunks,
			total_bytes  = EXCLUDED.total_bytes`,
		sourceID, sourceType, sourceMeta, time.Now().UTC(),
		len(chunks), totalBytes,
	); err != nil {
		return fmt.Errorf("upsert source: %w", err)
	}

	// Replace any prior chunks for this source — re-running the same
	// script should overwrite, not append.
	if _, err := tx.Exec(ctx,
		`DELETE FROM context.chunks WHERE source_id = $1`, sourceID,
	); err != nil {
		return fmt.Errorf("clear prior chunks: %w", err)
	}

	if len(chunks) > 0 {
		rows := make([][]any, len(chunks))
		for idx, c := range chunks {
			var emb any // nil maps to SQL NULL
			if embeddings != nil && embeddings[idx] != nil {
				v := pgvector.NewVector(embeddings[idx])
				emb = v
			}
			rows[idx] = []any{sourceID, idx, c.Heading, c.Content, emb}
		}
		if _, err := tx.CopyFrom(ctx,
			pgx.Identifier{"context", "chunks"},
			[]string{"source_id", "chunk_idx", "heading", "content", "embedding"},
			pgx.CopyFromRows(rows),
		); err != nil {
			return fmt.Errorf("copy chunks: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// embedChunks calls octen for every chunk's content and returns one
// slice per chunk (nil on per-chunk failure). Returns nil entirely
// when the octen client is unattached — callers treat that as "no
// embeddings written for this Insert".
func (i *Index) embedChunks(ctx context.Context, chunks []IndexedChunk) [][]float32 {
	if i.octen == nil || !i.octen.Enabled() || len(chunks) == 0 {
		return nil
	}
	contents := make([]string, len(chunks))
	for idx, c := range chunks {
		contents[idx] = c.Content
	}
	out, err := i.octen.EmbedPassage(ctx, contents)
	if err != nil {
		i.log.WithError(err).Warn("contextmode: octen embed failed; falling back to BM25-only for this insert")
		return nil
	}
	return out
}

// Snippet is one BM25-ranked match.
type Snippet struct {
	SourceID   string  `json:"source_id"`
	SourceType string  `json:"source_type"`
	SourceMeta string  `json:"source_meta"`
	ChunkIdx   int     `json:"chunk_idx"`
	Heading    string  `json:"heading,omitempty"`
	Score      float64 `json:"score"`
	Content    string  `json:"content"`
}

// Search runs a BM25 query across the whole index. topK <= 0 defaults
// to 10. Multi-term queries use AND semantics — every term must hit
// some chunk's heading or content.
func (i *Index) Search(query string, topK int) ([]Snippet, error) {
	return i.searchScoped("", query, topK, false)
}

// SearchAny is like Search but joins terms with OR. Used by the
// translated-intent retry — qwen's translated terms often span
// different topical chunks of the indexed content, and AND-ing them
// all would require one chunk to contain every single term.
func (i *Index) SearchAny(query string, topK int) ([]Snippet, error) {
	return i.searchScoped("", query, topK, true)
}

// SearchScoped runs the same BM25 query but limits matches to a single
// source (useful for follow-up exploration after an execute call).
func (i *Index) SearchScoped(sourceID, query string, topK int) ([]Snippet, error) {
	return i.searchScoped(sourceID, query, topK, false)
}

// SearchScopedAny is the OR-join variant of SearchScoped, scoped to one
// source. Used during the translated-intent retry stage.
func (i *Index) SearchScopedAny(sourceID, query string, topK int) ([]Snippet, error) {
	return i.searchScoped(sourceID, query, topK, true)
}

func (i *Index) searchScoped(sourceID, query string, topK int, joinOR bool) ([]Snippet, error) {
	if topK <= 0 {
		topK = 10
	}
	terms := strings.Fields(strings.TrimSpace(query))
	if len(terms) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	boolExpr, args := buildBoolQuery(terms, joinOR)

	// limit + optional sourceID get appended after the dynamic term args.
	var sql string
	if sourceID == "" {
		args = append(args, topK)
		sql = fmt.Sprintf(`
			SELECT c.source_id, s.source_type, s.source_meta, c.chunk_idx,
			       c.heading, c.content,
			       paradedb.score(c.id) AS score,
			       paradedb.snippet(c.content, start_tag => '[HL]', end_tag => '[/HL]') AS snip
			FROM context.chunks c
			JOIN context.sources s ON s.id = c.source_id
			WHERE c.id @@@ %s
			ORDER BY score DESC
			LIMIT $%d`,
			boolExpr, len(args))
	} else {
		args = append(args, sourceID, topK)
		sql = fmt.Sprintf(`
			SELECT c.source_id, s.source_type, s.source_meta, c.chunk_idx,
			       c.heading, c.content,
			       paradedb.score(c.id) AS score,
			       paradedb.snippet(c.content, start_tag => '[HL]', end_tag => '[/HL]') AS snip
			FROM context.chunks c
			JOIN context.sources s ON s.id = c.source_id
			WHERE c.id @@@ %s AND c.source_id = $%d
			ORDER BY score DESC
			LIMIT $%d`,
			boolExpr, len(args)-1, len(args))
	}

	rows, err := i.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("pg_search query: %w", err)
	}
	defer rows.Close()

	stripper := strings.NewReplacer("[HL]", "", "[/HL]", "")
	var out []Snippet
	for rows.Next() {
		var (
			snip *string
			s    Snippet
		)
		if err := rows.Scan(
			&s.SourceID, &s.SourceType, &s.SourceMeta, &s.ChunkIdx,
			&s.Heading, &s.Content, &s.Score, &snip,
		); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		// paradedb.snippet only highlights when the matching field is
		// the one we asked for. If a row matched on heading only, snip
		// comes back NULL — fall back to the raw content (already
		// truncated by the chunker, so the response stays bounded).
		if snip == nil || *snip == "" {
			s.Content = truncateForSnippet(s.Content)
		} else {
			s.Content = stripper.Replace(*snip)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SearchScopedDense runs a cosine-similarity ANN search against the
// per-chunk embedding column, optionally restricted to a single
// source. Returns nil + nil when octen is disabled (so the cascade
// can treat "no embedder configured" the same as "no matches" and
// move on). topK <= 0 defaults to 10.
//
// The HNSW index is partial on `embedding IS NOT NULL`, so chunks
// inserted while the sidecar was offline are silently skipped.
func (i *Index) SearchScopedDense(sourceID, query string, topK int) ([]Snippet, error) {
	if i.octen == nil || !i.octen.Enabled() {
		return nil, nil
	}
	if topK <= 0 {
		topK = 10
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	q, err := i.octen.EmbedQuery(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("dense: embed query: %w", err)
	}
	if q == nil {
		return nil, nil
	}
	v := pgvector.NewVector(q)

	var (
		rows pgx.Rows
		qerr error
	)
	if sourceID == "" {
		rows, qerr = i.pool.Query(ctx, `
			SELECT c.source_id, s.source_type, s.source_meta, c.chunk_idx,
			       c.heading, c.content,
			       1.0 - (c.embedding <=> $1) AS score
			FROM context.chunks c
			JOIN context.sources s ON s.id = c.source_id
			WHERE c.embedding IS NOT NULL
			ORDER BY c.embedding <=> $1
			LIMIT $2`, v, topK)
	} else {
		rows, qerr = i.pool.Query(ctx, `
			SELECT c.source_id, s.source_type, s.source_meta, c.chunk_idx,
			       c.heading, c.content,
			       1.0 - (c.embedding <=> $1) AS score
			FROM context.chunks c
			JOIN context.sources s ON s.id = c.source_id
			WHERE c.embedding IS NOT NULL AND c.source_id = $2
			ORDER BY c.embedding <=> $1
			LIMIT $3`, v, sourceID, topK)
	}
	if qerr != nil {
		return nil, fmt.Errorf("dense query: %w", qerr)
	}
	defer rows.Close()

	var out []Snippet
	for rows.Next() {
		var s Snippet
		if err := rows.Scan(
			&s.SourceID, &s.SourceType, &s.SourceMeta, &s.ChunkIdx,
			&s.Heading, &s.Content, &s.Score,
		); err != nil {
			return nil, fmt.Errorf("dense scan: %w", err)
		}
		// No highlighting on dense matches — there's no "matched span"
		// the way pg_search.snippet provides. Truncate to keep the
		// response bounded.
		s.Content = truncateForSnippet(s.Content)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListChunksForSource returns every chunk belonging to a source in
// chunk_idx order, up to `limit` rows. Used by stage 3 of the cascade
// to feed candidates to the BGE reranker — the reranker is more
// accurate than BM25/dense but can't recall anything on its own, so
// we hand it the full chunk inventory and let it pick the top-K.
// limit <= 0 defaults to 100.
func (i *Index) ListChunksForSource(sourceID string, limit int) ([]Snippet, error) {
	if sourceID == "" {
		return nil, errors.New("source id required")
	}
	if limit <= 0 {
		limit = 100
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	rows, err := i.pool.Query(ctx, `
		SELECT c.source_id, s.source_type, s.source_meta, c.chunk_idx,
		       c.heading, c.content
		FROM context.chunks c
		JOIN context.sources s ON s.id = c.source_id
		WHERE c.source_id = $1
		ORDER BY c.chunk_idx
		LIMIT $2`, sourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list chunks: %w", err)
	}
	defer rows.Close()

	var out []Snippet
	for rows.Next() {
		var s Snippet
		if err := rows.Scan(
			&s.SourceID, &s.SourceType, &s.SourceMeta, &s.ChunkIdx,
			&s.Heading, &s.Content,
		); err != nil {
			return nil, fmt.Errorf("list chunks scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// HeadingsForSource returns up to `limit` non-empty headings of the
// chunks belonging to a source, in chunk_idx order. Used by the
// service to feed a vocabulary hint into the intent-translation
// stage. Empty headings are skipped.
func (i *Index) HeadingsForSource(sourceID string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 32
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	rows, err := i.pool.Query(ctx, `
		SELECT heading FROM context.chunks
		WHERE source_id = $1 AND heading <> ''
		ORDER BY chunk_idx
		LIMIT $2`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out, rows.Err()
}

// Purge deletes a single source. The chunks table has ON DELETE CASCADE
// from sources, so dropping the sources row clears both atomically.
func (i *Index) Purge(sourceID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	_, err := i.pool.Exec(ctx, `DELETE FROM context.sources WHERE id = $1`, sourceID)
	return err
}

// buildBoolQuery builds the pg_search boolean predicate that goes on
// the right side of `c.id @@@ …`. For one term it returns the bare
// heading/content disjunction; for many terms it wraps them in an
// outer must (AND) or should (OR) boolean. Returns the SQL fragment
// plus the positional args to bind.
//
// Each term is referenced exactly once as a parameter even though it
// drives both the heading and content matches inside the inner
// paradedb.boolean — the SQL emits two paradedb.match calls per term
// against the same $N placeholder. That makes pg_search re-tokenise
// the literal against each field's own tokenizer (default for
// heading, ngram(3,3) for content) without us having to fork the
// term in Go.
func buildBoolQuery(terms []string, joinOR bool) (string, []any) {
	args := make([]any, 0, len(terms))
	subs := make([]string, 0, len(terms))
	for idx, t := range terms {
		args = append(args, t)
		pos := idx + 1
		subs = append(subs, fmt.Sprintf(
			`paradedb.boolean(should => ARRAY[
				paradedb.boost(5.0, paradedb.match('heading', $%d)),
				paradedb.boost(1.0, paradedb.match('content', $%d))
			])`, pos, pos))
	}
	if len(subs) == 1 {
		return subs[0], args
	}
	op := "must"
	if joinOR {
		op = "should"
	}
	return fmt.Sprintf(`paradedb.boolean(%s => ARRAY[%s])`, op, strings.Join(subs, ",")), args
}

// truncateForSnippet is the fallback used when paradedb.snippet returns
// empty (heading-only matches). Caps the raw content at the same
// ballpark size the FTS5 snippet used (~64 tokens / ~400 bytes).
func truncateForSnippet(s string) string {
	const cap = 400
	if len(s) <= cap {
		return s
	}
	return s[:cap] + "…"
}
