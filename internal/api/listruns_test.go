package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
)

// listRuns reads GET /v1/runs with the given query string.
func listRuns(t *testing.T, baseURL, query string) ([]store.Run, int) {
	t.Helper()
	url := baseURL + "/v1/runs"
	if query != "" {
		url += "?" + query
	}
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}
	var body struct {
		Runs []store.Run `json:"runs"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body.Runs, resp.StatusCode
}

// TestListRunsEndpoint covers the listing the viewer opens on: newest first,
// narrowed by status, bounded by limit.
func TestListRunsEndpoint(t *testing.T) {
	h := newHarness(t, fake.New())

	empty, code := listRuns(t, h.url, "")
	require.Equal(t, http.StatusOK, code)
	// Never null: the viewer iterates this without a nil check, and an absent
	// array and an empty one are different values in JavaScript.
	require.NotNil(t, empty)
	require.Empty(t, empty)

	var ids []string
	for _, goal := range []string{"first", "second", "third"} {
		ids = append(ids, submitRun(t, h.url, goal, ""))
	}

	all, code := listRuns(t, h.url, "")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, all, 3)
	// Newest first, so the last submitted leads.
	require.Equal(t, "third", all[0].Goal)
	require.Equal(t, "first", all[2].Goal)

	// The listed run marshals to the same shape GET /v1/runs/:id returns, which
	// is what lets the viewer render a row without refetching it.
	require.Equal(t, ids[2], all[0].ID.String())
	require.Equal(t, runtime.StatusQueued, all[0].Status)
	require.NotEmpty(t, all[0].BudgetUSD)

	queued, code := listRuns(t, h.url, "status="+runtime.StatusQueued)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, queued, 3)

	succeeded, code := listRuns(t, h.url, "status="+runtime.StatusSucceeded)
	require.Equal(t, http.StatusOK, code)
	require.Empty(t, succeeded)

	limited, code := listRuns(t, h.url, "limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, limited, 2)
	require.Equal(t, "third", limited[0].Goal)
}

// TestListRunsRejectsBadQuery pins the validation the store deliberately does
// not do: an unknown status matches nothing at the SQL level, so without this
// check a typo would read as "no runs are in that state".
func TestListRunsRejectsBadQuery(t *testing.T) {
	h := newHarness(t, fake.New())

	for _, query := range []string{"status=not_a_status", "limit=many"} {
		resp, err := http.Get(h.url + "/v1/runs?" + query)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "query %q body %s", query, body)
	}

	// An out-of-range limit is not a client error: the store clamps it.
	runs, code := listRuns(t, h.url, "limit=100000")
	require.Equal(t, http.StatusOK, code)
	require.Empty(t, runs)
}

// TestListRunsReflectsTerminalStatus is the filter doing the job the viewer
// needs it for: separating finished runs from live ones.
func TestListRunsReflectsTerminalStatus(t *testing.T) {
	h := newHarness(t, fake.New(fake.Text("done", model.Usage{InputTokens: 3, OutputTokens: 1})))
	h.startWorker(t)

	runID := submitRun(t, h.url, "finish quickly", "")
	events := readStream(t, h.url, runID, "")
	require.Equal(t, runtime.EventRunFinished, events[len(events)-1].Type)

	succeeded, code := listRuns(t, h.url, "status="+runtime.StatusSucceeded)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, succeeded, 1)
	require.Equal(t, runID, succeeded[0].ID.String())
	require.NotNil(t, succeeded[0].FinishedAt)
}

// TestViewerMountDoesNotShadowAPI is the reason ui.Mount is called last and
// the reason that is not load-bearing: a catch-all registered on the same
// router must not take traffic from the API, /healthz, or /metrics.
func TestViewerMountDoesNotShadowAPI(t *testing.T) {
	h := newHarness(t, fake.New())

	resp, err := http.Get(h.url + "/")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	require.Contains(t, strings.ToLower(string(body)), "<!doctype html")

	// The API endpoints still answer as themselves.
	for path, want := range map[string]string{
		"/healthz":  `"status":"ok"`,
		"/v1/tools": `"tools":`,
		"/v1/runs":  `"runs":`,
	} {
		resp, err := http.Get(h.url + path)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, "path %s", path)
		require.Contains(t, string(body), want, "path %s", path)
	}

	// POST /v1/runs still creates: adding GET on the same pattern must not
	// have displaced it.
	require.NotEmpty(t, submitRun(t, h.url, "still works", ""))
}
