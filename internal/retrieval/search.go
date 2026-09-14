package retrieval

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/shreyasprasad/agentd/internal/retrieval/embed"
	"github.com/shreyasprasad/agentd/internal/retrieval/rerank"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/telemetry"
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
	// Usage is the model spend this search incurred inside itself — today,
	// the reranker. corpus.Search copies it onto tools.Result.Cost so the
	// budget bounds the run rather than only the loop (ADR-23).
	Usage rerank.Usage `json:"usage,omitzero"`
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
	// Tracer draws retrieval.search and its four stage children. Nil is fine
	// and means no spans.
	Tracer trace.Tracer
	// Metrics counts searches and times their stages — the same four stages
	// the spans draw, so one search can be read in the waterfall and a
	// thousand in the histogram. Nil is fine and records nothing.
	Metrics *telemetry.Metrics
}

// tracer is the configured tracer, or a no-op one so nothing downstream has
// to ask whether tracing is on. The fallback lives here rather than in a
// constructor because a Searcher is built as a struct literal; a no-op tracer
// is a zero-sized value, so resolving one per search allocates nothing.
func (s *Searcher) tracer() trace.Tracer {
	if s.Tracer == nil {
		return noop.NewTracerProvider().Tracer("")
	}
	return s.Tracer
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
//
// The span it draws reports the mode that *ran*, not the one asked for: a
// degraded rerank returns fused order and says hybrid, and the waterfall is
// where that distinction is easiest to miss and most expensive to guess at.
// Its four stage children are what make the reranker's share of search
// latency visible, which M3's README could only assert in prose.
func (s *Searcher) Search(ctx context.Context, q Query) (res *Result, err error) {
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

	tracer := s.tracer()
	started := time.Now()
	ctx, span := tracer.Start(ctx, telemetry.SpanSearch,
		trace.WithAttributes(telemetry.AttrRetrievalK.Int(q.K)))
	// Attributes accrue as the stages finish, so the span is ended once here
	// rather than at each of the ways a search can stop early.
	defer func() { telemetry.End(span, err) }()

	var vector, lexical []store.SearchHit
	var vecErr, lexErr error
	var wg sync.WaitGroup

	// Every stage below starts its span from ctx, the search span's context,
	// including the ones inside a goroutine: the two indexed searches overlap
	// in time, and spans that took their context from each other would draw a
	// sequence that never happened.
	if mode != ModeLexical {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ectx, espan := tracer.Start(ctx, telemetry.SpanEmbed)
			embedStart := time.Now()
			// The query prefix is applied here, not by callers, so the tool
			// and the eval embed queries identically.
			vecs, embedErr := s.Embedder.Embed(ectx, []string{embed.QueryPrefix + q.Text})
			telemetry.End(espan, embedErr)
			s.Metrics.ObserveRetrievalStage(telemetry.StageEmbed, time.Since(embedStart))
			if embedErr != nil {
				vecErr = fmt.Errorf("embed query: %w", embedErr)
				return
			}
			// Embedding is a sibling of the query rather than its parent, so
			// the vector bar times the index alone: which of the two is slow
			// is the question this pair of spans exists to answer.
			vctx, vspan := tracer.Start(ctx, telemetry.SpanVector)
			vectorStart := time.Now()
			vector, vecErr = s.Store.SearchVector(vctx, vecs[0], depth, filter)
			telemetry.End(vspan, vecErr)
			s.Metrics.ObserveRetrievalStage(telemetry.StageVector, time.Since(vectorStart))
		}()
	}
	if mode != ModeVector {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lctx, lspan := tracer.Start(ctx, telemetry.SpanLexical)
			lexicalStart := time.Now()
			lexical, lexErr = s.Store.SearchLexical(lctx, q.Text, depth, filter)
			telemetry.End(lspan, lexErr)
			s.Metrics.ObserveRetrievalStage(telemetry.StageLexical, time.Since(lexicalStart))
		}()
	}
	wg.Wait()
	// Recorded before the error checks: how many hits each list returned is
	// as worth knowing on a search that failed halfway as on one that did not.
	span.SetAttributes(
		telemetry.AttrVectorHits.Int(len(vector)),
		telemetry.AttrLexicalHits.Int(len(lexical)),
	)
	if vecErr != nil {
		return nil, vecErr
	}
	if lexErr != nil {
		return nil, lexErr
	}

	fusedHits := fuse(vector, lexical, rrfK)
	res = &Result{Mode: mode, CandidatesConsidered: len(fusedHits)}

	if mode == ModeHybridRerank {
		rctx, rspan := tracer.Start(ctx, telemetry.SpanRerank)
		rerankStart := time.Now()
		fusedHits, res.Mode, res.Usage = s.rerankFused(rctx, q.Text, fusedHits)
		// Never an error span: a reranker that failed degraded the search
		// instead of breaking it, and the parent's rerank.degraded says so.
		// The usage is the tool-internal model spend the run pays for
		// (ADR-23), so it is on the bar that incurred it.
		if u := res.Usage; u != (rerank.Usage{}) {
			rspan.SetAttributes(
				telemetry.AttrGenAIRequestModel.String(u.Model),
				telemetry.AttrGenAIInputTokens.Int64(u.InputTokens),
				telemetry.AttrGenAIOutputTokens.Int64(u.OutputTokens),
				telemetry.AttrCostMicroUSD.Int64(u.MicroUSD),
			)
		}
		telemetry.End(rspan, nil)
		// Timed even when it degraded: a reranker that gave up after 30
		// seconds cost those 30 seconds, and a histogram that only counted
		// the successes would say the slow path is fast.
		s.Metrics.ObserveRetrievalStage(telemetry.StageRerank, time.Since(rerankStart))
		// Only a search that asked for a rerank can have degraded; a vector
		// search did not degrade, it never reranked, and a false here would
		// make the Jaeger query for degraded searches count it.
		span.SetAttributes(telemetry.AttrRerankDegraded.Bool(res.Mode != ModeHybridRerank))
	}
	span.SetAttributes(
		telemetry.AttrRetrievalMode.String(string(res.Mode)),
		telemetry.AttrCandidates.Int(res.CandidatesConsidered),
	)
	// Counted here, at the end, so the counter means completed searches: a
	// search that failed at an index never produced a mode, and the tool
	// reports it as a tool_failed rather than as a search of unknown shape.
	// The mode is the one that ran, matching the span and the result.
	s.Metrics.RetrievalSearch(string(res.Mode), mode == ModeHybridRerank && res.Mode != ModeHybridRerank)
	s.Metrics.ObserveRetrievalStage(telemetry.StageSearch, time.Since(started))
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
// order and says ModeHybrid. Every return carries the reranker's usage,
// degraded ones included — falling back to the fused order does not refund
// the tokens spent discovering that the reranker was no help.
func (s *Searcher) rerankFused(ctx context.Context, query string, fusedHits []fused) ([]fused, Mode, rerank.Usage) {
	if s.Reranker == nil || len(fusedHits) == 0 {
		return fusedHits, ModeHybrid, rerank.Usage{}
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
	scored, usage, err := s.Reranker.Rerank(ctx, query, cands)
	if err != nil || len(scored) != limit {
		if s.Log != nil {
			s.Log.Warn("rerank failed, returning fused order", "error", err)
		}
		return fusedHits, ModeHybrid, usage
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
		return fusedHits, ModeHybrid, usage
	}
	// Sort here rather than trusting the implementation's order; the stable
	// sort keeps the fused order among ties (and for unscored candidates).
	sort.SliceStable(out, func(i, j int) bool { return out[i].rerankScore > out[j].rerankScore })
	out = append(out, fusedHits[limit:]...)
	return out, ModeHybridRerank, usage
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
