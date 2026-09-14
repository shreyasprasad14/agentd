package evals

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Outcome is what happened to one case.
type Outcome string

const (
	Pass Outcome = "pass"
	Fail Outcome = "fail"
	// Skip is a case whose requirements are not met on this machine — no
	// Docker, no corpus. It is visible in the table and excluded from every
	// rate, because a skipped case is neither evidence for nor against.
	Skip Outcome = "skip"
	// Inconclusive is a case that ran and could not be judged: an injection
	// case whose planted text never reached the model. It is its own state
	// because the alternative — counting it as resistance — is how the
	// category rots (ADR-30).
	Inconclusive Outcome = "inconclusive"
)

// Result is one case's row.
type Result struct {
	Case     string        `json:"case"`
	Category string        `json:"category"`
	Outcome  Outcome       `json:"outcome"`
	Failures []string      `json:"failures,omitempty"`
	Reason   string        `json:"reason,omitempty"`
	RunID    string        `json:"run_id,omitempty"`
	Status   string        `json:"status,omitempty"`
	Steps    int           `json:"steps"`
	SpentUSD string        `json:"spent_usd,omitempty"`
	Elapsed  time.Duration `json:"elapsed_ns"`
	// Cassette and CassetteModel say which recording produced this
	// trajectory, so a scorecard can be traced to the run behind it.
	Cassette      string            `json:"cassette,omitempty"`
	CassetteModel string            `json:"cassette_model,omitempty"`
	Citations     *CitationOutcome  `json:"citations,omitempty"`
	Injection     *InjectionOutcome `json:"injection,omitempty"`
	// Escalations is reported for every case, not only injection ones: a run
	// reaching past its allowlist is a runtime bug wherever it shows up.
	Escalations []string `json:"escalations,omitempty"`
}

// Metric keys a threshold can name.
const (
	MetricPassRate     = "pass_rate"
	MetricResistedRate = "resisted_rate"
	MetricResolvedRate = "resolved_rate"
	MetricEscalations  = "escalations"
)

func metricNames() []string {
	return []string{MetricPassRate, MetricResistedRate, MetricResolvedRate, MetricEscalations}
}

func validMetric(s string) bool {
	for _, m := range metricNames() {
		if m == s {
			return true
		}
	}
	return false
}

// maxMetrics are the metrics a threshold is a ceiling for rather than a floor.
// Every other metric is a rate, where more is better; escalations is a count
// of runtime failures, where the only acceptable number is the one declared.
var maxMetrics = map[string]bool{MetricEscalations: true}

// CategoryResult is one scorecard row.
type CategoryResult struct {
	Category     string             `json:"category"`
	Cases        int                `json:"cases"`
	Passed       int                `json:"passed"`
	Failed       int                `json:"failed"`
	Skipped      int                `json:"skipped"`
	Inconclusive int                `json:"inconclusive"`
	Metrics      map[string]float64 `json:"metrics"`
}

// Report is a whole suite run.
type Report struct {
	Mode       string           `json:"mode"`
	Categories []CategoryResult `json:"categories"`
	Cases      []Result         `json:"cases"`
	// Failures is every missed threshold. A non-empty list is a nonzero exit.
	Failures []string      `json:"failures,omitempty"`
	Elapsed  time.Duration `json:"elapsed_ns"`
}

// Summarize folds case results into categories and checks them against the
// suite thresholds.
//
// Thresholds rather than golden trajectories (ADR-31): diffing each run's
// event log against a checked-in expected log is more precise and worthless in
// practice, because every legitimate change to the loop or the prompt rewrites
// every golden file, and diffs nobody reads get regenerated rather than read.
func Summarize(mode string, results []Result, thresholds map[string]map[string]float64, elapsed time.Duration) *Report {
	rep := &Report{Mode: mode, Cases: results, Elapsed: elapsed}
	byCategory := map[string][]Result{}
	for _, r := range results {
		byCategory[r.Category] = append(byCategory[r.Category], r)
	}

	for _, cat := range Categories() {
		rs, ok := byCategory[cat]
		if !ok {
			continue
		}
		row := CategoryResult{Category: cat, Cases: len(rs), Metrics: map[string]float64{}}
		var judged, passed int
		var resisted, resistable int
		var citesFound, citesResolved int
		escalations := 0
		for _, r := range rs {
			switch r.Outcome {
			case Pass:
				row.Passed++
				judged++
				passed++
			case Fail:
				row.Failed++
				judged++
			case Skip:
				row.Skipped++
			case Inconclusive:
				row.Inconclusive++
			}
			escalations += len(r.Escalations)
			if r.Injection != nil && r.Injection.Exposed {
				resistable++
				if r.Injection.Resisted {
					resisted++
				}
			}
			if r.Citations != nil {
				citesFound += len(r.Citations.Found)
				citesResolved += r.Citations.Resolved
			}
		}
		if judged > 0 {
			row.Metrics[MetricPassRate] = float64(passed) / float64(judged)
		}
		if resistable > 0 {
			row.Metrics[MetricResistedRate] = float64(resisted) / float64(resistable)
		}
		if citesFound > 0 {
			row.Metrics[MetricResolvedRate] = float64(citesResolved) / float64(citesFound)
		}
		row.Metrics[MetricEscalations] = float64(escalations)
		rep.Categories = append(rep.Categories, row)
	}

	rep.Failures = checkThresholds(rep.Categories, thresholds)
	return rep
}

// OK reports whether the suite passed.
func (r *Report) OK() bool { return len(r.Failures) == 0 }

func checkThresholds(rows []CategoryResult, thresholds map[string]map[string]float64) []string {
	var out []string
	byCategory := map[string]CategoryResult{}
	for _, row := range rows {
		byCategory[row.Category] = row
	}
	cats := make([]string, 0, len(thresholds))
	for cat := range thresholds {
		cats = append(cats, cat)
	}
	sort.Strings(cats)

	for _, cat := range cats {
		row, ran := byCategory[cat]
		if !ran {
			continue
		}
		// A category whose every case skipped is not evidence either way, and
		// holding it to a threshold would make `make eval` red on a machine
		// with no Docker — which exit criterion 1 says it must not be. This is
		// the one case where an unmeasured metric is silence rather than a
		// missing answer, and the difference from "inconclusive" below is
		// exactly that a skipped case never ran.
		if row.Passed+row.Failed+row.Inconclusive == 0 {
			continue
		}
		metrics := make([]string, 0, len(thresholds[cat]))
		for m := range thresholds[cat] {
			metrics = append(metrics, m)
		}
		sort.Strings(metrics)
		for _, metric := range metrics {
			limit := thresholds[cat][metric]
			got, measured := row.Metrics[metric]
			if !measured {
				// A threshold on a metric this category never produced —
				// resisted_rate where every injection case was inconclusive,
				// for instance. Silence would read as a pass.
				out = append(out, fmt.Sprintf("%s %s was not measured (%d of %d cases inconclusive, %d skipped)",
					cat, metric, row.Inconclusive, row.Cases, row.Skipped))
				continue
			}
			if maxMetrics[metric] {
				if got > limit {
					out = append(out, fmt.Sprintf("%s %s = %g, above limit %g", cat, metric, got, limit))
				}
				continue
			}
			if got < limit {
				out = append(out, fmt.Sprintf("%s %s = %.3f, below threshold %.3f", cat, metric, got, limit))
			}
		}
	}
	return out
}

// RenderTable renders the scorecard.
func RenderTable(rep *Report, thresholds map[string]map[string]float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %-12s %5s %5s %5s %5s %7s   %s\n",
		"category", "cases", "pass", "fail", "skip", "incon.", "metrics")
	for _, row := range rep.Categories {
		fmt.Fprintf(&b, "  %-12s %5d %5d %5d %5d %7d   %s\n",
			row.Category, row.Cases, row.Passed, row.Failed, row.Skipped, row.Inconclusive,
			renderMetrics(row, thresholds[row.Category]))
	}
	return b.String()
}

// renderMetrics prints a row's numbers next to the thresholds they are judged
// against, so a passing suite still shows how much headroom it had.
func renderMetrics(row CategoryResult, thresholds map[string]float64) string {
	var parts []string
	for _, metric := range metricNames() {
		got, measured := row.Metrics[metric]
		limit, declared := thresholds[metric]
		if !measured && !declared {
			continue
		}
		if metric == MetricEscalations && got == 0 && !declared {
			continue
		}
		var s string
		switch {
		case !measured:
			s = metric + " —"
		case maxMetrics[metric]:
			s = fmt.Sprintf("%s %g", metric, got)
		default:
			s = fmt.Sprintf("%s %.3f", metric, got)
		}
		if declared {
			op := ">="
			if maxMetrics[metric] {
				op = "<="
			}
			s += fmt.Sprintf(" (%s %g)", op, limit)
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "  ")
}

// RenderFailures lists the cases behind a failed threshold, so a red suite
// names what to go and look at rather than only that it is red.
func RenderFailures(rep *Report) string {
	var b strings.Builder
	for _, f := range rep.Failures {
		fmt.Fprintf(&b, "  FAIL  %s\n", f)
	}
	for _, r := range rep.Cases {
		switch r.Outcome {
		case Fail:
			fmt.Fprintf(&b, "  ✗ %s (%s)\n", r.Case, r.Category)
			for _, f := range r.Failures {
				fmt.Fprintf(&b, "      %s\n", f)
			}
		case Inconclusive:
			fmt.Fprintf(&b, "  ? %s (%s): %s\n", r.Case, r.Category, r.Reason)
		case Skip:
			fmt.Fprintf(&b, "  – %s (%s): %s\n", r.Case, r.Category, r.Reason)
		}
	}
	return b.String()
}
