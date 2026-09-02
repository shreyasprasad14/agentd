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

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/shreyasprasad/agentd/internal/api"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
)

// stepDelay is the stub agent's inter-event pause. Production defaults to one
// second; the test shortens it so the suite stays fast while still forcing the
// SSE tail to deliver events live rather than in one replayed batch.
const stepDelay = 200 * time.Millisecond

// sseEvent is one parsed `id:/event:/data:` block off the wire.
type sseEvent struct {
	ID      string
	Type    string
	Payload store.Event
}

// TestRunLifecycleOverSSE submits a run and asserts the stub agent's three
// events arrive over SSE in order, then that Last-Event-ID replays correctly.
func TestRunLifecycleOverSSE(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()
	st := startPostgres(t, ctx)

	srv := api.NewServer(st, nil, api.Options{PollInterval: 50 * time.Millisecond})
	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)

	workerCtx, stopWorker := context.WithCancel(ctx)
	t.Cleanup(stopWorker)
	worker := runtime.NewWorker(st, nil, runtime.WorkerConfig{
		Owner:        "test-worker",
		PollInterval: 50 * time.Millisecond,
		StepDelay:    stepDelay,
	})
	go func() { _ = worker.Run(workerCtx) }()

	runID := submitRun(t, httpSrv.URL, "summarize the holding in a stub opinion")

	t.Run("stream delivers the three events in order", func(t *testing.T) {
		events := readStream(t, httpSrv.URL, runID, "")
		require.Len(t, events, 3, "expected run_started, model_responded, run_finished")

		wantTypes := []string{
			runtime.EventRunStarted,
			runtime.EventModelResponded,
			runtime.EventRunFinished,
		}
		for i, ev := range events {
			require.Equal(t, wantTypes[i], ev.Type, "event %d type", i)
			require.Equal(t, wantTypes[i], ev.Payload.Type, "event %d payload type", i)
			require.Equal(t, int32(i+1), ev.Payload.Seq, "event %d seq", i)
			require.Equal(t, strconv.Itoa(i+1), ev.ID, "event %d SSE id", i)
		}
	})

	t.Run("Last-Event-ID replays only what the client missed", func(t *testing.T) {
		events := readStream(t, httpSrv.URL, runID, "1")
		require.Len(t, events, 2)
		require.Equal(t, runtime.EventModelResponded, events[0].Type)
		require.Equal(t, int32(2), events[0].Payload.Seq)
		require.Equal(t, runtime.EventRunFinished, events[1].Type)
		require.Equal(t, int32(3), events[1].Payload.Seq)
	})

	t.Run("run reaches a terminal status", func(t *testing.T) {
		resp, err := http.Get(httpSrv.URL + "/v1/runs/" + runID)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var run store.Run
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&run))
		require.Equal(t, "succeeded", run.Status)
		require.NotNil(t, run.FinishedAt)
		require.Nil(t, run.LeaseOwner, "lease should be released on finish")
	})

	t.Run("event log matches the stream", func(t *testing.T) {
		resp, err := http.Get(httpSrv.URL + "/v1/runs/" + runID + "/events")
		require.NoError(t, err)
		defer resp.Body.Close()

		var body struct {
			Events []store.Event `json:"events"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		require.Len(t, body.Events, 3)
	})
}

func TestUnknownRunIsNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()
	st := startPostgres(t, ctx)

	httpSrv := httptest.NewServer(api.NewServer(st, nil, api.Options{}).Router())
	t.Cleanup(httpSrv.Close)

	resp, err := http.Get(httpSrv.URL + "/v1/runs/6f1b6f0e-0000-4000-8000-000000000000")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// startPostgres boots a pgvector-enabled Postgres 16 and applies migrations.
func startPostgres(t *testing.T, ctx context.Context) *store.Store {
	t.Helper()

	container, err := tcpostgres.Run(ctx, "pgvector/pgvector:pg16",
		tcpostgres.WithDatabase("agentd"),
		tcpostgres.WithUsername("agentd"),
		tcpostgres.WithPassword("agentd"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120*time.Second),
		),
	)
	require.NoError(t, err, "start postgres container")
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	st, err := store.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(st.Close)

	require.NoError(t, st.Migrate(ctx))
	// Migrations must be idempotent: serve and work both apply them at boot.
	require.NoError(t, st.Migrate(ctx))
	return st
}

func submitRun(t *testing.T, baseURL, goal string) string {
	t.Helper()

	body, err := json.Marshal(api.CreateRunRequest{
		Goal:        goal,
		AgentConfig: json.RawMessage(`{"model":"stub","tools":[]}`),
	})
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
