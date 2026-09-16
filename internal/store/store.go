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
	// TraceID and RootSpanID are null for runs submitted with tracing off and
	// for every run created before M4, which is why they are pointers: the
	// trace endpoint has to tell "no trace" apart from "trace id is empty".
	TraceID    *string    `json:"trace_id,omitempty"`
	RootSpanID *string    `json:"root_span_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
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

// runColumns is shared by every query that returns a whole run — CreateRun,
// GetRun and ClaimRun — so a column added here must be added to scanRun in the
// same position. A mismatch is a scan error at runtime, not a compile error.
const runColumns = `id, status, goal, agent_config, max_steps, budget_usd::text, spent_usd::text,
	input_tokens, output_tokens, cancel_requested, lease_owner, lease_expires_at,
	trace_id, root_span_id, created_at, finished_at`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Status, &r.Goal, &r.AgentConfig, &r.MaxSteps, &r.BudgetUSD,
		&r.SpentUSD, &r.InputTokens, &r.OutputTokens, &r.CancelRequested, &r.LeaseOwner,
		&r.LeaseExpiresAt, &r.TraceID, &r.RootSpanID, &r.CreatedAt, &r.FinishedAt)
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

// NewRun is the input to CreateRun.
type NewRun struct {
	Goal        string
	AgentConfig json.RawMessage
	MaxSteps    int32
	BudgetUSD   string
	// TraceID and RootSpanID tie every span the run ever emits, from any
	// process and any attempt, into one trace (ADR-24). Empty when tracing
	// is off, which stores NULL and makes GET /v1/runs/:id/trace a 404.
	TraceID    string
	RootSpanID string
}

// nullString maps the zero string to a NULL column. An absent trace id must
// read back as NULL rather than as the empty string, because the difference is
// what the trace endpoint answers 404 on.
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// CreateRun inserts a queued run. Workers pick it up from the same table.
func (s *Store) CreateRun(ctx context.Context, p NewRun) (*Run, error) {
	agentConfig := p.AgentConfig
	if len(agentConfig) == 0 {
		agentConfig = json.RawMessage(`{}`)
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO runs (id, goal, agent_config, max_steps, budget_usd, trace_id, root_span_id)
		VALUES ($1, $2, $3, $4, $5::numeric, $6, $7)
		RETURNING `+runColumns,
		uuid.New(), p.Goal, agentConfig, p.MaxSteps, p.BudgetUSD,
		nullString(p.TraceID), nullString(p.RootSpanID))
	return scanRun(row)
}

// GetRun loads a run by id.
func (s *Store) GetRun(ctx context.Context, id uuid.UUID) (*Run, error) {
	return scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE id = $1`, id))
}

// Bounds on a run listing. The default is what an unparameterised listing
// returns: enough rows to fill a viewer's first screen without making the
// endpoint's cheapest call its most expensive one. The cap bounds the worst
// case a caller can ask for, since every row carries the run's whole
// agent_config and there is no index that satisfies the ordering.
const (
	DefaultRunLimit = 50
	MaxRunLimit     = 200
)

// ListRunsOptions narrows and bounds ListRuns. The zero value is meaningful:
// the newest DefaultRunLimit runs, of every status.
//
// It is a struct rather than two parameters because a listing is the endpoint
// most likely to grow filters later (by goal, by trace id, by date), and each
// one would otherwise be another positional argument that every existing call
// site has to pass a zero for.
type ListRunsOptions struct {
	// Status, when non-empty, keeps only runs in that state. An unrecognised
	// status matches nothing rather than failing — see ListRuns.
	Status string
	// Limit caps the rows returned. Zero or negative means DefaultRunLimit.
	// Anything larger than MaxRunLimit is clamped rather than rejected: a
	// caller asking for too much gets the most it may have, which is a more
	// useful answer than an error it can only respond to by asking again.
	Limit int
}

// ListRuns returns runs newest first.
//
// It selects runColumns — exactly what GetRun returns — so a run in the list
// and the same run from GET /v1/runs/:id marshal to identical JSON, and a
// viewer can render a row without refetching it. The alternative, a trimmed
// summary row, would save sending agent_config for runs nobody opens; at the
// scale this endpoint serves, giving the client two shapes of "a run" to
// reconcile costs more than the bytes do.
//
// created_at is not a total order — runs created in the same transaction, or
// within one clock tick, tie — so id breaks ties. Without a tiebreaker
// Postgres may return tied rows in a different order each call, which would
// make the listing flicker under a poll and, once this grows keyset
// pagination, silently drop or repeat runs at a page boundary.
//
// The status filter compares status::text instead of casting the argument to
// run_status: a value outside the enum then lists nothing, rather than failing
// the whole query with a cast error the handler would have to translate. The
// rejected alternative is validating the status in Go, which means keeping a
// copy of the enum here for a migration to silently desync from. The cast
// costs nothing in practice because no index covers this ordering anyway.
//
// An empty result is a nil slice, as in ListEvents; the API layer is what
// renders it as [].
func (s *Store) ListRuns(ctx context.Context, opts ListRunsOptions) ([]Run, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultRunLimit
	}
	if limit > MaxRunLimit {
		limit = MaxRunLimit
	}

	args := []any{limit}
	where := ""
	if opts.Status != "" {
		args = append(args, opts.Status)
		where = fmt.Sprintf(" WHERE status::text = $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+runColumns+` FROM runs`+where+`
		ORDER BY created_at DESC, id DESC
		LIMIT $1`, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		// pgx.Rows satisfies pgx.Row, so the listing scans through the same
		// scanRun that GetRun uses. That is the point: runColumns and scanRun
		// have to agree positionally, and a second hand-written scan here
		// would be a second place for a future column to be forgotten.
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, *run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	return out, nil
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

// bumpCounters adds one call's usage to the run's counters. It is the only
// writer of spent_usd, input_tokens, and output_tokens: both the model path
// and the tool path go through it, inside the same transaction as the event
// that reports the spend. A zero cost and zero tokens are a no-op.
func bumpCounters(ctx context.Context, tx pgx.Tx, runID uuid.UUID, inputTokens, outputTokens, costMicroUSD int64) error {
	// Every builtin tool completes with nothing to add, so skipping the
	// statement entirely — rather than adding zero — keeps the common tool
	// path at one round trip.
	if inputTokens == 0 && outputTokens == 0 && costMicroUSD == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE runs SET
			input_tokens  = input_tokens  + $2,
			output_tokens = output_tokens + $3,
			spent_usd     = spent_usd + ($4::numeric / 1000000)
		WHERE id = $1`, runID, inputTokens, outputTokens, costMicroUSD)
	return err
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
		return bumpCounters(ctx, tx, runID, inputTokens, outputTokens, costMicroUSD)
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
//
// The counter arguments are model spend the tool incurred inside itself, and
// they are committed with the event for the same reason the model path's are:
// the budget bounds the run, not only the loop (ADR-23). Every builtin passes
// zeroes.
func (s *Store) CompleteToolCall(ctx context.Context, runID uuid.UUID, owner string, seq int32,
	status string, result json.RawMessage, eventType string, payload any,
	inputTokens, outputTokens, costMicroUSD int64) (*Event, error) {
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
		if ev, err = appendEventTx(ctx, tx, runID, eventType, payload); err != nil {
			return err
		}
		return bumpCounters(ctx, tx, runID, inputTokens, outputTokens, costMicroUSD)
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

// IsCancelRequested reads just the cancel flag. The loop's cancel watcher
// polls it once a second per running run, so it is deliberately one indexed
// single-row read rather than a full GetRun (ADR-25).
func (s *Store) IsCancelRequested(ctx context.Context, runID uuid.UUID) (bool, error) {
	var requested bool
	err := s.pool.QueryRow(ctx, `SELECT cancel_requested FROM runs WHERE id = $1`, runID).Scan(&requested)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read cancel flag: %w", err)
	}
	return requested, nil
}

// StatusCount is one row of RunCounts.
type StatusCount struct {
	Status string
	Count  int64
}

// RunCounts groups every run by status. It backs the API's Postgres-backed
// Prometheus gauge: process counters reset on deploy, this does not (ADR-26).
//
// Statuses with no runs are absent rather than zero — the collector knows the
// enum and fills them in, and this query only reports what exists.
func (s *Store) RunCounts(ctx context.Context) ([]StatusCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT status::text, count(*) FROM runs GROUP BY status ORDER BY status`)
	if err != nil {
		return nil, fmt.Errorf("count runs by status: %w", err)
	}
	defer rows.Close()
	var out []StatusCount
	for rows.Next() {
		var sc StatusCount
		if err := rows.Scan(&sc.Status, &sc.Count); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// OldestQueuedAge is how long the oldest queued run has been waiting, and
// false when the queue is empty. It is the queue-depth signal worth paging on.
//
// The age is computed in Postgres so it does not depend on the scraping
// process's clock agreeing with the database's, which is the clock every
// created_at was stamped by.
func (s *Store) OldestQueuedAge(ctx context.Context) (time.Duration, bool, error) {
	// min() over no rows is NULL, so the empty queue arrives as a nil pointer
	// rather than as an age of zero — which is what a run queued this instant
	// would legitimately report.
	var seconds *float64
	err := s.pool.QueryRow(ctx, `
		SELECT EXTRACT(EPOCH FROM now() - min(created_at))::float8
		FROM runs WHERE status = 'queued'`).Scan(&seconds)
	if err != nil {
		return 0, false, fmt.Errorf("oldest queued age: %w", err)
	}
	if seconds == nil {
		return 0, false, nil
	}
	return time.Duration(*seconds * float64(time.Second)), true, nil
}
