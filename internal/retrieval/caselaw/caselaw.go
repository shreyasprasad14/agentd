// Package caselaw fetches court opinions from the Caselaw Access Project's
// static file service (static.case.law) and emits them as retrieval.InputDoc,
// the same JSONL the CourtListener fetcher produces. It is named for the
// project rather than its initials because a package named cap would shadow
// the builtin in every file that imports it.
//
// It exists because of a quota, not a preference. CourtListener's REST API is
// the better source for anything recent — it is current to the day, where CAP
// stops at 2014 — but its free tier allows 125 requests per day and the fetcher
// spends about two per opinion, which caps a pull at roughly 60 documents a day
// and puts the 2,500-document corpus the retrieval plan calls for 40 days out.
// CAP is served as static files from a CDN: no token, no quota, and one request
// per *volume* rather than two per opinion. The 2,500 documents are about 120
// requests. See ADR-40.
//
// The tradeoff is recency, and it is the right way round for a retrieval
// benchmark: measuring whether the ranker puts the correct paragraph first does
// not need this year's opinions, it needs enough of them that top-8 is a
// selective question.
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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shreyasprasad/agentd/internal/retrieval"
)

// DefaultBaseURL is the CAP static file root.
const DefaultBaseURL = "https://static.case.law"

// SourceIDPrefix keys corpus documents to CAP case ids: "cap-<case id>".
// It differs from CourtListener's "clop-" so a corpus assembled from both
// sources cannot collide, and so a source_id says where it came from.
const SourceIDPrefix = "cap-"

// typeMajority is the opinion CAP considers the opinion of the court. Every
// case has exactly one, including the cert denials and one-line orders that
// make up the bulk of a modern U.S. Reports volume — which is why MinChars,
// not the type, is what separates a merits opinion from an order.
const typeMajority = "majority"

// DefaultMinChars drops orders and cert denials. Measured on 570 U.S., 571 of
// 594 records are under 500 characters and 21 are over 4,000; the gap between
// them is nearly empty, so the threshold is not a close call.
const DefaultMinChars = 4000

// Client reads the CAP static files. There is no authentication: the data is
// free of known copyright restriction and served without a key.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Log     *slog.Logger
	// sleep is swapped in tests so backoff does not stall the suite.
	sleep func(ctx context.Context, d time.Duration) error
}

// New builds a Client.
func New(baseURL string, log *slog.Logger) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		// Volume archives are a couple of megabytes; the default 60s the
		// CourtListener client uses for single JSON objects is tight here.
		HTTP:  &http.Client{Timeout: 180 * time.Second},
		Log:   log,
		sleep: sleepCtx,
	}
}

// FetchOptions selects what to pull.
type FetchOptions struct {
	// Reporter is the CAP reporter slug: "us" for United States Reports,
	// "f2d"/"f3d" for the federal appellate reporters.
	Reporter string
	// Court is written to each document's Court field. CAP names the court
	// per case, but the corpus column is a filter key the search tool and
	// the evals share, so it is set once per pull rather than derived: a
	// corpus where half the documents say "scotus" and half say "U.S." is
	// not filterable.
	Court string
	// Limit caps emitted documents; <= 0 means no cap. Volumes are walked
	// newest first, so a limit takes the most recent N opinions.
	Limit int
	// MinChars is the shortest opinion emitted; 0 means DefaultMinChars.
	// Negative disables the filter, which is only useful for tests.
	MinChars int
	// MinYear bounds the walk by the volume's end year; 0 means no bound.
	MinYear int
	// Skip holds source_ids already written, which is what makes an
	// interrupted fetch resumable. A volume is still downloaded — it is one
	// request and the skip set is per case — but its cases are not re-emitted.
	Skip map[string]bool
}

// FetchSummary counts what happened.
type FetchSummary struct {
	Emitted          int `json:"emitted"`
	Volumes          int `json:"volumes"`
	SkippedExisting  int `json:"skipped_existing"`
	SkippedShort     int `json:"skipped_short"`
	SkippedNoOpinion int `json:"skipped_no_opinion"`
}

// Wire shapes: the subset of the CAP static files we read.

type volumeMeta struct {
	VolumeNumber string `json:"volume_number"`
	VolumeFolder string `json:"volume_folder"`
	ReporterSlug string `json:"reporter_slug"`
	EndYear      int    `json:"end_year"`
	StartYear    int    `json:"start_year"`
}

type caseJSON struct {
	ID           json.Number `json:"id"`
	Name         string      `json:"name"`
	NameAbbrev   string      `json:"name_abbreviation"`
	DecisionDate string      `json:"decision_date"`
	DocketNumber string      `json:"docket_number"`
	FirstPage    string      `json:"first_page"`
	Citations    []citation  `json:"citations"`
	Court        courtJSON   `json:"court"`
	Casebody     casebody    `json:"casebody"`
}

type citation struct {
	Type string `json:"type"`
	Cite string `json:"cite"`
}

type courtJSON struct {
	Name             string `json:"name"`
	NameAbbreviation string `json:"name_abbreviation"`
}

type casebody struct {
	Opinions []capOpinion `json:"opinions"`
}

type capOpinion struct {
	Type   string `json:"type"`
	Author string `json:"author"`
	Text   string `json:"text"`
}

// Fetch walks a reporter's volumes newest first, emitting one InputDoc per
// merits opinion. emit returning an error aborts the fetch (the CLI uses it to
// stop on a write failure).
func (c *Client) Fetch(ctx context.Context, opt FetchOptions, emit func(retrieval.InputDoc) error) (FetchSummary, error) {
	var sum FetchSummary
	if opt.Reporter == "" {
		return sum, fmt.Errorf("reporter is required")
	}
	minChars := opt.MinChars
	if minChars == 0 {
		minChars = DefaultMinChars
	}

	vols, err := c.volumes(ctx, opt.Reporter)
	if err != nil {
		return sum, err
	}

	for _, v := range vols {
		if opt.Limit > 0 && sum.Emitted >= opt.Limit {
			return sum, nil
		}
		if opt.MinYear > 0 && v.EndYear > 0 && v.EndYear < opt.MinYear {
			// Volumes are ordered newest first, so the first volume that
			// falls below the bound ends the walk.
			break
		}
		cases, err := c.volumeCases(ctx, opt.Reporter, v.VolumeFolder)
		if err != nil {
			return sum, fmt.Errorf("volume %s: %w", v.VolumeNumber, err)
		}
		sum.Volumes++
		for _, cs := range cases {
			if opt.Limit > 0 && sum.Emitted >= opt.Limit {
				return sum, nil
			}
			doc, ok := buildDoc(cs, opt.Court, v, minChars, &sum)
			if !ok {
				continue
			}
			if opt.Skip[doc.SourceID] {
				sum.SkippedExisting++
				continue
			}
			if err := emit(*doc); err != nil {
				return sum, err
			}
			sum.Emitted++
		}
		c.Log.Info("volume done", "reporter", opt.Reporter, "volume", v.VolumeNumber,
			"end_year", v.EndYear, "emitted_total", sum.Emitted)
	}
	return sum, nil
}

// volumes lists a reporter's volumes, newest first. Volume numbers are strings
// in the metadata because some reporters number volumes non-numerically; the
// ones that do not parse sort last rather than aborting the pull.
func (c *Client) volumes(ctx context.Context, reporter string) ([]volumeMeta, error) {
	raw, err := c.get(ctx, c.BaseURL+"/"+reporter+"/VolumesMetadata.json")
	if err != nil {
		return nil, fmt.Errorf("volumes metadata: %w", err)
	}
	var vols []volumeMeta
	if err := json.Unmarshal(raw, &vols); err != nil {
		return nil, fmt.Errorf("volumes metadata: %w", err)
	}
	sort.SliceStable(vols, func(i, j int) bool {
		a, aok := strconv.Atoi(vols[i].VolumeNumber)
		b, bok := strconv.Atoi(vols[j].VolumeNumber)
		if aok == nil && bok == nil {
			return a > b
		}
		return aok == nil && bok != nil
	})
	return vols, nil
}

// volumeCases downloads one volume archive and decodes its case JSON. The
// archive is a couple of megabytes and holds the volume's HTML rendering as
// well, which is read past: one request for a whole volume is the entire
// reason this source is usable under a quota, so the wasted bytes are cheaper
// than the requests avoided.
func (c *Client) volumeCases(ctx context.Context, reporter, folder string) ([]caseJSON, error) {
	raw, err := c.get(ctx, c.BaseURL+"/"+reporter+"/"+folder+".zip")
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	names := make([]string, 0, len(zr.File))
	byName := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "json/") || !strings.HasSuffix(f.Name, ".json") {
			continue
		}
		names = append(names, f.Name)
		byName[f.Name] = f
	}
	// Archive order is not guaranteed; sorting by name is page order within
	// the volume, which makes a resumed fetch emit in the same sequence.
	sort.Strings(names)

	out := make([]caseJSON, 0, len(names))
	for _, n := range names {
		rc, err := byName[n].Open()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		var cs caseJSON
		if err := json.Unmarshal(body, &cs); err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		out = append(out, cs)
	}
	return out, nil
}

// buildDoc maps one CAP case to an InputDoc, or reports why it was skipped.
func buildDoc(cs caseJSON, court string, v volumeMeta, minChars int, sum *FetchSummary) (*retrieval.InputDoc, bool) {
	var parts []string
	var author string
	for _, o := range cs.Casebody.Opinions {
		if o.Type != typeMajority {
			continue
		}
		if author == "" {
			author = o.Author
		}
		if t := normalise(o.Text); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		sum.SkippedNoOpinion++
		return nil, false
	}
	text := strings.Join(parts, "\n\n")
	if minChars > 0 && len(text) < minChars {
		sum.SkippedShort++
		return nil, false
	}

	title := cs.NameAbbrev
	if title == "" {
		title = cs.Name
	}
	cites := make([]string, 0, len(cs.Citations))
	for _, c := range cs.Citations {
		if c.Cite != "" {
			cites = append(cites, c.Cite)
		}
	}
	meta, _ := json.Marshal(map[string]any{
		"cap_id":        cs.ID.String(),
		"docket_number": NormalizeDocket(cs.DocketNumber),
		"citations":     cites,
		"reporter":      v.ReporterSlug,
		"volume":        v.VolumeNumber,
		"first_page":    cs.FirstPage,
		"court_name":    cs.Court.Name,
		"opinion_type":  typeMajority,
		"author":        author,
		"full_name":     cs.Name,
	})
	return &retrieval.InputDoc{
		SourceID:  SourceIDPrefix + cs.ID.String(),
		Title:     title,
		Court:     court,
		DecidedOn: cs.DecisionDate,
		Text:      text,
		Metadata:  meta,
	}, true
}

// docketNoise strips the "No." prefix and trailing punctuation CAP carries in
// printed docket numbers — "No. 12–71." — so the string is a stable key.
var docketNoise = regexp.MustCompile(`(?i)^\s*(nos?\.|no\s)\s*`)

// NormalizeDocket reduces a printed docket number to a comparable form. The
// dedupe pass keys on this field (ADR-39), and CAP prints what the reporter
// printed: en-dashes for hyphens, a leading "No.", a trailing period. A case
// reported in two reporters differs in all three and in none of the substance.
func NormalizeDocket(s string) string {
	s = strings.NewReplacer("–", "-", "—", "-", "‐", "-").Replace(s)
	s = docketNoise.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimRight(strings.TrimSpace(s), ".,; ")
}

// maxRetries bounds backoff loops per request.
const maxRetries = 5

// get GETs a URL, retrying 429 and 5xx. There is no quota to respect here, but
// a CDN in front of an object store still rate limits and still has bad
// minutes, and a 120-volume pull is long enough to meet one.
func (c *Client) get(ctx context.Context, u string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5
		if retryable && attempt < maxRetries {
			delay := time.Duration(1<<attempt) * time.Second
			if s := resp.Header.Get("Retry-After"); s != "" {
				if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
					delay = time.Duration(secs) * time.Second
				}
			}
			c.Log.Info("retrying", "status", resp.StatusCode, "delay", delay, "url", u)
			if err := c.sleep(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 256))
		}
		return raw, nil
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var (
	spaceRun = regexp.MustCompile(`[ \t]+`)
	blankRun = regexp.MustCompile(`\n{3,}`)
)

// normalise collapses whitespace while keeping paragraph breaks, which is what
// the chunker splits on. CAP opinion text is already plain text, so unlike the
// CourtListener path there is no HTML to strip.
func normalise(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, " ", " ")
	s = spaceRun.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = blankRun.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
