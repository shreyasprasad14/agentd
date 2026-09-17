package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EmbeddingDimensions is the width of chunks.embedding, fixed by the schema.
// The embedder is probed against it at boot so a mismatch fails loudly there
// rather than at the first insert.
const EmbeddingDimensions = 1024

// Document mirrors a row of the documents table.
type Document struct {
	ID            uuid.UUID       `json:"id"`
	SourceID      string          `json:"source_id"`
	Title         string          `json:"title"`
	Court         string          `json:"court,omitempty"`
	DecidedOn     *time.Time      `json:"decided_on,omitempty"`
	Metadata      json.RawMessage `json:"metadata"`
	ContentSHA256 string          `json:"content_sha256"`
	ChunkCount    int             `json:"chunk_count"`
	IngestedAt    *time.Time      `json:"ingested_at,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// Chunk mirrors a row of the chunks table. Embedding is write-only: queries
// order by distance in SQL and never scan vectors back out.
type Chunk struct {
	ID             uuid.UUID `json:"id"`
	DocumentID     uuid.UUID `json:"document_id"`
	Ordinal        int       `json:"ordinal"`
	Section        string    `json:"section,omitempty"`
	Content        string    `json:"content"`
	CharStart      int       `json:"char_start"`
	CharEnd        int       `json:"char_end"`
	EmbeddingModel string    `json:"embedding_model,omitempty"`
	Embedding      []float32 `json:"-"`
}

// SearchHit is one chunk returned by a corpus search, joined with the fields
// a citation needs.
type SearchHit struct {
	ChunkID    uuid.UUID  `json:"chunk_id"`
	DocumentID uuid.UUID  `json:"document_id"`
	SourceID   string     `json:"source_id"`
	Title      string     `json:"title"`
	Court      string     `json:"court,omitempty"`
	DecidedOn  *time.Time `json:"decided_on,omitempty"`
	Section    string     `json:"section,omitempty"`
	Ordinal    int        `json:"ordinal"`
	Content    string     `json:"content"`
}

// CorpusFilter narrows a search to a court and/or a decided_on range.
type CorpusFilter struct {
	Court string
	From  *time.Time
	To    *time.Time
}

// CorpusStats summarises what is ingested.
type CorpusStats struct {
	Documents       int      `json:"documents"`
	Chunks          int      `json:"chunks"`
	EmbeddingModels []string `json:"embedding_models"`
}

// formatVector renders a pgvector literal. Vectors only ever travel Go →
// Postgres, so a text cast is the whole codec; nothing scans one back.
func formatVector(v []float32) string {
	var b strings.Builder
	b.Grow(len(v)*10 + 2)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// IngestDocument upserts a document and replaces its chunks in one
// transaction: a crash mid-corpus leaves every committed document whole and
// every unstarted one absent. Chunks must carry their embeddings; ordinals
// are enforced unique by the schema.
func (s *Store) IngestDocument(ctx context.Context, doc Document, chunks []Chunk) (uuid.UUID, error) {
	if len(doc.Metadata) == 0 {
		doc.Metadata = json.RawMessage(`{}`)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	var docID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO documents (id, source_id, title, court, decided_on, metadata, content_sha256, chunk_count, ingested_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, now())
		ON CONFLICT (source_id) DO UPDATE SET
			title = EXCLUDED.title, court = EXCLUDED.court, decided_on = EXCLUDED.decided_on,
			metadata = EXCLUDED.metadata, content_sha256 = EXCLUDED.content_sha256,
			chunk_count = EXCLUDED.chunk_count, ingested_at = now()
		RETURNING id`,
		uuid.New(), doc.SourceID, doc.Title, doc.Court, doc.DecidedOn, doc.Metadata,
		doc.ContentSHA256, len(chunks)).Scan(&docID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("upsert document %s: %w", doc.SourceID, err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM chunks WHERE document_id = $1`, docID); err != nil {
		return uuid.Nil, fmt.Errorf("delete chunks: %w", err)
	}
	batch := &pgx.Batch{}
	for _, c := range chunks {
		if len(c.Embedding) != EmbeddingDimensions {
			return uuid.Nil, fmt.Errorf("chunk %d of %s: embedding has %d dimensions, schema wants %d",
				c.Ordinal, doc.SourceID, len(c.Embedding), EmbeddingDimensions)
		}
		batch.Queue(`
			INSERT INTO chunks (id, document_id, ordinal, content, section, char_start, char_end, embedding_model, embedding)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, NULLIF($8, ''), $9::vector)`,
			uuid.New(), docID, c.Ordinal, c.Content, c.Section, c.CharStart, c.CharEnd,
			c.EmbeddingModel, formatVector(c.Embedding))
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return uuid.Nil, fmt.Errorf("insert chunks: %w", err)
	}
	return docID, tx.Commit(ctx)
}

const documentColumns = `id, source_id, title, COALESCE(court, ''), decided_on, metadata,
	content_sha256, chunk_count, ingested_at, created_at`

func scanDocument(row pgx.Row) (*Document, error) {
	var d Document
	var title *string
	err := row.Scan(&d.ID, &d.SourceID, &title, &d.Court, &d.DecidedOn, &d.Metadata,
		&d.ContentSHA256, &d.ChunkCount, &d.IngestedAt, &d.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if title != nil {
		d.Title = *title
	}
	return &d, nil
}

// GetDocument loads a document by id.
func (s *Store) GetDocument(ctx context.Context, id uuid.UUID) (*Document, error) {
	return scanDocument(s.pool.QueryRow(ctx, `SELECT `+documentColumns+` FROM documents WHERE id = $1`, id))
}

// GetDocumentBySourceID loads a document by its external id.
func (s *Store) GetDocumentBySourceID(ctx context.Context, sourceID string) (*Document, error) {
	return scanDocument(s.pool.QueryRow(ctx, `SELECT `+documentColumns+` FROM documents WHERE source_id = $1`, sourceID))
}

// ListChunks returns a document's chunks in ordinal order, restricted to
// [from, to] when to >= 0.
func (s *Store) ListChunks(ctx context.Context, docID uuid.UUID, from, to int) ([]Chunk, error) {
	if from < 0 {
		from = 0
	}
	hi := to
	if hi < 0 {
		hi = 1<<31 - 1
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, document_id, ordinal, COALESCE(section, ''), content, char_start, char_end, COALESCE(embedding_model, '')
		FROM chunks WHERE document_id = $1 AND ordinal BETWEEN $2 AND $3 ORDER BY ordinal`,
		docID, from, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.Ordinal, &c.Section, &c.Content,
			&c.CharStart, &c.CharEnd, &c.EmbeddingModel); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetChunkByCitation resolves a (source_id, ordinal) pair — the citation
// format the corpus tools hand the model and the system prompt asks for — to
// the chunk it names, or ErrNotFound.
//
// It exists as one query rather than GetDocumentBySourceID followed by
// ListChunks because the M5 CITATION eval calls it once per citation in an
// answer, and a fabricated citation is exactly the case where the first half
// of that pair would have succeeded and told the caller nothing.
func (s *Store) GetChunkByCitation(ctx context.Context, sourceID string, ordinal int) (*SearchHit, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+hitColumns+`
		FROM chunks c JOIN documents d ON d.id = c.document_id
		WHERE d.source_id = $1 AND c.ordinal = $2`, sourceID, ordinal)
	var h SearchHit
	err := row.Scan(&h.ChunkID, &h.DocumentID, &h.SourceID, &h.Title, &h.Court,
		&h.DecidedOn, &h.Section, &h.Ordinal, &h.Content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

const hitColumns = `c.id, c.document_id, d.source_id, COALESCE(d.title, ''), COALESCE(d.court, ''),
	d.decided_on, COALESCE(c.section, ''), c.ordinal, c.content`

// stableTiebreak orders chunks that score identically by their citation
// coordinates, which are a property of the corpus text.
//
// Both search queries used to break ties on c.id, and that was wrong in a way
// only an eval could catch: IngestDocument mints a fresh uuid.New() for every
// chunk on every ingest, so the tiebreaker was itself random per ingest. Two
// ingests of the same file gave the same scores and a different order, and
// with a small corpus there are real ties — a bag-of-words or an embedding
// query matches many chunks equally — so the top-k changed. That made every
// retrieval number unreproducible across re-ingests, including the recall@k
// table in the README, and it broke cassette replay outright, which is how it
// was found (M5).
//
// (source_id, ordinal) is the pair the corpus tools already hand the model to
// cite with, and the schema makes it unique per chunk.
const stableTiebreak = `, d.source_id, c.ordinal`

// filterSQL renders the optional corpus filters as extra AND clauses, using
// placeholders starting after n existing arguments.
func (f CorpusFilter) filterSQL(n int, args []any) (string, []any) {
	var sb strings.Builder
	if f.Court != "" {
		n++
		fmt.Fprintf(&sb, " AND d.court = $%d", n)
		args = append(args, f.Court)
	}
	if f.From != nil {
		n++
		fmt.Fprintf(&sb, " AND d.decided_on >= $%d", n)
		args = append(args, *f.From)
	}
	if f.To != nil {
		n++
		fmt.Fprintf(&sb, " AND d.decided_on <= $%d", n)
		args = append(args, *f.To)
	}
	return sb.String(), args
}

func scanHits(rows pgx.Rows) ([]SearchHit, error) {
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ChunkID, &h.DocumentID, &h.SourceID, &h.Title, &h.Court,
			&h.DecidedOn, &h.Section, &h.Ordinal, &h.Content); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SearchVector returns the chunks nearest to embedding by cosine distance.
// hnsw.ef_search is raised for the transaction so the candidate list is not
// starved by the index default of 40.
func (s *Store) SearchVector(ctx context.Context, embedding []float32, limit int, f CorpusFilter) ([]SearchHit, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL hnsw.ef_search = 100`); err != nil {
		return nil, err
	}
	args := []any{formatVector(embedding), limit}
	where, args := f.filterSQL(len(args), args)
	rows, err := tx.Query(ctx, `
		SELECT `+hitColumns+`
		FROM chunks c JOIN documents d ON d.id = c.document_id
		WHERE c.embedding IS NOT NULL`+where+`
		ORDER BY c.embedding <=> $1::vector`+stableTiebreak+`
		LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	hits, err := scanHits(rows)
	if err != nil {
		return nil, err
	}
	return hits, tx.Commit(ctx)
}

// BM25 parameters. These are the textbook defaults and were not tuned: the
// only labels to tune against are drafts (ADR-41).
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// lexicalSearchSQL ranks chunks against a natural-language query with Okapi
// BM25. Placeholders: $1 query text, $2 limit; filterSQL's clauses follow.
//
// Candidates: the query is normalised into lexemes by the same dictionary that
// built the index, and a chunk is a candidate if it contains any of them. The
// lexemes are OR-ed, not AND-ed as websearch_to_tsquery would: a model sends a
// whole question, and requiring every one of ten lexemes in a 1,200-character
// chunk matches essentially nothing (measured: recall@8 of 0.15 against 0.85
// for OR). quote_literal keeps operator characters from being parsed as
// tsquery syntax.
//
// Ranking: the candidate set is the same one ts_rank_cd used to rank, and
// ts_rank_cd was the problem. It weighs every query lexeme alike, so on a
// case-name query a string citation repeating "v." outranked the one chunk
// naming the case. BM25 weights each lexeme by its IDF (lexeme_stats),
// saturates repeated occurrences, and normalises by chunk length
// (chunk_lexical_length, corpus_lexical_stats). Measured on the stratified set, recall@8 went from
// 0.217 to 0.831 on case_name queries and from 0.850 to 1.000 on paraphrases.
//
// A lexeme missing from lexeme_stats (added since the last refresh) counts as
// unseen, the highest IDF; df is capped at the chunk count so stale statistics
// cannot produce a negative IDF. A chunk missing from chunk_lexical_length is
// ranked as average length. Before the first refresh every statistic is empty
// and ranking falls back to term frequency alone, rather than returning nothing.
var lexicalSearchSQL = fmt.Sprintf(`
	WITH q AS (
		SELECT DISTINCT lex FROM unnest(tsvector_to_array(to_tsvector('english', $1))) AS lex
	), terms AS (
		SELECT q.lex,
		       ln(1 + (s.chunks - df.n + 0.5) / (df.n + 0.5)) AS idf
		FROM q
		CROSS JOIN corpus_lexical_stats s
		LEFT JOIN lexeme_stats l ON l.lexeme = q.lex
		CROSS JOIN LATERAL (SELECT LEAST(COALESCE(l.chunks, 0), s.chunks)::float8 AS n) df
	), candidates AS (
		SELECT c.id, c.tsv
		FROM chunks c JOIN documents d ON d.id = c.document_id
		WHERE c.tsv @@ to_tsquery('english', (SELECT string_agg(quote_literal(lex), ' | ') FROM q))%%s
	), scored AS (
		-- The inner join on terms matters. A candidate can match the tsquery
		-- without sharing a lexeme with q, because to_tsquery re-stems each
		-- quoted lexeme ("conting" becomes "cont"). Such a chunk has nothing
		-- BM25 can score; with an outer join its score was NULL, and NULL sorts
		-- first under DESC.
		SELECT cand.id,
		       sum(t.idf * t.tf * (%[1]g + 1) / (t.tf + %[1]g * (1 - %[2]g + %[2]g * len.ratio))) AS score
		FROM candidates cand
		CROSS JOIN corpus_lexical_stats s
		LEFT JOIN chunk_lexical_length cl ON cl.id = cand.id
		CROSS JOIN LATERAL (SELECT COALESCE(cl.length / NULLIF(s.avg_length, 0), 1) AS ratio) len
		CROSS JOIN LATERAL (
			SELECT terms.idf, COALESCE(array_length(u.positions, 1), 1)::float8 AS tf
			FROM unnest(cand.tsv) u JOIN terms ON terms.lex = u.lexeme
		) t
		GROUP BY cand.id
	)
	SELECT `+hitColumns+`
	FROM scored JOIN chunks c ON c.id = scored.id JOIN documents d ON d.id = c.document_id
	ORDER BY scored.score DESC`+stableTiebreak+`
	LIMIT $2`, bm25K1, bm25B)

// SearchLexical ranks chunks against a natural-language query with BM25 (see
// lexicalSearchSQL). A query whose every word is a stopword yields no lexemes
// and therefore no hits, rather than an error.
func (s *Store) SearchLexical(ctx context.Context, query string, limit int, f CorpusFilter) ([]SearchHit, error) {
	args := []any{query, limit}
	where, args := f.filterSQL(len(args), args)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(lexicalSearchSQL, where), args...)
	if err != nil {
		return nil, err
	}
	return scanHits(rows)
}

// RefreshLexicalStats recomputes the corpus statistics BM25 ranks with. It
// scans every chunk's tsvector twice (a few seconds for 70,000 chunks), so
// retrieval.Ingest calls it once per run rather than once per document.
// CONCURRENTLY keeps searches reading the previous statistics meanwhile; the
// transaction keeps the views from describing different corpora, and the order
// matters: corpus_lexical_stats is computed from chunk_lexical_length.
func (s *Store) RefreshLexicalStats(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, view := range []string{"lexeme_stats", "chunk_lexical_length", "corpus_lexical_stats"} {
		if _, err := tx.Exec(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY `+view); err != nil {
			return fmt.Errorf("refresh %s: %w", view, err)
		}
	}
	return tx.Commit(ctx)
}

// CorpusStats reports what is ingested, for the eval results file and the
// ingest summary.
func (s *Store) CorpusStats(ctx context.Context) (CorpusStats, error) {
	var st CorpusStats
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM documents),
		       (SELECT count(*) FROM chunks),
		       (SELECT COALESCE(array_agg(DISTINCT embedding_model), '{}') FROM chunks WHERE embedding_model IS NOT NULL)`).
		Scan(&st.Documents, &st.Chunks, &st.EmbeddingModels)
	return st, err
}
