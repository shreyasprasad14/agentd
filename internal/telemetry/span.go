package telemetry

import (
	"context"
	"crypto/rand"
	"errors"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// RemoteParent rebuilds a parent span context from the trace and span ids
// stored on the run row, so every span a worker emits lands in the run's
// trace no matter which process or which attempt produced it (ADR-24).
// In-process propagation cannot reach across the queue between the API and
// the worker, or across the crash between two attempts; ids on the row can.
//
// Empty or malformed ids return ctx unchanged. A run created before M4 has
// none and a run submitted with tracing off has none either, and both must
// degrade to an untraced run rather than a failed one.
func RemoteParent(ctx context.Context, traceID, spanID string) context.Context {
	tid, err := trace.TraceIDFromHex(traceID)
	if err != nil {
		return ctx
	}
	sid, err := trace.SpanIDFromHex(spanID)
	if err != nil {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid,
		SpanID:  sid,
		// The parent is marked sampled because the process that minted
		// these ids sampled everything. A parent-based sampler reading an
		// unsampled flag would drop precisely the children a resumed run
		// contributes, which is the half of the crash demo worth seeing.
		TraceFlags: trace.FlagsSampled,
	}))
}

// End finishes span, recording err first when there is one. Keeping the
// convention in one function is the point: every span in the runtime then
// reports failure the same way, and the waterfall can be read without
// knowing which package drew which bar.
//
// A cancelled context is recorded but deliberately left Unset rather than
// Error. Cancellation is an outcome someone asked for (ADR-25), not a fault,
// and marking it Error would paint a deliberately cancelled run red and
// count it in any error rate computed from span status. A deadline is a
// fault and keeps the Error status.
func End(span trace.Span, err error) {
	if span == nil {
		return
	}
	if err != nil {
		span.RecordError(err)
		if !errors.Is(err, context.Canceled) {
			span.SetStatus(codes.Error, err.Error())
		}
	}
	span.End()
}

// TraceIDFromContext is the trace id on ctx in hex, or "" when ctx carries
// no span. The slog handler puts it on every log line and the API returns it
// from GET /v1/runs/:id/trace, so both want the string rather than the array.
//
// Any valid span context counts, recording or not: a worker that has rebuilt
// the run's remote parent but not yet started a span of its own is already
// working on that trace, and its log lines belong to it.
func TraceIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// SpanIDFromContext is the id of the span on ctx in hex, or "" when there is
// none. Paired with TraceIDFromContext it is what lines up a log line with
// one bar of the waterfall rather than with the whole trace.
func SpanIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasSpanID() {
		return ""
	}
	return sc.SpanID().String()
}

// NewIDs mints a trace id and a root span id for a run the API is about to
// create. They are generated at submission and stored on the row, so the
// run's trace identity exists before any worker does (ADR-24).
//
// crypto/rand rather than the SDK's math/rand/v2 generator: these ids leave
// the process — they are stored, returned by the API, and pasted into a
// Jaeger URL — and predictable ones would let anyone with UI access
// enumerate other people's traces. The cost is one syscall per run.
func NewIDs() (trace.TraceID, trace.SpanID) {
	var tid trace.TraceID
	var sid trace.SpanID
	// An all-zero id is invalid and would silently un-trace the run, so
	// reject it instead of trusting 2^-128. rand.Read cannot fail: it
	// panics if the system entropy source is broken, which is the right
	// answer for a process that can no longer generate ids at all.
	for !tid.IsValid() {
		_, _ = rand.Read(tid[:])
	}
	for !sid.IsValid() {
		_, _ = rand.Read(sid[:])
	}
	return tid, sid
}
