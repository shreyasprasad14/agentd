// Package embed is the embedding seam for retrieval: an interface, an
// OpenAI-compatible /v1/embeddings client (Ollama serves it for
// mxbai-embed-large and bge-m3), and a deterministic fake for tests.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Embedder turns texts into vectors. Implementations must be safe for
// concurrent use.
type Embedder interface {
	// Embed returns one vector per input text, in input order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Model names the embedding model, recorded on every chunk row.
	Model() string
	// Dimensions is the vector width the model produces.
	Dimensions() int
}

// QueryPrefix is mxbai-embed-large's recommended prompt for search queries.
// Documents are embedded bare; the searcher applies this to queries, so the
// tool and the eval cannot disagree about it.
const QueryPrefix = "Represent this sentence for searching relevant passages: "

// batchSize bounds one /v1/embeddings request. Ollama accepts large arrays
// but serialises the work; small batches keep failures cheap to retry.
const batchSize = 32

// Config configures the HTTP client.
type Config struct {
	// BaseURL is the API root, e.g. http://localhost:11434/v1.
	BaseURL string
	// Model is the embedding model id, e.g. mxbai-embed-large.
	Model string
	// Dimensions is the expected vector width; responses of any other width
	// are an error. Defaults to 1024, the schema's VECTOR(1024).
	Dimensions int
	// Timeout bounds one batch. Defaults to two minutes (a cold model load).
	Timeout time.Duration
	// HTTPClient overrides the client (tests).
	HTTPClient *http.Client
}

// Client calls an OpenAI-compatible embeddings endpoint.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a Client.
func New(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.Dimensions <= 0 {
		cfg.Dimensions = 1024
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{cfg: cfg, http: hc}
}

// Model implements Embedder.
func (c *Client) Model() string { return c.cfg.Model }

// Dimensions implements Embedder.
func (c *Client) Dimensions() int { return c.cfg.Dimensions }

type wireRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type wireResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Embed implements Embedder, splitting texts into batches and reassembling
// the results in input order.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := min(start+batchSize, len(texts))
		vecs, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		copy(out[start:end], vecs)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(wireRequest{Model: c.cfg.Model, Input: texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("embeddings: read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("embeddings: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 512))
	}
	var wr wireResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return nil, fmt.Errorf("embeddings: decode: %w", err)
	}
	if wr.Error != nil {
		return nil, fmt.Errorf("embeddings: %s", wr.Error.Message)
	}
	if len(wr.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: got %d vectors for %d inputs", len(wr.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for _, d := range wr.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("embeddings: index %d out of range", d.Index)
		}
		if len(d.Embedding) != c.cfg.Dimensions {
			return nil, fmt.Errorf("embeddings: model %s returned %d dimensions, want %d",
				c.cfg.Model, len(d.Embedding), c.cfg.Dimensions)
		}
		out[d.Index] = d.Embedding
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("embeddings: no vector for input %d", i)
		}
	}
	return out, nil
}

// Probe embeds one string and checks the width, so a wrong-width or
// unreachable embedder fails at boot with a clear message rather than at the
// first insert.
func Probe(ctx context.Context, e Embedder) error {
	vecs, err := e.Embed(ctx, []string{"probe"})
	if err != nil {
		return err
	}
	if len(vecs) != 1 || len(vecs[0]) != e.Dimensions() {
		return fmt.Errorf("embedder %s returned width %d, want %d", e.Model(), len(vecs[0]), e.Dimensions())
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
