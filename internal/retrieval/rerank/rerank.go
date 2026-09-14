// Package rerank rescores fused search candidates against the query. The
// interface is the seam: the LLM reranker ships in M3, a cross-encoder or
// llama.cpp /v1/rerank backend is a later drop-in (ADR-19).
package rerank

import "context"

// Candidate is one chunk to score. Index is the caller's position for it
// (the fused RRF order), echoed back on Scored.
type Candidate struct {
	Index   int
	Content string
}

// Scored is one candidate's relevance. Scored=false means no score was
// produced (the batch failed to parse or the provider errored); the caller
// keeps such candidates in their original order and reports the rerank as
// absent.
type Scored struct {
	Index  int
	Score  float64
	Scored bool
}

// Usage is the model spend one rerank pass cost. The searcher sums it into
// retrieval.Result so search_corpus can attribute it to the run's budget
// (ADR-23); a reranker that calls no model, like Noop, reports the zero
// value and costs the run nothing.
type Usage struct {
	MicroUSD     int64  `json:"micro_usd,omitempty"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	Model        string `json:"model,omitempty"`
}

// Add sums usage. The first non-empty Model wins: a pass batches one query
// across one model, so the name is the same on every term worth keeping.
func (u Usage) Add(o Usage) Usage {
	sum := Usage{
		MicroUSD:     u.MicroUSD + o.MicroUSD,
		InputTokens:  u.InputTokens + o.InputTokens,
		OutputTokens: u.OutputTokens + o.OutputTokens,
		Model:        u.Model,
	}
	if sum.Model == "" {
		sum.Model = o.Model
	}
	return sum
}

// IsZero reports whether the pass spent nothing — no reranker, or one whose
// every call failed before the provider billed for it.
func (u Usage) IsZero() bool {
	return u.MicroUSD == 0 && u.InputTokens == 0 && u.OutputTokens == 0
}

// Reranker rescores candidates for a query, returning every candidate,
// highest score first, ties in input order, and what the pass spent.
// Implementations must degrade, not fail: a reranker outage should cost
// recall, never the run. Usage is reported even alongside an error, because
// tokens burned on the way to a failure were still billed.
type Reranker interface {
	Rerank(ctx context.Context, query string, cands []Candidate) ([]Scored, Usage, error)
}

// Noop returns the candidates unscored, preserving their order. It is the
// -rerank=off wiring.
type Noop struct{}

// Rerank implements Reranker.
func (Noop) Rerank(_ context.Context, _ string, cands []Candidate) ([]Scored, Usage, error) {
	out := make([]Scored, len(cands))
	for i, c := range cands {
		out[i] = Scored{Index: c.Index}
	}
	return out, Usage{}, nil
}
