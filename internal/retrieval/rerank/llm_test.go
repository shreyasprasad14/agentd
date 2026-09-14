package rerank

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
)

// scriptedProvider answers every Complete with a per-request function, and
// records the prompts it saw. usage is what every answered call reports;
// price is micro-USD per input token, so a test can assert on a number it
// can compute by hand.
type scriptedProvider struct {
	mu      sync.Mutex
	prompts []string
	answer  func(prompt string) (string, error)
	usage   model.Usage
	price   int64
}

func (s *scriptedProvider) Name() string { return "scripted" }
func (s *scriptedProvider) CostMicroUSD(_ string, u model.Usage) int64 {
	return s.price * u.InputTokens
}

// MaxOutputTokens satisfies model.Provider; the reranker names its own cap.
func (s *scriptedProvider) MaxOutputTokens(string) int { return 0 }

func (s *scriptedProvider) Complete(_ context.Context, req model.Request) (*model.Response, error) {
	prompt := ""
	for _, m := range req.Messages {
		for _, b := range m.Content {
			prompt += b.Text
		}
	}
	s.mu.Lock()
	s.prompts = append(s.prompts, prompt)
	s.mu.Unlock()
	text, err := s.answer(prompt)
	if err != nil {
		return nil, err
	}
	return &model.Response{
		Content:    []model.ContentBlock{{Type: model.BlockText, Text: text}},
		StopReason: model.StopEndTurn,
		Usage:      s.usage,
	}, nil
}

func cands(n int) []Candidate {
	out := make([]Candidate, n)
	for i := range out {
		out[i] = Candidate{Index: i, Content: fmt.Sprintf("passage number %d", i)}
	}
	return out
}

func TestRerankPromptContainsEveryCandidateOnce(t *testing.T) {
	p := &scriptedProvider{answer: func(string) (string, error) {
		return `{"scores":[{"id":1,"score":5}]}`, nil
	}}
	r := NewLLM(p, "test", nil)
	_, _, err := r.Rerank(context.Background(), "the query", cands(7))
	require.NoError(t, err)
	require.Len(t, p.prompts, 1, "7 candidates fit one batch of 10")
	prompt := p.prompts[0]
	require.Contains(t, prompt, "Query: the query")
	for i := 0; i < 7; i++ {
		require.Equal(t, 1, strings.Count(prompt, fmt.Sprintf("passage number %d\n", i)),
			"candidate %d must appear exactly once", i)
		require.Contains(t, prompt, fmt.Sprintf("[%d]", i+1), "stable 1-based ids")
	}
}

func TestRerankParsesNoisyOutputAndSorts(t *testing.T) {
	p := &scriptedProvider{answer: func(string) (string, error) {
		return "Sure! Here are the scores:\n```json\n{\"scores\":[{\"id\":1,\"score\":2},{\"id\":2,\"score\":9},{\"id\":3,\"score\":5}]}\n```", nil
	}}
	r := NewLLM(p, "test", nil)
	out, _, err := r.Rerank(context.Background(), "q", cands(3))
	require.NoError(t, err)
	require.Equal(t, []int{1, 2, 0}, []int{out[0].Index, out[1].Index, out[2].Index})
	require.Equal(t, 9.0, out[0].Score)
	require.True(t, out[0].Scored)
}

func TestRerankMissingIDsScoreZero(t *testing.T) {
	p := &scriptedProvider{answer: func(string) (string, error) {
		return `{"scores":[{"id":2,"score":8}]}`, nil
	}}
	r := NewLLM(p, "test", nil)
	out, _, err := r.Rerank(context.Background(), "q", cands(3))
	require.NoError(t, err)
	require.Equal(t, 1, out[0].Index)
	// The unmentioned candidates keep their relative order at score 0.
	require.Equal(t, 0, out[1].Index)
	require.Equal(t, 2, out[2].Index)
	require.Zero(t, out[1].Score)
	require.True(t, out[1].Scored, "a parsed batch scores every candidate, mentioned or not")
}

func TestRerankProviderErrorDegradesToInputOrder(t *testing.T) {
	p := &scriptedProvider{answer: func(string) (string, error) {
		return "", fmt.Errorf("model on fire")
	}}
	r := NewLLM(p, "test", nil)
	out, usage, err := r.Rerank(context.Background(), "q", cands(4))
	require.NoError(t, err, "a provider outage must not fail the caller")
	for i, s := range out {
		require.Equal(t, i, s.Index, "input (RRF) order preserved")
		require.False(t, s.Scored)
	}
	require.True(t, usage.IsZero(), "a call that never reached the provider bills nothing")
}

func TestRerankGarbageBatchScoresZero(t *testing.T) {
	p := &scriptedProvider{
		answer: func(string) (string, error) { return "I cannot help with that.", nil },
		usage:  model.Usage{InputTokens: 900, OutputTokens: 20},
		price:  2,
	}
	r := NewLLM(p, "test", nil)
	out, usage, err := r.Rerank(context.Background(), "q", cands(2))
	require.NoError(t, err)
	require.Equal(t, 0, out[0].Index)
	require.False(t, out[0].Scored)
	// The completion happened; the parse is ours, and the provider charged
	// for the tokens either way.
	require.Equal(t, Usage{MicroUSD: 1800, InputTokens: 900, OutputTokens: 20, Model: "test"}, usage)
}

func TestRerankBatchesAndOrderAcrossBatches(t *testing.T) {
	// 25 candidates → 3 batches. Batch containing global candidate 12 gives
	// it a top score; everything else scores 1.
	p := &scriptedProvider{answer: func(prompt string) (string, error) {
		var b strings.Builder
		b.WriteString(`{"scores":[`)
		first := true
		for i := 1; i <= 10; i++ {
			if !strings.Contains(prompt, fmt.Sprintf("[%d]", i)) {
				continue
			}
			if !first {
				b.WriteString(",")
			}
			first = false
			score := 1
			if strings.Contains(prompt, fmt.Sprintf("[%d] passage number 12\n", i)) {
				score = 10
			}
			fmt.Fprintf(&b, `{"id":%d,"score":%d}`, i, score)
		}
		b.WriteString("]}")
		return b.String(), nil
	}}
	r := NewLLM(p, "test", nil)
	out, _, err := r.Rerank(context.Background(), "q", cands(25))
	require.NoError(t, err)
	require.Len(t, p.prompts, 3)
	require.Len(t, out, 25)
	require.Equal(t, 12, out[0].Index, "the top-scored candidate leads regardless of batch")
	require.Equal(t, 10.0, out[0].Score)
}

func TestRerankUsageAccumulatesAcrossBatches(t *testing.T) {
	p := &scriptedProvider{
		answer: func(string) (string, error) { return `{"scores":[{"id":1,"score":5}]}`, nil },
		usage:  model.Usage{InputTokens: 1100, OutputTokens: 50},
		price:  3,
	}
	r := NewLLM(p, "reranker-7b", nil)
	_, usage, err := r.Rerank(context.Background(), "q", cands(25))
	require.NoError(t, err)
	require.Len(t, p.prompts, 3, "three concurrent batches")
	require.Equal(t, Usage{MicroUSD: 9900, InputTokens: 3300, OutputTokens: 150, Model: "reranker-7b"}, usage,
		"every batch is summed, not just the last one to finish")
}

func TestRerankUsageSurvivesPartialDegradation(t *testing.T) {
	// The batch holding candidate 12 never reaches the provider; the other
	// two answer and are billed. A search that degrades is still a search
	// the run paid for.
	p := &scriptedProvider{
		answer: func(prompt string) (string, error) {
			if strings.Contains(prompt, "passage number 12\n") {
				return "", fmt.Errorf("model on fire")
			}
			return `{"scores":[{"id":1,"score":5}]}`, nil
		},
		usage: model.Usage{InputTokens: 1000, OutputTokens: 40},
		price: 1,
	}
	r := NewLLM(p, "test", nil)
	out, usage, err := r.Rerank(context.Background(), "q", cands(25))
	require.NoError(t, err)
	require.Len(t, out, 25)
	require.Equal(t, Usage{MicroUSD: 2000, InputTokens: 2000, OutputTokens: 80, Model: "test"}, usage)
	for _, s := range out {
		if s.Index >= 10 && s.Index < 20 {
			require.False(t, s.Scored, "candidate %d was in the failed batch", s.Index)
		}
	}
}

func TestNoopPreservesOrder(t *testing.T) {
	out, usage, err := Noop{}.Rerank(context.Background(), "q", cands(3))
	require.NoError(t, err)
	for i, s := range out {
		require.Equal(t, i, s.Index)
		require.False(t, s.Scored)
	}
	require.Equal(t, Usage{}, usage, "the off wiring calls no model and charges the run nothing")
}

func TestParseScoresClampsRange(t *testing.T) {
	p := &scriptedProvider{answer: func(string) (string, error) {
		return `{"scores":[{"id":1,"score":99},{"id":2,"score":-3}]}`, nil
	}}
	r := NewLLM(p, "test", nil)
	out, _, err := r.Rerank(context.Background(), "q", cands(2))
	require.NoError(t, err)
	require.Equal(t, 10.0, out[0].Score)
	require.Equal(t, 0.0, out[1].Score)
}
