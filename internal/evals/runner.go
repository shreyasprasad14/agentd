package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/tools"
)

// Backend supplies the model for each case. Replay and live differ in nothing
// else, which is what lets one fixture format be a regression test in one mode
// and a quality measurement in the other (spec §12).
type Backend interface {
	// For returns the provider for one case, and a label naming where its
	// answers come from for the scorecard.
	For(c Case) (provider model.Provider, cassette, cassetteModel string, err error)
	// Finish is called once the case is over, whatever the outcome. The
	// cassette backend saves anything it recorded here.
	Finish(c Case) error
}

// evalLease is short on purpose. A RESILIENCE case waits out the dead
// worker's lease before a second worker can claim the run, so the lease
// duration is most of that case's wall time — and nothing else in the harness
// depends on it being realistic.
const evalLease = 2 * time.Second

// pollInterval is how often the harness reads the log while waiting for a
// chaos trigger or a terminal event. It is the same order as the SSE tail's
// 200ms and costs one indexed query per running case.
const pollInterval = 50 * time.Millisecond

// Config wires a Runner.
type Config struct {
	Store    *store.Store
	Registry *tools.Registry
	Backend  Backend
	Log      *slog.Logger
	// DefaultModel is what a case without one is run against.
	DefaultModel string
	// Capabilities is what a case's `requires:` is checked against. A
	// missing capability skips the case rather than failing it.
	Capabilities map[string]bool
	// Only, when set, restricts the run to these case ids.
	Only map[string]bool
}

// Runner executes cases.
type Runner struct {
	cfg Config
	log *slog.Logger
}

// NewRunner builds a Runner.
func NewRunner(cfg Config) *Runner {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Runner{cfg: cfg, log: log}
}

// Run executes every case of every suite, in file and then declaration order,
// and returns their results. Cases run one at a time: several would contend
// for the corpus, the Docker daemon, and — in live mode — one local model.
func (r *Runner) Run(ctx context.Context, suites []*Suite) ([]Result, error) {
	var out []Result
	for _, s := range suites {
		for _, c := range s.Cases {
			if r.cfg.Only != nil && !r.cfg.Only[c.ID] {
				continue
			}
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			res := r.runCase(ctx, c)
			r.logResult(res)
			out = append(out, res)
		}
	}
	return out, nil
}

func (r *Runner) logResult(res Result) {
	args := []any{"case", res.Case, "category", res.Category, "outcome", res.Outcome,
		"elapsed", res.Elapsed.Round(time.Millisecond)}
	if res.Status != "" {
		args = append(args, "status", res.Status, "steps", res.Steps)
	}
	if res.Reason != "" {
		args = append(args, "reason", res.Reason)
	}
	switch res.Outcome {
	case Pass:
		r.log.Info("case passed", args...)
	case Skip:
		r.log.Warn("case skipped", args...)
	case Inconclusive:
		r.log.Warn("case inconclusive", args...)
	default:
		r.log.Error("case failed", append(args, "failures", res.Failures)...)
	}
}

func (r *Runner) runCase(ctx context.Context, c Case) Result {
	res := Result{Case: c.ID, Category: c.Category, Outcome: Pass}
	for _, need := range c.Requires {
		if !r.cfg.Capabilities[need] {
			res.Outcome = Skip
			res.Reason = need + " unavailable"
			return res
		}
	}

	provider, cassette, cassetteModel, err := r.cfg.Backend.For(c)
	if err != nil {
		return failed(res, fmt.Sprintf("model backend: %v", err))
	}
	res.Cassette, res.CassetteModel = cassette, cassetteModel
	defer func() {
		if err := r.cfg.Backend.Finish(c); err != nil {
			r.log.Error("saving the case's cassette failed", "case", c.ID, "error", err)
		}
	}()

	start := time.Now()
	runID, err := r.submit(ctx, c)
	if err != nil {
		return failed(res, fmt.Sprintf("submit: %v", err))
	}
	res.RunID = runID.String()

	if err := r.drive(ctx, c, runID, provider); err != nil {
		res.Elapsed = time.Since(start)
		return failed(res, err.Error())
	}
	res.Elapsed = time.Since(start)

	ev, err := r.collect(ctx, runID, res.Elapsed)
	if err != nil {
		return failed(res, fmt.Sprintf("reading the run back: %v", err))
	}
	res.Status = ev.Run.Status
	res.Steps = ev.State.Steps
	res.SpentUSD = ev.Run.SpentUSD
	res.Escalations = ev.Escalations()

	failures, cites, inj, err := Check(ctx, r.cfg.Store, c.Assert, ev)
	if err != nil {
		return failed(res, fmt.Sprintf("checking assertions: %v", err))
	}
	res.Citations, res.Injection = cites, inj

	// An injection case whose plant never reached the model cannot say
	// anything about resistance. It is not a pass and not a failure of the
	// runtime: it is a broken case, and the scorecard names it as one.
	if inj != nil && !inj.Exposed {
		res.Outcome = Inconclusive
		res.Reason = "the planted text never reached the model: " + list(inj.Missing)
		res.Failures = failures
		return res
	}
	if len(failures) > 0 {
		res.Outcome = Fail
		res.Failures = failures
	}
	return res
}

func failed(res Result, reason string) Result {
	res.Outcome = Fail
	res.Failures = append(res.Failures, reason)
	return res
}

// submit creates the run exactly as POST /v1/runs would, including the
// allowlist snapshot the loop then enforces.
func (r *Runner) submit(ctx context.Context, c Case) (uuid.UUID, error) {
	cfg, err := json.Marshal(runtime.AgentConfig{
		Model:        c.Model,
		SystemPrompt: c.SystemPrompt,
		Tools:        c.Tools,
		ToolDelayMS:  c.ToolDelayMS,
	})
	if err != nil {
		return uuid.Nil, err
	}
	run, err := r.cfg.Store.CreateRun(ctx, store.NewRun{
		Goal:        c.Goal,
		AgentConfig: cfg,
		MaxSteps:    c.MaxSteps,
		BudgetUSD:   c.BudgetUSD,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return run.ID, nil
}

// drive runs the case's workers until the run reaches a terminal event. It is
// where a RESILIENCE case's kill happens, which is the reason this harness
// owns worker lifecycles instead of talking to a server (ADR-29).
func (r *Runner) drive(ctx context.Context, c Case, runID uuid.UUID, provider model.Provider) error {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	stopA := r.startWorker(ctx, "eval-"+c.ID+"-a", provider)
	stopped := false
	defer func() {
		if !stopped {
			stopA()
		}
	}()

	if c.Chaos != nil {
		if err := r.waitForEvent(ctx, runID, c.Chaos); err != nil {
			return err
		}
		// Cancelling the worker's context and dropping it is what Postgres
		// sees during a kill -9: the lease is left behind for the reaper, the
		// in-flight tool call keeps its `started` ledger row, and nothing
		// further is written by this worker.
		stopA()
		stopped = true
		r.log.Info("killed the worker mid-run", "case", c.ID, "after", c.Chaos.KillAfter)
		if c.Chaos.RestartIn > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("timed out before the second worker started")
			case <-time.After(c.Chaos.RestartIn):
			}
		}
		stopB := r.startWorker(ctx, "eval-"+c.ID+"-b", provider)
		defer stopB()
	}

	return r.waitTerminal(ctx, runID)
}

// startWorker runs a worker until the returned function is called. Stopping
// waits for the loop to unwind so the harness never reads a log that is still
// being written; what it does not do is let the worker finish the run, which
// is the distinction a crash test needs.
func (r *Runner) startWorker(ctx context.Context, owner string, provider model.Provider) func() {
	ctx, cancel := context.WithCancel(ctx)
	w := runtime.NewWorker(r.cfg.Store, r.log, runtime.WorkerConfig{
		Owner:          owner,
		PollInterval:   pollInterval,
		LeaseDuration:  evalLease,
		ReaperInterval: evalLease / 4,
		Provider:       provider,
		Registry:       r.cfg.Registry,
		DefaultModel:   r.cfg.DefaultModel,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// waitForEvent blocks until the chaos trigger lands in the log. For a tool
// trigger it also waits out the run's tool_delay_ms, so the kill arrives while
// the tool call is genuinely in flight rather than racing the event that
// announced it.
func (r *Runner) waitForEvent(ctx context.Context, runID uuid.UUID, ch *Chaos) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		events, err := r.cfg.Store.ListEvents(ctx, runID, 0)
		if err != nil && ctx.Err() == nil {
			return err
		}
		for _, e := range events {
			if e.Type != ch.KillAfter {
				continue
			}
			if ch.Matching != "" && !eventNames(e, ch.Matching) {
				continue
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s%s to kill the worker after",
				ch.KillAfter, matchSuffix(ch.Matching))
		case <-ticker.C:
		}
	}
}

func matchSuffix(matching string) string {
	if matching == "" {
		return ""
	}
	return " (" + matching + ")"
}

// eventNames reports whether an event's payload names a tool. Only the tool
// events carry a name, which is the only narrowing a trigger needs.
func eventNames(e store.Event, name string) bool {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return false
	}
	return p.Name == name
}

func (r *Runner) waitTerminal(ctx context.Context, runID uuid.UUID) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		run, err := r.cfg.Store.GetRun(ctx, runID)
		if err != nil && ctx.Err() == nil {
			return err
		}
		if run != nil && run.FinishedAt != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			// The timeout is a case failure with the run's own account of
			// where it got to, rather than a bare deadline: "timed out" with
			// no log position is the least useful failure an eval can print.
			return fmt.Errorf("timed out after %s%s", r.caseDeadline(ctx), r.describeProgress(runID))
		case <-ticker.C:
		}
	}
}

func (r *Runner) caseDeadline(ctx context.Context) string {
	if d, ok := ctx.Deadline(); ok {
		return time.Until(d).Round(time.Second).String()
	}
	return "the case timeout"
}

func (r *Runner) describeProgress(runID uuid.UUID) string {
	events, err := r.cfg.Store.ListEvents(context.WithoutCancel(context.Background()), runID, 0)
	if err != nil || len(events) == 0 {
		return " with an empty log"
	}
	last := events[len(events)-1]
	return fmt.Sprintf("; the log ends at seq %d (%s)", last.Seq, last.Type)
}

func (r *Runner) collect(ctx context.Context, runID uuid.UUID, elapsed time.Duration) (*Evidence, error) {
	ctx = context.WithoutCancel(ctx)
	run, err := r.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	events, err := r.cfg.Store.ListEvents(ctx, runID, 0)
	if err != nil {
		return nil, err
	}
	calls, err := r.cfg.Store.ListToolCalls(ctx, runID)
	if err != nil {
		return nil, err
	}
	ev, err := Collect(run, events, calls, elapsed)
	if err != nil {
		return nil, fmt.Errorf("the run's log does not reduce: %w", err)
	}
	// What the model was told its tools are. It comes from the registry rather
	// than the log, which records the allowlist by name only, and it is a
	// second exposure channel rather than more of the first: a poisoned tool
	// description never appears in a tool result (ADR-35).
	if r.cfg.Registry != nil {
		ev.DescribeTools(r.cfg.Registry.Defs(ev.State.Config.Tools))
	}
	return ev, nil
}

// ErrNoCassette is returned when a case has no recording and the backend is
// not allowed to make one.
var ErrNoCassette = errors.New("no cassette for this case")
