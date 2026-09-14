package evals_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/evals"
	"github.com/shreyasprasad/agentd/internal/model/cassette"
	"github.com/shreyasprasad/agentd/internal/testutil"
	"github.com/shreyasprasad/agentd/internal/tools"
	"github.com/shreyasprasad/agentd/internal/tools/builtin"
)

// suiteYAML is two cases that need no corpus, no sandbox, and no model: a
// plain run, and the crash demo as a fixture.
const suiteYAML = `
defaults:
  budget_usd: "1.00"
  max_steps: 6
  timeout: 90s
thresholds:
  SMOKE: {pass_rate: 1.00, escalations: 0}
  RESILIENCE: {pass_rate: 1.00, escalations: 0}
  SAFETY: {pass_rate: 1.00}
cases:
  - id: t-smoke
    category: SMOKE
    goal: Compute the deadline 30 weekdays after 2026-09-03 and finish.
    tools: [compute_deadline, finish]
    script:
      - tool: compute_deadline
        args: {start_date: "2026-09-03", days: 30, skip_weekends: true}
      - tool: finish
        args: {answer: "The deadline is 2026-10-15."}
    assert:
      status: succeeded
      max_steps: 2
      tools_called: [compute_deadline, finish]
      answer_contains: ["2026-10-15"]
      exactly_once: true

  - id: t-resilience
    category: RESILIENCE
    goal: Compute the deadline 30 weekdays after 2026-09-03 and finish.
    tools: [compute_deadline, finish]
    tool_delay_ms: 2000
    chaos: {kill_after: tool_requested, matching: compute_deadline}
    script:
      - tool: compute_deadline
        args: {start_date: "2026-09-03", days: 30, skip_weekends: true}
      - tool: finish
        args: {answer: "The deadline is 2026-10-15."}
    assert:
      status: succeeded
      answer_contains: ["2026-10-15"]
      exactly_once: true
      events_not_contain: [tool_failed]

  - id: t-needs-docker
    category: SAFETY
    goal: Run a script.
    tools: [run_python, finish]
    requires: [docker]
    script:
      - tool: finish
        args: {answer: "never reached"}
    assert:
      status: succeeded
`

type harness struct {
	t         *testing.T
	suites    []*evals.Suite
	cassettes string
	registry  *tools.Registry
	store     interface{}
	cfg       evals.Config
}

func newHarness(t *testing.T, yaml string) *harness {
	t.Helper()
	s, err := evals.ReadSuite(strings.NewReader(yaml), "test.yaml")
	require.NoError(t, err)
	st := testutil.Postgres(t)
	return &harness{
		t:         t,
		suites:    []*evals.Suite{s},
		cassettes: t.TempDir(),
		registry:  tools.NewRegistry().MustRegister(builtin.Finish{}, builtin.ComputeDeadline{}),
		cfg: evals.Config{
			Store:        st,
			DefaultModel: "fake-model",
			// Docker is deliberately absent: the SAFETY case must skip rather
			// than fail, which is what `make eval` does on a machine with no
			// daemon.
			Capabilities: map[string]bool{},
		},
	}
}

func (h *harness) run(backend evals.Backend) []evals.Result {
	h.t.Helper()
	cfg := h.cfg
	cfg.Registry = h.registry
	cfg.Backend = backend
	results, err := evals.NewRunner(cfg).Run(context.Background(), h.suites)
	require.NoError(h.t, err)
	return results
}

func (h *harness) record() []evals.Result {
	return h.run(&evals.CassetteBackend{
		Dir: h.cassettes, OnMiss: cassette.MissRecord, Script: true, Fresh: true, Model: "fake-model",
	})
}

func (h *harness) replay() []evals.Result {
	return h.run(&evals.CassetteBackend{Dir: h.cassettes, OnMiss: cassette.MissFail})
}

func byID(results []evals.Result) map[string]evals.Result {
	out := map[string]evals.Result{}
	for _, r := range results {
		out[r.Case] = r
	}
	return out
}

// TestRunnerRecordsThenReplays is the harness end to end: a suite recorded
// from its scripts, then replayed from the files with no model reachable at
// all — which is what `make eval` is.
func TestRunnerRecordsThenReplays(t *testing.T) {
	h := newHarness(t, suiteYAML)

	recorded := byID(h.record())
	require.Equal(t, evals.Pass, recorded["t-smoke"].Outcome, "%v", recorded["t-smoke"].Failures)
	require.Equal(t, evals.Pass, recorded["t-resilience"].Outcome, "%v", recorded["t-resilience"].Failures)

	replayed := byID(h.replay())
	require.Equal(t, evals.Pass, replayed["t-smoke"].Outcome, "%v", replayed["t-smoke"].Failures)
	require.Equal(t, 2, replayed["t-smoke"].Steps)
	require.Equal(t, "succeeded", replayed["t-smoke"].Status)
	require.Equal(t, "t-smoke.json", replayed["t-smoke"].Cassette)

	// The crash case is the one worth being sure about: a second worker took
	// the run over after the first was torn down mid-tool-call, and the two
	// workers' model calls came out of one cassette because it is keyed by
	// request rather than by call order.
	res := replayed["t-resilience"]
	require.Equal(t, evals.Pass, res.Outcome, "%v", res.Failures)
	require.Empty(t, res.Escalations)
}

// TestRunnerSkipsWhatItCannotRun keeps the scorecard honest about capability:
// a skip is neither evidence for nor against, and it must not fail the suite.
func TestRunnerSkipsWhatItCannotRun(t *testing.T) {
	h := newHarness(t, suiteYAML)
	results := byID(h.record())

	res := results["t-needs-docker"]
	require.Equal(t, evals.Skip, res.Outcome)
	require.Contains(t, res.Reason, "docker")

	rep := evals.Summarize("record", h.record(), h.suites[0].Thresholds, 0)
	require.True(t, rep.OK(), "a skipped case must not fail the suite: %v", rep.Failures)
}

// TestReplayFailsLoudlyOnDrift is exit criterion 2. A cassette that has fallen
// behind the loop must fail the case with a diff, never quietly return a stale
// answer and never reach for a live provider.
func TestReplayFailsLoudlyOnDrift(t *testing.T) {
	h := newHarness(t, suiteYAML)
	h.record()

	// The goal is part of the conversation, so changing it changes what the
	// model was asked — exactly as editing the system prompt would.
	h.suites[0].Cases[0].Goal = "Compute something entirely different and finish."

	res := byID(h.replay())["t-smoke"]
	require.Equal(t, evals.Fail, res.Outcome)
	joined := strings.Join(res.Failures, "\n")
	require.Contains(t, joined, "has no entry for this request")
	require.Contains(t, joined, "eval-record")
}

// TestReplayOfAMissingCassetteIsAFailureNotASilentPass covers the other
// direction: a case nobody has recorded yet.
func TestReplayOfAMissingCassetteIsAFailureNotASilentPass(t *testing.T) {
	h := newHarness(t, suiteYAML)
	res := byID(h.replay())["t-smoke"]
	require.Equal(t, evals.Fail, res.Outcome)
	require.Contains(t, strings.Join(res.Failures, "\n"), "no cassette for this case")
}

// TestRunnerReportsAFailedAssertion proves the harness can go red for the
// ordinary reason, not only for infrastructure problems.
func TestRunnerReportsAFailedAssertion(t *testing.T) {
	h := newHarness(t, suiteYAML)
	h.record()
	h.suites[0].Cases[0].Assert.AnswerContains = []string{"a date that is not in the answer"}

	res := byID(h.replay())["t-smoke"]
	require.Equal(t, evals.Fail, res.Outcome)
	require.Contains(t, strings.Join(res.Failures, "\n"), "does not contain")
}

// TestOnlyRunsTheNamedCase backs -case, which is how a single red row is
// iterated on without paying for the whole suite.
func TestOnlyRunsTheNamedCase(t *testing.T) {
	h := newHarness(t, suiteYAML)
	h.cfg.Only = map[string]bool{"t-smoke": true}
	results := h.record()
	require.Len(t, results, 1)
	require.Equal(t, "t-smoke", results[0].Case)
}
