CREATE EXTENSION IF NOT EXISTS vector;

CREATE TYPE run_status AS ENUM (
  'queued','running','succeeded','failed','cancelled','budget_exceeded'
);

CREATE TABLE runs (
  id              UUID PRIMARY KEY,
  status          run_status NOT NULL DEFAULT 'queued',
  goal            TEXT NOT NULL,
  agent_config    JSONB NOT NULL,        -- model, system prompt ref, tool allowlist
  max_steps       INT NOT NULL DEFAULT 30,
  budget_usd      NUMERIC(10,4) NOT NULL DEFAULT 1.00,
  spent_usd       NUMERIC(10,4) NOT NULL DEFAULT 0,
  input_tokens    BIGINT NOT NULL DEFAULT 0,
  output_tokens   BIGINT NOT NULL DEFAULT 0,
  cancel_requested BOOLEAN NOT NULL DEFAULT false,
  lease_owner     TEXT,
  lease_expires_at TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at     TIMESTAMPTZ
);

-- Claim path for the Postgres-backed queue: workers scan queued runs and runs
-- whose lease has expired, ordered by created_at, with SKIP LOCKED.
CREATE INDEX runs_claimable_idx ON runs (created_at)
  WHERE status IN ('queued','running');

-- Append-only. This table is the source of truth for run state.
CREATE TABLE run_events (
  run_id     UUID NOT NULL REFERENCES runs(id),
  seq        INT  NOT NULL,
  type       TEXT NOT NULL,   -- see event types in the spec
  payload    JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, seq)
);

-- Idempotency ledger. A tool call is uniquely identified by its position
-- in the run, so replay after a crash never double-executes a side effect.
CREATE TABLE tool_calls (
  run_id       UUID NOT NULL REFERENCES runs(id),
  seq          INT  NOT NULL,        -- seq of the tool_requested event
  tool_name    TEXT NOT NULL,
  args         JSONB NOT NULL,
  status       TEXT NOT NULL,        -- started | succeeded | failed
  result       JSONB,
  started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at  TIMESTAMPTZ,
  PRIMARY KEY (run_id, seq)
);

CREATE TABLE documents (
  id          UUID PRIMARY KEY,
  source_id   TEXT UNIQUE NOT NULL,   -- e.g. CourtListener opinion id
  title       TEXT,
  metadata    JSONB NOT NULL DEFAULT '{}',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE chunks (
  id          UUID PRIMARY KEY,
  document_id UUID NOT NULL REFERENCES documents(id),
  ordinal     INT NOT NULL,
  content     TEXT NOT NULL,
  tsv         TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
  embedding   VECTOR(1024)
);

CREATE INDEX chunks_tsv_idx ON chunks USING GIN (tsv);
CREATE INDEX chunks_embedding_idx ON chunks USING hnsw (embedding vector_cosine_ops);
