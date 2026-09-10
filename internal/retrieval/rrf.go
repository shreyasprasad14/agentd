// Package retrieval turns a query into ranked corpus chunks: embed, two
// indexed searches, Reciprocal Rank Fusion, and an optional rerank. The four
// eval modes are this same code with steps skipped, so the numbers in the
// README measure exactly what the search_corpus tool does.
package retrieval

import (
	"sort"

	"github.com/google/uuid"

	"github.com/shreyasprasad/agentd/internal/store"
)

// rrfK is the standard RRF constant: score = Σ 1/(k + rank) over the lists a
// chunk appears in. 60 is the value from the original paper and what
// pgvector's hybrid-search examples use.
const rrfK = 60

// fused is one chunk after fusion, carrying where it ranked in each list.
// rerankScore and reranked are filled in by the searcher's rerank step.
type fused struct {
	hit         store.SearchHit
	vectorRank  int // 1-based; 0 when absent from the list
	lexicalRank int
	rrf         float64
	rerankScore float64
	reranked    bool
}

// fuse merges the two ranked lists with Reciprocal Rank Fusion. Ties are
// broken by vector rank (absent ranks last), then chunk id, so the order is
// deterministic across runs.
func fuse(vector, lexical []store.SearchHit, k int) []fused {
	if k <= 0 {
		k = rrfK
	}
	byID := map[uuid.UUID]*fused{}
	order := []uuid.UUID{}
	add := func(hits []store.SearchHit, set func(*fused, int)) {
		for i, h := range hits {
			f, ok := byID[h.ChunkID]
			if !ok {
				f = &fused{hit: h}
				byID[h.ChunkID] = f
				order = append(order, h.ChunkID)
			}
			set(f, i+1)
			f.rrf += 1.0 / float64(k+i+1)
		}
	}
	add(vector, func(f *fused, r int) { f.vectorRank = r })
	add(lexical, func(f *fused, r int) { f.lexicalRank = r })

	out := make([]fused, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].rrf != out[j].rrf {
			return out[i].rrf > out[j].rrf
		}
		vi, vj := out[i].vectorRank, out[j].vectorRank
		if vi == 0 {
			vi = 1 << 30
		}
		if vj == 0 {
			vj = 1 << 30
		}
		if vi != vj {
			return vi < vj
		}
		return out[i].hit.ChunkID.String() < out[j].hit.ChunkID.String()
	})
	return out
}
