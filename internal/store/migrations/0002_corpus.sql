-- M3: retrieval corpus. court and decided_on are columns rather than JSONB
-- paths because the tools filter on them and citations print them; everything
-- else CourtListener returns stays in metadata. content_sha256 is the ingest
-- idempotency key; embedding_model on every chunk answers "which model
-- produced this vector" and is what ingest compares to decide re-embedding.

ALTER TABLE documents
  ADD COLUMN court          TEXT,
  ADD COLUMN decided_on     DATE,
  ADD COLUMN content_sha256 TEXT NOT NULL DEFAULT '',
  ADD COLUMN chunk_count    INT  NOT NULL DEFAULT 0,
  ADD COLUMN ingested_at    TIMESTAMPTZ;

CREATE INDEX documents_court_decided_idx ON documents (court, decided_on);

ALTER TABLE chunks
  ADD COLUMN section         TEXT,
  ADD COLUMN char_start      INT NOT NULL DEFAULT 0,
  ADD COLUMN char_end        INT NOT NULL DEFAULT 0,
  ADD COLUMN embedding_model TEXT,
  ADD CONSTRAINT chunks_document_ordinal_key UNIQUE (document_id, ordinal);
