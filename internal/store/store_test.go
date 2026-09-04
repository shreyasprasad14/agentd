package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/testutil"
)

func TestLeaseFencing(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()

	run, err := st.CreateRun(ctx, "fence me", nil, 5, "1.00")
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

	short, err := st.CreateRun(ctx, "short lease", nil, 5, "1.00")
	require.NoError(t, err)
	long, err := st.CreateRun(ctx, "long lease", nil, 5, "1.00")
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

	run, err := st.CreateRun(ctx, "ledger", nil, 5, "1.00")
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

	_, err = st.CompleteToolCall(ctx, run.ID, "a", req.Seq, store.ToolCallSucceeded, json.RawMessage(`{"ok":true}`), "tool_succeeded", map[string]any{})
	require.NoError(t, err)
	tc, err = st.GetToolCall(ctx, run.ID, req.Seq)
	require.NoError(t, err)
	require.Equal(t, store.ToolCallSucceeded, tc.Status)
	require.NotNil(t, tc.FinishedAt)
	require.JSONEq(t, `{"ok":true}`, string(tc.Result))

	// Completing a call that was never requested is a bug, not a no-op.
	_, err = st.CompleteToolCall(ctx, run.ID, "a", 999, store.ToolCallSucceeded, nil, "tool_succeeded", map[string]any{})
	require.Error(t, err)
	// And the failed transaction appended nothing.
	evs, err := st.ListEvents(ctx, run.ID, 0)
	require.NoError(t, err)
	require.Len(t, evs, 3)

	_, err = st.GetToolCall(ctx, run.ID, 999)
	require.ErrorIs(t, err, store.ErrNotFound)
}
