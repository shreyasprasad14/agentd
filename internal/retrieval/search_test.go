package retrieval

import (
	"context"
	"fmt"
	"strings"
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/shreyasprasad/agentd/internal/retrieval/embed"
	"github.com/shreyasprasad/agentd/internal/retrieval/rerank"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/telemetry"
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

// scriptReranker returns canned scores and canned usage for testing
// degradation paths.
type scriptReranker struct {
	fn    func(cands []rerank.Candidate) ([]rerank.Scored, error)
	usage rerank.Usage
}

func (s scriptReranker) Rerank(_ context.Context, _ string, cands []rerank.Candidate) ([]rerank.Scored, rerank.Usage, error) {
	scored, err := s.fn(cands)
	return scored, s.usage, err
}

// rerankSpend is what the scripted rerankers claim to have cost.
var rerankSpend = rerank.Usage{MicroUSD: 1200, InputTokens: 11000, OutputTokens: 500, Model: "reranker"}

func TestSearchRerankReorders(t *testing.T) {
	s, _ := newSearcher(t)
	// Score candidates in reverse fused order, so the reranker visibly wins.
	s.Reranker = scriptReranker{fn: func(cands []rerank.Candidate) ([]rerank.Scored, error) {
		out := make([]rerank.Scored, len(cands))
		for i, c := range cands {
			out[i] = rerank.Scored{Index: c.Index, Score: float64(c.Index), Scored: true}
		}
		return out, nil
	}, usage: rerankSpend}
	require.Equal(t, ModeHybridRerank, s.BestMode())

	plain, err := s.Search(context.Background(), Query{Text: plantedQuery, K: 3, Mode: ModeHybrid})
	require.NoError(t, err)
	require.Equal(t, rerank.Usage{}, plain.Usage, "a search that never reranks spends nothing")
	res, err := s.Search(context.Background(), Query{Text: plantedQuery, K: 3, Mode: ModeHybridRerank})
	require.NoError(t, err)
	require.Equal(t, ModeHybridRerank, res.Mode)
	require.Equal(t, plain.Hits[len(plain.Hits)-1].ChunkID, res.Hits[0].ChunkID, "highest reranker score leads")
	require.True(t, res.Hits[0].Scores.Reranked)
	require.Equal(t, rerankSpend, res.Usage, "the reranker's spend reaches the caller for the budget")
}

func TestSearchRerankDegradesToHybrid(t *testing.T) {
	s, _ := newSearcher(t)
	for name, tc := range map[string]struct {
		reranker rerank.Reranker
		want     rerank.Usage
	}{
		"error": {reranker: scriptReranker{fn: func([]rerank.Candidate) ([]rerank.Scored, error) {
			return nil, fmt.Errorf("reranker down")
		}, usage: rerankSpend}, want: rerankSpend},
		"short result": {reranker: scriptReranker{fn: func([]rerank.Candidate) ([]rerank.Scored, error) {
			return nil, nil
		}, usage: rerankSpend}, want: rerankSpend},
		"unscored": {reranker: rerank.Noop{}, want: rerank.Usage{}},
	} {
		s.Reranker = tc.reranker
		res, err := s.Search(context.Background(), Query{Text: plantedQuery, K: 3, Mode: ModeHybridRerank})
		require.NoError(t, err, name)
		require.Equal(t, ModeHybrid, res.Mode, "%s must degrade to the fused order, not fail", name)
		require.Equal(t, "clop-a", res.Hits[0].SourceID, name)
		require.Equal(t, tc.want, res.Usage, "%s: falling back to the fused order does not refund the tokens", name)
	}
}

// recordingSearcher is newSearcher with a tracer whose spans are kept in
// memory: no collector, no Jaeger, and the waterfall this search will draw is
// readable from the recorder.
func recordingSearcher(t *testing.T) (*Searcher, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	s, _ := newSearcher(t)
	s.Tracer = tp.Tracer("retrieval_test")
	return s, sr
}

// searchSpans splits a recording into the one retrieval.search span, the
// attributes on it, and the names of the stage spans that named it parent.
// Checking parentage rather than mere presence is the point: the two indexed
// searches run in goroutines, and a stage that took its context from the
// wrong place would still be recorded — just in the wrong place in the tree.
func searchSpans(t *testing.T, sr *tracetest.SpanRecorder) (map[attribute.Key]attribute.Value, []string) {
	t.Helper()
	var parent sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == telemetry.SpanSearch {
			require.Nil(t, parent, "one Search draws one retrieval.search span")
			parent = s
		}
	}
	require.NotNil(t, parent, "no retrieval.search span recorded")

	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range parent.Attributes() {
		attrs[kv.Key] = kv.Value
	}
	var children []string
	for _, s := range sr.Ended() {
		if s.Parent().SpanID() == parent.SpanContext().SpanID() {
			children = append(children, s.Name())
		}
	}
	return attrs, children
}

func TestSearchSpanRecordsStagesAndMode(t *testing.T) {
	s, sr := recordingSearcher(t)
	s.Reranker = scriptReranker{fn: func(cands []rerank.Candidate) ([]rerank.Scored, error) {
		out := make([]rerank.Scored, len(cands))
		for i, c := range cands {
			out[i] = rerank.Scored{Index: c.Index, Score: float64(c.Index), Scored: true}
		}
		return out, nil
	}, usage: rerankSpend}

	res, err := s.Search(context.Background(), Query{Text: plantedQuery, K: 3, Mode: ModeHybridRerank})
	require.NoError(t, err)

	attrs, children := searchSpans(t, sr)
	require.Equal(t, string(ModeHybridRerank), attrs[telemetry.AttrRetrievalMode].AsString())
	require.Equal(t, int64(3), attrs[telemetry.AttrRetrievalK].AsInt64())
	require.Equal(t, int64(res.CandidatesConsidered), attrs[telemetry.AttrCandidates].AsInt64())
	require.Greater(t, attrs[telemetry.AttrVectorHits].AsInt64(), int64(0))
	require.Greater(t, attrs[telemetry.AttrLexicalHits].AsInt64(), int64(0))
	require.False(t, attrs[telemetry.AttrRerankDegraded].AsBool())
	require.ElementsMatch(t,
		[]string{telemetry.SpanEmbed, telemetry.SpanVector, telemetry.SpanLexical, telemetry.SpanRerank},
		children, "all four stages hang off the search span")
}

func TestSearchSpanRecordsDegradedRerank(t *testing.T) {
	s, sr := recordingSearcher(t)
	s.Reranker = scriptReranker{fn: func([]rerank.Candidate) ([]rerank.Scored, error) {
		return nil, fmt.Errorf("reranker down")
	}, usage: rerankSpend}

	res, err := s.Search(context.Background(), Query{Text: plantedQuery, K: 3, Mode: ModeHybridRerank})
	require.NoError(t, err)
	require.Equal(t, ModeHybrid, res.Mode)

	attrs, _ := searchSpans(t, sr)
	// The span reports what ran, not what was asked for: a reader who sees
	// hybrid+rerank here would spend the rest of the afternoon wondering why
	// the order looks fused.
	require.Equal(t, string(ModeHybrid), attrs[telemetry.AttrRetrievalMode].AsString())
	require.True(t, attrs[telemetry.AttrRerankDegraded].AsBool())

	for _, sp := range sr.Ended() {
		if sp.Name() != telemetry.SpanRerank {
			continue
		}
		spend := map[attribute.Key]attribute.Value{}
		for _, kv := range sp.Attributes() {
			spend[kv.Key] = kv.Value
		}
		// A degraded pass still spent the tokens, and the bar that spent them
		// is where the run's budget can be read back (ADR-23).
		require.Equal(t, rerankSpend.MicroUSD, spend[telemetry.AttrCostMicroUSD].AsInt64())
		require.Equal(t, rerankSpend.InputTokens, spend[telemetry.AttrGenAIInputTokens].AsInt64())
		require.Equal(t, rerankSpend.Model, spend[telemetry.AttrGenAIRequestModel].AsString())
	}
}

func TestSearchSpanStagesFollowMode(t *testing.T) {
	s, sr := recordingSearcher(t)
	for mode, want := range map[Mode][]string{
		ModeVector:  {telemetry.SpanEmbed, telemetry.SpanVector},
		ModeLexical: {telemetry.SpanLexical},
		ModeHybrid:  {telemetry.SpanEmbed, telemetry.SpanVector, telemetry.SpanLexical},
	} {
		sr.Reset()
		_, err := s.Search(context.Background(), Query{Text: plantedQuery, K: 3, Mode: mode})
		require.NoError(t, err, mode)
		_, children := searchSpans(t, sr)
		require.ElementsMatch(t, want, children, "mode %s draws only the stages it runs", mode)
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

// TestSearchMetricsRecordModeAndStages is the searcher's half of M4's
// counters. The degraded case is the one worth pinning: the counter says what
// ran, so a fleet quietly falling back to fused order shows up as a rate
// rather than as a complaint about result quality.
func TestSearchMetricsRecordModeAndStages(t *testing.T) {
	s, _ := newSearcher(t)
	s.Metrics = telemetry.NewMetrics()
	s.Reranker = scriptReranker{fn: func([]rerank.Candidate) ([]rerank.Scored, error) {
		return nil, fmt.Errorf("reranker down")
	}, usage: rerankSpend}

	res, err := s.Search(context.Background(), Query{Text: plantedQuery, K: 3, Mode: ModeHybridRerank})
	require.NoError(t, err)
	require.Equal(t, ModeHybrid, res.Mode)

	require.Equal(t, 1, promtestutil.CollectAndCount(s.Metrics.Registry(), "agentd_retrieval_searches_total"))
	require.NoError(t, promtestutil.CollectAndCompare(s.Metrics.Registry(), strings.NewReader(`
# HELP agentd_retrieval_searches_total Corpus searches by the mode that ran and whether the reranker degraded.
# TYPE agentd_retrieval_searches_total counter
agentd_retrieval_searches_total{degraded="true",mode="hybrid"} 1
`), "agentd_retrieval_searches_total"))

	// Every stage that ran is timed, including the rerank that gave up: the
	// seconds it burned before degrading are exactly what a latency question
	// about the reranker is asking after.
	stages := map[string]bool{}
	families, err := s.Metrics.Registry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != "agentd_retrieval_duration_seconds" {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetName() == "stage" {
					stages[l.GetValue()] = true
				}
			}
		}
	}
	require.Equal(t, map[string]bool{
		telemetry.StageSearch: true, telemetry.StageEmbed: true,
		telemetry.StageVector: true, telemetry.StageLexical: true, telemetry.StageRerank: true,
	}, stages)
}
