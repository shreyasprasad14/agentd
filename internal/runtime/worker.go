package runtime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/store"
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
		loop:  NewLoop(st, cfg.Provider, cfg.Registry, log, cfg.Owner, cfg.DefaultModel),
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

func (w *Worker) claimAndExecute(ctx context.Context) (bool, error) {
	run, err := w.store.ClaimRun(ctx, w.cfg.Owner, w.cfg.LeaseDuration)
	if err != nil || run == nil {
		return false, err
	}
	w.log.Info("run claimed", "run_id", run.ID, "goal", run.Goal)

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.heartbeat(execCtx, run.ID, cancel)

	err = w.loop.Execute(execCtx, run)
	switch {
	case err == nil:
		return true, nil
	case ctx.Err() != nil:
		// Shutting down: leave the lease to expire so another worker
		// resumes the run. This is the crash-recovery path.
		w.log.Info("shutdown mid-run, leaving lease", "run_id", run.ID)
		return true, nil
	case errors.Is(err, store.ErrLeaseLost):
		w.log.Warn("lease lost mid-run, another worker owns it", "run_id", run.ID)
		return true, nil
	default:
		w.log.Error("run failed", "run_id", run.ID, "error", err)
		ferr := w.loop.finish(context.WithoutCancel(ctx), run.ID, StatusFailed, "", err.Error())
		if errors.Is(ferr, store.ErrLeaseLost) {
			return true, nil
		}
		return true, ferr
	}
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
// regardless; the sweep makes the hand-off visible in the logs and, from M4,
// in a metric.
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
