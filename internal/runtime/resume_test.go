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
)

// TestResumeAfterCrashMidToolCall is the headline durability property. Worker
// A completes one tool call, starts a second, and dies (its context is torn
// down with no cleanup, leaving the lease). Worker B takes over once the lease
// lapses, re-executes only the interrupted call, and finishes the run.
func TestResumeAfterCrashMidToolCall(t *testing.T) {
	f := newFixture(t, fake.New(
		deadlineCall("t1", 30),
		slowCall("t2"),
		finishCall("t3", "done after resume"),
	))
	const lease = 2 * time.Second

	stopA := f.startWorker("worker-a", lease)
	id := f.submit("survive a crash", runOpts{})

	f.slow.waitStarted(t)
	// Sanity: tool 1 is committed, tool 2 is in flight.
	before := f.events(id)
	require.Equal(t, []string{
		runtime.EventRunStarted,
		runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested, runtime.EventToolSucceeded,
		runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested,
	}, eventTypes(before))
	crashSeq := before[len(before)-1].Seq

	stopA() // kill -9

	run, err := f.st.GetRun(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, runtime.StatusRunning, run.Status)
	require.NotNil(t, run.LeaseOwner)
	require.Equal(t, "worker-a", *run.LeaseOwner, "a dead worker leaves its lease behind")

	tc, err := f.st.GetToolCall(context.Background(), id, crashSeq)
	require.NoError(t, err)
	require.Equal(t, store.ToolCallStarted, tc.Status, "interrupted call is still 'started' in the ledger")

	stopB := f.startWorker("worker-b", lease)
	defer stopB()

	// B claims after the lease lapses and re-runs the interrupted tool.
	f.slow.waitStarted(t)
	run, err = f.st.GetRun(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "worker-b", *run.LeaseOwner)

	close(f.slow.release)
	run = f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)
	require.Equal(t, "done after resume", f.state(id).FinalAnswer)

	// Exactly-once for the completed call, at-least-once for the interrupted one.
	require.Equal(t, int32(1), f.deadline.calls.Load(), "completed tool call must not be re-executed")
	require.Equal(t, int32(2), f.slow.calls.Load(), "interrupted tool call is re-executed once")

	evs := f.events(id)
	require.Equal(t, []string{
		runtime.EventRunStarted,
		runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested, runtime.EventToolSucceeded, // deadline, before the crash
		runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested, // slow, requested before the crash
		runtime.EventToolSucceeded, // slow, completed by worker B
		runtime.EventModelRequested, runtime.EventModelResponded,
		runtime.EventToolRequested, runtime.EventToolSucceeded, // finish
		runtime.EventRunFinished,
	}, eventTypes(evs))
	require.Equal(t, 0, countType(evs, runtime.EventToolFailed))
	require.Equal(t, 3, countType(evs, runtime.EventModelRequested), "no model call was repeated")

	slowDone := decode[runtime.ToolSucceededPayload](t, evs[8])
	require.Equal(t, "t2", slowDone.ToolUseID)
	require.False(t, slowDone.Replayed, "re-executed, not replayed from ledger")

	// The ledger row keeps its original seq and is now succeeded.
	tc, err = f.st.GetToolCall(context.Background(), id, crashSeq)
	require.NoError(t, err)
	require.Equal(t, store.ToolCallSucceeded, tc.Status)

	// Worker B's model call saw the whole rebuilt conversation, including
	// the result of the tool it did not run itself.
	reqs := f.provider.Requests()
	require.Len(t, reqs, 3)
	require.Len(t, reqs[2].Messages, 5)
	require.Equal(t, "t1", reqs[2].Messages[2].Content[0].ToolUseID)
	require.Equal(t, "t2", reqs[2].Messages[4].Content[0].ToolUseID)
}

// TestResumeAfterCrashMidModelCall covers the other crash window: the worker
// dies between model_requested and model_responded. The dangling request is
// left in the log and the next worker simply asks the model again.
func TestResumeAfterCrashMidModelCall(t *testing.T) {
	p := fake.New(
		deadlineCall("t1", 30),
		finishCall("unused", "never returned"), // call 1 is interrupted before this is returned
		finishCall("t2", "done after resume"),
	)
	modelStarted := make(chan struct{}, 4)
	p.OnComplete = func(ctx context.Context, _ model.Request, n int) error {
		if n == 1 {
			modelStarted <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	f := newFixture(t, p)
	const lease = 2 * time.Second

	stopA := f.startWorker("worker-a", lease)
	id := f.submit("survive a crash mid-model", runOpts{})
	<-modelStarted
	stopA()

	types := eventTypes(f.events(id))
	require.Equal(t, runtime.EventModelRequested, types[len(types)-1], "dangling model_requested")

	stopB := f.startWorker("worker-b", lease)
	defer stopB()

	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)
	require.Equal(t, "done after resume", f.state(id).FinalAnswer)
	require.Equal(t, int32(1), f.deadline.calls.Load())

	evs := f.events(id)
	require.Equal(t, 3, countType(evs, runtime.EventModelRequested), "1 + dangling + retry")
	require.Equal(t, 2, countType(evs, runtime.EventModelResponded))
	require.Equal(t, 3, f.provider.Calls())
	require.Len(t, f.provider.Requests()[2].Messages, 3)
}

// TestForceResumeViaReleaseLease is the POST /resume path: a run stuck on a
// worker that still heartbeats is handed to another worker on demand.
func TestForceResumeViaReleaseLease(t *testing.T) {
	f := newFixture(t, fake.New(slowCall("t1"), finishCall("t2", "done")))

	stopA := f.startWorker("worker-a", 30*time.Second)
	defer stopA()
	id := f.submit("get stuck", runOpts{})
	f.slow.waitStarted(t)

	ok, err := f.st.ReleaseLease(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)

	stopB := f.startWorker("worker-b", 30*time.Second)
	defer stopB()
	f.slow.waitStarted(t) // B re-runs the tool
	close(f.slow.release)

	run := f.waitTerminal(id)
	require.Equal(t, runtime.StatusSucceeded, run.Status)
	require.Equal(t, 2, countType(f.events(id), runtime.EventToolSucceeded))
	// Worker A's attempt to commit its (now released) tool call was fenced out.
	require.Equal(t, int32(2), f.slow.calls.Load())
}
