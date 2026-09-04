// Package store is the Postgres access layer: run records, the append-only
// event log, and the SKIP LOCKED work queue.
//
// The spec calls for sqlc-generated code. M0 hand-writes the small number of
// queries it needs so the schema can move quickly; swapping in sqlc later is
// mechanical because every query already lives in this one file.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNotFound is returned when a run does not exist.
var ErrNotFound = errors.New("not found")

// ErrLeaseLost is returned by every fenced write when the caller no longer
// holds the run's lease, or the run has already finished. A worker that sees
// it must stop touching the run: another worker owns it now.
var ErrLeaseLost = errors.New("lease lost")

// Tool call ledger statuses.
const (
	ToolCallStarted   = "started"
	ToolCallSucceeded = "succeeded"
	ToolCallFailed    = "failed"
)

// ToolCall mirrors a row of the tool_calls idempotency ledger.
type ToolCall struct {
	RunID      uuid.UUID       `json:"run_id"`
	Seq        int32           `json:"seq"`
	ToolName   string          `json:"tool_name"`
	Args       json.RawMessage `json:"args"`
	Status     string          `json:"status"`
	Result     json.RawMessage `json:"result,omitempty"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// Run mirrors a row of the runs table.
type Run struct {
	ID              uuid.UUID       `json:"id"`
	Status          string          `json:"status"`
	Goal            string          `json:"goal"`
	AgentConfig     json.RawMessage `json:"agent_config"`
	MaxSteps        int32           `json:"max_steps"`
	BudgetUSD       string          `json:"budget_usd"`
	SpentUSD        string          `json:"spent_usd"`
	InputTokens     int64           `json:"input_tokens"`
	OutputTokens    int64           `json:"output_tokens"`
	CancelRequested bool            `json:"cancel_requested"`
	LeaseOwner      *string         `json:"lease_owner,omitempty"`
	LeaseExpiresAt  *time.Time      `json:"lease_expires_at,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`
}

// Event mirrors a row of the append-only run_events table.
type Event struct {
	RunID     uuid.UUID       `json:"run_id"`
	Seq       int32           `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// Store owns a pgx pool. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to Postgres and verifies the connection.
func New(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the underlying pool for callers that need raw access (tests).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

const runColumns = `id, status, goal, agent_config, max_steps, budget_usd::text, spent_usd::text,
	input_tokens, output_tokens, cancel_requested, lease_owner, lease_expires_at, created_at, finished_at`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Status, &r.Goal, &r.AgentConfig, &r.MaxSteps, &r.BudgetUSD,
		&r.SpentUSD, &r.InputTokens, &r.OutputTokens, &r.CancelRequested, &r.LeaseOwner,
		&r.LeaseExpiresAt, &r.CreatedAt, &r.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Migrate applies every embedded migration that has not run yet, in filename
// order, each in its own transaction.
func (s *Store) Migrate(ctx context.Context) error {
	// CREATE TABLE IF NOT EXISTS is not atomic against a concurrent creator,
	// so the bootstrap takes the same advisory lock the migrations do.
	// serve and work both migrate at startup and do race in practice.
	if err := s.withMigrationLock(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
		return err
	}); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if err := s.applyMigration(ctx, name, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, name, body string) error {
	return s.withMigrationLock(ctx, func(tx pgx.Tx) error {
		var exists bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&exists)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
		if _, err := tx.Exec(ctx, body); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name)
		return err
	})
}

// migrationLockID is an arbitrary but fixed advisory lock key that serializes
// concurrent migrators.
const migrationLockID = 4711

// withMigrationLock runs fn in a transaction holding the migration advisory
// lock, committing if fn returns nil.
func (s *Store) withMigrationLock(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateRun inserts a queued run. Workers pick it up from the same table.
func (s *Store) CreateRun(ctx context.Context, goal string, agentConfig json.RawMessage, maxSteps int32, budgetUSD string) (*Run, error) {
	if len(agentConfig) == 0 {
		agentConfig = json.RawMessage(`{}`)
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO runs (id, goal, agent_config, max_steps, budget_usd)
		VALUES ($1, $2, $3, $4, $5::numeric)
		RETURNING `+runColumns,
		uuid.New(), goal, agentConfig, maxSteps, budgetUSD)
	return scanRun(row)
}

// GetRun loads a run by id.
func (s *Store) GetRun(ctx context.Context, id uuid.UUID) (*Run, error) {
	return scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE id = $1`, id))
}

// withRunLease runs fn inside a transaction that holds the run's row lock and
// has verified that owner still holds the lease. Every write the worker makes
// goes through here, so a worker whose lease was stolen (paused, not killed)
// cannot interleave events with the new owner: its transaction fails with
// ErrLeaseLost instead. Locking the row also serializes appends per run,
// which is what makes `max(seq)+1` safe.
func (s *Store) withRunLease(ctx context.Context, runID uuid.UUID, owner string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var leaseOwner *string
	var finishedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT lease_owner, finished_at FROM runs WHERE id = $1 FOR UPDATE`, runID).
		Scan(&leaseOwner, &finishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if finishedAt != nil || leaseOwner == nil || *leaseOwner != owner {
		return ErrLeaseLost
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// appendEventTx inserts the next event for a run. The caller must hold the
// run row lock.
func appendEventTx(ctx context.Context, tx pgx.Tx, runID uuid.UUID, eventType string, payload any) (*Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	var ev Event
	err = tx.QueryRow(ctx, `
		INSERT INTO run_events (run_id, seq, type, payload)
		VALUES ($1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1), $2, $3)
		RETURNING run_id, seq, type, payload, created_at`,
		runID, eventType, raw).Scan(&ev.RunID, &ev.Seq, &ev.Type, &ev.Payload, &ev.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

// AppendEvent appends one event to a run's log, fenced by the lease.
func (s *Store) AppendEvent(ctx context.Context, runID uuid.UUID, owner, eventType string, payload any) (*Event, error) {
	var ev *Event
	err := s.withRunLease(ctx, runID, owner, func(tx pgx.Tx) error {
		var err error
		ev, err = appendEventTx(ctx, tx, runID, eventType, payload)
		return err
	})
	return ev, err
}

// AppendModelResponse appends a model_responded event and bumps the run's
// token and spend counters in the same transaction, so the log and the
// counters can never disagree. Cost is in micro-USD.
func (s *Store) AppendModelResponse(ctx context.Context, runID uuid.UUID, owner, eventType string, payload any,
	inputTokens, outputTokens, costMicroUSD int64) (*Event, error) {
	var ev *Event
	err := s.withRunLease(ctx, runID, owner, func(tx pgx.Tx) error {
		var err error
		if ev, err = appendEventTx(ctx, tx, runID, eventType, payload); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE runs SET
				input_tokens  = input_tokens  + $2,
				output_tokens = output_tokens + $3,
				spent_usd     = spent_usd + ($4::numeric / 1000000)
			WHERE id = $1`, runID, inputTokens, outputTokens, costMicroUSD)
		return err
	})
	return ev, err
}

// RequestToolCall appends a tool_requested event and opens the ledger row
// keyed by that event's seq, in one transaction. ON CONFLICT DO NOTHING keeps
// it idempotent should the same seq ever be requested twice.
func (s *Store) RequestToolCall(ctx context.Context, runID uuid.UUID, owner, eventType string, payload any,
	toolName string, args json.RawMessage) (*Event, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var ev *Event
	err := s.withRunLease(ctx, runID, owner, func(tx pgx.Tx) error {
		var err error
		if ev, err = appendEventTx(ctx, tx, runID, eventType, payload); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO tool_calls (run_id, seq, tool_name, args, status)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (run_id, seq) DO NOTHING`, runID, ev.Seq, toolName, args, ToolCallStarted)
		return err
	})
	return ev, err
}

// CompleteToolCall records a tool's outcome in the ledger and appends the
// matching tool_succeeded or tool_failed event, in one transaction. Because
// the two are atomic, a resumed worker never finds a completed ledger row
// without its event, and never re-executes a call whose result committed.
func (s *Store) CompleteToolCall(ctx context.Context, runID uuid.UUID, owner string, seq int32,
	status string, result json.RawMessage, eventType string, payload any) (*Event, error) {
	var ev *Event
	err := s.withRunLease(ctx, runID, owner, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE tool_calls SET status = $3, result = $4, finished_at = now()
			WHERE run_id = $1 AND seq = $2`, runID, seq, status, result)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("tool call (%s, %d) has no ledger row", runID, seq)
		}
		ev, err = appendEventTx(ctx, tx, runID, eventType, payload)
		return err
	})
	return ev, err
}

// GetToolCall loads one ledger row. Returns ErrNotFound when absent.
func (s *Store) GetToolCall(ctx context.Context, runID uuid.UUID, seq int32) (*ToolCall, error) {
	var tc ToolCall
	err := s.pool.QueryRow(ctx, `
		SELECT run_id, seq, tool_name, args, status, result, started_at, finished_at
		FROM tool_calls WHERE run_id = $1 AND seq = $2`, runID, seq).
		Scan(&tc.RunID, &tc.Seq, &tc.ToolName, &tc.Args, &tc.Status, &tc.Result, &tc.StartedAt, &tc.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &tc, nil
}

// ListToolCalls returns a run's ledger in seq order.
func (s *Store) ListToolCalls(ctx context.Context, runID uuid.UUID) ([]ToolCall, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT run_id, seq, tool_name, args, status, result, started_at, finished_at
		FROM tool_calls WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolCall
	for rows.Next() {
		var tc ToolCall
		if err := rows.Scan(&tc.RunID, &tc.Seq, &tc.ToolName, &tc.Args, &tc.Status, &tc.Result, &tc.StartedAt, &tc.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// ListEvents returns a run's events with seq strictly greater than afterSeq,
// in order. Pass 0 for the whole log.
func (s *Store) ListEvents(ctx context.Context, runID uuid.UUID, afterSeq int32) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT run_id, seq, type, payload, created_at
		FROM run_events WHERE run_id = $1 AND seq > $2 ORDER BY seq`, runID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.RunID, &ev.Seq, &ev.Type, &ev.Payload, &ev.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ClaimRun leases one claimable run for the given owner: a queued run, or a
// running one whose lease expired (the kill -9 recovery path). Returns nil
// when the queue is empty.
//
// FOR UPDATE SKIP LOCKED is what lets several workers poll the same table
// without coordinating through a separate broker.
func (s *Store) ClaimRun(ctx context.Context, owner string, lease time.Duration) (*Run, error) {
	row := s.pool.QueryRow(ctx, `
		WITH claimed AS (
			SELECT id FROM runs
			WHERE status = 'queued'
			   OR (status = 'running' AND (lease_expires_at IS NULL OR lease_expires_at < now()))
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE runs SET status = 'running', lease_owner = $1, lease_expires_at = now() + $2::interval
		WHERE id IN (SELECT id FROM claimed)
		RETURNING `+runColumns, owner, lease.String())
	run, err := scanRun(row)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return run, err
}

// Heartbeat extends a lease the worker still holds. It reports false if the
// lease was stolen, which tells the worker to stop touching the run.
func (s *Store) Heartbeat(ctx context.Context, runID uuid.UUID, owner string, lease time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs SET lease_expires_at = now() + $3::interval
		WHERE id = $1 AND lease_owner = $2 AND finished_at IS NULL`, runID, owner, lease.String())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FinishRun appends the run_finished event and moves the run to its terminal
// status in one transaction, fenced by the lease. A client can therefore
// never observe a finished log against a still-running status, or the
// reverse.
func (s *Store) FinishRun(ctx context.Context, runID uuid.UUID, owner, status, eventType string, payload any) (*Event, error) {
	var ev *Event
	err := s.withRunLease(ctx, runID, owner, func(tx pgx.Tx) error {
		var err error
		if ev, err = appendEventTx(ctx, tx, runID, eventType, payload); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE runs SET status = $2::run_status, finished_at = now(), lease_owner = NULL, lease_expires_at = NULL
			WHERE id = $1`, runID, status)
		return err
	})
	return ev, err
}

// ReapExpiredLeases returns every running run whose lease has lapsed to the
// queue. ClaimRun would pick such runs up anyway; the reaper makes the
// hand-off explicit and observable. Returns the number of runs requeued.
func (s *Store) ReapExpiredLeases(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs SET status = 'queued', lease_owner = NULL, lease_expires_at = NULL
		WHERE status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ReleaseLease forcibly returns one unfinished run to the queue regardless of
// its lease state. It backs POST /v1/runs/:id/resume for runs stuck on a
// worker that is alive enough to heartbeat but not making progress. Returns
// false if the run is already finished.
func (s *Store) ReleaseLease(ctx context.Context, runID uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs SET status = 'queued', lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $1 AND finished_at IS NULL`, runID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RequestCancel sets the cooperative cancel flag. The worker notices it at
// its next loop iteration, appends cancel_requested, and finishes the run as
// cancelled. Returns false if the run is already finished.
func (s *Store) RequestCancel(ctx context.Context, runID uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs SET cancel_requested = true WHERE id = $1 AND finished_at IS NULL`, runID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
