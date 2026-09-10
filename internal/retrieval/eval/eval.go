package eval

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shreyasprasad/agentd/internal/retrieval"
)

// Searcher is the slice of retrieval.Searcher the eval needs.
type Searcher interface {
	Search(ctx context.Context, q retrieval.Query) (*retrieval.Result, error)
}

// AllModes is the README table, in row order.
var AllModes = []retrieval.Mode{
	retrieval.ModeVector, retrieval.ModeLexical, retrieval.ModeHybrid, retrieval.ModeHybridRerank,
}

// ModeKey is the threshold-file key for a mode ("hybrid+rerank" is awkward
// in YAML).
func ModeKey(m retrieval.Mode) string {
	return strings.ReplaceAll(string(m), "+", "_")
}

// evalDepth is how deep each search runs: recall@50 needs 50.
const evalDepth = 50

// ModeResult is one table row.
type ModeResult struct {
	Mode       retrieval.Mode `json:"mode"`
	RecallAt8  float64        `json:"recall_at_8"`
	RecallAt20 float64        `json:"recall_at_20"`
	RecallAt50 float64        `json:"recall_at_50"`
	MRR        float64        `json:"mrr"`
	Elapsed    time.Duration  `json:"elapsed_ns"`
}

// Metric returns a row's value by threshold key.
func (m ModeResult) Metric(key string) (float64, bool) {
	switch key {
	case "recall_at_8":
		return m.RecallAt8, true
	case "recall_at_20":
		return m.RecallAt20, true
	case "recall_at_50":
		return m.RecallAt50, true
	case "mrr":
		return m.MRR, true
	}
	return 0, false
}

// Report is a full eval run. Failures lists every missed threshold; a
// non-empty list is a nonzero exit.
type Report struct {
	Cases    int          `json:"cases"`
	Modes    []ModeResult `json:"modes"`
	Failures []string     `json:"failures,omitempty"`
}

// Run evaluates every case in every mode. The searcher must have its
// reranker wired for the hybrid+rerank row to differ from hybrid.
func Run(ctx context.Context, s Searcher, labels *Labels, modes []retrieval.Mode) (*Report, error) {
	if len(modes) == 0 {
		modes = AllModes
	}
	rep := &Report{Cases: len(labels.Cases)}
	for _, mode := range modes {
		row := ModeResult{Mode: mode}
		start := time.Now()
		for _, c := range labels.Cases {
			res, err := s.Search(ctx, retrieval.Query{Text: c.Query, K: evalDepth, Mode: mode})
			if err != nil {
				return nil, fmt.Errorf("mode %s query %q: %w", mode, c.Query, err)
			}
			if mode == retrieval.ModeHybridRerank && res.Mode != retrieval.ModeHybridRerank {
				return nil, fmt.Errorf("query %q: rerank degraded to %s; fix the reranker or evaluate without it", c.Query, res.Mode)
			}
			r8, r20, r50, mrr := scoreCase(c, res.Hits)
			row.RecallAt8 += r8
			row.RecallAt20 += r20
			row.RecallAt50 += r50
			row.MRR += mrr
		}
		n := float64(len(labels.Cases))
		row.RecallAt8 /= n
		row.RecallAt20 /= n
		row.RecallAt50 /= n
		row.MRR /= n
		row.Elapsed = time.Since(start)
		rep.Modes = append(rep.Modes, row)
	}
	rep.Failures = checkThresholds(labels.Thresholds, rep.Modes)
	return rep, nil
}

// scoreCase computes one query's recall@8/20/50 (fraction of labeled items
// found) and reciprocal rank of the first relevant hit.
func scoreCase(c Case, hits []retrieval.Hit) (r8, r20, r50, mrr float64) {
	found := make([]int, len(c.Relevant)) // 1-based rank of first hit per label, 0 = not found
	firstRelevant := 0
	for rank, h := range hits {
		for li, rel := range c.Relevant {
			if rel.Matches(h.SourceID, h.Ordinal) {
				if found[li] == 0 {
					found[li] = rank + 1
				}
				if firstRelevant == 0 {
					firstRelevant = rank + 1
				}
			}
		}
	}
	recallAt := func(k int) float64 {
		n := 0
		for _, r := range found {
			if r > 0 && r <= k {
				n++
			}
		}
		return float64(n) / float64(len(c.Relevant))
	}
	if firstRelevant > 0 {
		mrr = 1 / float64(firstRelevant)
	}
	return recallAt(8), recallAt(20), recallAt(50), mrr
}

func checkThresholds(thresholds map[string]map[string]float64, rows []ModeResult) []string {
	var out []string
	for _, row := range rows {
		for metric, minimum := range thresholds[ModeKey(row.Mode)] {
			got, ok := row.Metric(metric)
			if !ok {
				out = append(out, fmt.Sprintf("%s: unknown metric %q in thresholds", ModeKey(row.Mode), metric))
				continue
			}
			if got < minimum {
				out = append(out, fmt.Sprintf("%s %s = %.3f, below threshold %.3f", ModeKey(row.Mode), metric, got, minimum))
			}
		}
	}
	return out
}

// RenderTable renders the report as the markdown table the README carries.
func RenderTable(rep *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "| mode | recall@8 | recall@20 | recall@50 | MRR |\n|---|---|---|---|---|\n")
	for _, r := range rep.Modes {
		fmt.Fprintf(&b, "| %s | %.3f | %.3f | %.3f | %.3f |\n", r.Mode, r.RecallAt8, r.RecallAt20, r.RecallAt50, r.MRR)
	}
	return b.String()
}
