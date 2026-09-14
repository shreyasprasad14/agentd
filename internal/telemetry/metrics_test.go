package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// expose renders the registry the way a scrape would, so the assertions below
// are against the text Prometheus receives rather than against the in-memory
// instruments. The two can differ — a collector that errors, a name that does
// not parse — and the exposition is the contract.
func expose(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return string(body)
}

// histogram sums one histogram family across its label sets. Asserting on
// bucket-by-bucket exposition text would be unreadable and would break every
// time the bucket list is retuned, while the count and the sum are what the
// call sites are actually being tested for.
func histogram(t *testing.T, m *Metrics, name string) (count uint64, sum float64) {
	t.Helper()
	families, err := m.Registry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, metric := range f.GetMetric() {
			count += metric.GetHistogram().GetSampleCount()
			sum += metric.GetHistogram().GetSampleSum()
		}
	}
	return count, sum
}

func TestMetrics_NilIsSafeAndRecordsNothing(t *testing.T) {
	// The loop, the worker, the sandbox and the searcher all take a *Metrics
	// that is nil in every test and in any command that serves no /metrics.
	// If one of these panicked, it would panic in production too, on the path
	// that is meant to cost nothing.
	var m *Metrics
	m.RunClaimed(true)
	m.RunFinished("succeeded")
	m.ObserveRunDuration("succeeded", time.Second)
	m.StepStarted()
	m.ModelCall("anthropic", "claude-opus-5", ModelOutcomeOK, time.Second)
	m.ModelUsage("anthropic", "claude-opus-5", 1, 2, 3, 4, 5)
	m.ToolInvoked("finish", OutcomeSucceeded)
	m.ObserveToolDuration("finish", time.Second)
	m.SandboxTimedOut()
	m.SandboxOrphansSwept(2)
	m.RetrievalSearch("hybrid", true)
	m.ObserveRetrievalStage(StageRerank, time.Second)
	m.LeasesReaped(3)
	m.LeaseLost()
	m.BudgetTermination("would_exceed")
	m.Cancellation("tool")
	m.HTTPRequest("/v1/runs", http.MethodPost, http.StatusCreated)
	m.SSEStreamOpened()
	m.SSEStreamClosed()
	require.Nil(t, m.Registry())
	require.NoError(t, m.Register())

	// A nil Metrics serves 404 rather than an empty 200: a scrape target that
	// answers with nothing looks healthy and reports no runs.
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMetrics_TwoRegistriesInOneProcess(t *testing.T) {
	// Spec §2 wants two workers proving leases work, and the integration
	// tests run several in one process. A package-level MustRegister would
	// panic on the second.
	a, b := NewMetrics(), NewMetrics()
	a.RunFinished("succeeded")

	require.Equal(t, 1, testutil.CollectAndCount(a.Registry(), "agentd_runs_finished_total"))
	require.Equal(t, 0, testutil.CollectAndCount(b.Registry(), "agentd_runs_finished_total"))
}

func TestMetrics_RunAndModelCounters(t *testing.T) {
	m := NewMetrics()
	m.RunClaimed(false)
	m.RunClaimed(true)
	m.RunFinished("succeeded")
	m.RunFinished("budget_exceeded")
	m.ObserveRunDuration("succeeded", 42*time.Second)
	m.StepStarted()
	m.StepStarted()
	m.ModelCall("anthropic", "claude-opus-5", ModelOutcomeRetried, 2*time.Second)
	m.ModelCall("anthropic", "claude-opus-5", ModelOutcomeOK, 3*time.Second)
	m.ModelUsage("anthropic", "claude-opus-5", 1200, 300, 0, 0, 7100)

	require.NoError(t, testutil.CollectAndCompare(m.Registry(), strings.NewReader(`
# HELP agentd_runs_claimed_total Run claims by a worker; resumed=true means the run already had events.
# TYPE agentd_runs_claimed_total counter
agentd_runs_claimed_total{resumed="false"} 1
agentd_runs_claimed_total{resumed="true"} 1
# HELP agentd_runs_finished_total Runs that reached a terminal event, by terminal status.
# TYPE agentd_runs_finished_total counter
agentd_runs_finished_total{status="budget_exceeded"} 1
agentd_runs_finished_total{status="succeeded"} 1
# HELP agentd_steps_total Loop iterations executed.
# TYPE agentd_steps_total counter
agentd_steps_total 2
`), "agentd_runs_claimed_total", "agentd_runs_finished_total", "agentd_steps_total"))

	// One increment per attempt, so a call that succeeded on its second try
	// is one "ok" and one "retried" rather than one success that hides a
	// retry (ADR-27's number is read off exactly this).
	require.NoError(t, testutil.CollectAndCompare(m.Registry(), strings.NewReader(`
# HELP agentd_model_calls_total Model call attempts by outcome (ok, retried, failed, non_retryable).
# TYPE agentd_model_calls_total counter
agentd_model_calls_total{model="claude-opus-5",outcome="ok",provider="anthropic"} 1
agentd_model_calls_total{model="claude-opus-5",outcome="retried",provider="anthropic"} 1
# HELP agentd_model_cost_micro_usd_total Model spend in micro-USD, the same integer unit the budget is kept in (ADR-6).
# TYPE agentd_model_cost_micro_usd_total counter
agentd_model_cost_micro_usd_total{model="claude-opus-5",provider="anthropic"} 7100
# HELP agentd_model_tokens_total Tokens reported by the provider, by kind (input, output, cache_read, cache_write).
# TYPE agentd_model_tokens_total counter
agentd_model_tokens_total{kind="input",model="claude-opus-5",provider="anthropic"} 1200
agentd_model_tokens_total{kind="output",model="claude-opus-5",provider="anthropic"} 300
`), "agentd_model_calls_total", "agentd_model_cost_micro_usd_total", "agentd_model_tokens_total"))

	// Two attempts, one call: the latency histogram is per attempt, which is
	// what keeps a slow provider and a retried one distinguishable.
	require.Equal(t, 1, testutil.CollectAndCount(m.Registry(), "agentd_model_latency_seconds"))
	count, sum := histogram(t, m, "agentd_model_latency_seconds")
	require.Equal(t, uint64(2), count)
	require.InDelta(t, 5.0, sum, 0.001)
}

func TestMetrics_ZeroUsageMintsNoSeries(t *testing.T) {
	// Every local run reports zero cost and no cache tokens. Recording them
	// would put three permanently-zero series per model in the exposition,
	// which is noise in every dashboard that lists what a model cost.
	m := NewMetrics()
	m.ModelUsage("local", "qwen2.5:7b", 900, 120, 0, 0, 0)

	require.Equal(t, 2, testutil.CollectAndCount(m.Registry(), "agentd_model_tokens_total"))
	require.Equal(t, 0, testutil.CollectAndCount(m.Registry(), "agentd_model_cost_micro_usd_total"))
}

func TestMetrics_ToolAndControlCounters(t *testing.T) {
	m := NewMetrics()
	m.ToolInvoked("run_python", OutcomeSucceeded)
	m.ObserveToolDuration("run_python", 4*time.Second)
	m.ToolInvoked("search_corpus", OutcomeRejected)
	m.SandboxTimedOut()
	m.SandboxOrphansSwept(2)
	m.LeasesReaped(3)
	m.LeaseLost()
	m.BudgetTermination("would_exceed")
	m.Cancellation("tool")
	m.RetrievalSearch("hybrid", true)

	require.NoError(t, testutil.CollectAndCompare(m.Registry(), strings.NewReader(`
# HELP agentd_tool_invocations_total Tool calls by outcome (succeeded, failed, rejected, replayed).
# TYPE agentd_tool_invocations_total counter
agentd_tool_invocations_total{outcome="rejected",tool="search_corpus"} 1
agentd_tool_invocations_total{outcome="succeeded",tool="run_python"} 1
`), "agentd_tool_invocations_total"))

	// A rejected call never reached a tool, so it has no duration to record:
	// one series, from the one call that ran.
	require.Equal(t, 1, testutil.CollectAndCount(m.Registry(), "agentd_tool_duration_seconds"))

	// ADR-7's promised counter, ADR-22's reason label, ADR-25's phase label.
	require.NoError(t, testutil.CollectAndCompare(m.Registry(), strings.NewReader(`
# HELP agentd_leases_reaped_total Runs returned to the queue by the expired-lease sweep (ADR-7).
# TYPE agentd_leases_reaped_total counter
agentd_leases_reaped_total 3
# HELP agentd_budget_terminations_total Runs stopped by the budget, by reason (would_exceed pre-flight, spent post-hoc).
# TYPE agentd_budget_terminations_total counter
agentd_budget_terminations_total{reason="would_exceed"} 1
# HELP agentd_cancellations_total Cancelled runs by the phase they were interrupted in (idle, model, tool).
# TYPE agentd_cancellations_total counter
agentd_cancellations_total{phase="tool"} 1
# HELP agentd_retrieval_searches_total Corpus searches by the mode that ran and whether the reranker degraded.
# TYPE agentd_retrieval_searches_total counter
agentd_retrieval_searches_total{degraded="true",mode="hybrid"} 1
# HELP agentd_sandbox_orphans_swept_total Sandbox containers removed by the age-based orphan sweep (ADR-16).
# TYPE agentd_sandbox_orphans_swept_total counter
agentd_sandbox_orphans_swept_total 2
# HELP agentd_sandbox_timeouts_total Sandboxed tool calls killed at their wall-clock limit.
# TYPE agentd_sandbox_timeouts_total counter
agentd_sandbox_timeouts_total 1
`), "agentd_leases_reaped_total", "agentd_budget_terminations_total", "agentd_cancellations_total",
		"agentd_retrieval_searches_total", "agentd_sandbox_orphans_swept_total", "agentd_sandbox_timeouts_total"))
}

func TestMetrics_HTTPAndStreams(t *testing.T) {
	m := NewMetrics()
	m.HTTPRequest("/v1/runs", http.MethodPost, http.StatusCreated)
	// An unmatched route has no pattern, and the raw path must never become
	// one: it carries the run id.
	m.HTTPRequest("", http.MethodGet, http.StatusNotFound)
	m.SSEStreamOpened()
	m.SSEStreamOpened()
	m.SSEStreamClosed()

	require.NoError(t, testutil.CollectAndCompare(m.Registry(), strings.NewReader(`
# HELP agentd_http_requests_total API requests by matched route pattern, method, and status code.
# TYPE agentd_http_requests_total counter
agentd_http_requests_total{code="201",method="POST",route="/v1/runs"} 1
agentd_http_requests_total{code="404",method="GET",route="other"} 1
# HELP agentd_sse_streams_active SSE event streams currently connected.
# TYPE agentd_sse_streams_active gauge
agentd_sse_streams_active 1
`), "agentd_http_requests_total", "agentd_sse_streams_active"))
}

func TestMetrics_NoLabelCarriesARunID(t *testing.T) {
	// The cardinality rule, asserted rather than asserted in a comment: a run
	// id in a label mints one series per run and eventually takes the process
	// down. Nothing below passes one, and the check is that no instrument has
	// a label *named* for one either — the shape a future call site would
	// most plausibly reach for.
	m := NewMetrics()
	m.RunFinished("succeeded")
	m.ModelCall("local", "qwen2.5:7b", ModelOutcomeOK, time.Second)
	m.ToolInvoked("finish", OutcomeSucceeded)
	m.HTTPRequest("/v1/runs/{id}", http.MethodGet, http.StatusOK)

	families, err := m.Registry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), "agentd_") {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, label := range metric.GetLabel() {
				require.NotContains(t, label.GetName(), "run_id", "metric %s", f.GetName())
				require.NotContains(t, label.GetName(), "goal", "metric %s", f.GetName())
			}
		}
	}
}

func TestRunStateCollector_OneSeriesPerStatus(t *testing.T) {
	m := NewMetrics()
	statuses := []string{"queued", "running", "succeeded", "failed", "cancelled", "budget_exceeded"}
	require.NoError(t, m.Register(NewRunStateCollector(statuses, func(context.Context) (RunState, error) {
		return RunState{
			Counts:       map[string]int64{"succeeded": 4, "queued": 2},
			OldestQueued: 90 * time.Second,
			HasQueued:    true,
		}, nil
	}, slog.Default())))

	// Statuses with no runs are explicit zeroes: a series that appears only
	// when something goes wrong is one every alert rule must handle twice.
	require.NoError(t, testutil.CollectAndCompare(m.Registry(), strings.NewReader(`
# HELP agentd_runs Runs in each status, from the database.
# TYPE agentd_runs gauge
agentd_runs{status="budget_exceeded"} 0
agentd_runs{status="cancelled"} 0
agentd_runs{status="failed"} 0
agentd_runs{status="queued"} 2
agentd_runs{status="running"} 0
agentd_runs{status="succeeded"} 4
# HELP agentd_runs_oldest_queued_age_seconds Age of the oldest queued run; absent when the queue is empty.
# TYPE agentd_runs_oldest_queued_age_seconds gauge
agentd_runs_oldest_queued_age_seconds 90
`), "agentd_runs", "agentd_runs_oldest_queued_age_seconds"))
}

func TestRunStateCollector_EmptyQueueReportsNoAge(t *testing.T) {
	// Zero is what a run submitted this instant reports, so an empty queue
	// must be absent instead: an alert on "oldest queued run older than five
	// minutes" must not be able to read an empty queue as a fresh one.
	m := NewMetrics()
	require.NoError(t, m.Register(NewRunStateCollector([]string{"queued"}, func(context.Context) (RunState, error) {
		return RunState{Counts: map[string]int64{}}, nil
	}, slog.Default())))

	require.Equal(t, 0, testutil.CollectAndCount(m.Registry(), "agentd_runs_oldest_queued_age_seconds"))
	require.Equal(t, 1, testutil.CollectAndCount(m.Registry(), "agentd_runs"))
}

func TestRunStateCollector_UnknownStatusIsStillReported(t *testing.T) {
	// A status the caller did not name still counts: silently dropping runs
	// from a total because someone added an enum value is worse than an
	// unexpected series.
	m := NewMetrics()
	require.NoError(t, m.Register(NewRunStateCollector([]string{"queued"}, func(context.Context) (RunState, error) {
		return RunState{Counts: map[string]int64{"queued": 1, "paused": 3}}, nil
	}, slog.Default())))

	require.Equal(t, 2, testutil.CollectAndCount(m.Registry(), "agentd_runs"))
}

func TestRunStateCollector_FailedQueryKeepsTheScrapeAlive(t *testing.T) {
	// A database blip must not take the process counters down with it: an
	// invalid metric would fail the whole scrape, and the counters that still
	// work are the ones being scraped for.
	m := NewMetrics()
	m.RunFinished("succeeded")
	require.NoError(t, m.Register(NewRunStateCollector([]string{"queued"}, func(context.Context) (RunState, error) {
		return RunState{}, errors.New("postgres is down")
	}, slog.New(slog.DiscardHandler))))

	body := expose(t, m)
	require.Contains(t, body, `agentd_runs_finished_total{status="succeeded"} 1`)
	require.NotContains(t, body, "agentd_runs{")
}
