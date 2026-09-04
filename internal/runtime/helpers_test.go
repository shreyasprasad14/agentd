package runtime_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/testutil"
	"github.com/shreyasprasad/agentd/internal/tools"
	"github.com/shreyasprasad/agentd/internal/tools/builtin"
)

// countingTool wraps a tool and counts completed Invoke calls.
type countingTool struct {
	tools.Tool
	calls atomic.Int32
}

func (c *countingTool) Invoke(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	c.calls.Add(1)
	return c.Tool.Invoke(ctx, inv)
}

// blockingTool signals when it starts and then waits to be released, or for
// ctx to end. It is how tests hold a run "mid-tool-call" deterministically.
type blockingTool struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func newBlockingTool() *blockingTool {
	return &blockingTool{started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (b *blockingTool) Name() string               { return "slow" }
func (b *blockingTool) Description() string        { return "blocks until released" }
func (b *blockingTool) Schema() json.RawMessage    { return json.RawMessage(`{"type":"object"}`) }
func (b *blockingTool) TrustTier() tools.TrustTier { return tools.Builtin }
func (b *blockingTool) Invoke(ctx context.Context, _ tools.Invocation) (tools.Result, error) {
	b.calls.Add(1)
	b.started <- struct{}{}
	select {
	case <-b.release:
		return tools.Result{Content: json.RawMessage(`{"ok":true}`)}, nil
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	}
}

func (b *blockingTool) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-b.started:
	case <-time.After(15 * time.Second):
		t.Fatal("blocking tool never started")
	}
}

type fixture struct {
	t        *testing.T
	st       *store.Store
	provider *fake.Provider
	registry *tools.Registry
	deadline *countingTool
	slow     *blockingTool
}

func newFixture(t *testing.T, provider *fake.Provider) *fixture {
	t.Helper()
	f := &fixture{
		t:        t,
		st:       testutil.Postgres(t),
		provider: provider,
		deadline: &countingTool{Tool: builtin.ComputeDeadline{}},
		slow:     newBlockingTool(),
	}
	f.registry = tools.NewRegistry().MustRegister(builtin.Finish{}, f.deadline, f.slow)
	return f
}

// startWorker runs a worker until the returned stop func is called. Stopping
// cancels the context without any cleanup, which is exactly what a kill -9
// looks like from Postgres' point of view: the lease is left behind.
func (f *fixture) startWorker(owner string, lease time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	w := runtime.NewWorker(f.st, nil, runtime.WorkerConfig{
		Owner:          owner,
		PollInterval:   50 * time.Millisecond,
		LeaseDuration:  lease,
		ReaperInterval: 200 * time.Millisecond,
		Provider:       f.provider,
		Registry:       f.registry,
		DefaultModel:   "fake-model",
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	f.t.Cleanup(cancel)
	return func() {
		cancel()
		<-done
	}
}

type runOpts struct {
	tools     []string
	maxSteps  int32
	budget    string
	toolDelay int
}

func (f *fixture) submit(goal string, o runOpts) uuid.UUID {
	f.t.Helper()
	if o.tools == nil {
		o.tools = []string{builtin.DeadlineName, builtin.FinishName, "slow"}
	}
	if o.maxSteps == 0 {
		o.maxSteps = 10
	}
	if o.budget == "" {
		o.budget = "1.00"
	}
	cfg, err := json.Marshal(runtime.AgentConfig{Tools: o.tools, ToolDelayMS: o.toolDelay})
	require.NoError(f.t, err)
	run, err := f.st.CreateRun(context.Background(), goal, cfg, o.maxSteps, o.budget)
	require.NoError(f.t, err)
	return run.ID
}

func (f *fixture) waitTerminal(id uuid.UUID) *store.Run {
	f.t.Helper()
	var run *store.Run
	testutil.WaitFor(f.t, 30*time.Second, "run to finish", func() bool {
		r, err := f.st.GetRun(context.Background(), id)
		require.NoError(f.t, err)
		run = r
		return r.FinishedAt != nil
	})
	return run
}

func (f *fixture) events(id uuid.UUID) []store.Event {
	f.t.Helper()
	evs, err := f.st.ListEvents(context.Background(), id, 0)
	require.NoError(f.t, err)
	return evs
}

func (f *fixture) state(id uuid.UUID) runtime.State {
	f.t.Helper()
	s, err := runtime.Reduce(f.events(id))
	require.NoError(f.t, err)
	return s
}

func eventTypes(evs []store.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func countType(evs []store.Event, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func decode[T any](t *testing.T, ev store.Event) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(ev.Payload, &v))
	return v
}

var usage = model.Usage{InputTokens: 100, OutputTokens: 10}

func deadlineCall(id string, days int) *model.Response {
	return fake.ToolUse(id, builtin.DeadlineName, map[string]any{"start_date": "2026-09-03", "days": days}, usage)
}

func finishCall(id, answer string) *model.Response {
	return fake.ToolUse(id, builtin.FinishName, map[string]any{"answer": answer}, usage)
}

func slowCall(id string) *model.Response {
	return fake.ToolUse(id, "slow", map[string]any{}, usage)
}
