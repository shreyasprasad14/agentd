package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/testutil"
)

func TestLeaseFencing(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()

	run, err := st.CreateRun(ctx, store.NewRun{Goal: "fence me", MaxSteps: 5, BudgetUSD: "1.00"})
	require.NoError(t, err)

	// Nobody holds the lease yet: every fenced write is refused.
	_, err = st.AppendEvent(ctx, run.ID, "a", "run_started", map[string]any{})
	require.ErrorIs(t, err, store.ErrLeaseLost)

	claimed, err := st.ClaimRun(ctx, "a", time.Minute)
	require.NoError(t, err)
	require.Equal(t, run.ID, claimed.ID)

	_, err = st.AppendEvent(ctx, run.ID, "b", "run_started", map[string]any{})
	require.ErrorIs(t, err, store.ErrLeaseLost, "a non-holder cannot append")

	ev, err := st.AppendEvent(ctx, run.ID, "a", "run_started", map[string]any{})
	require.NoError(t, err)
	require.Equal(t, int32(1), ev.Seq)

	// Force-release, then another worker claims. The old holder is fenced.
	ok, err := st.ReleaseLease(ctx, run.ID)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = st.AppendEvent(ctx, run.ID, "a", "x", map[string]any{})
	require.ErrorIs(t, err, store.ErrLeaseLost)

	claimed, err = st.ClaimRun(ctx, "b", time.Minute)
	require.NoError(t, err)
	require.Equal(t, run.ID, claimed.ID)
	alive, err := st.Heartbeat(ctx, run.ID, "a", time.Minute)
	require.NoError(t, err)
	require.False(t, alive, "stale heartbeat reports the lease as lost")
	alive, err = st.Heartbeat(ctx, run.ID, "b", time.Minute)
	require.NoError(t, err)
	require.True(t, alive)

	// Finishing is fenced and drops the lease; nothing can follow it.
	_, err = st.FinishRun(ctx, run.ID, "a", "failed", "run_finished", map[string]any{})
	require.ErrorIs(t, err, store.ErrLeaseLost)
	_, err = st.FinishRun(ctx, run.ID, "b", "succeeded", "run_finished", map[string]any{"status": "succeeded"})
	require.NoError(t, err)
	_, err = st.AppendEvent(ctx, run.ID, "b", "x", map[string]any{})
	require.ErrorIs(t, err, store.ErrLeaseLost)

	got, err := st.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", got.Status)
	require.Nil(t, got.LeaseOwner)
	require.NotNil(t, got.FinishedAt)
	ok, err = st.ReleaseLease(ctx, run.ID)
	require.NoError(t, err)
	require.False(t, ok, "finished runs cannot be released")
	ok, err = st.RequestCancel(ctx, run.ID)
	require.NoError(t, err)
	require.False(t, ok, "finished runs cannot be cancelled")
}

func TestReaperRequeuesOnlyExpiredLeases(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()

	short, err := st.CreateRun(ctx, store.NewRun{Goal: "short lease", MaxSteps: 5, BudgetUSD: "1.00"})
	require.NoError(t, err)
	long, err := st.CreateRun(ctx, store.NewRun{Goal: "long lease", MaxSteps: 5, BudgetUSD: "1.00"})
	require.NoError(t, err)

	c1, err := st.ClaimRun(ctx, "a", 100*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, short.ID, c1.ID, "claims in created_at order")
	c2, err := st.ClaimRun(ctx, "b", time.Minute)
	require.NoError(t, err)
	require.Equal(t, long.ID, c2.ID)

	time.Sleep(200 * time.Millisecond)
	n, err := st.ReapExpiredLeases(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	got, err := st.GetRun(ctx, short.ID)
	require.NoError(t, err)
	require.Equal(t, "queued", got.Status)
	require.Nil(t, got.LeaseOwner)
	got, err = st.GetRun(ctx, long.ID)
	require.NoError(t, err)
	require.Equal(t, "running", got.Status)
	require.Equal(t, "b", *got.LeaseOwner)

	// Reclaimed by a new worker; the old one is fenced out.
	c3, err := st.ClaimRun(ctx, "c", time.Minute)
	require.NoError(t, err)
	require.Equal(t, short.ID, c3.ID)
	_, err = st.AppendEvent(ctx, short.ID, "a", "x", map[string]any{})
	require.ErrorIs(t, err, store.ErrLeaseLost)
}

func TestToolCallLedgerAndCounters(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()

	run, err := st.CreateRun(ctx, store.NewRun{Goal: "ledger", MaxSteps: 5, BudgetUSD: "1.00"})
	require.NoError(t, err)
	_, err = st.ClaimRun(ctx, "a", time.Minute)
	require.NoError(t, err)

	_, err = st.AppendModelResponse(ctx, run.ID, "a", "model_responded", map[string]any{}, 100, 10, 1_234_567)
	require.NoError(t, err)
	got, err := st.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, int64(100), got.InputTokens)
	require.Equal(t, int64(10), got.OutputTokens)
	require.Equal(t, "1.2346", got.SpentUSD, "numeric(10,4) rounds the micro-USD sum")

	args := json.RawMessage(`{"n":1}`)
	req, err := st.RequestToolCall(ctx, run.ID, "a", "tool_requested", map[string]any{}, "t", args)
	require.NoError(t, err)
	tc, err := st.GetToolCall(ctx, run.ID, req.Seq)
	require.NoError(t, err)
	require.Equal(t, store.ToolCallStarted, tc.Status)
	require.Equal(t, "t", tc.ToolName)
	require.JSONEq(t, `{"n":1}`, string(tc.Args))
	require.Nil(t, tc.FinishedAt)

	_, err = st.CompleteToolCall(ctx, run.ID, "a", req.Seq, store.ToolCallSucceeded, json.RawMessage(`{"ok":true}`), "tool_succeeded", map[string]any{}, 0, 0, 0)
	require.NoError(t, err)
	tc, err = st.GetToolCall(ctx, run.ID, req.Seq)
	require.NoError(t, err)
	require.Equal(t, store.ToolCallSucceeded, tc.Status)
	require.NotNil(t, tc.FinishedAt)
	require.JSONEq(t, `{"ok":true}`, string(tc.Result))

	// Completing a call that was never requested is a bug, not a no-op.
	_, err = st.CompleteToolCall(ctx, run.ID, "a", 999, store.ToolCallSucceeded, nil, "tool_succeeded", map[string]any{}, 0, 0, 0)
	require.Error(t, err)
	// And the failed transaction appended nothing.
	evs, err := st.ListEvents(ctx, run.ID, 0)
	require.NoError(t, err)
	require.Len(t, evs, 3)

	_, err = st.GetToolCall(ctx, run.ID, 999)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestTraceIDsRoundTrip(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()

	traced, err := st.CreateRun(ctx, store.NewRun{
		Goal: "traced", MaxSteps: 5, BudgetUSD: "1.00",
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", RootSpanID: "00f067aa0ba902b7",
	})
	require.NoError(t, err)
	require.NotNil(t, traced.TraceID)
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", *traced.TraceID)
	require.NotNil(t, traced.RootSpanID)
	require.Equal(t, "00f067aa0ba902b7", *traced.RootSpanID)

	// The ids have to survive the trip through the row, since the worker that
	// reads them is not the process that generated them.
	got, err := st.GetRun(ctx, traced.ID)
	require.NoError(t, err)
	require.Equal(t, traced.TraceID, got.TraceID)
	require.Equal(t, traced.RootSpanID, got.RootSpanID)

	// Tracing off stores NULL rather than "", so /trace can 404 on it.
	untraced, err := st.CreateRun(ctx, store.NewRun{Goal: "untraced", MaxSteps: 5, BudgetUSD: "1.00"})
	require.NoError(t, err)
	require.Nil(t, untraced.TraceID)
	require.Nil(t, untraced.RootSpanID)
	got, err = st.GetRun(ctx, untraced.ID)
	require.NoError(t, err)
	require.Nil(t, got.TraceID)
	require.Nil(t, got.RootSpanID)

	// ClaimRun shares runColumns with CreateRun and GetRun; claiming proves
	// the third query scans the new columns in the same positions.
	claimed, err := st.ClaimRun(ctx, "a", time.Minute)
	require.NoError(t, err)
	require.Equal(t, traced.ID, claimed.ID)
	require.Equal(t, traced.TraceID, claimed.TraceID)
	require.Equal(t, traced.RootSpanID, claimed.RootSpanID)
}

func TestCountersAccumulateAcrossModelAndToolSpend(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()

	run, err := st.CreateRun(ctx, store.NewRun{Goal: "spend", MaxSteps: 5, BudgetUSD: "10.00"})
	require.NoError(t, err)
	_, err = st.ClaimRun(ctx, "a", time.Minute)
	require.NoError(t, err)

	_, err = st.AppendModelResponse(ctx, run.ID, "a", "model_responded", map[string]any{}, 100, 10, 1_250_000)
	require.NoError(t, err)

	// A tool that called a model inside itself contributes to the same
	// counters, so spent_usd is the fold of both event kinds (ADR-23).
	req, err := st.RequestToolCall(ctx, run.ID, "a", "tool_requested", map[string]any{}, "search_corpus", nil)
	require.NoError(t, err)
	_, err = st.CompleteToolCall(ctx, run.ID, "a", req.Seq, store.ToolCallSucceeded,
		json.RawMessage(`{"ok":true}`), "tool_succeeded", map[string]any{}, 900, 40, 500_000)
	require.NoError(t, err)

	got, err := st.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1000), got.InputTokens)
	require.Equal(t, int64(50), got.OutputTokens)
	require.Equal(t, "1.7500", got.SpentUSD)

	// A builtin spends nothing, and must leave the counters exactly alone.
	req, err = st.RequestToolCall(ctx, run.ID, "a", "tool_requested", map[string]any{}, "read_file", nil)
	require.NoError(t, err)
	_, err = st.CompleteToolCall(ctx, run.ID, "a", req.Seq, store.ToolCallSucceeded,
		json.RawMessage(`{"ok":true}`), "tool_succeeded", map[string]any{}, 0, 0, 0)
	require.NoError(t, err)

	got, err = st.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1000), got.InputTokens)
	require.Equal(t, int64(50), got.OutputTokens)
	require.Equal(t, "1.7500", got.SpentUSD)
}

func TestIsCancelRequested(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()

	run, err := st.CreateRun(ctx, store.NewRun{Goal: "cancel me", MaxSteps: 5, BudgetUSD: "1.00"})
	require.NoError(t, err)

	requested, err := st.IsCancelRequested(ctx, run.ID)
	require.NoError(t, err)
	require.False(t, requested)

	ok, err := st.RequestCancel(ctx, run.ID)
	require.NoError(t, err)
	require.True(t, ok)

	requested, err = st.IsCancelRequested(ctx, run.ID)
	require.NoError(t, err)
	require.True(t, requested, "the watcher sees the flag the API set")

	_, err = st.IsCancelRequested(ctx, uuid.New())
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestRunCountsGroupsByStatus(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()

	counts, err := st.RunCounts(ctx)
	require.NoError(t, err)
	require.Empty(t, counts, "an empty table reports no statuses, not zeroes")

	for _, goal := range []string{"first", "second", "third"} {
		_, err := st.CreateRun(ctx, store.NewRun{Goal: goal, MaxSteps: 5, BudgetUSD: "1.00"})
		require.NoError(t, err)
	}
	running, err := st.ClaimRun(ctx, "a", time.Minute)
	require.NoError(t, err)
	finished, err := st.ClaimRun(ctx, "b", time.Minute)
	require.NoError(t, err)
	_, err = st.FinishRun(ctx, finished.ID, "b", "failed", "run_finished", map[string]any{})
	require.NoError(t, err)

	counts, err = st.RunCounts(ctx)
	require.NoError(t, err)
	byStatus := map[string]int64{}
	for _, c := range counts {
		byStatus[c.Status] = c.Count
	}
	require.Equal(t, map[string]int64{"queued": 1, "running": 1, "failed": 1}, byStatus)
	require.NotEqual(t, running.ID, finished.ID)
}

func TestOldestQueuedAge(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()

	age, ok, err := st.OldestQueuedAge(ctx)
	require.NoError(t, err)
	require.False(t, ok, "an empty queue has no oldest run")
	require.Zero(t, age)

	oldest, err := st.CreateRun(ctx, store.NewRun{Goal: "oldest", MaxSteps: 5, BudgetUSD: "1.00"})
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)
	_, err = st.CreateRun(ctx, store.NewRun{Goal: "newer", MaxSteps: 5, BudgetUSD: "1.00"})
	require.NoError(t, err)

	age, ok, err = st.OldestQueuedAge(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.GreaterOrEqual(t, age, 50*time.Millisecond, "the age is the oldest run's, not the newest's")

	// Claiming takes a run out of the queue, so the signal tracks what is
	// still waiting rather than what has ever waited.
	claimed, err := st.ClaimRun(ctx, "a", time.Minute)
	require.NoError(t, err)
	require.Equal(t, oldest.ID, claimed.ID)
	after, ok, err := st.OldestQueuedAge(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Less(t, after, age)

	_, err = st.ClaimRun(ctx, "b", time.Minute)
	require.NoError(t, err)
	_, ok, err = st.OldestQueuedAge(ctx)
	require.NoError(t, err)
	require.False(t, ok)
}
