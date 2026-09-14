package telemetry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is one process's Prometheus instruments and the registry they are
// registered in.
//
// Metrics go through client_golang while traces go through OpenTelemetry
// (ADR-26). The deciding factor is the Postgres-backed gauge below:
// prometheus.Collector is the abstraction built for a scrape-time read from
// an external source of truth, and the event half of the problem is commodity
// in either library.
//
// The registry is per-Metrics rather than prometheus.DefaultRegisterer, which
// is what makes two workers in one test process possible: a package-level
// MustRegister would panic the second time it ran. For the same reason there
// are no package-level instruments — everything is reached through a *Metrics
// that its owner constructs and injects.
//
// Every method is safe on a nil *Metrics, and nil is what the loop, the
// worker, the sandbox, and the searcher get in tests and in the commands that
// serve no /metrics. Call sites therefore record unconditionally instead of
// branching on whether metrics are on.
type Metrics struct {
	reg *prometheus.Registry

	runsFinished *prometheus.CounterVec
	runDuration  *prometheus.HistogramVec
	runsClaimed  *prometheus.CounterVec
	steps        prometheus.Counter

	modelCalls   *prometheus.CounterVec
	modelTokens  *prometheus.CounterVec
	modelCost    *prometheus.CounterVec
	modelLatency *prometheus.HistogramVec

	toolInvocations *prometheus.CounterVec
	toolDuration    *prometheus.HistogramVec

	sandboxTimeouts prometheus.Counter
	sandboxOrphans  prometheus.Counter

	retrievalSearches *prometheus.CounterVec
	retrievalDuration *prometheus.HistogramVec

	leasesReaped prometheus.Counter
	leasesLost   prometheus.Counter

	budgetTerminations *prometheus.CounterVec
	cancellations      *prometheus.CounterVec

	httpRequests *prometheus.CounterVec
	sseStreams   prometheus.Gauge
}

// Outcomes for agentd_model_calls_total. One increment per *attempt*, so a
// call that succeeded on its third try records two "retried" and one "ok" —
// which is what makes the retry rate a rate rather than something hidden
// inside a success.
const (
	// ModelOutcomeOK is an attempt that returned a usable response.
	ModelOutcomeOK = "ok"
	// ModelOutcomeRetried is an attempt that failed and will be tried again.
	ModelOutcomeRetried = "retried"
	// ModelOutcomeFailed is the last attempt of a call that gave up.
	ModelOutcomeFailed = "failed"
	// ModelOutcomeNonRetryable is an attempt the loop refused to repeat: a
	// refusal, an unknown model, a bad key (ADR-27). Watching this rather
	// than "failed" is how ADR-12's promise comes due — three attempts burnt
	// on an error that could not succeed is now a visible number.
	ModelOutcomeNonRetryable = "non_retryable"
)

// Token kinds for agentd_model_tokens_total.
const (
	TokenKindInput      = "input"
	TokenKindOutput     = "output"
	TokenKindCacheRead  = "cache_read"
	TokenKindCacheWrite = "cache_write"
)

// RouteOther is the route label for a request that matched no route. The raw
// path must never become a label: /v1/runs/<uuid> would mint a new time
// series per run and take the process down with it, which is the one failure
// mode of a metrics layer that is worse than having none.
const RouteOther = "other"

// callBuckets are the latency buckets for model, tool, and retrieval calls.
//
// They are set explicitly because prometheus.DefBuckets tops out at 10
// seconds, and nothing here is a web request: the local reranker takes 10-30s
// per search, sandboxed scripts run to 120s, and -model-timeout defaults to
// ten minutes. With the defaults almost every observation would land in +Inf
// and the histograms would be decorative.
var callBuckets = []float64{.1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// runBuckets cover a whole run, which is minutes of model calls and tool
// calls plus however long it waited in the queue, so they start where
// callBuckets are still counting and end past the step limit.
var runBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600}

// NewMetrics builds the instruments and registers them.
//
// The Go and process collectors come along: a worker's goroutine count and
// heap are the first things anyone asks about when runs get slow, and they
// cost one registration each.
func NewMetrics() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),

		runsFinished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentd_runs_finished_total",
			Help: "Runs that reached a terminal event, by terminal status.",
		}, []string{"status"}),
		runDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "agentd_run_duration_seconds",
			Help:    "Wall time from submission to terminal event, including queue wait.",
			Buckets: runBuckets,
		}, []string{"status"}),
		runsClaimed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentd_runs_claimed_total",
			Help: "Run claims by a worker; resumed=true means the run already had events.",
		}, []string{"resumed"}),
		steps: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "agentd_steps_total",
			Help: "Loop iterations executed.",
		}),

		modelCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentd_model_calls_total",
			Help: "Model call attempts by outcome (ok, retried, failed, non_retryable).",
		}, []string{"provider", "model", "outcome"}),
		modelTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentd_model_tokens_total",
			Help: "Tokens reported by the provider, by kind (input, output, cache_read, cache_write).",
		}, []string{"provider", "model", "kind"}),
		modelCost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentd_model_cost_micro_usd_total",
			Help: "Model spend in micro-USD, the same integer unit the budget is kept in (ADR-6).",
		}, []string{"provider", "model"}),
		modelLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "agentd_model_latency_seconds",
			Help:    "Duration of one model call attempt.",
			Buckets: callBuckets,
		}, []string{"provider", "model"}),

		toolInvocations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentd_tool_invocations_total",
			Help: "Tool calls by outcome (succeeded, failed, rejected, replayed).",
		}, []string{"tool", "outcome"}),
		toolDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "agentd_tool_duration_seconds",
			Help:    "Duration of a tool call that actually executed.",
			Buckets: callBuckets,
		}, []string{"tool"}),

		sandboxTimeouts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "agentd_sandbox_timeouts_total",
			Help: "Sandboxed tool calls killed at their wall-clock limit.",
		}),
		sandboxOrphans: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "agentd_sandbox_orphans_swept_total",
			Help: "Sandbox containers removed by the age-based orphan sweep (ADR-16).",
		}),

		retrievalSearches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentd_retrieval_searches_total",
			Help: "Corpus searches by the mode that ran and whether the reranker degraded.",
		}, []string{"mode", "degraded"}),
		retrievalDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "agentd_retrieval_duration_seconds",
			Help:    "Duration of one retrieval stage (search, embed, vector, lexical, rerank).",
			Buckets: callBuckets,
		}, []string{"stage"}),

		leasesReaped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "agentd_leases_reaped_total",
			Help: "Runs returned to the queue by the expired-lease sweep (ADR-7).",
		}),
		leasesLost: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "agentd_leases_lost_total",
			Help: "Attempts that stopped because another worker had taken the run.",
		}),

		budgetTerminations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentd_budget_terminations_total",
			Help: "Runs stopped by the budget, by reason (would_exceed pre-flight, spent post-hoc).",
		}, []string{"reason"}),
		cancellations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentd_cancellations_total",
			Help: "Cancelled runs by the phase they were interrupted in (idle, model, tool).",
		}, []string{"phase"}),

		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentd_http_requests_total",
			Help: "API requests by matched route pattern, method, and status code.",
		}, []string{"route", "method", "code"}),
		sseStreams: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "agentd_sse_streams_active",
			Help: "SSE event streams currently connected.",
		}),
	}

	m.reg.MustRegister(
		m.runsFinished, m.runDuration, m.runsClaimed, m.steps,
		m.modelCalls, m.modelTokens, m.modelCost, m.modelLatency,
		m.toolInvocations, m.toolDuration,
		m.sandboxTimeouts, m.sandboxOrphans,
		m.retrievalSearches, m.retrievalDuration,
		m.leasesReaped, m.leasesLost,
		m.budgetTerminations, m.cancellations,
		m.httpRequests, m.sseStreams,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Registry is the registry these instruments live in. It is exported for the
// Postgres-backed collector and for tests; nothing else should need it.
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.reg
}

// Register adds a collector — in practice the run-state collector — to this
// process's registry.
func (m *Metrics) Register(cs ...prometheus.Collector) error {
	if m == nil {
		return nil
	}
	for _, c := range cs {
		if err := m.reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}

// Handler serves the exposition text. A nil *Metrics serves 404 rather than
// an empty body, because a /metrics that answers 200 with nothing is a
// scrape target that looks healthy and reports no runs.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		// A broken collector should show up as a failed scrape rather than
		// as a silently truncated exposition.
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// RunClaimed counts one worker claim. resumed is true when the run already
// had events, which is the crash-resume path.
func (m *Metrics) RunClaimed(resumed bool) {
	if m == nil {
		return
	}
	m.runsClaimed.WithLabelValues(strconv.FormatBool(resumed)).Inc()
}

// RunFinished counts a terminal event. It is separate from ObserveRunDuration
// because the two are known at different moments: the status is what was just
// written, while the duration needs the run's created_at, which is one more
// read and can fail.
func (m *Metrics) RunFinished(status string) {
	if m == nil {
		return
	}
	m.runsFinished.WithLabelValues(status).Inc()
}

// ObserveRunDuration records submission-to-termination wall time. Callers that
// cannot establish created_at skip it rather than observing zero, which would
// put a lie in the fastest bucket.
func (m *Metrics) ObserveRunDuration(status string, d time.Duration) {
	if m == nil {
		return
	}
	m.runDuration.WithLabelValues(status).Observe(d.Seconds())
}

// StepStarted counts one loop iteration.
func (m *Metrics) StepStarted() {
	if m == nil {
		return
	}
	m.steps.Inc()
}

// ModelCall records one attempt: its outcome and how long the provider took.
// The latency is per attempt rather than per call so that a slow provider and
// a retried one stay distinguishable.
func (m *Metrics) ModelCall(provider, model, outcome string, d time.Duration) {
	if m == nil {
		return
	}
	m.modelCalls.WithLabelValues(provider, model, outcome).Inc()
	m.modelLatency.WithLabelValues(provider, model).Observe(d.Seconds())
}

// ModelUsage records the tokens and cost of one response. Cache counts are
// separate kinds rather than folded into input, because they are priced
// differently and the difference is the point of caching.
func (m *Metrics) ModelUsage(provider, model string, input, output, cacheRead, cacheWrite, costMicroUSD int64) {
	if m == nil {
		return
	}
	for kind, n := range map[string]int64{
		TokenKindInput:      input,
		TokenKindOutput:     output,
		TokenKindCacheRead:  cacheRead,
		TokenKindCacheWrite: cacheWrite,
	} {
		if n > 0 {
			m.modelTokens.WithLabelValues(provider, model, kind).Add(float64(n))
		}
	}
	if costMicroUSD > 0 {
		m.modelCost.WithLabelValues(provider, model).Add(float64(costMicroUSD))
	}
}

// ToolInvoked counts one tool call. outcome is one of the Outcome constants
// in this package, so a span and a counter describe the same call the same
// way.
func (m *Metrics) ToolInvoked(tool, outcome string) {
	if m == nil {
		return
	}
	m.toolInvocations.WithLabelValues(tool, outcome).Inc()
}

// ObserveToolDuration records how long a tool that actually ran took. A
// rejected or replayed call has no duration worth recording: one never
// reached a tool and the other was read back from the ledger.
func (m *Metrics) ObserveToolDuration(tool string, d time.Duration) {
	if m == nil {
		return
	}
	m.toolDuration.WithLabelValues(tool).Observe(d.Seconds())
}

// SandboxTimedOut counts a container killed at its wall-clock limit.
func (m *Metrics) SandboxTimedOut() {
	if m == nil {
		return
	}
	m.sandboxTimeouts.Inc()
}

// SandboxOrphansSwept counts containers removed by the boot-time and
// periodic sweeps.
func (m *Metrics) SandboxOrphansSwept(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.sandboxOrphans.Add(float64(n))
}

// RetrievalSearch counts one search by the mode that actually ran. A degraded
// rerank reports the fused mode and degraded=true, exactly as the result and
// the span do.
func (m *Metrics) RetrievalSearch(mode string, degraded bool) {
	if m == nil {
		return
	}
	m.retrievalSearches.WithLabelValues(mode, strconv.FormatBool(degraded)).Inc()
}

// ObserveRetrievalStage records one stage's duration. The stage names are the
// span names without their prefix, so the histogram and the waterfall answer
// the same question at two resolutions.
func (m *Metrics) ObserveRetrievalStage(stage string, d time.Duration) {
	if m == nil {
		return
	}
	m.retrievalDuration.WithLabelValues(stage).Observe(d.Seconds())
}

// LeasesReaped counts runs the sweep returned to the queue. This is the
// counter ADR-7 promised when it kept a reaper whose only observable effect
// was a log line.
func (m *Metrics) LeasesReaped(n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.leasesReaped.Add(float64(n))
}

// LeaseLost counts an attempt that stopped because another worker owns the
// run. It is recorded once per attempt, by the worker that lost it.
func (m *Metrics) LeaseLost() {
	if m == nil {
		return
	}
	m.leasesLost.Inc()
}

// BudgetTermination counts a run stopped by its budget. The reason separates
// the pre-flight ceiling from the post-hoc backstop, which is the difference
// between a budget that held and one that was already exceeded (ADR-22).
func (m *Metrics) BudgetTermination(reason string) {
	if m == nil {
		return
	}
	m.budgetTerminations.WithLabelValues(reason).Inc()
}

// Cancellation counts a cancelled run by the phase it was interrupted in.
// The split is the point: cancels landing in "idle" mean the interrupt is not
// reaching in-flight calls, which is the whole of ADR-25.
func (m *Metrics) Cancellation(phase string) {
	if m == nil {
		return
	}
	m.cancellations.WithLabelValues(phase).Inc()
}

// HTTPRequest counts one API request. route must be a matched route pattern
// ("/v1/runs/{id}"), never a raw path.
func (m *Metrics) HTTPRequest(route, method string, code int) {
	if m == nil {
		return
	}
	if route == "" {
		route = RouteOther
	}
	m.httpRequests.WithLabelValues(route, method, strconv.Itoa(code)).Inc()
}

// SSEStreamOpened and SSEStreamClosed move the gauge of connected streams.
func (m *Metrics) SSEStreamOpened() {
	if m == nil {
		return
	}
	m.sseStreams.Inc()
}

// SSEStreamClosed is the deferred half of SSEStreamOpened.
func (m *Metrics) SSEStreamClosed() {
	if m == nil {
		return
	}
	m.sseStreams.Dec()
}
