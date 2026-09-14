package telemetry

import (
	"context"
	"log/slog"
)

// Log field names for the correlation ids. They are spelled the way every
// other OTel-aware log pipeline spells them, because the value of these two
// fields is entirely in a query someone else's tooling can run.
const (
	logKeyTraceID = "trace_id"
	logKeySpanID  = "span_id"
)

// Handler wraps a slog.Handler and adds trace_id and span_id taken from the
// context a record is logged with, so a log line and a bar of the waterfall
// can be matched up in either direction: find the slow span in Jaeger and
// grep its id in the logs, or find the interesting log line and open its
// trace. Without it the only join is the timestamp, and a worker that runs
// several steps a second does not give that join enough to work with.
//
// It is the other half of §11's "structured logs with run_id and seq on every
// line": run_id says which run a line belongs to, trace_id and span_id say
// which piece of work inside it.
type Handler struct {
	// inner is the handler that actually formats and writes. Every method
	// delegates to it; this type only ever adds two attributes.
	inner slog.Handler
}

// Compile-time proof that the wrapper is substitutable for what it wraps —
// cmd/agentd hands the result straight to slog.New.
var _ slog.Handler = (*Handler)(nil)

// NewHandler wraps inner so that every record logged with a context carrying
// a span gains trace_id and span_id.
//
// A record whose context carries no span is passed through untouched rather
// than given empty fields. `agentd ingest` and `agentd eval` log constantly
// with no trace at all, and "trace_id": "" on all of those lines is noise
// that also defeats a "has a trace id" query.
func NewHandler(inner slog.Handler) *Handler {
	return &Handler{inner: inner}
}

// Enabled reports whether the inner handler wants records at this level. The
// wrapper has no opinion of its own: level filtering belongs to the handler
// that was configured with a level.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle adds the correlation ids and passes the record on.
//
// The ids come from any valid span context, recording or not — see
// TraceIDFromContext. A worker that has rebuilt the run's remote parent but
// not yet started a span of its own is already working on that trace, and the
// lines it writes while claiming the run are exactly the ones worth
// correlating when a claim goes wrong.
//
// One consequence worth knowing rather than rediscovering: under a handler
// that has been through WithGroup, the ids are qualified by that group like
// any other record attribute, so they appear as group.trace_id. That is what
// slog groups mean, and nothing in agentd opens one.
func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	traceID := TraceIDFromContext(ctx)
	spanID := SpanIDFromContext(ctx)
	if traceID == "" && spanID == "" {
		return h.inner.Handle(ctx, rec)
	}
	// Clone before adding: a Record may be handed to several handlers, and
	// AddAttrs on an uncloned copy can write through into the shared backing
	// array, giving the other handler our attributes too.
	rec = rec.Clone()
	if traceID != "" {
		rec.AddAttrs(slog.String(logKeyTraceID, traceID))
	}
	if spanID != "" {
		rec.AddAttrs(slog.String(logKeySpanID, spanID))
	}
	return h.inner.Handle(ctx, rec)
}

// WithAttrs returns a Handler around the inner handler's result, keeping the
// wrapper in place for every line the derived logger writes.
//
// Wrapping the result rather than returning it bare is the whole of this
// method: returning h.inner.WithAttrs(attrs) would silently drop correlation
// from the moment anyone called logger.With(...), which is how nearly every
// logger in this codebase is built.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &Handler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup returns a Handler around the inner handler's result, for the same
// reason WithAttrs does.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		// slog's contract: an empty group name is a no-op, and creating one
		// would nest every later attribute under "".
		return h
	}
	return &Handler{inner: h.inner.WithGroup(name)}
}
