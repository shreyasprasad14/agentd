package telemetry

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

func TestPlantSpanID_FirstSpanTakesThePlantedID(t *testing.T) {
	tel, sr := recorder(t)
	_, want := NewIDs()

	ctx := PlantSpanID(context.Background(), want)
	_, span := tel.Tracer().Start(ctx, SpanRun)
	End(span, nil)

	ended := sr.Ended()
	require.Len(t, ended, 1)
	require.Equal(t, want, ended[0].SpanContext().SpanID(),
		"the root span has to carry the id every attempt stored a parent link to")
}

// TestPlantSpanID_IsSpentByTheFirstSpan is the corruption risk this whole
// mechanism runs: the generator is a provider-wide hook, so a planted context
// that escapes the one Start it was made for would stamp every later span
// with the same id and leave a trace no viewer can untangle.
func TestPlantSpanID_IsSpentByTheFirstSpan(t *testing.T) {
	tel, sr := recorder(t)
	_, want := NewIDs()

	planted := PlantSpanID(context.Background(), want)
	first, span := tel.Tracer().Start(planted, SpanRun)
	End(span, nil)

	// Both of the ways the id could leak: the context handed back by Start,
	// which carries everything the planted one did, and the planted context
	// itself, reused.
	_, child := tel.Tracer().Start(first, SpanAttempt)
	End(child, nil)
	_, sibling := tel.Tracer().Start(planted, SpanStep)
	End(sibling, nil)

	ended := sr.Ended()
	require.Len(t, ended, 3)
	seen := map[trace.SpanID]string{}
	for _, s := range ended {
		id := s.SpanContext().SpanID()
		require.True(t, id.IsValid())
		require.Empty(t, seen[id], "%s reused the span id of %s", s.Name(), seen[id])
		seen[id] = s.Name()
	}
	require.Equal(t, SpanRun, seen[want], "the planted id belongs to the first span and to nothing else")
}

func TestPlantSpanID_ConcurrentStartsGetOneWinner(t *testing.T) {
	// The rule is one Start per planted context; this asserts what happens
	// when it is broken from several goroutines at once, because "consumed
	// once" is a claim about a race, not about an ordering.
	tel, sr := recorder(t)
	_, want := NewIDs()
	planted := PlantSpanID(context.Background(), want)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, span := tel.Tracer().Start(planted, SpanStep)
			End(span, nil)
		}()
	}
	wg.Wait()

	hits := 0
	for _, s := range sr.Ended() {
		if s.SpanContext().SpanID() == want {
			hits++
		}
	}
	require.Equal(t, 1, hits, "exactly one span may take a planted id")
}

func TestPlantSpanID_InvalidIDPlantsNothing(t *testing.T) {
	base := context.Background()
	require.Equal(t, base, PlantSpanID(base, trace.SpanID{}),
		"an id that cannot be honoured must not make the context look planted")
}

func TestIDGenerator_RandomWithoutAPlantedID(t *testing.T) {
	tel, sr := recorder(t)
	for range 8 {
		_, span := tel.Tracer().Start(context.Background(), SpanStep)
		End(span, nil)
	}

	ended := sr.Ended()
	require.Len(t, ended, 8)
	traces := map[trace.TraceID]bool{}
	spans := map[trace.SpanID]bool{}
	for _, s := range ended {
		require.False(t, traces[s.SpanContext().TraceID()], "roots must not share a trace")
		require.False(t, spans[s.SpanContext().SpanID()], "spans must not share an id")
		traces[s.SpanContext().TraceID()] = true
		spans[s.SpanContext().SpanID()] = true
	}
}

// TestRootContext_ForgesTheStoredRoot is the shape the run's summary bar has
// to have: in the stored trace, under the stored span id, and with no parent
// of its own, so the attempts that name it as their parent hang beneath it.
func TestRootContext_ForgesTheStoredRoot(t *testing.T) {
	tel, sr := recorder(t)
	tid, sid := NewIDs()

	ctx, ok := RootContext(context.Background(), tid.String(), sid.String())
	require.True(t, ok)
	_, span := tel.Tracer().Start(ctx, SpanRun)
	End(span, nil)

	ended := sr.Ended()
	require.Len(t, ended, 1)
	require.Equal(t, tid, ended[0].SpanContext().TraceID())
	require.Equal(t, sid, ended[0].SpanContext().SpanID())
	require.False(t, ended[0].Parent().IsValid(), "the run's root span is a root")
	require.True(t, ended[0].SpanContext().IsSampled())
}

// TestRootContext_AttemptsHangOffTheForgedRoot puts the two halves together:
// a worker's attempt built from RemoteParent, and the root emitted afterwards
// by whichever worker wrote the terminal event.
func TestRootContext_AttemptsHangOffTheForgedRoot(t *testing.T) {
	tel, sr := recorder(t)
	tid, sid := NewIDs()

	_, attempt := tel.Tracer().Start(RemoteParent(context.Background(), tid.String(), sid.String()), SpanAttempt)
	End(attempt, nil)

	rootCtx, ok := RootContext(context.Background(), tid.String(), sid.String())
	require.True(t, ok)
	_, root := tel.Tracer().Start(rootCtx, SpanRun)
	End(root, nil)

	ended := sr.Ended()
	require.Len(t, ended, 2)
	require.Equal(t, SpanAttempt, ended[0].Name())
	require.Equal(t, SpanRun, ended[1].Name())
	require.Equal(t, ended[1].SpanContext().SpanID(), ended[0].Parent().SpanID(),
		"the attempt was drawn before the root existed and still ends up under it")
}

func TestRootContext_RefusesIDsItCannotUse(t *testing.T) {
	tid, sid := NewIDs()
	tests := []struct {
		name    string
		traceID string
		spanID  string
	}{
		{name: "pre-M4 run", traceID: "", spanID: ""},
		{name: "no span id", traceID: tid.String(), spanID: ""},
		{name: "not hex", traceID: "not-a-trace-id-at-all-nope-nope", spanID: sid.String()},
		{name: "too short", traceID: tid.String()[:16], spanID: sid.String()},
		{name: "all zero", traceID: "00000000000000000000000000000000", spanID: sid.String()},
		{name: "zero span", traceID: tid.String(), spanID: "0000000000000000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := context.Background()
			ctx, ok := RootContext(base, tc.traceID, tc.spanID)
			require.False(t, ok, "a root span in an invented trace is worse than none")
			require.Equal(t, base, ctx)
		})
	}
}
