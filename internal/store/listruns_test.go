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

// ids returns the run ids in order, which is what every ordering assertion
// below compares: a mismatch then prints two short id lists rather than two
// dumps of whole Run structs.
func ids(runs []store.Run) []uuid.UUID {
	out := make([]uuid.UUID, len(runs))
	for i, r := range runs {
		out[i] = r.ID
	}
	return out
}

func TestListRunsOrderingAndFilters(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()

	// Five runs, oldest first, then driven into four different statuses.
	// ClaimRun takes the oldest queued run, so the claims below walk the
	// runs in creation order.
	var created []*store.Run
	for _, goal := range []string{"one", "two", "three", "four", "five"} {
		run, err := st.CreateRun(ctx, store.NewRun{Goal: goal, MaxSteps: 5, BudgetUSD: "1.00"})
		require.NoError(t, err)
		created = append(created, run)
	}
	first, second, third, fourth, fifth := created[0], created[1], created[2], created[3], created[4]

	claimed, err := st.ClaimRun(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.Equal(t, first.ID, claimed.ID)
	_, err = st.FinishRun(ctx, first.ID, "w1", "succeeded", "run_finished", map[string]any{"status": "succeeded"})
	require.NoError(t, err)

	claimed, err = st.ClaimRun(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.Equal(t, second.ID, claimed.ID)
	_, err = st.FinishRun(ctx, second.ID, "w1", "failed", "run_finished", map[string]any{"status": "failed"})
	require.NoError(t, err)

	// Third stays running; fourth and fifth stay queued.
	claimed, err = st.ClaimRun(ctx, "w2", time.Minute)
	require.NoError(t, err)
	require.Equal(t, third.ID, claimed.ID)

	all, err := st.ListRuns(ctx, store.ListRunsOptions{})
	require.NoError(t, err)
	require.Equal(t,
		[]uuid.UUID{fifth.ID, fourth.ID, third.ID, second.ID, first.ID},
		ids(all), "newest first")

	// The listing must be byte-identical to GET /v1/runs/:id for the same run,
	// which is the whole reason it selects GetRun's columns. Comparing the
	// marshalled JSON, not the structs, is what actually checks that.
	single, err := st.GetRun(ctx, third.ID)
	require.NoError(t, err)
	wantJSON, err := json.Marshal(single)
	require.NoError(t, err)
	gotJSON, err := json.Marshal(all[2])
	require.NoError(t, err)
	require.JSONEq(t, string(wantJSON), string(gotJSON))
	require.NotNil(t, all[2].LeaseOwner, "the running run carries its lease through the listing")

	limited, err := st.ListRuns(ctx, store.ListRunsOptions{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{fifth.ID, fourth.ID}, ids(limited))

	queued, err := st.ListRuns(ctx, store.ListRunsOptions{Status: "queued"})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{fifth.ID, fourth.ID}, ids(queued))

	succeeded, err := st.ListRuns(ctx, store.ListRunsOptions{Status: "succeeded"})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{first.ID}, ids(succeeded))

	// The filter and the limit compose: the limit applies after the filter,
	// not to the rows the filter scanned.
	oneQueued, err := st.ListRuns(ctx, store.ListRunsOptions{Status: "queued", Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{fifth.ID}, ids(oneQueued))

	// A status outside the run_status enum lists nothing instead of failing
	// the query, so a bad query parameter cannot 500 the endpoint.
	none, err := st.ListRuns(ctx, store.ListRunsOptions{Status: "not-a-status"})
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestListRunsTiebreaksOnID(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()

	// created_at ties are what the id tiebreaker exists for, and CreateRun
	// cannot produce one: the timestamps have to be forced to be equal.
	stamp := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	want := []uuid.UUID{
		uuid.MustParse("cccccccc-0000-4000-8000-000000000003"),
		uuid.MustParse("bbbbbbbb-0000-4000-8000-000000000002"),
		uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001"),
	}
	// Inserted ascending, so a listing that merely preserved insertion order
	// would fail this.
	for i := len(want) - 1; i >= 0; i-- {
		_, err := st.Pool().Exec(ctx, `
			INSERT INTO runs (id, goal, agent_config, max_steps, budget_usd, created_at)
			VALUES ($1, 'tied', '{}'::jsonb, 5, 1.00, $2)`, want[i], stamp)
		require.NoError(t, err)
	}

	tied, err := st.ListRuns(ctx, store.ListRunsOptions{})
	require.NoError(t, err)
	require.Equal(t, want, ids(tied), "ties break on id DESC")
}

func TestListRunsDefaultAndCap(t *testing.T) {
	st := testutil.Postgres(t)
	ctx := context.Background()

	// More runs than the cap, inserted in one statement: the bounds are what
	// is under test, not CreateRun, and 250 round trips would dominate this
	// test's runtime for nothing.
	const total = store.MaxRunLimit + 50
	_, err := st.Pool().Exec(ctx, `
		INSERT INTO runs (id, goal, agent_config, max_steps, budget_usd, created_at)
		SELECT gen_random_uuid(), 'bulk ' || i, '{}'::jsonb, 5, 1.00, now() - (i || ' seconds')::interval
		FROM generate_series(1, $1) AS i`, total)
	require.NoError(t, err)

	def, err := st.ListRuns(ctx, store.ListRunsOptions{})
	require.NoError(t, err)
	require.Len(t, def, store.DefaultRunLimit, "an unbounded listing falls back to the default")

	negative, err := st.ListRuns(ctx, store.ListRunsOptions{Limit: -1})
	require.NoError(t, err)
	require.Len(t, negative, store.DefaultRunLimit, "a nonsense limit falls back too")

	capped, err := st.ListRuns(ctx, store.ListRunsOptions{Limit: total * 10})
	require.NoError(t, err)
	require.Len(t, capped, store.MaxRunLimit, "an oversized limit is clamped, not rejected")

	under, err := st.ListRuns(ctx, store.ListRunsOptions{Limit: 3})
	require.NoError(t, err)
	require.Len(t, under, 3)

	// The cap truncates the tail, not the head: the newest runs survive it.
	require.Equal(t, ids(under), ids(capped)[:3])
}
