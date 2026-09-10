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

// Reranker rescores candidates for a query, returning every candidate,
// highest score first, ties in input order. Implementations must degrade,
// not fail: a reranker outage should cost recall, never the run.
type Reranker interface {
	Rerank(ctx context.Context, query string, cands []Candidate) ([]Scored, error)
}

// Noop returns the candidates unscored, preserving their order. It is the
// -rerank=off wiring.
type Noop struct{}

// Rerank implements Reranker.
func (Noop) Rerank(_ context.Context, _ string, cands []Candidate) ([]Scored, error) {
	out := make([]Scored, len(cands))
	for i, c := range cands {
		out[i] = Scored{Index: c.Index}
	}
	return out, nil
}
