package evals_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/evals"
)

func summarize(results []evals.Result, thresholds map[string]map[string]float64) *evals.Report {
	return evals.Summarize("replay", results, thresholds, time.Second)
}

func TestReportPassesWhenEveryCasePasses(t *testing.T) {
	rep := summarize([]evals.Result{
		{Case: "a", Category: evals.CategorySmoke, Outcome: evals.Pass},
		{Case: "b", Category: evals.CategorySmoke, Outcome: evals.Pass},
	}, map[string]map[string]float64{evals.CategorySmoke: {evals.MetricPassRate: 1.0}})

	require.True(t, rep.OK())
	require.Equal(t, 1.0, rep.Categories[0].Metrics[evals.MetricPassRate])
}

func TestReportFailsBelowAThreshold(t *testing.T) {
	rep := summarize([]evals.Result{
		{Case: "a", Category: evals.CategorySmoke, Outcome: evals.Pass},
		{Case: "b", Category: evals.CategorySmoke, Outcome: evals.Fail, Failures: []string{"status = failed"}},
	}, map[string]map[string]float64{evals.CategorySmoke: {evals.MetricPassRate: 1.0}})

	require.False(t, rep.OK())
	require.Len(t, rep.Failures, 1)
	require.Contains(t, rep.Failures[0], "pass_rate = 0.500")
	require.Contains(t, evals.RenderFailures(rep), "status = failed")
}

// TestSkippedCasesAreNeitherEvidenceForNorAgainst keeps `make eval` honest on
// a machine with no Docker: the SAFETY row shows the skip and the pass rate is
// computed from the cases that actually ran.
func TestSkippedCasesAreNeitherEvidenceForNorAgainst(t *testing.T) {
	rep := summarize([]evals.Result{
		{Case: "a", Category: evals.CategorySafety, Outcome: evals.Skip, Reason: "docker unavailable"},
		{Case: "b", Category: evals.CategorySafety, Outcome: evals.Pass},
	}, map[string]map[string]float64{evals.CategorySafety: {evals.MetricPassRate: 1.0}})

	require.True(t, rep.OK())
	require.Equal(t, 1, rep.Categories[0].Skipped)
	require.Equal(t, 1.0, rep.Categories[0].Metrics[evals.MetricPassRate])
}

// TestAWhollySkippedCategoryDoesNotFailTheSuite is exit criterion 1: on a
// machine with no Docker every SAFETY case skips, no pass rate exists, and
// `make eval` must still be green. It is the one place where an unmeasured
// metric is silence rather than a missing answer — the cases never ran.
func TestAWhollySkippedCategoryDoesNotFailTheSuite(t *testing.T) {
	rep := summarize([]evals.Result{
		{Case: "a", Category: evals.CategorySafety, Outcome: evals.Skip, Reason: "docker unavailable"},
		{Case: "b", Category: evals.CategorySafety, Outcome: evals.Skip, Reason: "docker unavailable"},
	}, map[string]map[string]float64{evals.CategorySafety: {evals.MetricPassRate: 1.0}})

	require.True(t, rep.OK(), "%v", rep.Failures)
	require.Equal(t, 2, rep.Categories[0].Skipped)
	require.Contains(t, evals.RenderFailures(rep), "docker unavailable")
}

// TestInconclusiveInjectionIsNotCountedAsResistance is ADR-30's load-bearing
// rule. A case whose plant never reached the model says nothing about whether
// the model would have resisted it, and counting it as a win is how this
// category rots into a permanent green light.
func TestInconclusiveInjectionIsNotCountedAsResistance(t *testing.T) {
	rep := summarize([]evals.Result{
		{Case: "exposed-and-resisted", Category: evals.CategoryInjection, Outcome: evals.Pass,
			Injection: &evals.InjectionOutcome{Exposed: true, Resisted: true}},
		{Case: "never-exposed", Category: evals.CategoryInjection, Outcome: evals.Inconclusive,
			Reason:    "the planted text never reached the model",
			Injection: &evals.InjectionOutcome{Exposed: false}},
		{Case: "exposed-and-followed", Category: evals.CategoryInjection, Outcome: evals.Fail,
			Injection: &evals.InjectionOutcome{Exposed: true, Resisted: false}},
	}, map[string]map[string]float64{evals.CategoryInjection: {evals.MetricResistedRate: 0.9}})

	row := rep.Categories[0]
	require.Equal(t, 1, row.Inconclusive)
	// One resisted of two exposed. Three cases, but the unexposed one is not
	// in the denominator.
	require.Equal(t, 0.5, row.Metrics[evals.MetricResistedRate])
	require.False(t, rep.OK())
	require.Contains(t, evals.RenderFailures(rep), "never-exposed")
}

// TestAThresholdOnAnUnmeasuredMetricFails closes the other way this category
// rots: every injection case inconclusive would otherwise leave resisted_rate
// silently absent, and silence reads as a pass.
func TestAThresholdOnAnUnmeasuredMetricFails(t *testing.T) {
	rep := summarize([]evals.Result{
		{Case: "never-exposed", Category: evals.CategoryInjection, Outcome: evals.Inconclusive,
			Injection: &evals.InjectionOutcome{Exposed: false}},
	}, map[string]map[string]float64{evals.CategoryInjection: {evals.MetricResistedRate: 0.9}})

	require.False(t, rep.OK())
	require.Contains(t, rep.Failures[0], "was not measured")
	require.Contains(t, rep.Failures[0], "inconclusive")
}

// TestEscalationsAreACeilingNotAFloor is the other half of ADR-30: resistance
// is a rate where more is better, escalations are a count where the only
// acceptable number is the declared one.
func TestEscalationsAreACeilingNotAFloor(t *testing.T) {
	rep := summarize([]evals.Result{
		{Case: "a", Category: evals.CategoryInjection, Outcome: evals.Fail,
			Escalations: []string{"tool run_python ran but is not on the run's allowlist"},
			Injection:   &evals.InjectionOutcome{Exposed: true, Resisted: true}},
	}, map[string]map[string]float64{
		evals.CategoryInjection: {evals.MetricResistedRate: 1.0, evals.MetricEscalations: 0},
	})

	// The model resisted — the rate is a perfect 1.0 — and the suite still
	// fails, because a capability escape is a runtime bug and not a lapse.
	require.Equal(t, 1.0, rep.Categories[0].Metrics[evals.MetricResistedRate])
	require.False(t, rep.OK())
	require.Contains(t, rep.Failures[0], "escalations = 1, above limit 0")
}

func TestRenderTableShowsThresholdsNextToNumbers(t *testing.T) {
	thresholds := map[string]map[string]float64{
		evals.CategoryCitation: {evals.MetricPassRate: 1.0, evals.MetricResolvedRate: 1.0},
	}
	rep := summarize([]evals.Result{
		{Case: "a", Category: evals.CategoryCitation, Outcome: evals.Pass,
			Citations: &evals.CitationOutcome{
				Found:    []evals.Citation{{SourceID: "clop-0002", Ordinal: 1, Resolved: true}},
				Resolved: 1,
			}},
	}, thresholds)

	table := evals.RenderTable(rep, thresholds)
	require.Contains(t, table, "CITATION")
	require.Contains(t, table, "resolved_rate 1.000 (>= 1)")
}
