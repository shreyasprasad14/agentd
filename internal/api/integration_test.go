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
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	promodel "github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/shreyasprasad/agentd/internal/api"
	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/telemetry"
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

// newHarness builds an API with tracing off, which is what most of these
// tests want: a run submitted without a tracer stores no ids, exactly like a
// run created before M4.
func newHarness(t *testing.T, provider *fake.Provider) *harness {
	t.Helper()
	return newTracedHarness(t, provider, api.Options{})
}

// newTracedHarness builds an API from opts, filling in the fields every test
// needs. It is the seam for the tracing tests: they pass a Tracer backed by a
// tracetest.SpanRecorder, so `make test` asserts on real spans without a
// Jaeger or a network.
func newTracedHarness(t *testing.T, provider *fake.Provider, opts api.Options) *harness {
	t.Helper()
	h := &harness{
		st:       testutil.Postgres(t),
		provider: provider,
		registry: tools.NewRegistry().MustRegister(builtin.Finish{}, builtin.ComputeDeadline{}),
	}
	opts.PollInterval = 50 * time.Millisecond
	opts.Registry = h.registry
	srv := api.NewServer(h.st, nil, opts)
	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)
	h.url = httpSrv.URL
	return h
}

// recordingTracer returns a tracer that keeps its finished spans in memory.
// AlwaysSample is explicit rather than inherited from the remote parent the
// API plants, so the test asserts on the API's behaviour and not on the
// default sampler's.
func recordingTracer(t *testing.T) (trace.Tracer, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	return tp.Tracer("api-test"), sr
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

	// Both endpoints answer off the run's terminal status rather than off a
	// lease or a flag, so this stays the regression check that M4's control
	// work — interrupting cancel especially — did not make a finished run
	// cancellable again.
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

// TestTracedSubmission covers the API's half of ADR-24: submission mints the
// run's trace identity, stores it on the row, and records api.create_run
// under it — under the run's ids, not a fresh trace of its own, or the
// submission would sit in a different waterfall from the run it submitted.
func TestTracedSubmission(t *testing.T) {
	tracer, recorder := recordingTracer(t)
	// A trailing slash on the base, because that is the other way an
	// operator spells -jaeger-ui and the link must come out the same.
	h := newTracedHarness(t, fake.New(), api.Options{Tracer: tracer, JaegerUI: "http://jaeger.test:16686/"})

	runID := submitRun(t, h.url, "trace me", `{"tools":["finish"]}`)
	run, err := h.st.GetRun(context.Background(), uuid.MustParse(runID))
	require.NoError(t, err)

	t.Run("the run row carries both ids", func(t *testing.T) {
		require.NotNil(t, run.TraceID, "a traced submission must store a trace id")
		require.NotNil(t, run.RootSpanID)
		require.Len(t, *run.TraceID, 32, "trace id is 16 bytes of hex")
		require.Len(t, *run.RootSpanID, 16, "span id is 8 bytes of hex")
	})

	t.Run("api.create_run is in the run's trace, under its root span", func(t *testing.T) {
		spans := recorder.Ended()
		require.Len(t, spans, 1)
		span := spans[0]
		require.Equal(t, telemetry.SpanCreateRun, span.Name())
		require.Equal(t, *run.TraceID, span.SpanContext().TraceID().String(),
			"the span must use the trace id stored on the run")
		require.Equal(t, *run.RootSpanID, span.Parent().SpanID().String(),
			"its parent is the agent.run span the finishing worker will emit later")
		require.True(t, span.Parent().IsRemote(), "that parent belongs to another process")

		attrs := attrsOf(span.Attributes())
		require.Equal(t, "1.00", attrs[telemetry.AttrBudgetUSD].AsString())
		require.Equal(t, int64(30), attrs[telemetry.AttrMaxSteps].AsInt64())
		require.Equal(t, int64(1), attrs[telemetry.AttrToolCount].AsInt64())
		require.Equal(t, runID, attrs[telemetry.AttrRunID].AsString())
	})

	t.Run("the deep link opens that trace", func(t *testing.T) {
		var body api.TraceResponse
		resp, err := http.Get(h.url + "/v1/runs/" + runID + "/trace")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

		require.Equal(t, *run.TraceID, body.TraceID)
		require.Equal(t, "http://jaeger.test:16686/trace/"+*run.TraceID, body.URL)
	})
}

// TestTraceEndpointWithoutTrace pins the 404: the endpoint must not invent a
// link to a trace that was never exported, which looks like a lost trace
// rather than an untraced run.
func TestTraceEndpointWithoutTrace(t *testing.T) {
	h := newHarness(t, fake.New()) // tracing off

	t.Run("a run submitted with tracing off stores no ids", func(t *testing.T) {
		runID := submitRun(t, h.url, "untraced", "")
		run, err := h.st.GetRun(context.Background(), uuid.MustParse(runID))
		require.NoError(t, err)
		require.Nil(t, run.TraceID, "ids are minted but discarded when no span records")
		require.Nil(t, run.RootSpanID)
		requireTrace404(t, h.url, runID)
	})

	t.Run("a pre-M4 run 404s too", func(t *testing.T) {
		// Created straight through the store with empty ids, which is the
		// shape every row had before migration 0003 backfilled nothing.
		run, err := h.st.CreateRun(context.Background(), store.NewRun{
			Goal: "created before M4", MaxSteps: 1, BudgetUSD: "1.00",
		})
		require.NoError(t, err)
		requireTrace404(t, h.url, run.ID.String())
	})
}

func requireTrace404(t *testing.T, baseURL, runID string) {
	t.Helper()
	resp, err := http.Get(baseURL + "/v1/runs/" + runID + "/trace")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// attrsOf indexes a span's attributes by key so a test can assert on one
// without depending on the order they were set in.
func attrsOf(kvs []attribute.KeyValue) map[attribute.Key]attribute.Value {
	out := make(map[attribute.Key]attribute.Value, len(kvs))
	for _, kv := range kvs {
		out[kv.Key] = kv.Value
	}
	return out
}

// TestMetricsEndpoint covers the API's half of ADR-26: the process counter
// for requests, the Postgres-backed gauges that survive a restart, and the
// cardinality rule that keeps both from mounting a run id as a label.
func TestMetricsEndpoint(t *testing.T) {
	h := newTracedHarness(t, fake.New(), api.Options{Metrics: telemetry.NewMetrics()})
	// No worker, so the run stays queued and the gauges have one exact value
	// to assert rather than a race with a loop.
	runID := submitRun(t, h.url, "count me", "")

	resp, err := http.Get(h.url + "/v1/runs/" + runID)
	require.NoError(t, err)
	resp.Body.Close()

	families := scrapeMetrics(t, h.url)

	t.Run("the run gauges come from the database", func(t *testing.T) {
		// One indexed GROUP BY per scrape, so this number is right after a
		// deploy that reset every counter in the process.
		require.Equal(t, 1.0, gaugeValue(t, families, "agentd_runs", map[string]string{"status": "queued"}))
		require.Equal(t, 0.0, gaugeValue(t, families, "agentd_runs", map[string]string{"status": "failed"}),
			"a status with no runs is an explicit zero, not a missing series")
		require.Contains(t, families, "agentd_runs_oldest_queued_age_seconds",
			"a queued run means a queue age")
	})

	t.Run("requests are counted by route pattern", func(t *testing.T) {
		require.Equal(t, 1.0, counterValue(t, families, "agentd_http_requests_total",
			map[string]string{"route": "/v1/runs", "method": "POST", "code": "201"}))
		require.Equal(t, 1.0, counterValue(t, families, "agentd_http_requests_total",
			map[string]string{"route": "/v1/runs/{id}", "method": "GET", "code": "200"}))
	})

	t.Run("no label value is a run id", func(t *testing.T) {
		// The failure this guards against is silent: /v1/runs/<uuid> as a
		// label mints one series per run, and nothing breaks until the
		// process runs out of memory weeks later.
		for name, f := range families {
			for _, metric := range f.GetMetric() {
				for _, label := range metric.GetLabel() {
					require.NotContains(t, label.GetValue(), runID, "metric %s label %s", name, label.GetName())
				}
			}
		}
	})
}

// TestCancelFinishedRunConflicts pins the 409. A finished run has no worker
// to notice a flag, so accepting the cancel would leave the caller waiting
// for a termination that already happened.
func TestCancelFinishedRunConflicts(t *testing.T) {
	h := newHarness(t, fake.New(fake.Text("done", model.Usage{InputTokens: 3, OutputTokens: 1})))
	h.startWorker(t)

	runID := submitRun(t, h.url, "finish quickly", "")
	events := readStream(t, h.url, runID, "")
	require.Equal(t, runtime.EventRunFinished, events[len(events)-1].Type)

	resp, err := http.Post(h.url+"/v1/runs/"+runID+"/cancel", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

// scrapeMetrics reads GET /metrics and parses it the way Prometheus does.
// Parsing rather than substring-matching is the point: a duplicated metric
// name or a bad help string is served as a 200 with text in it, and only the
// parser calls that a failure.
func scrapeMetrics(t *testing.T, baseURL string) map[string]*dto.MetricFamily {
	t.Helper()
	resp, err := http.Get(baseURL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The validation scheme has to be named: a zero TextParser has none and
	// panics rather than defaulting, which is a v0.70 change worth pinning
	// here instead of rediscovering in a year.
	parser := expfmt.NewTextParser(promodel.UTF8Validation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	require.NoError(t, err, "the exposition must parse")
	return families
}

// metricWith finds the one series of family whose labels match want.
func metricWith(t *testing.T, families map[string]*dto.MetricFamily, name string, want map[string]string) *dto.Metric {
	t.Helper()
	f, ok := families[name]
	require.True(t, ok, "no metric family %q in the exposition", name)
	for _, metric := range f.GetMetric() {
		got := map[string]string{}
		for _, label := range metric.GetLabel() {
			got[label.GetName()] = label.GetValue()
		}
		matched := true
		for k, v := range want {
			if got[k] != v {
				matched = false
				break
			}
		}
		if matched {
			return metric
		}
	}
	t.Fatalf("no series of %s with labels %v", name, want)
	return nil
}

func gaugeValue(t *testing.T, families map[string]*dto.MetricFamily, name string, want map[string]string) float64 {
	t.Helper()
	return metricWith(t, families, name, want).GetGauge().GetValue()
}

func counterValue(t *testing.T, families map[string]*dto.MetricFamily, name string, want map[string]string) float64 {
	t.Helper()
	return metricWith(t, families, name, want).GetCounter().GetValue()
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
