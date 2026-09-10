package courtlistener

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shreyasprasad/agentd/internal/retrieval"
)

// DefaultBaseURL is the production REST API root.
const DefaultBaseURL = "https://www.courtlistener.com/api/rest/v4"

// SourceIDPrefix keys corpus documents to CourtListener opinion ids:
// "clop-<opinion id>".
const SourceIDPrefix = "clop-"

// Lead opinion types: one document per decision, dissents and concurrences
// are a follow-up.
const (
	typeCombined = "010combined"
	typeLead     = "020lead"
)

// Client talks to the CourtListener REST API. The token comes from
// COURTLISTENER_TOKEN; anonymous requests work but are rate limited harder.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	Log     *slog.Logger
	// sleep is swapped in tests so 429 backoff does not stall the suite.
	sleep func(ctx context.Context, d time.Duration) error
}

// New builds a Client.
func New(baseURL, token string, log *slog.Logger) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
		Log:     log,
		sleep:   sleepCtx,
	}
}

// FetchOptions selects what to pull.
type FetchOptions struct {
	// Court is the CourtListener court id, e.g. "scotus".
	Court string
	// FiledAfter bounds the search, YYYY-MM-DD.
	FiledAfter string
	// Limit caps emitted documents; <= 0 means no cap.
	Limit int
	// Skip holds source_ids already written; their clusters are not
	// re-fetched, which is what makes an interrupted fetch resumable.
	Skip map[string]bool
}

// FetchSummary counts what happened.
type FetchSummary struct {
	Emitted         int `json:"emitted"`
	SkippedExisting int `json:"skipped_existing"`
	SkippedEmpty    int `json:"skipped_empty"`
	SkippedNoLead   int `json:"skipped_no_lead"`
}

// Wire shapes: the subset of the v4 API we read.

type searchPage struct {
	Next    string         `json:"next"`
	Results []searchResult `json:"results"`
}

type searchResult struct {
	ClusterID    json.Number  `json:"cluster_id"`
	CaseName     string       `json:"caseName"`
	DateFiled    string       `json:"dateFiled"`
	DocketNumber string       `json:"docketNumber"`
	Citation     []string     `json:"citation"`
	AbsoluteURL  string       `json:"absolute_url"`
	Opinions     []opinionRef `json:"opinions"`
}

type opinionRef struct {
	ID json.Number `json:"id"`
}

type opinion struct {
	ID                json.Number `json:"id"`
	Type              string      `json:"type"`
	HTMLWithCitations string      `json:"html_with_citations"`
	PlainText         string      `json:"plain_text"`
}

// Fetch walks the search results for one court, resolves each cluster to its
// lead opinion, and emits one InputDoc per decision. emit returning an error
// aborts the fetch (the CLI uses it to stop on a write failure).
func (c *Client) Fetch(ctx context.Context, opt FetchOptions, emit func(retrieval.InputDoc) error) (FetchSummary, error) {
	var sum FetchSummary
	q := url.Values{
		"type":     {"o"},
		"court":    {opt.Court},
		"order_by": {"dateFiled desc"},
	}
	if opt.FiledAfter != "" {
		q.Set("filed_after", opt.FiledAfter)
	}
	next := c.BaseURL + "/search/?" + q.Encode()

	for next != "" {
		var page searchPage
		if err := c.getJSON(ctx, next, &page); err != nil {
			return sum, fmt.Errorf("search: %w", err)
		}
		for _, r := range page.Results {
			if opt.Limit > 0 && sum.Emitted >= opt.Limit {
				return sum, nil
			}
			if c.alreadyHave(r.Opinions, opt.Skip) {
				sum.SkippedExisting++
				continue
			}
			doc, status, err := c.resolveCluster(ctx, r, opt.Court)
			if err != nil {
				return sum, err
			}
			switch status {
			case clusterEmpty:
				sum.SkippedEmpty++
				continue
			case clusterNoLead:
				sum.SkippedNoLead++
				continue
			}
			if err := emit(*doc); err != nil {
				return sum, err
			}
			sum.Emitted++
		}
		next = page.Next
	}
	return sum, nil
}

func (c *Client) alreadyHave(ops []opinionRef, skip map[string]bool) bool {
	for _, o := range ops {
		if skip[SourceIDPrefix+o.ID.String()] {
			return true
		}
	}
	return false
}

type clusterStatus int

const (
	clusterOK clusterStatus = iota
	clusterEmpty
	clusterNoLead
)

// resolveCluster fetches the cluster's opinions in order and takes the first
// lead (010combined or 020lead) with non-empty text.
func (c *Client) resolveCluster(ctx context.Context, r searchResult, court string) (*retrieval.InputDoc, clusterStatus, error) {
	sawLead := false
	for _, entry := range r.Opinions {
		var op opinion
		u := fmt.Sprintf("%s/opinions/%s/?fields=id,type,html_with_citations,plain_text", c.BaseURL, entry.ID.String())
		if err := c.getJSON(ctx, u, &op); err != nil {
			return nil, clusterOK, fmt.Errorf("opinion %s: %w", entry.ID.String(), err)
		}
		if op.Type != typeCombined && op.Type != typeLead {
			continue
		}
		sawLead = true
		text := HTMLToText(op.HTMLWithCitations)
		if text == "" {
			text = normalise(op.PlainText)
		}
		if text == "" {
			continue
		}
		meta, _ := json.Marshal(map[string]any{
			"cluster_id":    r.ClusterID.String(),
			"docket_number": r.DocketNumber,
			"citations":     r.Citation,
			"absolute_url":  r.AbsoluteURL,
			"opinion_type":  op.Type,
		})
		return &retrieval.InputDoc{
			SourceID:  SourceIDPrefix + op.ID.String(),
			Title:     r.CaseName,
			Court:     court,
			DecidedOn: r.DateFiled,
			Text:      text,
			Metadata:  meta,
		}, clusterOK, nil
	}
	if sawLead {
		return nil, clusterEmpty, nil
	}
	return nil, clusterNoLead, nil
}

// maxRetries bounds 429 backoff loops per request.
const maxRetries = 5

// getJSON GETs a URL with auth, honouring Retry-After on 429.
func (c *Client) getJSON(ctx context.Context, u string, into any) error {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		if c.Token != "" {
			req.Header.Set("Authorization", "Token "+c.Token)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("read body: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			if attempt >= maxRetries {
				return fmt.Errorf("HTTP 429 after %d retries", attempt)
			}
			delay := 5 * time.Second
			if s := resp.Header.Get("Retry-After"); s != "" {
				if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
					delay = time.Duration(secs) * time.Second
				}
			}
			c.Log.Info("rate limited, backing off", "delay", delay, "url", u)
			if err := c.sleep(ctx, delay); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 256))
		}
		return json.Unmarshal(raw, into)
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
