DROP INDEX IF EXISTS context.chunks_embedding_hnsw;
ALTER TABLE context.chunks DROP COLUMN IF EXISTS embedding;
