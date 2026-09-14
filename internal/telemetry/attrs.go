package telemetry

import "go.opentelemetry.io/otel/attribute"

// Span names. They live here rather than at each emitter so the names in a
// waterfall, the names a test asserts on, and the names the README documents
// cannot drift apart. The dotted hierarchy is cosmetic — a span's real parent
// is its context — but it makes an unfamiliar trace readable top to bottom.
const (
	// SpanCreateRun is the API's submission span, the only span that process
	// emits for a run.
	SpanCreateRun = "api.create_run"
	// SpanRun is the run's root, emitted retroactively by whichever worker
	// writes the terminal event (ADR-24). A run that never finishes has none.
	SpanRun = "agent.run"
	// SpanAttempt is one worker's claim of a run. A crashed attempt has no
	// span, because the process died before it could be ended — that gap in
	// the waterfall is the crash, drawn accurately.
	SpanAttempt = "agent.run.attempt"
	// SpanStep is one iteration of the loop.
	SpanStep = "agent.step"
	// SpanModel is one completion including its retries, so a call that
	// succeeded on the third attempt shows as one bar with attempt=3 rather
	// than as three bars the reader has to relate.
	SpanModel = "model.complete"
	// SpanTool is one tool invocation, including the ones served from the
	// idempotency ledger after a resume.
	SpanTool = "tool.invoke"
	// SpanSandbox is one container's lifetime, under the tool that asked for
	// it.
	SpanSandbox = "sandbox.exec"
	// SpanSearch is one corpus search, under the tool that asked for it.
	SpanSearch = "retrieval.search"
	// The four search stages. Spec §11 only asks for retrieval.search, but
	// the children are what make the reranker's dominance of search latency
	// visible in the waterfall — which is the number M3's README had to be
	// honest about in prose instead.
	SpanEmbed   = "retrieval.embed"
	SpanVector  = "retrieval.vector"
	SpanLexical = "retrieval.lexical"
	SpanRerank  = "retrieval.rerank"
)

// GenAI attribute keys, from the OTel semantic conventions. They are spelled
// out rather than taken from the semconv package because that package moves
// these keys between versions while the wire names are what a Jaeger query
// actually matches on.
const (
	AttrGenAISystem       = attribute.Key("gen_ai.system")
	AttrGenAIRequestModel = attribute.Key("gen_ai.request.model")
	AttrGenAIInputTokens  = attribute.Key("gen_ai.usage.input_tokens")
	AttrGenAIOutputTokens = attribute.Key("gen_ai.usage.output_tokens")
)

// agentd's own attribute keys. Everything the conventions do not name lives
// under agentd.* so a trace makes plain which attributes are standard and
// which are this system's.
const (
	AttrRunID       = attribute.Key("agentd.run.id")
	AttrRunStatus   = attribute.Key("agentd.run.status")
	AttrRunSteps    = attribute.Key("agentd.run.steps")
	AttrRunResumed  = attribute.Key("agentd.run.resumed")
	AttrEventsSeen  = attribute.Key("agentd.events.replayed")
	AttrWorkerOwner = attribute.Key("agentd.worker.owner")
	AttrStep        = attribute.Key("agentd.step")
	AttrSeq         = attribute.Key("agentd.seq")
	AttrAttempt     = attribute.Key("agentd.attempt")
	// AttrAttemptOutcome is why one worker's attempt ended: it finished the
	// run, it stopped for a shutdown, or it lost the lease. Without it an
	// attempt that handed the run back looks exactly like one that completed
	// it, since neither is an error and both simply end.
	AttrAttemptOutcome = attribute.Key("agentd.attempt.outcome")

	AttrCostMicroUSD = attribute.Key("agentd.cost_micro_usd")
	AttrBudgetUSD    = attribute.Key("agentd.budget_usd")
	AttrMaxSteps     = attribute.Key("agentd.max_steps")
	AttrToolCount    = attribute.Key("agentd.tool.count")
	AttrTools        = attribute.Key("agentd.tools")

	AttrCacheReadTokens  = attribute.Key("agentd.usage.cache_read_tokens")
	AttrCacheWriteTokens = attribute.Key("agentd.usage.cache_write_tokens")
	AttrStopReason       = attribute.Key("agentd.stop_reason")

	AttrToolName      = attribute.Key("agentd.tool.name")
	AttrToolTrustTier = attribute.Key("agentd.tool.trust_tier")
	AttrToolExitCode  = attribute.Key("agentd.tool.exit_code")
	AttrToolReplayed  = attribute.Key("agentd.tool.replayed")
	AttrToolOutcome   = attribute.Key("agentd.tool.outcome")

	AttrSandboxImage    = attribute.Key("agentd.sandbox.image")
	AttrSandboxExitCode = attribute.Key("agentd.sandbox.exit_code")
	AttrSandboxTimedOut = attribute.Key("agentd.sandbox.timed_out")
	AttrSandboxOOM      = attribute.Key("agentd.sandbox.oom_killed")

	AttrRetrievalMode  = attribute.Key("agentd.retrieval.mode")
	AttrRetrievalK     = attribute.Key("agentd.retrieval.k")
	AttrCandidates     = attribute.Key("agentd.retrieval.candidates")
	AttrVectorHits     = attribute.Key("agentd.retrieval.vector_hits")
	AttrLexicalHits    = attribute.Key("agentd.retrieval.lexical_hits")
	AttrRerankDegraded = attribute.Key("agentd.rerank.degraded")
)

// Tool outcomes, matching the values the M4 metric uses as a label so a span
// and a counter describe the same call the same way.
const (
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeRejected  = "rejected"
	OutcomeReplayed  = "replayed"
)

// Retrieval stage labels for agentd_retrieval_duration_seconds. They are the
// span names above with the "retrieval." prefix dropped, because the prefix
// is already the metric's name: a histogram of agentd_retrieval_duration by
// stage="retrieval.rerank" says retrieval twice.
const (
	StageSearch  = "search"
	StageEmbed   = "embed"
	StageVector  = "vector"
	StageLexical = "lexical"
	StageRerank  = "rerank"
)
