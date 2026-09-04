package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/api"
	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/testutil"
	"github.com/shreyasprasad/agentd/internal/tools"
	"github.com/shreyasprasad/agentd/internal/tools/builtin"
)

// sseEvent is one parsed `id:/event:/data:` block off the wire.
type sseEvent struct {
	ID      string
	Type    string
	Payload store.Event
}

type harness struct {
	st       *store.Store
	url      string
	provider *fake.Provider
	registry *tools.Registry
}

func newHarness(t *testing.T, provider *fake.Provider) *harness {
	t.Helper()
	h := &harness{
		st:       testutil.Postgres(t),
		provider: provider,
		registry: tools.NewRegistry().MustRegister(builtin.Finish{}, builtin.ComputeDeadline{}),
	}
	srv := api.NewServer(h.st, nil, api.Options{PollInterval: 50 * time.Millisecond, Registry: h.registry})
	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)
	h.url = httpSrv.URL
	return h
}

func (h *harness) startWorker(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := runtime.NewWorker(h.st, nil, runtime.WorkerConfig{
		Owner:        "test-worker",
		PollInterval: 50 * time.Millisecond,
		Provider:     h.provider,
		Registry:     h.registry,
		DefaultModel: "fake-model",
	})
	go func() { _ = w.Run(ctx) }()
}

// TestRunLifecycleOverSSE submits a run and asserts the loop's events arrive
// over SSE in order, then that Last-Event-ID replays correctly.
func TestRunLifecycleOverSSE(t *testing.T) {
	h := newHarness(t, fake.New(fake.Text("stub answer", model.Usage{InputTokens: 5, OutputTokens: 2})))
	h.startWorker(t)

	runID := submitRun(t, h.url, "summarize the holding in a stub opinion", `{"model":"fake-model"}`)

	wantTypes := []string{
		runtime.EventRunStarted,
		runtime.EventModelRequested,
		runtime.EventModelResponded,
		runtime.EventRunFinished,
	}

	t.Run("stream delivers the events in order", func(t *testing.T) {
		events := readStream(t, h.url, runID, "")
		require.Len(t, events, len(wantTypes))
		for i, ev := range events {
			require.Equal(t, wantTypes[i], ev.Type, "event %d type", i)
			require.Equal(t, wantTypes[i], ev.Payload.Type, "event %d payload type", i)
			require.Equal(t, int32(i+1), ev.Payload.Seq, "event %d seq", i)
			require.Equal(t, strconv.Itoa(i+1), ev.ID, "event %d SSE id", i)
		}
	})

	t.Run("Last-Event-ID replays only what the client missed", func(t *testing.T) {
		events := readStream(t, h.url, runID, "2")
		require.Len(t, events, 2)
		require.Equal(t, runtime.EventModelResponded, events[0].Type)
		require.Equal(t, int32(3), events[0].Payload.Seq)
		require.Equal(t, runtime.EventRunFinished, events[1].Type)
		require.Equal(t, int32(4), events[1].Payload.Seq)
	})

	t.Run("run returns terminal status and reduced state", func(t *testing.T) {
		resp, err := http.Get(h.url + "/v1/runs/" + runID)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var body api.RunResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		require.Equal(t, "succeeded", body.Run.Status)
		require.NotNil(t, body.Run.FinishedAt)
		require.Nil(t, body.Run.LeaseOwner, "lease should be released on finish")
		require.Equal(t, int64(5), body.Run.InputTokens)

		require.Equal(t, "succeeded", body.State.Status)
		require.Equal(t, "stub answer", body.State.FinalAnswer)
		require.Equal(t, 1, body.State.Steps)
		require.Equal(t, []string{builtin.DeadlineName, builtin.FinishName}, body.State.Config.Tools,
			"allowlist defaulted to every registered tool at submission")
		require.Len(t, body.State.Messages, 2)
	})

	t.Run("event log matches the stream", func(t *testing.T) {
		resp, err := http.Get(h.url + "/v1/runs/" + runID + "/events")
		require.NoError(t, err)
		defer resp.Body.Close()

		var body struct {
			Events []store.Event `json:"events"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		require.Len(t, body.Events, len(wantTypes))
	})

	t.Run("finished runs cannot be cancelled or resumed", func(t *testing.T) {
		for _, action := range []string{"cancel", "resume"} {
			resp, err := http.Post(h.url+"/v1/runs/"+runID+"/"+action, "application/json", nil)
			require.NoError(t, err)
			resp.Body.Close()
			require.Equal(t, http.StatusConflict, resp.StatusCode, action)
		}
	})
}

func TestCancelAndResumeEndpoints(t *testing.T) {
	h := newHarness(t, fake.New(fake.Text("never", model.Usage{})))
	// No worker: the run stays queued, so both endpoints act on a live run.
	runID := submitRun(t, h.url, "wait", "")

	resp, err := http.Post(h.url+"/v1/runs/"+runID+"/cancel", "application/json", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	run, err := h.st.GetRun(context.Background(), uuid.MustParse(runID))
	require.NoError(t, err)
	require.True(t, run.CancelRequested)

	resp, err = http.Post(h.url+"/v1/runs/"+runID+"/resume", "application/json", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	// A worker that starts now sees the cancel flag and finishes the run
	// without calling the model.
	h.startWorker(t)
	events := readStream(t, h.url, runID, "")
	require.Equal(t, runtime.EventCancelRequested, events[1].Type)
	require.Equal(t, runtime.EventRunFinished, events[2].Type)
	require.Equal(t, 0, h.provider.Calls())
}

func TestCreateRunValidatesConfig(t *testing.T) {
	h := newHarness(t, fake.New())
	cases := []struct {
		name string
		cfg  string
		want int
	}{
		{"unknown tool", `{"tools":["python"]}`, http.StatusBadRequest},
		{"unknown field", `{"modle":"x"}`, http.StatusBadRequest},
		{"negative delay", `{"tool_delay_ms":-1}`, http.StatusBadRequest},
		{"explicit allowlist", `{"tools":["finish"]}`, http.StatusCreated},
		{"empty allowlist", `{"tools":[]}`, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(api.CreateRunRequest{Goal: "g", AgentConfig: json.RawMessage(tc.cfg)})
			resp, err := http.Post(h.url+"/v1/runs", "application/json", bytes.NewReader(body))
			require.NoError(t, err)
			resp.Body.Close()
			require.Equal(t, tc.want, resp.StatusCode)
		})
	}
}

func TestListTools(t *testing.T) {
	h := newHarness(t, fake.New())
	resp, err := http.Get(h.url + "/v1/tools")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Tools []api.ToolInfo `json:"tools"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Tools, 2)
	require.Equal(t, builtin.DeadlineName, body.Tools[0].Name)
	require.Equal(t, tools.Builtin, body.Tools[0].TrustTier)
	require.Contains(t, string(body.Tools[0].Schema), "start_date")
}

func TestUnknownRunIsNotFound(t *testing.T) {
	h := newHarness(t, fake.New())
	resp, err := http.Get(h.url + "/v1/runs/6f1b6f0e-0000-4000-8000-000000000000")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func submitRun(t *testing.T, baseURL, goal, agentConfig string) string {
	t.Helper()
	req := api.CreateRunRequest{Goal: goal}
	if agentConfig != "" {
		req.AgentConfig = json.RawMessage(agentConfig)
	}
	body, err := json.Marshal(req)
	require.NoError(t, err)

	resp, err := http.Post(baseURL+"/v1/runs", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var created api.CreateRunResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	require.NotEmpty(t, created.ID)
	require.Equal(t, "queued", created.Status)
	return created.ID.String()
}

// readStream opens the SSE endpoint and reads until the server closes the
// stream, which it does after run_finished.
func readStream(t *testing.T, baseURL, runID, lastEventID string) []sseEvent {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/runs/"+runID+"/stream", nil)
	require.NoError(t, err)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	var (
		events  []sseEvent
		current sseEvent
		haveID  bool
	)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if haveID {
				events = append(events, current)
			}
			current, haveID = sseEvent{}, false
		case strings.HasPrefix(line, ":"):
			// keep-alive comment
		case strings.HasPrefix(line, "id: "):
			current.ID = strings.TrimPrefix(line, "id: ")
			haveID = true
		case strings.HasPrefix(line, "event: "):
			current.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &current.Payload))
		default:
			t.Fatalf("unexpected SSE line: %q", line)
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		require.NoError(t, err, "read SSE stream")
	}
	return events
}
