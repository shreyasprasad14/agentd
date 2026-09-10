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

const hitColumns = `c.id, c.document_id, d.source_id, COALESCE(d.title, ''), COALESCE(d.court, ''),
	d.decided_on, COALESCE(c.section, ''), c.ordinal, c.content`

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
		ORDER BY c.embedding <=> $1::vector, c.id
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

// lexicalQuerySQL builds the tsquery for a natural-language query: the
// query is normalised into lexemes by the same dictionary that built the
// index, and those lexemes are OR-ed.
//
// The obvious choice, websearch_to_tsquery, ANDs its terms, which is right
// for a search box and wrong here: a model sends a whole question, and
// requiring every one of ten lexemes to appear in a 1,200-character chunk
// matches essentially nothing (measured: recall@8 of 0.15 against 0.85 for
// this version). With OR, ts_rank_cd does the discriminating, and because it
// is a cover-density rank it already scores chunks that contain the terms
// together, in order, above chunks that merely mention them.
//
// quote_literal on each lexeme keeps operator characters from being parsed
// as tsquery syntax.
const lexicalQuerySQL = `to_tsquery('english',
	(SELECT string_agg(quote_literal(lex), ' | ')
	   FROM unnest(tsvector_to_array(to_tsvector('english', $1))) AS lex))`

// SearchLexical ranks chunks against a natural-language query. A query whose
// every word is a stopword yields no lexemes and therefore no hits, rather
// than an error.
func (s *Store) SearchLexical(ctx context.Context, query string, limit int, f CorpusFilter) ([]SearchHit, error) {
	args := []any{query, limit}
	where, args := f.filterSQL(len(args), args)
	rows, err := s.pool.Query(ctx, `
		SELECT `+hitColumns+`
		FROM chunks c JOIN documents d ON d.id = c.document_id,
		     LATERAL (SELECT `+lexicalQuerySQL+` AS q) tq
		WHERE tq.q IS NOT NULL AND c.tsv @@ tq.q`+where+`
		ORDER BY ts_rank_cd(c.tsv, tq.q) DESC, c.id
		LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	return scanHits(rows)
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
