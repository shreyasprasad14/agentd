package telemetry

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// scrapeTimeout bounds the database read a scrape performs. Prometheus gives
// up on a scrape of its own accord (10s by default), and a collector that
// outlives that would pile queries on a database that is already the reason
// the scrape is slow.
const scrapeTimeout = 5 * time.Second

// RunState is what the run-state collector reads from Postgres on each
// scrape.
type RunState struct {
	// Counts is the number of runs in each status, as the database has them.
	Counts map[string]int64
	// OldestQueued is how long the oldest queued run has been waiting, and
	// HasQueued is false when the queue is empty — which is not the same as
	// an age of zero, and is why this is two fields.
	OldestQueued time.Duration
	HasQueued    bool
}

// RunStateCollector reports the runs table on every scrape.
//
// It exists because process counters answer a different question than the
// database does. agentd_runs_finished_total says what this process has seen
// since it started, and resets on deploy; "how many runs have ever failed"
// and "how long has the oldest queued run been waiting" are properties of the
// system, and only Postgres knows them (ADR-26). Process counters for events,
// database gauges for state.
//
// Collect runs one query per scrape rather than caching: it is a single
// indexed GROUP BY over a table with one row per run, and a cached answer
// would report a queue depth that is minutes old — precisely the number
// staleness ruins.
type RunStateCollector struct {
	read     func(context.Context) (RunState, error)
	statuses []string
	log      *slog.Logger

	runs         *prometheus.Desc
	oldestQueued *prometheus.Desc
}

// NewRunStateCollector builds the collector. read performs the database
// query; statuses is the run_status enum, passed in rather than imported
// because the enum belongs to the runtime package and this one is below it.
//
// Naming the statuses matters for more than tidiness: a status no run has
// reached yet has no row to report, and a series that appears only once
// something goes wrong is one that every alert rule and dashboard panel has
// to be written twice to handle. They are reported as explicit zeroes.
func NewRunStateCollector(statuses []string, read func(context.Context) (RunState, error), log *slog.Logger) *RunStateCollector {
	if log == nil {
		log = slog.Default()
	}
	return &RunStateCollector{
		read:     read,
		statuses: statuses,
		log:      log,
		runs: prometheus.NewDesc("agentd_runs", "Runs in each status, from the database.",
			[]string{"status"}, nil),
		oldestQueued: prometheus.NewDesc("agentd_runs_oldest_queued_age_seconds",
			"Age of the oldest queued run; absent when the queue is empty.", nil, nil),
	}
}

// Describe sends the collector's descriptors. Implementing it (rather than
// being an unchecked collector) is what makes the registry reject a duplicate
// registration at startup instead of producing a corrupt exposition later.
func (c *RunStateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.runs
	ch <- c.oldestQueued
}

// Collect queries the database and emits the gauges.
//
// A failed query logs and emits nothing, rather than sending an invalid
// metric. An invalid metric fails the whole scrape, which would throw away
// this process's counters — the ones that still work — every time Postgres
// hiccups. A gap in two gauges is the smaller loss, and Prometheus draws gaps
// correctly.
func (c *RunStateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	state, err := c.read(ctx)
	if err != nil {
		c.log.Warn("run state scrape failed", "error", err)
		return
	}

	seen := make(map[string]bool, len(state.Counts))
	for _, status := range c.statuses {
		seen[status] = true
		ch <- prometheus.MustNewConstMetric(c.runs, prometheus.GaugeValue,
			float64(state.Counts[status]), status)
	}
	// A status the caller did not name is still reported: the alternative is
	// silently dropping runs from a total because someone added an enum value
	// and forgot this list.
	for status, n := range state.Counts {
		if !seen[status] {
			ch <- prometheus.MustNewConstMetric(c.runs, prometheus.GaugeValue, float64(n), status)
		}
	}

	// Deliberately absent rather than zero when nothing is queued: zero is
	// what a run submitted this instant reports, and an alert on "queue older
	// than five minutes" must not be able to read an empty queue as a fresh
	// one.
	if state.HasQueued {
		ch <- prometheus.MustNewConstMetric(c.oldestQueued, prometheus.GaugeValue,
			state.OldestQueued.Seconds())
	}
}
