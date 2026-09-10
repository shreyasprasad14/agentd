package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/retrieval"
)

// rankedSearcher returns a fixed ranking per query, ignoring mode (each mode
// gets the same list, which is fine for metric math).
type rankedSearcher struct {
	rankings map[string][]retrieval.Hit
}

func (r rankedSearcher) Search(_ context.Context, q retrieval.Query) (*retrieval.Result, error) {
	hits := r.rankings[q.Text]
	return &retrieval.Result{Mode: q.Mode, Hits: hits, CandidatesConsidered: len(hits)}, nil
}

func h(sourceID string, ordinal int) retrieval.Hit {
	return retrieval.Hit{SourceID: sourceID, Ordinal: ordinal}
}

func TestMetricsOnHandBuiltRanking(t *testing.T) {
	labels := &Labels{Cases: []Case{
		{
			// Two labels: chunk-level found at rank 2; doc-level found at rank 10.
			Query: "q1",
			Relevant: []Relevant{
				{SourceID: "doc-a", Ordinals: []int{3}},
				{SourceID: "doc-b"},
			},
		},
		{
			// One label, never found.
			Query:    "q2",
			Relevant: []Relevant{{SourceID: "doc-z"}},
		},
	}}
	ranking := []retrieval.Hit{
		h("doc-x", 0),
		h("doc-a", 3), // rank 2: matches the chunk-level label
		h("doc-a", 4), // wrong ordinal, no match
	}
	for i := 0; i < 6; i++ {
		ranking = append(ranking, h("filler", i))
	}
	ranking = append(ranking, h("doc-b", 7)) // rank 10: doc-level, any ordinal counts

	s := rankedSearcher{rankings: map[string][]retrieval.Hit{"q1": ranking, "q2": nil}}
	rep, err := Run(context.Background(), s, labels, []retrieval.Mode{retrieval.ModeHybrid})
	require.NoError(t, err)
	require.Equal(t, 2, rep.Cases)
	row := rep.Modes[0]

	// q1: recall@8 = 1/2 (only the chunk label inside top 8), recall@20 = 2/2.
	// q2: all zero. Averages: 0.25, 0.5, 0.5.
	require.InDelta(t, 0.25, row.RecallAt8, 1e-9)
	require.InDelta(t, 0.5, row.RecallAt20, 1e-9)
	require.InDelta(t, 0.5, row.RecallAt50, 1e-9)
	// q1 first relevant at rank 2 → 0.5; q2 → 0. Average 0.25.
	require.InDelta(t, 0.25, row.MRR, 1e-9)
}

func TestThresholdsPassWhenMet(t *testing.T) {
	labels := &Labels{
		Thresholds: map[string]map[string]float64{
			"hybrid_rerank": {"recall_at_8": 0.9},
			"hybrid":        {"mrr": 0.1},
		},
		Cases: []Case{{Query: "q", Relevant: []Relevant{{SourceID: "doc-a"}}}},
	}
	s := rankedSearcher{rankings: map[string][]retrieval.Hit{"q": {h("doc-a", 0)}}}

	// The fake searcher echoes the requested mode, so hybrid+rerank "ran".
	rep, err := Run(context.Background(), s, labels, []retrieval.Mode{retrieval.ModeHybrid, retrieval.ModeHybridRerank})
	require.NoError(t, err)
	require.Empty(t, rep.Failures, "both metrics are 1.0, above their thresholds")
}

func TestThresholdMissListsTheMetric(t *testing.T) {
	labels := &Labels{
		Thresholds: map[string]map[string]float64{"vector": {"recall_at_8": 0.9}},
		Cases: []Case{
			{Query: "hit", Relevant: []Relevant{{SourceID: "doc-a"}}},
			{Query: "miss", Relevant: []Relevant{{SourceID: "doc-z"}}},
		},
	}
	s := rankedSearcher{rankings: map[string][]retrieval.Hit{"hit": {h("doc-a", 0)}}}
	rep, err := Run(context.Background(), s, labels, []retrieval.Mode{retrieval.ModeVector})
	require.NoError(t, err)
	require.Len(t, rep.Failures, 1)
	require.Contains(t, rep.Failures[0], "vector recall_at_8 = 0.500")
}

func TestRerankDegradationIsAnError(t *testing.T) {
	// A searcher that degrades hybrid+rerank to hybrid must fail the eval:
	// a table row labeled rerank that silently measured hybrid is worse than
	// no row.
	degrading := degradingSearcher{}
	labels := &Labels{Cases: []Case{{Query: "q", Relevant: []Relevant{{SourceID: "a"}}}}}
	_, err := Run(context.Background(), degrading, labels, []retrieval.Mode{retrieval.ModeHybridRerank})
	require.ErrorContains(t, err, "degraded")
}

type degradingSearcher struct{}

func (degradingSearcher) Search(_ context.Context, q retrieval.Query) (*retrieval.Result, error) {
	return &retrieval.Result{Mode: retrieval.ModeHybrid}, nil
}

func TestReadLabels(t *testing.T) {
	src := `
thresholds:
  hybrid_rerank: {recall_at_8: 0.70}
cases:
  - query: "standard for qualified immunity"
    relevant:
      - {source_id: "clop-1", ordinals: [14, 15]}
      - {source_id: "clop-2"}
`
	l, err := ReadLabels(strings.NewReader(src))
	require.NoError(t, err)
	require.Len(t, l.Cases, 1)
	require.Equal(t, 0.70, l.Thresholds["hybrid_rerank"]["recall_at_8"])
	require.True(t, l.Cases[0].Relevant[0].Matches("clop-1", 15))
	require.False(t, l.Cases[0].Relevant[0].Matches("clop-1", 16))
	require.True(t, l.Cases[0].Relevant[1].Matches("clop-2", 99), "document-level label matches any ordinal")

	for name, bad := range map[string]string{
		"no cases":     `cases: []`,
		"empty query":  "cases:\n  - query: \"\"\n    relevant: [{source_id: a}]",
		"no relevant":  "cases:\n  - query: q\n    relevant: []",
		"no source_id": "cases:\n  - query: q\n    relevant: [{ordinals: [1]}]",
		"unknown key":  "cases:\n  - query: q\n    releva: [{source_id: a}]",
	} {
		_, err := ReadLabels(strings.NewReader(bad))
		require.Error(t, err, name)
	}
}

func TestRenderTable(t *testing.T) {
	rep := &Report{Modes: []ModeResult{{Mode: retrieval.ModeVector, RecallAt8: 0.5, MRR: 0.25}}}
	out := RenderTable(rep)
	require.Contains(t, out, "| mode | recall@8 |")
	require.Contains(t, out, "| vector | 0.500 | 0.000 | 0.000 | 0.250 |")
}
