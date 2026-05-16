CREATE SCHEMA IF NOT EXISTS context;

-- One row per execute/fetch call. source_meta is opaque to the
-- index (command line, URL, etc. — kept around so the caller can
-- echo it back in search results).
CREATE TABLE context.sources (
    id           TEXT        PRIMARY KEY,
    source_type  TEXT        NOT NULL,
    source_meta  TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    total_chunks INTEGER     NOT NULL DEFAULT 0,
    total_bytes  BIGINT      NOT NULL DEFAULT 0
);

CREATE INDEX sources_created_idx ON context.sources (created_at DESC);

-- Chunks of indexed content. heading carries a synthesised label
-- (markdown heading, file header, etc.) that we boost 5× during
-- BM25 ranking — same shape as the FTS5 index it replaces.
CREATE TABLE context.chunks (
    id        BIGSERIAL PRIMARY KEY,
    source_id TEXT      NOT NULL REFERENCES context.sources(id) ON DELETE CASCADE,
    chunk_idx INTEGER   NOT NULL,
    heading   TEXT      NOT NULL DEFAULT '',
    content   TEXT      NOT NULL DEFAULT ''
);

CREATE INDEX chunks_source_chunkidx_idx ON context.chunks (source_id, chunk_idx);

-- Single pg_search BM25 index. pg_search only permits one bm25 index
-- per table, so heading + content share the index with per-field
-- tokenizers: heading uses the default word-level tokenizer,
-- content uses ngram(3,3) so substring queries like 'useEff' match
-- 'useEffect' the way FTS5's trigram tokenizer did.
--
-- source_id is included so the same index can answer scoped queries
-- ("only chunks from this source") via paradedb.match('source_id', …)
-- inside the boolean tree instead of needing a separate predicate.
CREATE INDEX chunks_bm25 ON context.chunks
USING bm25 (id, source_id, heading, (content::pdb.ngram(3,3)))
WITH (key_field='id');
