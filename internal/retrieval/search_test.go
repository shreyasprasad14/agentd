package retrieval

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/retrieval/embed"
	"github.com/shreyasprasad/agentd/internal/retrieval/rerank"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/testutil"
)

// seedSearchCorpus ingests three one-chunk documents embedded with the fake
// embedder:
//
//	clop-a: strong overlap with the planted query (both lists, rank 1)
//	clop-b: partial overlap (both lists, lower)
//	clop-c: unrelated (vector list only; no lexical match)
func seedSearchCorpus(t *testing.T, st *store.Store, f embed.Embedder) {
	t.Helper()
	ctx := context.Background()
	docs := map[string]string{
		"clop-a": "The qualified immunity defense requires clearly established law at the time of the conduct.",
		"clop-b": "Government officials assert immunity against damages claims in federal court.",
		"clop-c": "Cell site location records reveal the phone owner's movements over long periods.",
	}
	for sid, content := range docs {
		vecs, err := f.Embed(ctx, []string{content})
		require.NoError(t, err)
		_, err = st.IngestDocument(ctx, store.Document{SourceID: sid, Title: "Case " + sid},
			[]store.Chunk{{Ordinal: 0, Content: content, Embedding: vecs[0], EmbeddingModel: f.Model()}})
		require.NoError(t, err)
	}
}

func newSearcher(t *testing.T) (*Searcher, *store.Store) {
	st := testutil.Postgres(t)
	f := embed.NewFake()
	seedSearchCorpus(t, st, f)
	return &Searcher{Store: st, Embedder: f, Candidates: 3}, st
}

const plantedQuery = "qualified immunity clearly established law"

func TestSearchEachModeFindsPlantedChunk(t *testing.T) {
	s, _ := newSearcher(t)
	for _, mode := range []Mode{ModeVector, ModeLexical, ModeHybrid} {
		res, err := s.Search(context.Background(), Query{Text: plantedQuery, K: 3, Mode: mode})
		require.NoError(t, err)
		require.NotEmpty(t, res.Hits, "mode %s", mode)
		require.Equal(t, "clop-a", res.Hits[0].SourceID, "mode %s must rank the planted chunk first", mode)
		require.Equal(t, mode, res.Mode)
	}
}

func TestSearchHybridRanksBothListsAboveEitherAlone(t *testing.T) {
	s, _ := newSearcher(t)
	res, err := s.Search(context.Background(), Query{Text: plantedQuery, K: 3, Mode: ModeHybrid})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(res.Hits), 2)

	pos := map[string]int{}
	for i, h := range res.Hits {
		pos[h.SourceID] = i + 1
	}
	require.Equal(t, 1, pos["clop-a"])
	if bPos, ok := pos["clop-b"]; ok {
		if cPos, ok := pos["clop-c"]; ok {
			require.Less(t, bPos, cPos, "a chunk found by both lists must outrank one found by vector alone")
		}
	}
	// The top hit was found by both searches at rank 1.
	require.Equal(t, 1, res.Hits[0].Scores.VectorRank)
	require.Equal(t, 1, res.Hits[0].Scores.LexicalRank)
	require.False(t, res.Hits[0].Scores.Reranked)
}

// scriptReranker returns canned scores for testing degradation paths.
type scriptReranker struct {
	fn func(cands []rerank.Candidate) ([]rerank.Scored, error)
}

func (s scriptReranker) Rerank(_ context.Context, _ string, cands []rerank.Candidate) ([]rerank.Scored, error) {
	return s.fn(cands)
}

func TestSearchRerankReorders(t *testing.T) {
	s, _ := newSearcher(t)
	// Score candidates in reverse fused order, so the reranker visibly wins.
	s.Reranker = scriptReranker{fn: func(cands []rerank.Candidate) ([]rerank.Scored, error) {
		out := make([]rerank.Scored, len(cands))
		for i, c := range cands {
			out[i] = rerank.Scored{Index: c.Index, Score: float64(c.Index), Scored: true}
		}
		return out, nil
	}}
	require.Equal(t, ModeHybridRerank, s.BestMode())

	plain, err := s.Search(context.Background(), Query{Text: plantedQuery, K: 3, Mode: ModeHybrid})
	require.NoError(t, err)
	res, err := s.Search(context.Background(), Query{Text: plantedQuery, K: 3, Mode: ModeHybridRerank})
	require.NoError(t, err)
	require.Equal(t, ModeHybridRerank, res.Mode)
	require.Equal(t, plain.Hits[len(plain.Hits)-1].ChunkID, res.Hits[0].ChunkID, "highest reranker score leads")
	require.True(t, res.Hits[0].Scores.Reranked)
}

func TestSearchRerankDegradesToHybrid(t *testing.T) {
	s, _ := newSearcher(t)
	for name, r := range map[string]rerank.Reranker{
		"error": scriptReranker{fn: func([]rerank.Candidate) ([]rerank.Scored, error) {
			return nil, fmt.Errorf("reranker down")
		}},
		"unscored": rerank.Noop{},
	} {
		s.Reranker = r
		res, err := s.Search(context.Background(), Query{Text: plantedQuery, K: 3, Mode: ModeHybridRerank})
		require.NoError(t, err, name)
		require.Equal(t, ModeHybrid, res.Mode, "%s must degrade to the fused order, not fail", name)
		require.Equal(t, "clop-a", res.Hits[0].SourceID, name)
	}
}

func TestSearchValidation(t *testing.T) {
	s, _ := newSearcher(t)
	_, err := s.Search(context.Background(), Query{Text: ""})
	require.Error(t, err)

	res, err := s.Search(context.Background(), Query{Text: plantedQuery}) // defaults: K=8, best mode
	require.NoError(t, err)
	require.Equal(t, ModeHybrid, res.Mode, "no reranker wired")
	require.LessOrEqual(t, len(res.Hits), 8)
}
