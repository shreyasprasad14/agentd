package embed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// server fakes /v1/embeddings: vector i of a request is [seq, idx, 0, 0]
// where seq counts requests, so reassembly across batches is checkable.
func server(t *testing.T, dim int, status int) (*Client, *[][]string) {
	t.Helper()
	var mu sync.Mutex
	var batches [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/embeddings", r.URL.Path)
		var req wireRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.Equal(t, "test-model", req.Model)
		mu.Lock()
		batches = append(batches, req.Input)
		mu.Unlock()
		if status != 200 {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"message":"boom"}}`)
			return
		}
		// Return entries in reversed order with the true index on each, so
		// only a client that honours the index field reassembles correctly.
		var wr wireResponse
		for i := len(req.Input) - 1; i >= 0; i-- {
			vec := make([]float32, dim)
			vec[0] = float32(i)
			wr.Data = append(wr.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: i, Embedding: vec})
		}
		require.NoError(t, json.NewEncoder(w).Encode(wr))
	}))
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL + "/v1", Model: "test-model", Dimensions: dim})
	return c, &batches
}

func TestEmbedBatchesAndReassembles(t *testing.T) {
	c, batches := server(t, 4, 200)
	texts := make([]string, 70) // 3 batches of 32, 32, 6
	for i := range texts {
		texts[i] = fmt.Sprintf("text-%d", i)
	}
	vecs, err := c.Embed(context.Background(), texts)
	require.NoError(t, err)
	require.Len(t, vecs, 70)
	require.Len(t, *batches, 3)
	require.Len(t, (*batches)[0], 32)
	require.Len(t, (*batches)[2], 6)
	// The server returns vectors in reversed index order; reassembly must
	// restore input order: vector i within a batch encodes its index.
	require.Equal(t, float32(0), vecs[0][0])
	require.Equal(t, float32(5), vecs[64+5][0])
}

func TestEmbedWrongWidthIsError(t *testing.T) {
	c, _ := server(t, 4, 200)
	c.cfg.Dimensions = 8
	_, err := c.Embed(context.Background(), []string{"x"})
	require.ErrorContains(t, err, "dimensions")
}

func TestEmbedHTTPErrorIncludesBody(t *testing.T) {
	c, _ := server(t, 4, 500)
	_, err := c.Embed(context.Background(), []string{"x"})
	require.ErrorContains(t, err, "HTTP 500")
	require.ErrorContains(t, err, "boom")
}

func TestEmbedEmptyInput(t *testing.T) {
	c, batches := server(t, 4, 200)
	vecs, err := c.Embed(context.Background(), nil)
	require.NoError(t, err)
	require.Nil(t, vecs)
	require.Empty(t, *batches)
}

func TestProbe(t *testing.T) {
	c, _ := server(t, 4, 200)
	require.NoError(t, Probe(context.Background(), c))
	bad, _ := server(t, 4, 200)
	bad.cfg.Dimensions = 16
	require.Error(t, Probe(context.Background(), bad))
}

func TestFakeIsDeterministicAndNormalised(t *testing.T) {
	f := &Fake{Dim: 64}
	a, err := f.Embed(context.Background(), []string{"qualified immunity doctrine", "qualified immunity doctrine", "cell site records"})
	require.NoError(t, err)
	require.Equal(t, a[0], a[1], "same text, same vector")
	require.NotEqual(t, a[0], a[2])

	dot := func(x, y []float32) float64 {
		var s float64
		for i := range x {
			s += float64(x[i]) * float64(y[i])
		}
		return s
	}
	require.InDelta(t, 1.0, dot(a[0], a[0]), 1e-5, "vectors are unit length")
	// Shared words make texts nearer than disjoint ones.
	b, err := f.Embed(context.Background(), []string{"qualified immunity shields officials"})
	require.NoError(t, err)
	require.Greater(t, dot(a[0], b[0]), dot(a[2], b[0]))
}
