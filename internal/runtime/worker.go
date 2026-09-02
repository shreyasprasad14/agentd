package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/shreyasprasad/agentd/internal/store"
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
	// StepDelay is the pause the stub agent takes between events.
	StepDelay time.Duration
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
	if c.StepDelay <= 0 {
		c.StepDelay = time.Second
	}
	return c
}

// Worker claims runs off the Postgres queue and executes them.
type Worker struct {
	store *store.Store
	log   *slog.Logger
	cfg   WorkerConfig
}

// NewWorker builds a Worker. A nil logger falls back to the default.
func NewWorker(st *store.Store, log *slog.Logger, cfg WorkerConfig) *Worker {
	cfg = cfg.withDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Worker{store: st, log: log.With("worker", cfg.Owner), cfg: cfg}
}

// Owner is this worker's lease identity.
func (w *Worker) Owner() string { return w.cfg.Owner }

// Run polls the queue until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("worker started", "poll_interval", w.cfg.PollInterval, "lease", w.cfg.LeaseDuration)
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	for {
		claimed, err := w.claimAndExecute(ctx)
		switch {
		case errors.Is(err, context.Canceled):
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
	go w.heartbeat(execCtx, run.ID)

	if err := w.executeStub(execCtx, run); err != nil {
		if ctx.Err() != nil {
			// Shutting down: leave the lease to expire so another worker
			// resumes the run. This is the crash-recovery path.
			return true, nil
		}
		w.log.Error("run failed", "run_id", run.ID, "error", err)
		return true, w.finish(context.WithoutCancel(ctx), run.ID, "failed", err.Error())
	}
	return true, nil
}

// heartbeat renews the lease at a third of its duration so a brief stall does
// not hand the run to another worker.
func (w *Worker) heartbeat(ctx context.Context, runID uuid.UUID) {
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
				w.log.Warn("lease lost", "run_id", runID)
				return
			}
		}
	}
}

// executeStub is the M0 stand-in for the agent loop: three canned events with
// a delay between them, enough to prove the log and the SSE tail. M1 replaces
// this with load events -> reduce -> model call -> tool calls.
func (w *Worker) executeStub(ctx context.Context, run *store.Run) error {
	config := run.AgentConfig
	if len(config) == 0 {
		config = json.RawMessage(`{}`)
	}

	if _, err := w.store.AppendEvent(ctx, run.ID, EventRunStarted, map[string]any{
		"goal":         run.Goal,
		"agent_config": config,
		"max_steps":    run.MaxSteps,
		"budget_usd":   run.BudgetUSD,
		"worker":       w.cfg.Owner,
	}); err != nil {
		return err
	}

	if err := sleep(ctx, w.cfg.StepDelay); err != nil {
		return err
	}

	if _, err := w.store.AppendEvent(ctx, run.ID, EventModelResponded, map[string]any{
		"model": "stub",
		"content": []map[string]string{
			{"type": "text", "text": "stub response for goal: " + run.Goal},
		},
		"stop_reason": "end_turn",
		"usage":       map[string]int{"input_tokens": 0, "output_tokens": 0},
	}); err != nil {
		return err
	}

	if err := sleep(ctx, w.cfg.StepDelay); err != nil {
		return err
	}

	return w.finish(ctx, run.ID, "succeeded", "stub response for goal: "+run.Goal)
}

// finish sets the terminal status before appending run_finished, so a client
// that sees the final event and immediately re-reads the run never observes a
// finished log against a still-running status.
func (w *Worker) finish(ctx context.Context, runID uuid.UUID, status, finalAnswer string) error {
	if err := w.store.FinishRun(ctx, runID, status); err != nil {
		return err
	}
	if _, err := w.store.AppendEvent(ctx, runID, EventRunFinished, map[string]any{
		"status":       status,
		"final_answer": finalAnswer,
	}); err != nil {
		return err
	}
	w.log.Info("run finished", "run_id", runID, "status", status)
	return nil
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
