package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/telemetry"
	"github.com/shreyasprasad/agentd/internal/tools"
	"github.com/shreyasprasad/agentd/internal/tools/builtin"
)

// counter reads one series out of a registry. The instruments are nil-safe by
// design, which is what keeps the call sites one line each — and also what
// makes a *missing* call site invisible to every other test in this package.
// These tests exist to catch that: they assert the numbers a real run moves.
func counter(t *testing.T, m *telemetry.Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := m.Registry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, metric := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range metric.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			matched := true
			for k, v := range labels {
				if got[k] != v {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
			if c := metric.GetCounter(); c != nil {
				return c.GetValue()
			}
			return metric.GetGauge().GetValue()
		}
	}
	return 0
}

// TestMetricsCountAWholeRun is the wiring test for the loop's instruments: a
// scripted two-step run, and every counter the exit criteria name moving by
// the amount that run should have moved it.
func TestMetricsCountAWholeRun(t *testing.T) {
	f := newFixture(t, fake.New(
		deadlineCall("t1", 30),
		finishCall("t2", "counted"),
	).WithPrice(model.Price{InputPerMTok: 3 * model.MicroUSD, OutputPerMTok: 15 * model.MicroUSD}))
	f.metrics = telemetry.NewMetrics()

	id := f.submit("count a whole run", runOpts{tools: []string{builtin.DeadlineName, builtin.FinishName}})
	stop := f.startWorker("w-metrics", 30*time.Second)
	run := f.waitTerminal(id)
	// The terminal counters are written by finish, which returns before the
	// wait above sees the row; stopping the worker first makes the read
	// deterministic rather than racing the last increments.
	stop()
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	m := f.metrics
	require.Equal(t, 1.0, counter(t, m, "agentd_runs_claimed_total", map[string]string{"resumed": "false"}))
	require.Equal(t, 1.0, counter(t, m, "agentd_runs_finished_total", map[string]string{"status": "succeeded"}))
	require.Equal(t, 1, testutil.CollectAndCount(m.Registry(), "agentd_run_duration_seconds"))

	// Four iterations for two model calls: each call is followed by an
	// iteration that drains the tool it asked for.
	require.Equal(t, 4.0, counter(t, m, "agentd_steps_total", nil))
	require.Equal(t, 2.0, counter(t, m, "agentd_model_calls_total",
		map[string]string{"provider": "fake", "model": "fake-model", "outcome": telemetry.ModelOutcomeOK}))

	// The token and dollar counters agree with the run's own columns, which
	// is the invariant that makes either worth reading.
	require.Equal(t, float64(run.InputTokens), counter(t, m, "agentd_model_tokens_total",
		map[string]string{"kind": telemetry.TokenKindInput, "model": "fake-model"}))
	require.Equal(t, float64(run.OutputTokens), counter(t, m, "agentd_model_tokens_total",
		map[string]string{"kind": telemetry.TokenKindOutput, "model": "fake-model"}))
	require.Positive(t, counter(t, m, "agentd_model_cost_micro_usd_total", map[string]string{"model": "fake-model"}))

	require.Equal(t, 1.0, counter(t, m, "agentd_tool_invocations_total",
		map[string]string{"tool": builtin.DeadlineName, "outcome": telemetry.OutcomeSucceeded}))
	require.Equal(t, 1.0, counter(t, m, "agentd_tool_invocations_total",
		map[string]string{"tool": builtin.FinishName, "outcome": telemetry.OutcomeSucceeded}))
	require.Equal(t, 2, testutil.CollectAndCount(m.Registry(), "agentd_tool_duration_seconds"))
}

// TestMetricsCountToolSpendUnderItsOwnModel covers ADR-23's half of the cost
// counters: a tool that called a model inside itself reaches the same dollar
// counter a loop call does, so summing the metric and summing spent_usd give
// the same answer.
func TestMetricsCountToolSpendUnderItsOwnModel(t *testing.T) {
	f := newFixture(t, fake.New(
		fake.ToolUse("t1", "costly", map[string]any{}, usage),
		finishCall("t2", "paid for"),
	))
	f.costly.cost = tools.Cost{MicroUSD: 4200, InputTokens: 11000, OutputTokens: 500, Model: "rerank-model"}
	f.metrics = telemetry.NewMetrics()

	id := f.submit("spend inside a tool", runOpts{tools: []string{"costly", builtin.FinishName}})
	stop := f.startWorker("w-toolcost", 30*time.Second)
	run := f.waitTerminal(id)
	stop()
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	m := f.metrics
	require.Equal(t, 4200.0, counter(t, m, "agentd_model_cost_micro_usd_total",
		map[string]string{"model": "rerank-model"}), "the tool's spend is filed under the model it named")
	require.Equal(t, 11000.0, counter(t, m, "agentd_model_tokens_total",
		map[string]string{"kind": telemetry.TokenKindInput, "model": "rerank-model"}))
	// The fake model is priced at zero here, so the run's whole spend is the
	// tool's — and the counter and the column agree on it.
	require.Equal(t, "0.0042", run.SpentUSD)
}

// TestMetricsCountBudgetTerminations pins the label that distinguishes a
// budget that held from one that was already over: a run refused before the
// call is would_exceed, and it is the number `make demo-budget` demonstrates.
func TestMetricsCountBudgetTerminations(t *testing.T) {
	f := newFixture(t, fake.New(
		deadlineCall("t1", 30),
		finishCall("t2", "never reached"),
	).WithPrice(model.Price{InputPerMTok: 3000 * model.MicroUSD, OutputPerMTok: 15000 * model.MicroUSD}).
		WithMaxOutputTokens(4096))
	f.metrics = telemetry.NewMetrics()

	// A budget too small for even the first call's worst case.
	id := f.submit("priced out before the first call", runOpts{budget: "0.01"})
	stop := f.startWorker("w-budget", 30*time.Second)
	run := f.waitTerminal(id)
	stop()

	require.Equal(t, runtime.StatusBudgetExceeded, run.Status)
	require.Equal(t, 1.0, counter(t, f.metrics, "agentd_budget_terminations_total",
		map[string]string{"reason": runtime.BudgetReasonWouldExceed}))
	require.Equal(t, 0.0, counter(t, f.metrics, "agentd_budget_terminations_total",
		map[string]string{"reason": runtime.BudgetReasonSpent}))
	require.Equal(t, 1.0, counter(t, f.metrics, "agentd_runs_finished_total",
		map[string]string{"status": runtime.StatusBudgetExceeded}))
	// Nothing was spent, because nothing was called: that is the difference
	// between a ceiling and a receipt (ADR-22).
	require.Equal(t, 0, f.provider.Calls())
}

// TestMetricsCountCancellationsByPhase is the counter that says cancel really
// interrupts. A cancel recorded in "idle" would mean the run stopped at a step
// boundary it would never have reached, which is the behaviour ADR-25
// replaced — so the phase is the assertion, not the count.
func TestMetricsCountCancellationsByPhase(t *testing.T) {
	f := newFixture(t, fake.New(slowCall("t1")))
	f.metrics = telemetry.NewMetrics()
	stop := f.startWorker("w-cancel", 10*time.Second)
	defer stop()

	id := f.submit("cancel me mid-tool", runOpts{})
	f.slow.waitStarted(t)
	ok, err := f.st.RequestCancel(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)

	run := f.waitTerminal(id)
	stop()
	require.Equal(t, runtime.StatusCancelled, run.Status)

	m := f.metrics
	require.Equal(t, 1.0, counter(t, m, "agentd_cancellations_total",
		map[string]string{"phase": runtime.CancelPhaseTool}))
	require.Equal(t, 1.0, counter(t, m, "agentd_runs_finished_total",
		map[string]string{"status": runtime.StatusCancelled}))
	// The torn-off call is counted as a failure, matching its tool_failed,
	// but contributes no duration: a call cut off partway through says
	// nothing about how long that tool takes.
	require.Equal(t, 1.0, counter(t, m, "agentd_tool_invocations_total",
		map[string]string{"tool": "slow", "outcome": telemetry.OutcomeFailed}))
	require.Equal(t, 0, testutil.CollectAndCount(m.Registry(), "agentd_tool_duration_seconds"))
}

// TestMetricsCountResumedClaims covers the crash path's counter. A claim of a
// run that already has events is labelled resumed=true, which is how the
// hand-off in the crash demo shows up in a dashboard rather than only in a log
// line.
func TestMetricsCountResumedClaims(t *testing.T) {
	f := newFixture(t, fake.New(
		deadlineCall("t1", 30),
		slowCall("t2"),
		finishCall("t3", "done after resume"),
	))
	f.metrics = telemetry.NewMetrics()
	const lease = 2 * time.Second

	stopA := f.startWorker("w-a", lease)
	id := f.submit("crash then resume", runOpts{})
	f.slow.waitStarted(t) // the first tool call has committed; the second is in flight
	stopA()               // kill -9: the lease is left behind

	stopB := f.startWorker("w-b", lease)
	f.slow.waitStarted(t) // B has claimed and re-executed the interrupted call
	close(f.slow.release)
	run := f.waitTerminal(id)
	stopB()
	require.Equal(t, runtime.StatusSucceeded, run.Status)

	m := f.metrics
	require.Equal(t, 1.0, counter(t, m, "agentd_runs_claimed_total", map[string]string{"resumed": "false"}),
		"worker A claimed a run with an empty log")
	require.Equal(t, 1.0, counter(t, m, "agentd_runs_claimed_total", map[string]string{"resumed": "true"}),
		"worker B claimed a run with events already in it")
	// One run, one terminal event, however many workers it took: the counter
	// is per run rather than per attempt.
	require.Equal(t, 1.0, counter(t, m, "agentd_runs_finished_total", map[string]string{"status": "succeeded"}))
}
