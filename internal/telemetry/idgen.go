package telemetry

import (
	"context"
	"sync/atomic"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// PlantSpanID returns a context that makes the NEXT span started from it take
// the given span id instead of a random one, so a run's root span can carry
// the id every worker already stored a parent link to (ADR-24). An invalid id
// plants nothing.
//
// The hazard, stated here because it is not obvious: the IDGenerator is a
// provider-wide hook consulted for every span creation. A context carrying a
// planted id that leaks past the one tracer.Start it was made for gives
// several spans the same span id and corrupts the trace. Plant it on a
// context used for exactly one Start, and never pass that context downward.
//
// The generator therefore consumes the id, so a second Start cannot reuse it
// even if the context does escape. That makes the failure mode of a leak a
// span with a random id — a bar in the wrong place — rather than a duplicate
// id, which no trace viewer can untangle. It is a backstop for the rule
// above, not a licence to ignore it.
func PlantSpanID(ctx context.Context, id trace.SpanID) context.Context {
	if !id.IsValid() {
		return ctx
	}
	return context.WithValue(ctx, plantedKey{}, &plantedSpanID{id: id})
}

// RootContext returns a context from which exactly one span can be started to
// become the root of a run's stored trace: it carries the run's trace id but
// no parent span id, which is how the SDK is told "continue this trace, and
// this span has no parent", and it plants the stored root span id, so that
// span takes the id every attempt from every worker has already named as its
// parent (ADR-24).
//
// It reports false when either id is unusable — a run created before M4, or
// one submitted with tracing off. Starting the span anyway would draw a root
// in a trace nobody stored, which is worse than the missing summary bar.
//
// Like PlantSpanID, whose hazard it inherits, the returned context is for one
// tracer.Start and must not be passed on to anything else.
func RootContext(ctx context.Context, traceID, spanID string) (context.Context, bool) {
	tid, err := trace.TraceIDFromHex(traceID)
	if err != nil {
		return ctx, false
	}
	sid, err := trace.SpanIDFromHex(spanID)
	if err != nil {
		return ctx, false
	}
	// Sampled for the same reason RemoteParent's parent is: the process that
	// minted these ids sampled everything, and an unsampled parent would drop
	// the one span that summarises the whole run.
	ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		TraceFlags: trace.FlagsSampled,
	}))
	return PlantSpanID(ctx, sid), true
}

// NewIDGenerator returns the generator Init installs, which honours a planted
// span id once and is otherwise indistinguishable from the SDK's own.
//
// It is exported for the other kind of caller that builds a TracerProvider —
// a test recording spans in memory — because a provider without it silently
// ignores a planted id, and the run's root span then appears under an id no
// attempt points at. That is a difference worth failing a test over, so the
// tests have to be able to install it.
func NewIDGenerator() sdktrace.IDGenerator { return idGenerator{} }

// plantedKey is the context key the planted span id hangs off. A private type
// with no exported field cannot collide with another package's key.
type plantedKey struct{}

// plantedSpanID is one span id offered to the generator, and a record of
// whether it has already been taken. It is addressed by pointer so that
// consuming it is visible through every context derived from the one it was
// planted on — which is the whole point: a derived context must not be able
// to spend the id a second time.
type plantedSpanID struct {
	id   trace.SpanID
	used atomic.Bool
}

// idGenerator mints span ids, preferring one planted on the context. It is
// stateless: everything it remembers lives on the context value, so two
// providers in one process (two tests, say) cannot interfere with each other.
type idGenerator struct{}

var _ sdktrace.IDGenerator = idGenerator{}

// NewIDs mints ids for a span with no parent at all. The trace id is always
// fresh, because a planted id names one span and never a trace; the span id
// follows the same rule as NewSpanID so that a root span can be forged the
// same way a child is.
func (g idGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	tid, _ := NewIDs()
	return tid, g.NewSpanID(ctx, tid)
}

// NewSpanID mints the id for a span whose trace is already decided.
func (idGenerator) NewSpanID(ctx context.Context, _ trace.TraceID) trace.SpanID {
	if id, ok := takePlantedSpanID(ctx); ok {
		return id
	}
	// The trace id NewIDs also mints is discarded: this span already has one.
	// Minting both costs a few nanoseconds of crypto/rand and keeps a single
	// function in the package touching the entropy source, which is where the
	// argument for crypto/rand over the SDK's math/rand/v2 is written down.
	_, sid := NewIDs()
	return sid
}

// takePlantedSpanID returns the planted id and marks it spent, so only the
// first span started from a planted context gets it. Every other span in the
// process reaches this function too — the generator is provider-wide — and
// pays one context lookup for the privilege.
func takePlantedSpanID(ctx context.Context) (trace.SpanID, bool) {
	planted, ok := ctx.Value(plantedKey{}).(*plantedSpanID)
	if !ok {
		return trace.SpanID{}, false
	}
	if !planted.used.CompareAndSwap(false, true) {
		return trace.SpanID{}, false
	}
	return planted.id, true
}
