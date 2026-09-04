// Package testutil shares the real-Postgres fixture across integration test
// packages. One pgvector container is started per test binary; each test gets
// its own freshly migrated database inside it, so tests stay isolated without
// paying container start-up per test.
package testutil

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/shreyasprasad/agentd/internal/store"
)

var (
	once    sync.Once
	adminDS string
	bootErr error
	dbSeq   int
	dbMu    sync.Mutex
)

func boot() {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "pgvector/pgvector:pg16",
		tcpostgres.WithDatabase("agentd"),
		tcpostgres.WithUsername("agentd"),
		tcpostgres.WithPassword("agentd"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		bootErr = fmt.Errorf("start postgres container: %w", err)
		return
	}
	// The container is reaped by testcontainers' Ryuk sidecar when the test
	// process exits; there is no package-level hook to terminate it earlier.
	adminDS, bootErr = container.ConnectionString(ctx, "sslmode=disable")
}

// Postgres returns a Store connected to a fresh, migrated database. It skips
// the test under -short, since it needs Docker.
func Postgres(t *testing.T) *store.Store {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test requires Docker")
	}
	once.Do(boot)
	require.NoError(t, bootErr)

	ctx := context.Background()
	dbMu.Lock()
	dbSeq++
	name := fmt.Sprintf("t%d_%d", time.Now().UnixNano()%1_000_000, dbSeq)
	dbMu.Unlock()

	admin, err := pgx.Connect(ctx, adminDS)
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, admin.Close(ctx))
	require.NoError(t, err)

	u, err := url.Parse(adminDS)
	require.NoError(t, err)
	u.Path = "/" + name

	st, err := store.New(ctx, u.String())
	require.NoError(t, err)
	t.Cleanup(st.Close)

	require.NoError(t, st.Migrate(ctx))
	// Migrations must be idempotent: serve and work both apply them at boot.
	require.NoError(t, st.Migrate(ctx))
	return st
}

// WaitFor polls cond until it returns true or the timeout elapses.
func WaitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
