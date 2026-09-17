-- Corpus statistics for BM25 ranking in SearchLexical (ADR-41).
--
-- ts_rank_cd scores term frequency and proximity but not rarity, so a chunk
-- that repeats "v." twenty times outranked the only chunk naming the case the
-- query asked about. BM25 needs two corpus-level numbers ts_rank_cd never
-- looks at: how many chunks contain each lexeme (for IDF), and the average
-- chunk length (for length normalization).
--
-- They are materialized views rather than tables maintained inside
-- IngestDocument because every document's transaction would otherwise update
-- the row for "v", "court" and "state", and concurrent ingests would queue on
-- those rows. retrieval.Ingest refreshes them at the end of a run instead
-- (Store.RefreshLexicalStats). Between an ingest and its refresh, new chunks
-- are already searchable, because candidates come from the live GIN index, and
-- rank against slightly stale statistics.
--
-- All three are created WITH DATA so REFRESH ... CONCURRENTLY, which requires a
-- populated view and a unique index, works from the first refresh on.

CREATE MATERIALIZED VIEW lexeme_stats AS
  SELECT word AS lexeme, ndoc AS chunks, nentry AS occurrences
  FROM ts_stat('SELECT tsv FROM chunks');

CREATE UNIQUE INDEX lexeme_stats_lexeme_key ON lexeme_stats (lexeme);

-- A chunk's length in lexeme occurrences. Computing it at query time, from the
-- tsvector scan that already finds term frequencies, measured 2.4x slower
-- than reading it here. A chunk ingested since the last refresh has no row and
-- is ranked as if it were average length.
CREATE MATERIALIZED VIEW chunk_lexical_length AS
  SELECT c.id,
         (SELECT COALESCE(sum(COALESCE(array_length(u.positions, 1), 1)), 0) FROM unnest(c.tsv) u)::float8 AS length
  FROM chunks c;

CREATE UNIQUE INDEX chunk_lexical_length_id_key ON chunk_lexical_length (id);

-- One row: the corpus size IDF divides by, and the average length BM25
-- normalises against.
CREATE MATERIALIZED VIEW corpus_lexical_stats AS
  SELECT 1 AS id,
         count(*)::float8 AS chunks,
         COALESCE(avg(length), 0)::float8 AS avg_length
  FROM chunk_lexical_length;

CREATE UNIQUE INDEX corpus_lexical_stats_id_key ON corpus_lexical_stats (id);
