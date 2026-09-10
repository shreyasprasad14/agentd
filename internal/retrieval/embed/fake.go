package embed

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// Fake is a deterministic, model-free embedder: each word hashes into a
// bucket of a fixed-width bag-of-words vector, L2-normalised. Texts that
// share words are near under cosine distance, so a test corpus has real
// nearest-neighbour structure without a model.
type Fake struct {
	Dim int
}

// NewFake builds a Fake at the schema's width.
func NewFake() *Fake { return &Fake{Dim: 1024} }

// Model implements Embedder.
func (f *Fake) Model() string { return "fake-bow" }

// Dimensions implements Embedder.
func (f *Fake) Dimensions() int { return f.Dim }

// Embed implements Embedder.
func (f *Fake) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = f.vector(t)
	}
	return out, nil
}

func (f *Fake) vector(text string) []float32 {
	v := make([]float32, f.Dim)
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	for _, w := range words {
		h := fnv.New32a()
		h.Write([]byte(w))
		v[int(h.Sum32())%f.Dim]++
	}
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		v[0] = 1 // an empty text still needs a valid vector
		return v
	}
	n := float32(math.Sqrt(norm))
	for i := range v {
		v[i] /= n
	}
	return v
}
