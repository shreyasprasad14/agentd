package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/model/fake"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
)

// The tests in this file drive Loop.Execute directly instead of through a
// Worker. The thing under test is the Outcome itself — the value that replaced
// the worker inferring its next move from ctx.Err() and from finish happening
// to fail with ErrLeaseLost (ADR-25) — and a Worker consumes that value into a
// log line, which is not something to assert on. Each test therefore asserts
// the returned Outcome and the row and log it left behind, which together are
// the whole contract: cancel finishes the run, shutdown leaves it claimable
// for the next worker, and lease loss does neither and is not an error.

// execResult is one Execute return, carried off the goroutine that made it.
type execResult struct {
	outcome runtime.Outcome
	err     error
}

// newDirectLoop builds the Loop a Worker would build, with the fixture's short
// cancel poll so a test that measures cancellation does not spend a second
// waiting for it.
func newDirectLoop(f *fixture, owner string) *runtime.Loop {
	return runtime.NewLoop(f.st, f.provider, f.registry, nil, runtime.LoopConfig{
		Owner:        owner,
		DefaultModel: "fake-model",
		CancelPoll:   f.cancelPoll,
	})
}

// claimRun leases a run the way the worker's queue poll would, so Execute is
// handed the same row and the same fencing identity it sees in production.
func claimRun(t *testing.T, f *fixture, owner string, lease time.Duration) *store.Run {
	t.Helper()
	run, err := f.st.ClaimRun(context.Background(), owner, lease)
	require.NoError(t, err)
	require.NotNil(t, run, "the submitted run should be claimable")
	return run
}

func goExecute(ctx context.Context, l *runtime.Loop, run *store.Run) <-chan execResult {
	done := make(chan execResult, 1)
	go func() {
		outcome, err := l.Execute(ctx, run)
		done <- execResult{outcome, err}
	}()
	return done
}

func awaitExecute(t *testing.T, done <-chan execResult) execResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("Execute never returned")
		return execResult{}
	}
}

// TestWorkerOutcomeCancelFinishesTheRun: the loop writes the terminal event itself,
// so the worker has nothing left to do. This is the outcome that used to be
// indistinguishable from a shutdown, because both reached the worker as a
// context that had died.
func TestWorkerOutcomeCancelFinishesTheRun(t *testing.T) {
	f := newFixture(t, fake.New(slowCall("t1")))
	id := f.submit("cancel mid-tool", runOpts{})
	run := claimRun(t, f, "w1", 10*time.Second)

	done := goExecute(context.Background(), newDirectLoop(f, "w1"), run)
	f.slow.waitStarted(t)
	ok, err := f.st.RequestCancel(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)

	res := awaitExecute(t, done)
	require.NoError(t, res.err, "a cancelled run is an outcome, not a failure")
	require.Equal(t, runtime.OutcomeFinished, res.outcome)
	require.Equal(t, "finished", res.outcome.String())

	after, err := f.st.GetRun(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, runtime.StatusCancelled, after.Status)
	require.NotNil(t, after.FinishedAt)
	require.Nil(t, after.LeaseOwner, "a finished run holds no lease")
	evs := f.events(id)
	require.Equal(t, runtime.EventRunFinished, evs[len(evs)-1].Type)
}

// TestWorkerOutcomeShutdownLeavesTheLease: a worker going away mid-run writes
// nothing terminal. The lease is left to expire rather than released, which is
// what makes the hand-off identical to a kill -9 — the case the durability
// story is actually built for, and the one a graceful stop must not diverge
// from.
func TestWorkerOutcomeShutdownLeavesTheLease(t *testing.T) {
	f := newFixture(t, fake.New(slowCall("t1")))
	id := f.submit("shut down mid-tool", runOpts{})
	run := claimRun(t, f, "w1", 10*time.Second)

	ctx, shutdown := context.WithCancel(context.Background())
	done := goExecute(ctx, newDirectLoop(f, "w1"), run)
	f.slow.waitStarted(t)
	shutdown()

	res := awaitExecute(t, done)
	require.NoError(t, res.err, "shutdown is an outcome, not a failure")
	require.Equal(t, runtime.OutcomeShutdown, res.outcome)
	require.Equal(t, "shutdown", res.outcome.String())

	after, err := f.st.GetRun(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, runtime.StatusRunning, after.Status)
	require.Nil(t, after.FinishedAt, "the run is unfinished, for the next worker to resume")
	require.NotNil(t, after.LeaseOwner)
	require.Equal(t, "w1", *after.LeaseOwner, "the lease is left to expire, not handed back")

	// Nothing was recorded about the interrupted call: no tool_failed, and a
	// ledger row still "started", which is what tells the next worker to
	// re-execute it.
	evs := f.events(id)
	require.Equal(t, runtime.EventToolRequested, evs[len(evs)-1].Type)
	require.Zero(t, countType(evs, runtime.EventToolFailed))
	require.Zero(t, countType(evs, runtime.EventCancelRequested))
	tc, err := f.st.GetToolCall(context.Background(), id, evs[len(evs)-1].Seq)
	require.NoError(t, err)
	require.Equal(t, store.ToolCallStarted, tc.Status)
}

// TestWorkerOutcomeLeaseLostIsNotAnError: once another worker owns the run, nothing
// this one writes would commit, so there is nothing to report — in particular
// not a failed run, which is what treating ErrLeaseLost as an error would
// produce, and which would finish a run the new owner is still executing.
func TestWorkerOutcomeLeaseLostIsNotAnError(t *testing.T) {
	f := newFixture(t, fake.New(slowCall("t1")))
	id := f.submit("lose the lease mid-tool", runOpts{})
	run := claimRun(t, f, "w1", 30*time.Second)

	done := goExecute(context.Background(), newDirectLoop(f, "w1"), run)
	f.slow.waitStarted(t)

	// POST /v1/runs/:id/resume, or a reaper on an expired lease: the run goes
	// back to the queue under this worker's feet.
	ok, err := f.st.ReleaseLease(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)
	close(f.slow.release) // the tool succeeds; its commit is what gets fenced

	res := awaitExecute(t, done)
	require.NoError(t, res.err, "losing a lease is an outcome, not a failure")
	require.Equal(t, runtime.OutcomeLeaseLost, res.outcome)
	require.Equal(t, "lease_lost", res.outcome.String())

	after, err := f.st.GetRun(context.Background(), id)
	require.NoError(t, err)
	require.Nil(t, after.FinishedAt, "the run belongs to whoever claims it next")
	require.Equal(t, runtime.StatusQueued, after.Status)

	evs := f.events(id)
	require.Zero(t, countType(evs, runtime.EventRunFinished))
	require.Zero(t, countType(evs, runtime.EventToolSucceeded), "the fenced result never committed")
	require.Equal(t, int32(1), f.slow.calls.Load())
}
