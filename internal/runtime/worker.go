package runtime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/telemetry"
	"github.com/shreyasprasad/agentd/internal/tools"
)

// WorkerConfig tunes queue polling and leasing.
type WorkerConfig struct {
	// Owner identifies this worker in runs.lease_owner. Defaults to
	// "<hostname>/<random>" so two workers on one box stay distinguishable.
	Owner string
	// PollInterval is how often an idle worker looks for claimable runs.
	PollInterval time.Duration
	// LeaseDuration is how long a claim is valid without a heartbeat. A
	// kill -9'd worker's runs become claimable again after this window.
	LeaseDuration time.Duration
	// ReaperInterval is how often expired leases are swept back to the
	// queue. Defaults to half the lease duration.
	ReaperInterval time.Duration
	// Provider is the model backend. Required.
	Provider model.Provider
	// Registry holds the tools runs may be granted. Required.
	Registry *tools.Registry
	// DefaultModel is used when a run's agent_config has no model.
	DefaultModel string
	// CancelPoll is how often the loop checks a running run's cancel flag.
	// Defaults to DefaultCancelPoll.
	CancelPoll time.Duration
	// Tracer emits the spans of every run this worker claims. Nil means no
	// tracing, which is what a worker started without a collector gets.
	Tracer trace.Tracer
	// Metrics counts claims, lease reaps, and everything the loop records.
	// Nil records nothing, which is what a worker serving no /metrics gets.
	Metrics *telemetry.Metrics
}

func (c WorkerConfig) withDefaults() WorkerConfig {
	if c.Owner == "" {
		host, err := os.Hostname()
		if err != nil {
			host = "worker"
		}
		c.Owner = host + "/" + uuid.NewString()[:8]
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 250 * time.Millisecond
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = 60 * time.Second
	}
	if c.ReaperInterval <= 0 {
		c.ReaperInterval = c.LeaseDuration / 2
	}
	if c.Registry == nil {
		c.Registry = tools.NewRegistry()
	}
	if c.CancelPoll <= 0 {
		c.CancelPoll = DefaultCancelPoll
	}
	if c.Tracer == nil {
		// Defaulted here as well as in NewLoop so that cfg.Tracer is a usable
		// tracer everywhere a Worker reads its own config, rather than a
		// field whose nilness each reader has to remember.
		c.Tracer = noop.NewTracerProvider().Tracer("")
	}
	return c
}

// Worker claims runs off the Postgres queue and executes them.
type Worker struct {
	store *store.Store
	log   *slog.Logger
	cfg   WorkerConfig
	loop  *Loop
}

// NewWorker builds a Worker. A nil logger falls back to the default. It
// panics without a Provider, since a worker that cannot call a model is a
// misconfiguration, not a runtime condition.
func NewWorker(st *store.Store, log *slog.Logger, cfg WorkerConfig) *Worker {
	cfg = cfg.withDefaults()
	if cfg.Provider == nil {
		panic("runtime.NewWorker: Provider is required")
	}
	if log == nil {
		log = slog.Default()
	}
	log = log.With("worker", cfg.Owner)
	return &Worker{
		store: st,
		log:   log,
		cfg:   cfg,
		loop: NewLoop(st, cfg.Provider, cfg.Registry, log, LoopConfig{
			Owner:        cfg.Owner,
			DefaultModel: cfg.DefaultModel,
			CancelPoll:   cfg.CancelPoll,
			Tracer:       cfg.Tracer,
			Metrics:      cfg.Metrics,
		}),
	}
}

// Owner is this worker's lease identity.
func (w *Worker) Owner() string { return w.cfg.Owner }

// Run polls the queue until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("worker started", "poll_interval", w.cfg.PollInterval, "lease", w.cfg.LeaseDuration,
		"provider", w.cfg.Provider.Name(), "model", w.cfg.DefaultModel, "tools", w.cfg.Registry.Names())
	go w.reaper(ctx)

	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	for {
		claimed, err := w.claimAndExecute(ctx)
		switch {
		case IsShutdown(err) || ctx.Err() != nil:
			return nil
		case err != nil:
			w.log.Error("claim/execute", "error", err)
		case claimed:
			// Immediately look for more work rather than waiting a tick.
			continue
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// runContext puts the run's trace identity on ctx so the worker's log lines
// about it name the same trace its spans do. It starts no span: the loop's
// attempt span is the first real span of this attempt, and a second one here
// would draw a bar for the worker's bookkeeping.
//
// A run submitted before M4, or with tracing off, has no ids and gets ctx back
// unchanged — the log lines then simply carry no trace id, as they always did.
func (w *Worker) runContext(ctx context.Context, run *store.Run) context.Context {
	if run.TraceID == nil || run.RootSpanID == nil {
		return ctx
	}
	return telemetry.RemoteParent(ctx, *run.TraceID, *run.RootSpanID)
}

func (w *Worker) claimAndExecute(ctx context.Context) (bool, error) {
	run, err := w.store.ClaimRun(ctx, w.cfg.Owner, w.cfg.LeaseDuration)
	if err != nil || run == nil {
		return false, err
	}
	// The worker's own lines about this run sit outside every span the loop
	// creates, so without this they would be the only lines in a run's life
	// with no trace id — and they are the ones the crash demo is read through:
	// "run claimed" by the second worker is where the resume becomes visible.
	// Rebuilding the remote parent is enough, because a valid span context is
	// all the log handler needs to name the trace.
	ctx = w.runContext(ctx, run)
	w.log.InfoContext(ctx, "run claimed", "run_id", run.ID, "goal", run.Goal)

	execCtx, lost := context.WithCancel(ctx)
	defer lost()
	go w.heartbeat(execCtx, run.ID, lost)

	outcome, err := w.loop.Execute(execCtx, run)
	if err != nil {
		// A genuine failure: the loop left no terminal event, so the worker
		// writes one. Under a detached context, because a run that failed
		// for its own reasons should not be left for another worker to
		// rediscover.
		w.log.ErrorContext(ctx, "run failed", "run_id", run.ID, "error", err)
		ferr := w.loop.finish(context.WithoutCancel(ctx), run.ID, StatusFailed, "", err.Error())
		if errors.Is(ferr, store.ErrLeaseLost) {
			// The run failed *and* the lease was gone, so this worker wrote
			// no terminal event: the run belongs to whoever holds the lease
			// now, and only the lease loss is this worker's to report.
			w.cfg.Metrics.LeaseLost()
			return true, nil
		}
		return true, ferr
	}

	// The one place that can tell the three non-failure endings apart, which
	// is why the lease-loss counter lives here rather than next to the
	// heartbeat that noticed or the fenced write that refused (ADR-25).
	switch outcome {
	case OutcomeLeaseLost:
		w.cfg.Metrics.LeaseLost()
		w.log.WarnContext(ctx, "lease lost mid-run, another worker owns it", "run_id", run.ID)
	case OutcomeShutdown:
		// The loop reports a shutdown for any context death it did not cause
		// itself, and cannot tell the two apart: both arrive as a dead
		// parent context. The worker can, because it owns both — the outer
		// context is its own shutdown, execCtx is what the heartbeat kills
		// when the lease is gone. Either way the lease is left to expire and
		// another worker resumes the run; only the log line differs.
		if ctx.Err() == nil {
			w.cfg.Metrics.LeaseLost()
			w.log.WarnContext(ctx, "lease lost mid-run, another worker owns it", "run_id", run.ID)
			break
		}
		w.log.InfoContext(ctx, "shutdown mid-run, leaving lease", "run_id", run.ID)
	}
	return true, nil
}

// heartbeat renews the lease at a third of its duration so a brief stall does
// not hand the run to another worker. If the lease is gone anyway, it cancels
// execution: the fenced store rejects our writes from here on, and stopping
// early avoids doing tool work whose result can never be committed.
func (w *Worker) heartbeat(ctx context.Context, runID uuid.UUID, lost context.CancelFunc) {
	ticker := time.NewTicker(w.cfg.LeaseDuration / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := w.store.Heartbeat(ctx, runID, w.cfg.Owner, w.cfg.LeaseDuration)
			if err != nil && ctx.Err() == nil {
				w.log.Warn("heartbeat failed", "run_id", runID, "error", err)
			}
			if err == nil && !ok {
				w.log.Warn("lease lost, cancelling execution", "run_id", runID)
				lost()
				return
			}
		}
	}
}

// reaper sweeps expired leases back to the queue. ClaimRun would find them
// regardless; the sweep makes the hand-off visible in the logs and in
// agentd_leases_reaped_total, which is the counter ADR-7 promised when it
// kept a reaper whose only observable effect was a log line.
func (w *Worker) reaper(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.ReaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := w.store.ReapExpiredLeases(ctx)
			if err != nil && ctx.Err() == nil {
				w.log.Warn("reaper failed", "error", err)
				continue
			}
			if n > 0 {
				w.cfg.Metrics.LeasesReaped(n)
				w.log.Info("requeued runs with expired leases", "count", n)
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
