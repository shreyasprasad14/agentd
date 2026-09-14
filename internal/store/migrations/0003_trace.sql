-- One trace per run has to survive the process that created it, so the ids
-- live on the row rather than in a context (ADR-24). Null for runs created
-- before M4 and for runs submitted with tracing off.
ALTER TABLE runs
  ADD COLUMN trace_id     TEXT,
  ADD COLUMN root_span_id TEXT;
