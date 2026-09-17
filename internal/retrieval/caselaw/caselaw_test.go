package caselaw

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/retrieval"
)

// fakeCAP serves a three-volume "us" reporter. Volume 10 (2013) holds a merits
// opinion and a cert denial, volume 3 (2012) a merits opinion and a case whose
// only opinion is a dissent, volume 2 (1999) one merits opinion — which is the
// one a MinYear bound should cut off.
type fakeCAP struct {
	t         *testing.T
	failNext  atomic.Int32 // requests to answer 503 before succeeding
	volHits   atomic.Int32
	longText  string
	shortText string
}

func newFakeCAP(t *testing.T) *fakeCAP {
	return &fakeCAP{
		t:         t,
		longText:  "The question presented is whether the statute reaches this conduct. " + repeat("We hold that it does not, for the reasons that follow. ", 120),
		shortText: "Petition for writ of certiorari denied.",
	}
}

func repeat(s string, n int) string {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		b.WriteString(s)
	}
	return b.String()
}

func (f *fakeCAP) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if f.failNext.Load() > 0 {
			f.failNext.Add(-1)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/us/VolumesMetadata.json":
			// Listed out of order, and numbered so that a string sort would
			// walk them 2, 3, 10 — the reverse of what a numeric sort gives.
			fmt.Fprint(w, `[
				{"volume_number":"3","volume_folder":"3","reporter_slug":"us","start_year":2012,"end_year":2012},
				{"volume_number":"2","volume_folder":"2","reporter_slug":"us","start_year":1999,"end_year":1999},
				{"volume_number":"10","volume_folder":"10","reporter_slug":"us","start_year":2013,"end_year":2013}
			]`)
		case "/us/10.zip":
			f.volHits.Add(1)
			f.writeZip(w, []caseJSON{
				f.mkCase(301, "Alpha v. Beta", "2013-06-17", "No. 12–71.", typeMajority, f.longText),
				f.mkCase(302, "Gamma v. Delta", "2013-06-18", "No. 12–99.", typeMajority, f.shortText),
			})
		case "/us/3.zip":
			f.volHits.Add(1)
			f.writeZip(w, []caseJSON{
				f.mkCase(201, "Epsilon v. Zeta", "2012-03-04", "No. 11–5.", typeMajority, f.longText),
				f.mkCase(202, "Eta v. Theta", "2012-03-05", "No. 11–6.", "dissent", f.longText),
			})
		case "/us/2.zip":
			f.volHits.Add(1)
			f.writeZip(w, []caseJSON{
				f.mkCase(101, "Iota v. Kappa", "1999-01-02", "No. 98–1.", typeMajority, f.longText),
			})
		default:
			f.t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (f *fakeCAP) mkCase(id int, name, date, docket, opType, text string) caseJSON {
	return caseJSON{
		ID:           json.Number(fmt.Sprint(id)),
		Name:         name + ", Petitioner",
		NameAbbrev:   name,
		DecisionDate: date,
		DocketNumber: docket,
		FirstPage:    "1",
		Citations:    []citation{{Type: "official", Cite: fmt.Sprintf("570 U.S. %d", id)}},
		Court:        courtJSON{Name: "Supreme Court of the United States", NameAbbreviation: "U.S."},
		Casebody: casebody{Opinions: []capOpinion{
			{Type: opType, Author: "Justice SCALIA", Text: text},
		}},
	}
}

// writeZip builds a volume archive in the shape CAP publishes: case JSON under
// json/, plus html/ and metadata/ entries the client must read past.
func (f *fakeCAP) writeZip(w http.ResponseWriter, cases []caseJSON) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i, c := range cases {
		wr, err := zw.Create(fmt.Sprintf("json/%04d-01.json", i+1))
		require.NoError(f.t, err)
		require.NoError(f.t, json.NewEncoder(wr).Encode(c))

		hw, err := zw.Create(fmt.Sprintf("html/%04d-01.html", i+1))
		require.NoError(f.t, err)
		fmt.Fprint(hw, "<p>rendered</p>")
	}
	mw, err := zw.Create("metadata/VolumeMetadata.json")
	require.NoError(f.t, err)
	fmt.Fprint(mw, `{"volume_number":"3"}`)
	require.NoError(f.t, zw.Close())
	w.Write(buf.Bytes())
}

func newTestClient(t *testing.T, f *fakeCAP) *Client {
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c := New(srv.URL, discardLogger())
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func collect(t *testing.T, c *Client, opt FetchOptions) ([]retrieval.InputDoc, FetchSummary) {
	t.Helper()
	var docs []retrieval.InputDoc
	sum, err := c.Fetch(context.Background(), opt, func(d retrieval.InputDoc) error {
		docs = append(docs, d)
		return nil
	})
	require.NoError(t, err)
	return docs, sum
}

func TestFetchWalksVolumesNewestFirstAndDropsOrders(t *testing.T) {
	f := newFakeCAP(t)
	c := newTestClient(t, f)

	docs, sum := collect(t, c, FetchOptions{Reporter: "us", Court: "scotus"})

	// Volume 10 is the oldest despite sorting first as a string; volume 3 is
	// the newest despite being listed last.
	require.Equal(t, []string{"cap-301", "cap-201", "cap-101"}, ids(docs))
	require.Equal(t, 3, sum.Emitted)
	require.Equal(t, 3, sum.Volumes)
	require.Equal(t, 1, sum.SkippedShort, "the cert denial is below MinChars")
	require.Equal(t, 1, sum.SkippedNoOpinion, "the dissent-only case has no majority")

	d := docs[0]
	require.Equal(t, "Alpha v. Beta", d.Title)
	require.Equal(t, "scotus", d.Court, "Court is the pull's value, not CAP's per-case name")
	require.Equal(t, "2013-06-17", d.DecidedOn)
	require.Contains(t, d.Text, "The question presented")

	var meta map[string]any
	require.NoError(t, json.Unmarshal(d.Metadata, &meta))
	require.Equal(t, "12-71", meta["docket_number"], "dedupe keys on this; it must be normalized")
	require.Equal(t, "us", meta["reporter"])
	require.Equal(t, "10", meta["volume"])
	require.Equal(t, "Supreme Court of the United States", meta["court_name"])
}

func TestFetchLimitStopsEarly(t *testing.T) {
	f := newFakeCAP(t)
	c := newTestClient(t, f)

	docs, sum := collect(t, c, FetchOptions{Reporter: "us", Court: "scotus", Limit: 1})

	require.Equal(t, []string{"cap-301"}, ids(docs))
	require.Equal(t, 1, sum.Emitted)
	require.Equal(t, int32(1), f.volHits.Load(), "a limit must not download volumes it cannot use")
}

func TestFetchMinYearEndsTheWalk(t *testing.T) {
	f := newFakeCAP(t)
	c := newTestClient(t, f)

	docs, sum := collect(t, c, FetchOptions{Reporter: "us", Court: "scotus", MinYear: 2012})

	require.Equal(t, []string{"cap-301", "cap-201"}, ids(docs))
	require.Equal(t, 2, sum.Volumes, "the 1999 volume is never downloaded")
}

func TestFetchResumeSkipsExisting(t *testing.T) {
	f := newFakeCAP(t)
	c := newTestClient(t, f)

	docs, sum := collect(t, c, FetchOptions{
		Reporter: "us", Court: "scotus",
		Skip: map[string]bool{"cap-301": true, "cap-201": true},
	})

	require.Equal(t, []string{"cap-101"}, ids(docs))
	require.Equal(t, 2, sum.SkippedExisting)
}

func TestFetchMinCharsIsConfigurable(t *testing.T) {
	f := newFakeCAP(t)
	c := newTestClient(t, f)

	// -1 disables the filter, so the cert denial comes through.
	docs, sum := collect(t, c, FetchOptions{Reporter: "us", Court: "scotus", MinChars: -1})

	require.Contains(t, ids(docs), "cap-302")
	require.Equal(t, 0, sum.SkippedShort)
}

func TestFetchRetriesTransientFailures(t *testing.T) {
	f := newFakeCAP(t)
	f.failNext.Store(2)
	c := newTestClient(t, f)

	docs, _ := collect(t, c, FetchOptions{Reporter: "us", Court: "scotus", Limit: 1})

	require.Equal(t, []string{"cap-301"}, ids(docs))
}

func TestFetchEmitErrorAborts(t *testing.T) {
	f := newFakeCAP(t)
	c := newTestClient(t, f)

	_, err := c.Fetch(context.Background(), FetchOptions{Reporter: "us", Court: "scotus"},
		func(retrieval.InputDoc) error { return fmt.Errorf("disk full") })

	require.ErrorContains(t, err, "disk full")
}

func TestFetchRequiresReporter(t *testing.T) {
	c := New("", discardLogger())
	_, err := c.Fetch(context.Background(), FetchOptions{}, func(retrieval.InputDoc) error { return nil })
	require.ErrorContains(t, err, "reporter is required")
}

func TestNormalizeDocket(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"No. 12–71.", "12-71"},
		{"No. 12-71", "12-71"},
		{"Nos. 12-71, 12-72.", "12-71, 12-72"},
		{"  12—71  ", "12-71"},
		{"", ""},
		{"Original No. 5", "Original No. 5"},
	} {
		require.Equal(t, tc.want, NormalizeDocket(tc.in), "input %q", tc.in)
	}
}

func TestNormaliseKeepsParagraphs(t *testing.T) {
	in := "First   paragraph.\r\n\r\n\r\n  Second paragraph.  \n\nThird."
	require.Equal(t, "First paragraph.\n\nSecond paragraph.\n\nThird.", normalise(in))
}

// discardLogger keeps the per-volume progress lines out of test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func ids(docs []retrieval.InputDoc) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.SourceID
	}
	return out
}
