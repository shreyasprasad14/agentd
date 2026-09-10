package corpus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/retrieval"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/tools"
)

// fakeSearcher scripts retrieval results.
type fakeSearcher struct {
	got  retrieval.Query
	res  *retrieval.Result
	err  error
	mode retrieval.Mode
}

func (f *fakeSearcher) Search(_ context.Context, q retrieval.Query) (*retrieval.Result, error) {
	f.got = q
	return f.res, f.err
}
func (f *fakeSearcher) BestMode() retrieval.Mode { return f.mode }

// fakeStore scripts document lookups.
type fakeStore struct {
	doc    *store.Document
	chunks []store.Chunk
}

func (f *fakeStore) GetDocument(_ context.Context, id uuid.UUID) (*store.Document, error) {
	if f.doc == nil || f.doc.ID != id {
		return nil, store.ErrNotFound
	}
	return f.doc, nil
}
func (f *fakeStore) GetDocumentBySourceID(_ context.Context, sid string) (*store.Document, error) {
	if f.doc == nil || f.doc.SourceID != sid {
		return nil, store.ErrNotFound
	}
	return f.doc, nil
}
func (f *fakeStore) ListChunks(_ context.Context, _ uuid.UUID, from, to int) ([]store.Chunk, error) {
	var out []store.Chunk
	for _, c := range f.chunks {
		if c.Ordinal >= from && (to < 0 || c.Ordinal <= to) {
			out = append(out, c)
		}
	}
	return out, nil
}

// resolve registers the tool and runs args through the registry's schema
// validation, exactly as the loop does.
func resolve(t *testing.T, tool tools.Tool, args string) error {
	t.Helper()
	reg := tools.NewRegistry()
	require.NoError(t, reg.Register(tool))
	_, err := reg.Resolve(tool.Name(), []string{tool.Name()}, json.RawMessage(args))
	return err
}

func TestSearchSchemaBounds(t *testing.T) {
	tool := NewSearch(&fakeSearcher{})
	require.NoError(t, resolve(t, tool, `{"query":"qualified immunity"}`))
	require.NoError(t, resolve(t, tool, `{"query":"immunity","k":20,"court":"scotus","date_from":"2010-01-01","date_to":"2020-12-31"}`))
	for name, args := range map[string]string{
		"query too short": `{"query":"ab"}`,
		"query missing":   `{}`,
		"k zero":          `{"query":"immunity","k":0}`,
		"k over cap":      `{"query":"immunity","k":21}`,
		"bad date":        `{"query":"immunity","date_from":"June 2010"}`,
		"unknown field":   `{"query":"immunity","mode":"vector"}`,
	} {
		require.Error(t, resolve(t, tool, args), name)
	}
}

func TestSearchNilDepsUnavailable(t *testing.T) {
	_, err := NewSearch(nil).Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(`{"query":"immunity"}`)})
	require.ErrorIs(t, err, ErrUnavailable)
}

func hits(n int, contentLen int) []retrieval.Hit {
	d := time.Date(2015, 6, 1, 0, 0, 0, 0, time.UTC)
	out := make([]retrieval.Hit, n)
	for i := range out {
		out[i] = retrieval.Hit{
			ChunkID:    uuid.New(),
			DocumentID: uuid.New(),
			SourceID:   fmt.Sprintf("clop-%d", i),
			Title:      "Case",
			Court:      "scotus",
			DecidedOn:  &d,
			Section:    "II. Analysis",
			Ordinal:    i,
			Content:    strings.Repeat("x", contentLen),
			Scores:     retrieval.Scores{RRF: 0.03, Rerank: float64(9 - i), Reranked: true},
		}
	}
	return out
}

func TestSearchResultShape(t *testing.T) {
	fs := &fakeSearcher{res: &retrieval.Result{Mode: retrieval.ModeHybridRerank, CandidatesConsidered: 42, Hits: hits(2, 50)}}
	tool := NewSearch(fs)
	res, err := tool.Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(`{"query":"qualified immunity","k":2,"court":"scotus","date_from":"2010-01-01"}`)})
	require.NoError(t, err)

	require.Equal(t, "qualified immunity", fs.got.Text)
	require.Equal(t, 2, fs.got.K)
	require.Equal(t, "scotus", fs.got.Court)
	require.NotNil(t, fs.got.From)

	var out SearchResult
	require.NoError(t, json.Unmarshal(res.Content, &out))
	require.Equal(t, "hybrid+rerank", out.Mode)
	require.Equal(t, 42, out.CandidatesConsidered)
	require.Len(t, out.Hits, 2)
	require.Equal(t, "clop-0", out.Hits[0].SourceID)
	require.Equal(t, "2015-06-01", out.Hits[0].DecidedOn)
	require.Equal(t, "II. Analysis", out.Hits[0].Section)
	require.Equal(t, 9.0, out.Hits[0].Score, "reranked hits report the rerank score")
	require.False(t, out.Truncated)
}

func TestSearchResultCapped(t *testing.T) {
	fs := &fakeSearcher{res: &retrieval.Result{Mode: retrieval.ModeHybrid, Hits: hits(20, 2000)}}
	res, err := NewSearch(fs).Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(`{"query":"immunity","k":20}`)})
	require.NoError(t, err)
	require.LessOrEqual(t, len(res.Content), 24<<10)
	var out SearchResult
	require.NoError(t, json.Unmarshal(res.Content, &out))
	require.True(t, out.Truncated)
	require.NotEmpty(t, out.Hits)
	require.Less(t, len(out.Hits), 20)
}

func TestSearchSingleOversizeHitIsTrimmedNotDropped(t *testing.T) {
	h := hits(1, 40000)
	// Multibyte content, so a naive byte cut would split a rune.
	h[0].Content = strings.Repeat("§¶é ", 10000)
	fs := &fakeSearcher{res: &retrieval.Result{Mode: retrieval.ModeHybrid, Hits: h}}
	res, err := NewSearch(fs).Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(`{"query":"immunity"}`)})
	require.NoError(t, err)
	require.LessOrEqual(t, len(res.Content), 24<<10)
	require.True(t, utf8.Valid(res.Content), "trimming must not split a rune")

	var out SearchResult
	require.NoError(t, json.Unmarshal(res.Content, &out))
	require.Len(t, out.Hits, 1, "the only hit is trimmed, not dropped")
	require.True(t, out.Truncated)
	require.NotEmpty(t, out.Hits[0].Content)
	require.Equal(t, "clop-0", out.Hits[0].SourceID, "metadata survives the trim")
}

func TestFetchSchemaBoundsAndExactlyOneID(t *testing.T) {
	docID := uuid.New()
	fs := &fakeStore{doc: &store.Document{ID: docID, SourceID: "clop-1", Title: "Case", Metadata: json.RawMessage(`{}`)}}
	tool := NewFetch(fs)

	require.NoError(t, resolve(t, tool, `{"source_id":"clop-1"}`))
	require.Error(t, resolve(t, tool, `{"max_chars":10,"source_id":"clop-1"}`), "max_chars below minimum")
	require.Error(t, resolve(t, tool, `{"source_id":"clop-1","extra":true}`))

	_, err := tool.Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(`{}`)})
	require.ErrorContains(t, err, "exactly one")
	_, err = tool.Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(fmt.Sprintf(`{"document_id":%q,"source_id":"clop-1"}`, docID))})
	require.ErrorContains(t, err, "exactly one")
}

func TestFetchNilDepsUnavailable(t *testing.T) {
	_, err := NewFetch(nil).Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(`{"source_id":"clop-1"}`)})
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestFetchByEitherIDAndRange(t *testing.T) {
	docID := uuid.New()
	d := time.Date(1966, 6, 13, 0, 0, 0, 0, time.UTC)
	fs := &fakeStore{
		doc: &store.Document{ID: docID, SourceID: "clop-1", Title: "Miranda", Court: "scotus", DecidedOn: &d,
			Metadata: json.RawMessage(`{"citation":"384 U.S. 436"}`), ChunkCount: 3},
		chunks: []store.Chunk{
			{Ordinal: 0, Section: "SYLLABUS", Content: "chunk zero"},
			{Ordinal: 1, Section: "I. Background", Content: "chunk one"},
			{Ordinal: 2, Section: "II. Holding", Content: "chunk two"},
		},
	}
	tool := NewFetch(fs)

	res, err := tool.Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(`{"source_id":"clop-1"}`)})
	require.NoError(t, err)
	var out FetchResult
	require.NoError(t, json.Unmarshal(res.Content, &out))
	require.Equal(t, "Miranda", out.Document.Title)
	require.Equal(t, "1966-06-13", out.Document.DecidedOn)
	require.Equal(t, 3, out.Document.ChunkCount)
	require.Len(t, out.Chunks, 3)

	res, err = tool.Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(fmt.Sprintf(`{"document_id":%q,"ordinal_from":1,"ordinal_to":1}`, docID))})
	require.NoError(t, err)
	out = FetchResult{}
	require.NoError(t, json.Unmarshal(res.Content, &out))
	require.Len(t, out.Chunks, 1)
	require.Equal(t, 1, out.Chunks[0].Ordinal)
	require.Equal(t, "chunk one", out.Chunks[0].Content)

	_, err = tool.Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(`{"source_id":"clop-404"}`)})
	require.ErrorContains(t, err, "no such document")

	_, err = tool.Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(`{"source_id":"clop-1","ordinal_from":2,"ordinal_to":1}`)})
	require.ErrorContains(t, err, "before")
}

func TestFetchMaxCharsTruncates(t *testing.T) {
	docID := uuid.New()
	fs := &fakeStore{
		doc: &store.Document{ID: docID, SourceID: "clop-1", Metadata: json.RawMessage(`{}`)},
		chunks: []store.Chunk{
			{Ordinal: 0, Content: strings.Repeat("a", 900)},
			{Ordinal: 1, Content: strings.Repeat("b", 900)},
			{Ordinal: 2, Content: strings.Repeat("c", 900)},
		},
	}
	res, err := NewFetch(fs).Invoke(context.Background(), tools.Invocation{Args: json.RawMessage(`{"source_id":"clop-1","max_chars":2000}`)})
	require.NoError(t, err)
	var out FetchResult
	require.NoError(t, json.Unmarshal(res.Content, &out))
	require.Len(t, out.Chunks, 2, "the third chunk would exceed max_chars")
	require.True(t, out.Truncated)
}
