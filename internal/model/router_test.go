package model

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// stub is a leaf provider that records the model it was asked for.
type stub struct {
	name   string
	price  Price
	maxOut int
	seen   []string
}

func (s *stub) Name() string { return s.name }
func (s *stub) Complete(_ context.Context, req Request) (*Response, error) {
	s.seen = append(s.seen, req.Model)
	return &Response{Model: req.Model, StopReason: StopEndTurn, Usage: Usage{InputTokens: 1}}, nil
}
func (s *stub) CostMicroUSD(_ string, u Usage) int64 { return s.price.Cost(u) }
func (s *stub) MaxOutputTokens(string) int           { return s.maxOut }

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

func TestRouterMaxOutputTokens(t *testing.T) {
	local := &stub{name: "local"}
	claude := &stub{name: "anthropic", maxOut: 16000}
	r := NewRouter().
		Register("local", local, nil).
		Register("anthropic", claude, func(m string) bool { return strings.HasPrefix(m, "claude-") })

	cases := []struct {
		model string
		want  int
	}{
		{"claude-opus-5", 16000},
		{"anthropic/claude-opus-5", 16000},
		{"qwen2.5:7b", 0},
		{"local/claude-lookalike", 0},
		// Unroutable: the same fallback CostMicroUSD takes, since the call
		// this would have bounded is about to fail anyway.
		{"nope/x", 0},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			require.Equal(t, tc.want, r.MaxOutputTokens(tc.model))
		})
	}
	require.Equal(t, 0, NewRouter().MaxOutputTokens("m"), "no backends at all is unroutable too")
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

// TestBackendNamesTheServingProvider covers the labels M4's metrics hang off.
// They are resolved before a call is made so that a failed attempt is filed
// under the same provider and model a successful one would be: a Router's own
// Name is "router", and Response.Provider only exists once there is a
// response.
func TestBackendNamesTheServingProvider(t *testing.T) {
	local, hosted := &stub{name: "local-stub"}, &stub{name: "anthropic-stub"}
	r := NewRouter().
		Register("local", local, nil).
		Register("anthropic", hosted, func(m string) bool { return strings.HasPrefix(m, "claude-") })

	cases := []struct{ model, wantProvider, wantModel string }{
		{"claude-opus-5", "anthropic", "claude-opus-5"},
		{"anthropic/claude-opus-5", "anthropic", "claude-opus-5"},
		{"local/qwen2.5:7b", "local", "qwen2.5:7b"},
		{"qwen2.5:7b", "local", "qwen2.5:7b"},
		// Unroutable: the composite's own name and the name as given, so the
		// metric says which model nobody could serve rather than dropping it.
		{"nope/x", "router", "nope/x"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			provider, model := Backend(r, tc.model)
			require.Equal(t, tc.wantProvider, provider)
			require.Equal(t, tc.wantModel, model)
		})
	}

	// A leaf provider is its own answer, with the model untouched.
	provider, model := Backend(local, "qwen2.5:7b")
	require.Equal(t, "local-stub", provider)
	require.Equal(t, "qwen2.5:7b", model)
}
