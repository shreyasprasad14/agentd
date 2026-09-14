package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/testutil"
)

// cancelLatencyBound is how long a cancel may take in these tests before it
// counts as a failure. The fixture polls the flag every 50ms, so the bound is
// two orders of magnitude of slack for a loaded box — but it is asserted
// rather than left implicit, because "within the poll interval" is the exit
// criterion. The interruption itself is proved by construction: the blocking
// tool and the blocking model call in these tests are never released, so a
// cancel that only took effect at the next step boundary would not take
// effect at all.
const cancelLatencyBound = 5 * time.Second

// TestCancelDuringToolCall is M4's headline control property. A run sitting
// inside a tool call that will never return on its own is cancelled, and the
// log it leaves behind is well-formed: the in-flight call is answered with a
// tool_failed rather than left as a dangling tool_requested, and the ledger
// row that fences its idempotency moves to failed with it.
func TestCancelDuringToolCall(t *testing.T) {
	f := newFixture(t, fake.New(slowCall("t1")))
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("cancel me mid-tool", runOpts{})
	f.slow.waitStarted(t)

	asked := time.Now()
	ok, err := f.st.RequestCancel(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)

	run := f.waitTerminal(id)
	require.Less(t, time.Since(asked), cancelLatencyBound,
		"cancel interrupts the call rather than waiting for a step boundary the run never reaches")
	require.Equal(t, runtime.StatusCancelled, run.Status)
	require.Equal(t, int32(1), f.slow.calls.Load(), "the tool was torn off, not re-run")

	evs := f.events(id)
	require.Equal(t, []string{
		runtime.EventRunStarted,
		runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested, runtime.EventToolFailed,
		runtime.EventCancelRequested, runtime.EventRunFinished,
	}, eventTypes(evs))
	require.Equal(t,
		countType(evs, runtime.EventToolRequested),
		countType(evs, runtime.EventToolFailed)+countType(evs, runtime.EventToolSucceeded),
		"no tool_requested is left without a completion")

	// The failure says why the call died, not how: "context canceled" is the
	// mechanism, and it is not what an operator reading the log asked about.
	failed := decode[runtime.ToolFailedPayload](t, evs[4])
	require.Equal(t, "t1", failed.ToolUseID)
	require.Equal(t, "slow", failed.Name)
	require.Equal(t, "run cancelled", failed.Error)
	require.False(t, failed.Retryable, "a cancelled call is not worth re-offering to the model")

	cancelled := decode[runtime.CancelRequestedPayload](t, evs[5])
	require.Equal(t, runtime.CancelPhaseTool, cancelled.Phase)
	require.Equal(t, "cancel requested", decode[runtime.RunFinishedPayload](t, evs[6]).Error)

	// The ledger agrees with the log, in the transaction that wrote it: a row
	// left "started" is the crash shape, and a resumed worker would re-execute
	// the tool on the strength of it.
	calls, err := f.st.ListToolCalls(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Equal(t, evs[3].Seq, calls[0].Seq)
	require.Equal(t, store.ToolCallFailed, calls[0].Status)

	// The one completed model call is still accounted for.
	require.Equal(t, int64(100), run.InputTokens)
	require.Equal(t, int64(10), run.OutputTokens)
}

// TestCancelBeforeTheToolRuns covers the second place a cancel can land
// inside a tool call: the window between tool_requested being committed and
// the tool's own work starting, which agent_config.tool_delay_ms widens on
// purpose for the demos — and which is therefore where a demo's cancel most
// often lands.
//
// The tool never ran, but the request and its ledger row are already in the
// database, so the same completion is owed: a log that ends with an
// unanswered tool_requested is a shape no other path produces, and a ledger
// row left "started" is the crash shape, which would have the next worker
// execute a call the run was cancelled out of.
func TestCancelBeforeTheToolRuns(t *testing.T) {
	f := newFixture(t, fake.New(deadlineCall("t1", 30)))
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("cancel me during the delay", runOpts{toolDelay: 30000})
	testutil.WaitFor(t, 30*time.Second, "the tool call to be requested", func() bool {
		return countType(f.events(id), runtime.EventToolRequested) == 1
	})

	asked := time.Now()
	ok, err := f.st.RequestCancel(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)

	run := f.waitTerminal(id)
	require.Less(t, time.Since(asked), cancelLatencyBound,
		"the delay is 30s; a cancel that waited it out would not be interrupting anything")
	require.Equal(t, runtime.StatusCancelled, run.Status)
	require.Equal(t, int32(0), f.deadline.calls.Load(), "the tool itself never ran")

	evs := f.events(id)
	require.Equal(t, []string{
		runtime.EventRunStarted,
		runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested, runtime.EventToolFailed,
		runtime.EventCancelRequested, runtime.EventRunFinished,
	}, eventTypes(evs))
	failed := decode[runtime.ToolFailedPayload](t, evs[4])
	require.Equal(t, "run cancelled", failed.Error)
	require.False(t, failed.Retryable)
	require.Equal(t, runtime.CancelPhaseTool, decode[runtime.CancelRequestedPayload](t, evs[5]).Phase)

	calls, err := f.st.ListToolCalls(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Equal(t, store.ToolCallFailed, calls[0].Status)
}

// TestCancelDuringModelCall covers the other half of "cancel interrupts". A
// model call has no honest failure event — the loop does not know what the
// provider did with the request — so the interrupt deliberately leaves a
// dangling model_requested. The point of the test is that this is not a new
// corruption: it is the same shape a crash mid-call produces, which Reduce has
// modelled as ModelInFlight since M1.
func TestCancelDuringModelCall(t *testing.T) {
	p := fake.New(finishCall("t1", "never returned"))
	started := make(chan struct{}, 1)
	p.OnComplete = func(ctx context.Context, _ model.Request, _ int) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	f := newFixture(t, p)
	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	id := f.submit("cancel me mid-model", runOpts{})
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("model call never started")
	}

	asked := time.Now()
	ok, err := f.st.RequestCancel(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)

	run := f.waitTerminal(id)
	require.Less(t, time.Since(asked), cancelLatencyBound,
		"cancel interrupts the completion rather than waiting for it to return")
	require.Equal(t, runtime.StatusCancelled, run.Status)
	require.Equal(t, 1, f.provider.Calls(), "the interrupted call was not retried")

	evs := f.events(id)
	require.Equal(t, []string{
		runtime.EventRunStarted, runtime.EventModelRequested,
		runtime.EventCancelRequested, runtime.EventRunFinished,
	}, eventTypes(evs))
	require.Zero(t, countType(evs, runtime.EventModelResponded), "the model call is left dangling on purpose")
	require.Equal(t, runtime.CancelPhaseModel, decode[runtime.CancelRequestedPayload](t, evs[2]).Phase)

	s, err := runtime.Reduce(evs)
	require.NoError(t, err, "an interrupted model call leaves a log the reducer already reads")
	require.True(t, s.ModelInFlight, "the dangling request reduces to the state a crash mid-call produces")
	require.Equal(t, 0, s.Steps)
	require.Equal(t, runtime.StatusCancelled, s.Status)

	// Whatever the provider spent on the torn-off call is lost, because there
	// is no event to hang it on — the caveat the loop already documents for
	// empty responses, now reachable by cancelling too.
	require.Equal(t, "0.0000", run.SpentUSD)
	require.Zero(t, run.InputTokens)
}

// TestCancelQueuedRunIsFinishedByAWorker answers the obvious question about
// the cancel path: why does POST /v1/runs/:id/cancel not just finish a run
// that has not started yet, instead of setting a flag and waiting for a worker
// to pick it up? Because the event log requires run_started first, and
// run_started carries the *resolved* agent-config snapshot — the default model
// filled in, the tool allowlist normalised — which only a worker knows. So a
// queued cancel is claimed like any other run and finished in milliseconds.
// This needs no code; it needs the test that says it works.
func TestCancelQueuedRunIsFinishedByAWorker(t *testing.T) {
	f := newFixture(t, fake.New(deadlineCall("t1", 1)))

	id := f.submit("cancel before anyone claims me", runOpts{})
	ok, err := f.st.RequestCancel(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)

	stop := f.startWorker("w1", 10*time.Second)
	defer stop()

	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusCancelled, run.Status)
	require.Nil(t, run.LeaseOwner)
	require.Equal(t, 0, f.provider.Calls(), "a run cancelled before it started costs nothing")

	evs := f.events(id)
	require.Equal(t, []string{
		runtime.EventRunStarted, runtime.EventCancelRequested, runtime.EventRunFinished,
	}, eventTypes(evs))

	// The snapshot the API could not have written by itself.
	started := decode[runtime.RunStartedPayload](t, evs[0])
	require.Equal(t, "w1", started.Worker)
	require.Equal(t, "fake-model", started.AgentConfig.Model, "the worker's default, resolved at claim time")

	// Idle, not "tool" or "model": the flag was seen at the top of the step
	// loop, with no wait at all, rather than by the watcher during a call.
	require.Equal(t, runtime.CancelPhaseIdle, decode[runtime.CancelRequestedPayload](t, evs[1]).Phase)
}
