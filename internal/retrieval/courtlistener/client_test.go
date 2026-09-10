package courtlistener

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/retrieval"
)

// fakeAPI serves two search pages (cursor pagination) and opinions. Cluster
// ids 1..n map to opinion ids 100+n; opinion 103 is a dissent-only cluster,
// opinion 104 has empty text.
type fakeAPI struct {
	t          *testing.T
	rateLimit  atomic.Int32 // requests to answer 429 before succeeding
	searchHits atomic.Int32
}

func (f *fakeAPI) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		require.Equal(f.t, "Token test-token", r.Header.Get("Authorization"))
		if f.rateLimit.Load() > 0 {
			f.rateLimit.Add(-1)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		switch {
		case r.URL.Path == "/search/":
			f.searchHits.Add(1)
			f.search(w, r)
		case r.URL.Path == "/opinions/101/":
			f.opinion(w, r, 101, "010combined", "<p>Opinion one text about qualified immunity.</p>", "")
		case r.URL.Path == "/opinions/102/":
			f.opinion(w, r, 102, "020lead", "", "Plain text of opinion two about search warrants.")
		case r.URL.Path == "/opinions/103/":
			f.opinion(w, r, 103, "040dissent", "<p>A dissent, not a lead opinion.</p>", "")
		case r.URL.Path == "/opinions/104/":
			f.opinion(w, r, 104, "010combined", "", "")
		default:
			f.t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}
}

func (f *fakeAPI) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("cursor") == "" {
		require.Equal(f.t, "o", q.Get("type"))
		require.Equal(f.t, "scotus", q.Get("court"))
		require.Equal(f.t, "2010-01-01", q.Get("filed_after"))
		// Page 1: clusters 1 (lead) and 3 (dissent only).
		fmt.Fprintf(w, `{"next":"http://%s/search/?cursor=p2&type=o&court=scotus","results":[
			{"cluster_id":1,"caseName":"Case One","dateFiled":"2015-01-02","docketNumber":"14-1","citation":["555 U.S. 1"],"absolute_url":"/opinion/1/","opinions":[{"id":101}]},
			{"cluster_id":3,"caseName":"Case Three","dateFiled":"2014-03-04","opinions":[{"id":103}]}
		]}`, r.Host)
		return
	}
	// Page 2: clusters 2 (lead, plain text) and 4 (empty text).
	fmt.Fprint(w, `{"next":null,"results":[
		{"cluster_id":2,"caseName":"Case Two","dateFiled":"2013-05-06","opinions":[{"id":102}]},
		{"cluster_id":4,"caseName":"Case Four","dateFiled":"2012-07-08","opinions":[{"id":104}]}
	]}`)
}

func (f *fakeAPI) opinion(w http.ResponseWriter, r *http.Request, id int, typ, html, plain string) {
	require.Equal(f.t, "id,type,html_with_citations,plain_text", r.URL.Query().Get("fields"), "field selection keeps responses small")
	require.NoError(f.t, json.NewEncoder(w).Encode(map[string]any{
		"id": id, "type": typ, "html_with_citations": html, "plain_text": plain,
	}))
}

func newTestClient(t *testing.T) (*Client, *fakeAPI) {
	api := &fakeAPI{t: t}
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)
	c := New(srv.URL, "test-token", nil)
	c.sleep = func(ctx context.Context, d time.Duration) error { return nil } // no real backoff waits in tests
	return c, api
}

func fetchAll(t *testing.T, c *Client, opt FetchOptions) ([]retrieval.InputDoc, FetchSummary) {
	t.Helper()
	var docs []retrieval.InputDoc
	sum, err := c.Fetch(context.Background(), opt, func(d retrieval.InputDoc) error {
		docs = append(docs, d)
		return nil
	})
	require.NoError(t, err)
	return docs, sum
}

func TestFetchFollowsCursorToEnd(t *testing.T) {
	c, api := newTestClient(t)
	docs, sum := fetchAll(t, c, FetchOptions{Court: "scotus", FiledAfter: "2010-01-01"})

	require.Equal(t, int32(2), api.searchHits.Load(), "both pages fetched")
	require.Len(t, docs, 2)
	require.Equal(t, "clop-101", docs[0].SourceID)
	require.Equal(t, "Case One", docs[0].Title)
	require.Equal(t, "scotus", docs[0].Court)
	require.Equal(t, "2015-01-02", docs[0].DecidedOn)
	require.Contains(t, docs[0].Text, "qualified immunity")
	var meta map[string]any
	require.NoError(t, json.Unmarshal(docs[0].Metadata, &meta))
	require.Equal(t, "14-1", meta["docket_number"])

	require.Equal(t, "clop-102", docs[1].SourceID, "plain_text fallback when html is empty")
	require.Contains(t, docs[1].Text, "search warrants")

	require.Equal(t, 2, sum.Emitted)
	require.Equal(t, 1, sum.SkippedNoLead, "dissent-only cluster skipped")
	require.Equal(t, 1, sum.SkippedEmpty, "empty text skipped and counted")
}

func TestFetchLimitStopsEarly(t *testing.T) {
	c, _ := newTestClient(t)
	docs, sum := fetchAll(t, c, FetchOptions{Court: "scotus", FiledAfter: "2010-01-01", Limit: 1})
	require.Len(t, docs, 1)
	require.Equal(t, 1, sum.Emitted)
}

func TestFetchResumeSkipsExisting(t *testing.T) {
	c, _ := newTestClient(t)
	docs, sum := fetchAll(t, c, FetchOptions{
		Court: "scotus", FiledAfter: "2010-01-01",
		Skip: map[string]bool{"clop-101": true},
	})
	require.Len(t, docs, 1)
	require.Equal(t, "clop-102", docs[0].SourceID)
	require.Equal(t, 1, sum.SkippedExisting)
}

func TestFetchRetriesOn429(t *testing.T) {
	c, api := newTestClient(t)
	api.rateLimit.Store(2)
	docs, _ := fetchAll(t, c, FetchOptions{Court: "scotus", FiledAfter: "2010-01-01"})
	require.Len(t, docs, 2, "429s with Retry-After are retried, not fatal")
}

func TestFetchGivesUpAfterMaxRetries(t *testing.T) {
	c, api := newTestClient(t)
	api.rateLimit.Store(100)
	_, err := c.Fetch(context.Background(), FetchOptions{Court: "scotus"}, func(retrieval.InputDoc) error { return nil })
	require.ErrorContains(t, err, "429")
}
