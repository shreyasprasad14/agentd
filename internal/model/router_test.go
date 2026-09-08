package model

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// stub is a leaf provider that records the model it was asked for.
type stub struct {
	name  string
	price Price
	seen  []string
}

func (s *stub) Name() string { return s.name }
func (s *stub) Complete(_ context.Context, req Request) (*Response, error) {
	s.seen = append(s.seen, req.Model)
	return &Response{Model: req.Model, StopReason: StopEndTurn, Usage: Usage{InputTokens: 1}}, nil
}
func (s *stub) CostMicroUSD(_ string, u Usage) int64 { return s.price.Cost(u) }

func TestRouterResolution(t *testing.T) {
	local := &stub{name: "local"}
	claude := &stub{name: "anthropic", price: Price{InputPerMTok: 1_000_000}}
	r := NewRouter().
		Register("local", local, nil).
		Register("anthropic", claude, func(m string) bool { return strings.HasPrefix(m, "claude-") })

	cases := []struct {
		model, wantBackend, wantModel string
	}{
		{"qwen2.5:7b", "local", "qwen2.5:7b"},
		{"claude-opus-5", "anthropic", "claude-opus-5"},
		{"anthropic/claude-opus-5", "anthropic", "claude-opus-5"},
		{"local/claude-lookalike", "local", "claude-lookalike"},
		{"local/llama3.1:8b", "local", "llama3.1:8b"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			name, p, m, err := r.Resolve(tc.model)
			require.NoError(t, err)
			require.Equal(t, tc.wantBackend, name)
			require.Equal(t, tc.wantBackend, p.Name())
			require.Equal(t, tc.wantModel, m)

			resp, err := r.Complete(context.Background(), Request{Model: tc.model})
			require.NoError(t, err)
			require.Equal(t, tc.wantBackend, resp.Provider, "response is tagged with the real backend")
			require.Equal(t, tc.wantModel, resp.Model, "backend never sees the prefix")
		})
	}
	require.Equal(t, []string{"qwen2.5:7b", "claude-lookalike", "llama3.1:8b"}, local.seen)
	require.Equal(t, []string{"claude-opus-5", "claude-opus-5"}, claude.seen)

	// Pricing follows the same rule as completion.
	require.Equal(t, int64(1), r.CostMicroUSD("claude-opus-5", Usage{InputTokens: 1}))
	require.Equal(t, int64(1), r.CostMicroUSD("anthropic/claude-opus-5", Usage{InputTokens: 1}))
	require.Equal(t, int64(0), r.CostMicroUSD("qwen2.5:7b", Usage{InputTokens: 1}))
	require.Equal(t, int64(0), r.CostMicroUSD("nope/x", Usage{InputTokens: 1}))
	require.Equal(t, []string{"anthropic", "local"}, r.Backends())
}

func TestRouterErrors(t *testing.T) {
	_, err := NewRouter().Complete(context.Background(), Request{Model: "m"})
	require.ErrorContains(t, err, "no backends")

	r := NewRouter().Register("local", &stub{name: "local"}, nil)
	_, err = r.Complete(context.Background(), Request{Model: "anthropic/claude-opus-5"})
	require.ErrorContains(t, err, `no provider "anthropic"`)
	_, err = r.Complete(context.Background(), Request{Model: "local/"})
	require.ErrorContains(t, err, "empty model name")

	require.Panics(t, func() { r.Register("local", &stub{}, nil) })
	require.Panics(t, func() { r.SetDefault("missing") })
}

func TestRouterDefault(t *testing.T) {
	a, b := &stub{name: "a"}, &stub{name: "b"}
	r := NewRouter().Register("a", a, nil).Register("b", b, nil)
	name, _, _, err := r.Resolve("anything")
	require.NoError(t, err)
	require.Equal(t, "a", name, "first registered is the default")
	r.SetDefault("b")
	name, _, _, err = r.Resolve("anything")
	require.NoError(t, err)
	require.Equal(t, "b", name)
}
