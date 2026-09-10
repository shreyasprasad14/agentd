package retrieval

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/shreyasprasad/agentd/internal/retrieval/embed"
	"github.com/shreyasprasad/agentd/internal/retrieval/rerank"
	"github.com/shreyasprasad/agentd/internal/store"
)

// Mode selects which retrieval steps run. The tool always uses the best
// available; the eval runs all four so the README table isolates each step's
// contribution.
type Mode string

const (
	ModeVector       Mode = "vector"
	ModeLexical      Mode = "bm25"
	ModeHybrid       Mode = "hybrid"
	ModeHybridRerank Mode = "hybrid+rerank"
)

// Query is one retrieval request.
type Query struct {
	Text string
	// K is how many hits to return; default 8.
	K    int
	Mode Mode
	// Optional filters.
	Court    string
	From, To *time.Time
}

// Scores says where a hit came from. Ranks are 1-based, 0 when the hit was
// absent from that list. Reranked is false when the reranker was off or
// degraded, in which case Rerank is meaningless.
type Scores struct {
	VectorRank  int     `json:"vector_rank,omitempty"`
	LexicalRank int     `json:"lexical_rank,omitempty"`
	RRF         float64 `json:"rrf,omitempty"`
	Rerank      float64 `json:"rerank,omitempty"`
	Reranked    bool    `json:"reranked,omitempty"`
}

// Hit is one returned chunk with everything a citation needs.
type Hit struct {
	ChunkID    uuid.UUID  `json:"chunk_id"`
	DocumentID uuid.UUID  `json:"document_id"`
	SourceID   string     `json:"source_id"`
	Title      string     `json:"title"`
	Court      string     `json:"court,omitempty"`
	DecidedOn  *time.Time `json:"decided_on,omitempty"`
	Section    string     `json:"section,omitempty"`
	Ordinal    int        `json:"ordinal"`
	Content    string     `json:"content"`
	Scores     Scores     `json:"scores"`
}

// Result is one search's outcome. Mode is what actually ran: a degraded
// rerank reports ModeHybrid so the caller (and the model) knows the order is
// the fused one.
type Result struct {
	Mode                 Mode  `json:"mode"`
	CandidatesConsidered int   `json:"candidates_considered"`
	Hits                 []Hit `json:"hits"`
}

// Searcher runs retrieval. Store and Embedder are required for vector modes;
// Reranker may be nil (hybrid only).
type Searcher struct {
	Store    *store.Store
	Embedder embed.Embedder
	Reranker rerank.Reranker
	// Candidates is the depth of each indexed search (default 50).
	Candidates int
	// RerankCandidates caps how many fused hits are reranked (default 50);
	// the reranker is the slow step on a laptop.
	RerankCandidates int
	Log              *slog.Logger
}

// BestMode is what the search_corpus tool runs: hybrid+rerank when a
// reranker is wired, hybrid otherwise.
func (s *Searcher) BestMode() Mode {
	if s.Reranker != nil {
		return ModeHybridRerank
	}
	return ModeHybrid
}

// Search runs one query. Vector and lexical searches run concurrently; RRF
// and rerank happen in Go (ADR-18).
func (s *Searcher) Search(ctx context.Context, q Query) (*Result, error) {
	if q.Text == "" {
		return nil, fmt.Errorf("empty query")
	}
	if q.K <= 0 {
		q.K = 8
	}
	mode := q.Mode
	if mode == "" {
		mode = s.BestMode()
	}
	depth := s.Candidates
	if depth <= 0 {
		depth = 50
	}
	filter := store.CorpusFilter{Court: q.Court, From: q.From, To: q.To}

	var vector, lexical []store.SearchHit
	var vecErr, lexErr error
	var wg sync.WaitGroup

	if mode != ModeLexical {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The query prefix is applied here, not by callers, so the tool
			// and the eval embed queries identically.
			vecs, err := s.Embedder.Embed(ctx, []string{embed.QueryPrefix + q.Text})
			if err != nil {
				vecErr = fmt.Errorf("embed query: %w", err)
				return
			}
			vector, vecErr = s.Store.SearchVector(ctx, vecs[0], depth, filter)
		}()
	}
	if mode != ModeVector {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lexical, lexErr = s.Store.SearchLexical(ctx, q.Text, depth, filter)
		}()
	}
	wg.Wait()
	if vecErr != nil {
		return nil, vecErr
	}
	if lexErr != nil {
		return nil, lexErr
	}

	fusedHits := fuse(vector, lexical, rrfK)
	res := &Result{Mode: mode, CandidatesConsidered: len(fusedHits)}

	if mode == ModeHybridRerank {
		fusedHits, res.Mode = s.rerankFused(ctx, q.Text, fusedHits)
	}
	for i, f := range fusedHits {
		if i >= q.K {
			break
		}
		res.Hits = append(res.Hits, toHit(f))
	}
	return res, nil
}

// rerankFused reranks the top candidates and reports which mode actually
// happened: a nil reranker or a fully degraded pass falls back to the fused
// order and says ModeHybrid.
func (s *Searcher) rerankFused(ctx context.Context, query string, fusedHits []fused) ([]fused, Mode) {
	if s.Reranker == nil || len(fusedHits) == 0 {
		return fusedHits, ModeHybrid
	}
	limit := s.RerankCandidates
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, len(fusedHits))

	cands := make([]rerank.Candidate, limit)
	for i := 0; i < limit; i++ {
		cands[i] = rerank.Candidate{Index: i, Content: fusedHits[i].hit.Content}
	}
	scored, err := s.Reranker.Rerank(ctx, query, cands)
	if err != nil || len(scored) != limit {
		if s.Log != nil {
			s.Log.Warn("rerank failed, returning fused order", "error", err)
		}
		return fusedHits, ModeHybrid
	}

	any := false
	out := make([]fused, 0, len(fusedHits))
	for _, sc := range scored {
		f := fusedHits[sc.Index]
		if sc.Scored {
			any = true
			f.rerankScore = sc.Score
			f.reranked = true
		}
		out = append(out, f)
	}
	if !any {
		return fusedHits, ModeHybrid
	}
	// Sort here rather than trusting the implementation's order; the stable
	// sort keeps the fused order among ties (and for unscored candidates).
	sort.SliceStable(out, func(i, j int) bool { return out[i].rerankScore > out[j].rerankScore })
	out = append(out, fusedHits[limit:]...)
	return out, ModeHybridRerank
}

func toHit(f fused) Hit {
	return Hit{
		ChunkID:    f.hit.ChunkID,
		DocumentID: f.hit.DocumentID,
		SourceID:   f.hit.SourceID,
		Title:      f.hit.Title,
		Court:      f.hit.Court,
		DecidedOn:  f.hit.DecidedOn,
		Section:    f.hit.Section,
		Ordinal:    f.hit.Ordinal,
		Content:    f.hit.Content,
		Scores: Scores{
			VectorRank:  f.vectorRank,
			LexicalRank: f.lexicalRank,
			RRF:         f.rrf,
			Rerank:      f.rerankScore,
			Reranked:    f.reranked,
		},
	}
}
