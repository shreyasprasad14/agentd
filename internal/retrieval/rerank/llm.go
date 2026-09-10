package rerank

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/shreyasprasad/agentd/internal/model"
)

// LLM scores candidates pointwise through a model.Provider: batches of
// candidates, one completion per batch, a 0-10 relevance score per candidate
// as JSON. It reuses the local model the worker already runs, which is why
// there is no cross-encoder runtime here (ADR-19).
type LLM struct {
	provider model.Provider
	model    string
	log      *slog.Logger

	// BatchSize is candidates per completion (default 10); Parallel is
	// batches in flight (default 4).
	BatchSize int
	Parallel  int
	// MaxPassageChars truncates each candidate in the prompt so a batch stays
	// inside a small local context window (default 800).
	MaxPassageChars int
}

// NewLLM builds the reranker.
func NewLLM(p model.Provider, modelName string, log *slog.Logger) *LLM {
	if log == nil {
		log = slog.Default()
	}
	return &LLM{provider: p, model: modelName, log: log, BatchSize: 10, Parallel: 4, MaxPassageChars: 800}
}

const systemPrompt = `You judge how relevant passages from court opinions are to a search query.
Score each numbered passage from 0 (irrelevant) to 10 (directly answers the query).
Reply with only a JSON object of the form {"scores":[{"id":1,"score":7},{"id":2,"score":0}]}, one entry per passage. No other text.`

// Rerank implements Reranker. A batch whose completion fails or does not
// parse scores 0 for every candidate in it and is logged; only context
// cancellation is returned as an error.
func (l *LLM) Rerank(ctx context.Context, query string, cands []Candidate) ([]Scored, error) {
	if len(cands) == 0 {
		return nil, nil
	}
	batchSize := l.BatchSize
	if batchSize <= 0 {
		batchSize = 10
	}
	parallel := l.Parallel
	if parallel <= 0 {
		parallel = 4
	}

	out := make([]Scored, len(cands))
	for i, c := range cands {
		out[i] = Scored{Index: c.Index}
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, parallel)
	for start := 0; start < len(cands); start += batchSize {
		end := min(start+batchSize, len(cands))
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			scores, err := l.scoreBatch(ctx, query, cands[start:end])
			if err != nil {
				l.log.Warn("rerank batch degraded to fused order", "error", err, "batch_start", start)
				return
			}
			for i, s := range scores {
				out[start+i].Score = s
				out[start+i].Scored = true
			}
		}(start, end)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Highest score first; ties (including whole failed batches at 0) keep
	// the caller's fused order because the sort is stable.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// scoreBatch runs one completion and parses one score per candidate, in
// candidate order.
func (l *LLM) scoreBatch(ctx context.Context, query string, cands []Candidate) ([]float64, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Query: %s\n\nPassages:\n", query)
	for i, c := range cands {
		text := c.Content
		if l.MaxPassageChars > 0 && len(text) > l.MaxPassageChars {
			text = text[:l.MaxPassageChars] + "…"
		}
		fmt.Fprintf(&b, "[%d] %s\n\n", i+1, text)
	}

	temp := 0.0
	resp, err := l.provider.Complete(ctx, model.Request{
		Model:       l.model,
		System:      systemPrompt,
		Messages:    []model.Message{{Role: model.RoleUser, Content: []model.ContentBlock{{Type: model.BlockText, Text: b.String()}}}},
		MaxTokens:   256,
		Temperature: &temp,
	})
	if err != nil {
		return nil, err
	}
	parsed, err := parseScores(resp.Text())
	if err != nil {
		return nil, err
	}
	out := make([]float64, len(cands))
	for _, p := range parsed {
		if p.ID >= 1 && p.ID <= len(out) {
			out[p.ID-1] = clamp(p.Score, 0, 10)
		}
	}
	return out, nil
}

type parsedScore struct {
	ID    int     `json:"id"`
	Score float64 `json:"score"`
}

// parseScores finds the first JSON object in text and reads its scores.
// Models wrap JSON in prose and code fences; scanning for a decodable object
// is cheaper than arguing with the model about format.
func parseScores(text string) ([]parsedScore, error) {
	for i := strings.IndexByte(text, '{'); i >= 0 && i < len(text); i = next(text, i) {
		var v struct {
			Scores []parsedScore `json:"scores"`
		}
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		if err := dec.Decode(&v); err == nil && v.Scores != nil {
			return v.Scores, nil
		}
	}
	return nil, fmt.Errorf("no scores object in reranker output %q", truncate(text, 200))
}

func next(text string, i int) int {
	j := strings.IndexByte(text[i+1:], '{')
	if j < 0 {
		return len(text)
	}
	return i + 1 + j
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
