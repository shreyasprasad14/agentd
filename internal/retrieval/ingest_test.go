package retrieval

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/retrieval/embed"
	"github.com/shreyasprasad/agentd/internal/testutil"
)

func loadFixture(t *testing.T) []InputDoc {
	t.Helper()
	f, err := os.Open("../../evals/retrieval/fixture.jsonl")
	require.NoError(t, err)
	defer f.Close()
	docs, err := ReadJSONL(f)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(docs), 10, "the fixture should hold ~12 opinions")
	return docs
}

// renamedFake is the fake embedder under a different model name, to exercise
// the re-embed-on-model-change path.
type renamedFake struct {
	*embed.Fake
	name string
}

func (r renamedFake) Model() string { return r.name }

// failingEmbedder errors on every call.
type failingEmbedder struct{ *embed.Fake }

func (failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, fmt.Errorf("embedder unreachable")
}

func TestIngestFixtureIdempotent(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()
	docs := loadFixture(t)
	fake := embed.NewFake()

	sum, err := Ingest(ctx, st, fake, docs, IngestOptions{})
	require.NoError(t, err)
	require.Equal(t, len(docs), sum.Seen)
	require.Equal(t, len(docs), sum.Ingested)
	require.Zero(t, sum.Skipped)
	require.Zero(t, sum.Failed)
	require.Greater(t, sum.Chunks, len(docs), "each opinion should yield at least one chunk, most several")

	stats, err := st.CorpusStats(ctx)
	require.NoError(t, err)
	require.Equal(t, len(docs), stats.Documents)
	require.Equal(t, sum.Chunks, stats.Chunks)
	require.Equal(t, []string{fake.Model()}, stats.EmbeddingModels)

	// Second run: nothing changed, everything skips.
	sum2, err := Ingest(ctx, st, fake, docs, IngestOptions{})
	require.NoError(t, err)
	require.Equal(t, len(docs), sum2.Skipped)
	require.Zero(t, sum2.Ingested)

	// Force re-embeds everything.
	sum3, err := Ingest(ctx, st, fake, docs, IngestOptions{Force: true})
	require.NoError(t, err)
	require.Equal(t, len(docs), sum3.Ingested)

	// A different embedding model re-embeds even though the hash matches.
	renamed := renamedFake{embed.NewFake(), "fake-v2"}
	sum4, err := Ingest(ctx, st, renamed, docs, IngestOptions{})
	require.NoError(t, err)
	require.Equal(t, len(docs), sum4.Ingested, "model change must re-embed")
	stats, err = st.CorpusStats(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"fake-v2"}, stats.EmbeddingModels)
}

func TestIngestChangedContentReingests(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()
	fake := embed.NewFake()
	doc := InputDoc{SourceID: "clop-x", Title: "X", Text: "The original opinion text, long enough to form a chunk on its own for this test."}

	_, err := Ingest(ctx, st, fake, []InputDoc{doc}, IngestOptions{})
	require.NoError(t, err)
	doc.Text = "The revised opinion text, also long enough to form a chunk on its own for this test."
	sum, err := Ingest(ctx, st, fake, []InputDoc{doc}, IngestOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, sum.Ingested)

	d, err := st.GetDocumentBySourceID(ctx, "clop-x")
	require.NoError(t, err)
	chunks, err := st.ListChunks(ctx, d.ID, 0, -1)
	require.NoError(t, err)
	require.Contains(t, chunks[0].Content, "revised")
}

func TestIngestFailingEmbedderLeavesNoPartialDocument(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()
	docs := loadFixture(t)[:3]

	sum, err := Ingest(ctx, st, failingEmbedder{embed.NewFake()}, docs, IngestOptions{})
	require.NoError(t, err, "per-document failures are counted, not returned")
	require.Equal(t, 3, sum.Failed)
	require.Zero(t, sum.Ingested)

	stats, err := st.CorpusStats(ctx)
	require.NoError(t, err)
	require.Zero(t, stats.Documents, "a failing embedder must leave nothing behind")
	require.Zero(t, stats.Chunks)
}

func TestIngestEmptyTextSkipped(t *testing.T) {
	st := testutil.Postgres(t)
	sum, err := Ingest(context.Background(), st, embed.NewFake(),
		[]InputDoc{{SourceID: "clop-empty", Text: ""}}, IngestOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, sum.Skipped)
}

func TestReadJSONLRejectsMissingSourceID(t *testing.T) {
	_, err := ReadJSONL(strings.NewReader(`{"title":"no id","text":"x"}` + "\n"))
	require.ErrorContains(t, err, "source_id")
}
