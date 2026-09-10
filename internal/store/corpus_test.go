package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/testutil"
)

// axis returns a unit vector along dimension i, so nearest-neighbour order
// under cosine distance is exactly known.
func axis(i int) []float32 {
	v := make([]float32, store.EmbeddingDimensions)
	v[i] = 1
	return v
}

// blend returns a vector between two axes, nearer the first.
func blend(i, j int) []float32 {
	v := make([]float32, store.EmbeddingDimensions)
	v[i] = 0.9
	v[j] = 0.1
	return v
}

func date(s string) *time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &d
}

func TestIngestDocumentUpsertReplacesChunks(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()

	doc := store.Document{SourceID: "clop-1", Title: "Smith v. Jones", Court: "scotus", DecidedOn: date("2015-06-01"), ContentSHA256: "aaa"}
	id1, err := st.IngestDocument(ctx, doc, []store.Chunk{
		{Ordinal: 0, Content: "first version chunk zero", Embedding: axis(0), EmbeddingModel: "fake"},
		{Ordinal: 1, Content: "first version chunk one", Embedding: axis(1), EmbeddingModel: "fake"},
		{Ordinal: 2, Content: "first version chunk two", Embedding: axis(2), EmbeddingModel: "fake"},
	})
	require.NoError(t, err)

	doc.ContentSHA256 = "bbb"
	id2, err := st.IngestDocument(ctx, doc, []store.Chunk{
		{Ordinal: 0, Content: "second version only chunk", Embedding: axis(3), EmbeddingModel: "fake2"},
	})
	require.NoError(t, err)
	require.Equal(t, id1, id2, "upsert must keep the same document row")

	got, err := st.GetDocumentBySourceID(ctx, "clop-1")
	require.NoError(t, err)
	require.Equal(t, "bbb", got.ContentSHA256)
	require.Equal(t, 1, got.ChunkCount)
	require.NotNil(t, got.IngestedAt)

	chunks, err := st.ListChunks(ctx, id1, 0, -1)
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Equal(t, "second version only chunk", chunks[0].Content)
	require.Equal(t, "fake2", chunks[0].EmbeddingModel)

	stats, err := st.CorpusStats(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Documents)
	require.Equal(t, 1, stats.Chunks)
	require.Equal(t, []string{"fake2"}, stats.EmbeddingModels)
}

func TestIngestDocumentDuplicateOrdinalRejected(t *testing.T) {
	st := testutil.Postgres(t)
	_, err := st.IngestDocument(context.Background(),
		store.Document{SourceID: "clop-dup"},
		[]store.Chunk{
			{Ordinal: 0, Content: "a", Embedding: axis(0)},
			{Ordinal: 0, Content: "b", Embedding: axis(1)},
		})
	require.Error(t, err, "unique (document_id, ordinal) must be enforced")
}

func TestIngestDocumentWrongWidthRejected(t *testing.T) {
	st := testutil.Postgres(t)
	_, err := st.IngestDocument(context.Background(),
		store.Document{SourceID: "clop-narrow"},
		[]store.Chunk{{Ordinal: 0, Content: "a", Embedding: []float32{1, 2, 3}}})
	require.ErrorContains(t, err, "dimensions")
}

// seedCorpus ingests three documents across two courts and three years, with
// axis-aligned embeddings so every search order is predictable.
func seedCorpus(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	docs := []struct {
		doc    store.Document
		chunks []store.Chunk
	}{
		{
			store.Document{SourceID: "clop-qi", Title: "Immunity Case", Court: "scotus", DecidedOn: date("2012-03-01")},
			[]store.Chunk{
				{Ordinal: 0, Content: "The doctrine of qualified immunity shields officials from suit.", Embedding: axis(0)},
				{Ordinal: 1, Content: "Clearly established law requires a case directly on point.", Embedding: axis(1)},
			},
		},
		{
			store.Document{SourceID: "clop-4a", Title: "Search Case", Court: "scotus", DecidedOn: date("2018-06-22")},
			[]store.Chunk{
				{Ordinal: 0, Content: "Cell site location information receives Fourth Amendment protection.", Embedding: axis(2)},
			},
		},
		{
			store.Document{SourceID: "clop-state", Title: "State Case", Court: "cal", DecidedOn: date("2020-01-15")},
			[]store.Chunk{
				{Ordinal: 0, Content: "The state constitution provides qualified immunity to municipal officers.", Embedding: blend(0, 2)},
			},
		},
	}
	for _, d := range docs {
		for i := range d.chunks {
			d.chunks[i].EmbeddingModel = "fake"
		}
		_, err := st.IngestDocument(ctx, d.doc, d.chunks)
		require.NoError(t, err)
	}
}

func TestSearchVector(t *testing.T) {
	st := testutil.Postgres(t)
	seedCorpus(t, st)
	ctx := context.Background()

	hits, err := st.SearchVector(ctx, blend(0, 1), 2, store.CorpusFilter{})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	require.Equal(t, "clop-qi", hits[0].SourceID)
	require.Equal(t, 0, hits[0].Ordinal)
	require.Equal(t, "Immunity Case", hits[0].Title)
	require.Equal(t, "scotus", hits[0].Court)

	// Court filter drops the scotus rows.
	hits, err = st.SearchVector(ctx, axis(0), 5, store.CorpusFilter{Court: "cal"})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "clop-state", hits[0].SourceID)

	// Date filter keeps only decisions from 2018 on.
	hits, err = st.SearchVector(ctx, axis(0), 5, store.CorpusFilter{From: date("2018-01-01")})
	require.NoError(t, err)
	for _, h := range hits {
		require.NotEqual(t, "clop-qi", h.SourceID)
	}
}

func TestSearchLexical(t *testing.T) {
	st := testutil.Postgres(t)
	seedCorpus(t, st)
	ctx := context.Background()

	hits, err := st.SearchLexical(ctx, `"cell site location"`, 5, store.CorpusFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	require.Equal(t, "clop-4a", hits[0].SourceID, "the phrase's chunk ranks first")

	hits, err = st.SearchLexical(ctx, "qualified immunity", 5, store.CorpusFilter{})
	require.NoError(t, err)
	require.Len(t, hits, 2)

	hits, err = st.SearchLexical(ctx, "qualified immunity", 5, store.CorpusFilter{Court: "scotus", To: date("2013-01-01")})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "clop-qi", hits[0].SourceID)
}

// TestSearchLexicalVerboseQuery is the regression the retrieval eval caught:
// with AND semantics (websearch_to_tsquery) a whole question matches nothing,
// because every lexeme has to appear in the same chunk.
func TestSearchLexicalVerboseQuery(t *testing.T) {
	st := testutil.Postgres(t)
	seedCorpus(t, st)
	ctx := context.Background()

	// No single chunk contains every lexeme of this question, which is
	// exactly the case AND semantics returned nothing for.
	hits, err := st.SearchLexical(ctx,
		"what is the standard for qualified immunity when officials claim the law was not clearly established",
		5, store.CorpusFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, hits, "a natural-language question must still retrieve")

	found := map[string]bool{}
	for _, h := range hits {
		found[h.SourceID] = true
	}
	require.True(t, found["clop-qi"], "the on-point opinion must be retrieved")

	// A question whose terms sit in one chunk ranks that chunk first; this is
	// the discrimination ts_rank_cd is doing now that the filter is OR.
	hits, err = st.SearchLexical(ctx,
		"when is law clearly established by a case directly on point", 5, store.CorpusFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	require.Equal(t, "clop-qi", hits[0].SourceID)
	require.Equal(t, 1, hits[0].Ordinal, "the 'clearly established' chunk carries all of these terms")
}

func TestSearchLexicalStopwordOnlyQuery(t *testing.T) {
	st := testutil.Postgres(t)
	seedCorpus(t, st)
	hits, err := st.SearchLexical(context.Background(), "the of and", 5, store.CorpusFilter{})
	require.NoError(t, err, "a query with no lexemes is empty, not an error")
	require.Empty(t, hits)
}

func TestGetDocumentAndListChunksRange(t *testing.T) {
	st := testutil.Postgres(t)
	seedCorpus(t, st)
	ctx := context.Background()

	doc, err := st.GetDocumentBySourceID(ctx, "clop-qi")
	require.NoError(t, err)
	byID, err := st.GetDocument(ctx, doc.ID)
	require.NoError(t, err)
	require.Equal(t, doc.SourceID, byID.SourceID)

	chunks, err := st.ListChunks(ctx, doc.ID, 1, 1)
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.Equal(t, 1, chunks[0].Ordinal)

	_, err = st.GetDocument(ctx, uuid.New())
	require.ErrorIs(t, err, store.ErrNotFound)
}
