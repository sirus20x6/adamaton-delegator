-- Phase 3: add the per-chunk embedding column + HNSW index for cosine
-- ANN. The chunks table already exists from migration 0001; we ALTER
-- to add the new column nullable so existing rows (and inserts from
-- a contextmode build without the octen sidecar configured) remain
-- valid.
--
-- vector(1024) matches Octen-Embedding-0.6B / Qwen3-Embedding-0.6B.
-- If the embedder changes, this dim AND the OctenEmbedDim constant
-- in octen_client.go MUST move in lock-step.
ALTER TABLE context.chunks ADD COLUMN embedding vector(1024);

-- HNSW with cosine distance — best general-purpose ANN index for
-- pgvector. m=16, ef_construction=64 are pgvector's defaults; we
-- accept them for now and tune if recall isn't enough on real
-- workloads. The WHERE clause makes this a partial index so chunks
-- with NULL embeddings (inserted while the sidecar was offline)
-- don't bloat the structure.
CREATE INDEX chunks_embedding_hnsw
    ON context.chunks
    USING hnsw (embedding vector_cosine_ops)
    WHERE embedding IS NOT NULL;
