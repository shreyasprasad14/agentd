package telemetry

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recorder returns a Telemetry whose spans are kept in memory. No collector,
// no network, no Jaeger: everything this package promises is observable from
// the spans it hands the processor.
func recorder(t *testing.T) (*Telemetry, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tel := newRecording(sr)
	t.Cleanup(func() {
		require.NoError(t, tel.Shutdown(context.Background()))
	})
	return tel, sr
}

func TestInit_EmptyEndpointDisablesTracing(t *testing.T) {
	tel, err := Init(context.Background(), Config{ServiceName: "agentd-worker"})
	require.NoError(t, err)
	require.NotNil(t, tel, "a disabled Telemetry is still a usable one")
	require.False(t, tel.Enabled())

	_, span := tel.Tracer().Start(context.Background(), "agent.run.attempt")
	require.False(t, span.IsRecording(), "no endpoint means no spans anywhere")
	End(span, errors.New("boom"))

	require.NoError(t, tel.Shutdown(context.Background()))
}

func TestInit_NilTelemetryIsUsable(t *testing.T) {
	// The wiring hands a *Telemetry to the loop, the sandbox, and the
	// searcher. One of them holding nil must not be a crash.
	var tel *Telemetry
	require.False(t, tel.Enabled())
	require.NoError(t, tel.Shutdown(context.Background()))

	_, span := tel.Tracer().Start(context.Background(), "tool.invoke")
	require.False(t, span.IsRecording())
	End(span, nil)
}

func TestInit_RejectsUnusableEndpoint(t *testing.T) {
	_, err := Init(context.Background(), Config{Endpoint: "grpc://jaeger:4317"})
	require.ErrorContains(t, err, "not http or https")
}

func TestExporterOptions_Endpoints(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  string
	}{
		{name: "compose spelling", endpoint: "http://jaeger:4318"},
		{name: "bare host port", endpoint: "jaeger:4318"},
		{name: "tls", endpoint: "https://collector.example.com:4318"},
		{name: "path prefix", endpoint: "https://collector.example.com/otlp"},
		{name: "wrong scheme", endpoint: "grpc://jaeger:4317", wantErr: "not http or https"},
		{name: "no host", endpoint: "http://", wantErr: "no host"},
		{name: "unparseable", endpoint: "http://jae ger:4318", wantErr: "parse otlp endpoint"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := exporterOptions(tc.endpoint)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, opts)
		})
	}
}

func TestNewResource_CarriesServiceIdentity(t *testing.T) {
	res, err := newResource(Config{ServiceName: "agentd-api", Version: "m4"})
	require.NoError(t, err)

	got := map[string]string{}
	for _, kv := range res.Attributes() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	require.Equal(t, "agentd-api", got["service.name"], "the attribute Jaeger groups by")
	require.Equal(t, "m4", got["service.version"])
}

func TestRemoteParent_ChildJoinsTheStoredTrace(t *testing.T) {
	tel, sr := recorder(t)
	tid, sid := NewIDs()

	ctx := RemoteParent(context.Background(), tid.String(), sid.String())
	_, span := tel.Tracer().Start(ctx, SpanAttempt)
	End(span, nil)

	ended := sr.Ended()
	require.Len(t, ended, 1)
	require.Equal(t, tid, ended[0].SpanContext().TraceID(), "the worker's span is in the run's trace")
	require.Equal(t, sid, ended[0].Parent().SpanID(), "and hangs off the stored root span")
	require.True(t, ended[0].Parent().IsRemote(), "the parent was produced by another process")
	require.True(t, ended[0].SpanContext().IsSampled())
}

func TestRemoteParent_MissingIDsLeaveTheContextAlone(t *testing.T) {
	tid, sid := NewIDs()
	tests := []struct {
		name    string
		traceID string
		spanID  string
	}{
		{name: "both empty", traceID: "", spanID: ""},
		{name: "pre-M4 run", traceID: "", spanID: sid.String()},
		{name: "no span id", traceID: tid.String(), spanID: ""},
		{name: "not hex", traceID: "not-a-trace-id-at-all-nope-nope", spanID: sid.String()},
		{name: "too short", traceID: tid.String()[:16], spanID: sid.String()},
		{name: "all zero", traceID: "00000000000000000000000000000000", spanID: sid.String()},
		{name: "zero span", traceID: tid.String(), spanID: "0000000000000000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := context.Background()
			require.Equal(t, base, RemoteParent(base, tc.traceID, tc.spanID),
				"an untraceable run must stay a run")
		})
	}
}

func TestEnd_RecordsFailureOnTheSpan(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name       string
		err        error
		wantCode   codes.Code
		wantEvents int
	}{
		{name: "success", err: nil, wantCode: codes.Unset},
		{name: "failure", err: boom, wantCode: codes.Error, wantEvents: 1},
		{name: "wrapped failure", err: fmt.Errorf("model.complete: %w", boom), wantCode: codes.Error, wantEvents: 1},
		{name: "deadline", err: context.DeadlineExceeded, wantCode: codes.Error, wantEvents: 1},
		// A cancel is what the operator asked for, so it is on the span
		// but not an error on it.
		{name: "cancelled", err: context.Canceled, wantCode: codes.Unset, wantEvents: 1},
		{name: "wrapped cancel", err: fmt.Errorf("tool.invoke: %w", context.Canceled), wantCode: codes.Unset, wantEvents: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tel, sr := recorder(t)
			_, span := tel.Tracer().Start(context.Background(), "model.complete")
			End(span, tc.err)

			ended := sr.Ended()
			require.Len(t, ended, 1)
			require.Equal(t, tc.wantCode, ended[0].Status().Code)
			require.Len(t, ended[0].Events(), tc.wantEvents)
			if tc.err != nil {
				require.Equal(t, tc.err.Error(), errorMessage(t, ended[0].Events()[0]))
			}
			if tc.wantCode == codes.Error {
				require.Equal(t, tc.err.Error(), ended[0].Status().Description)
			}
		})
	}
}

func TestEnd_NilSpanIsANoop(t *testing.T) {
	// Call sites hold a trace.Span that a disabled path may never have set.
	End(nil, errors.New("boom"))
}

func TestTraceIDFromContext_RoundTrips(t *testing.T) {
	tel, _ := recorder(t)

	require.Empty(t, TraceIDFromContext(context.Background()), "no span, nothing to correlate")
	require.Empty(t, SpanIDFromContext(context.Background()))

	ctx, span := tel.Tracer().Start(context.Background(), "agent.step")
	defer End(span, nil)
	sc := span.SpanContext()
	require.Equal(t, sc.TraceID().String(), TraceIDFromContext(ctx))
	require.Equal(t, sc.SpanID().String(), SpanIDFromContext(ctx))
}

func TestTraceIDFromContext_ReadsARemoteParent(t *testing.T) {
	// A worker logs under the run's trace from the moment it rebuilds the
	// parent, before it has started a span of its own.
	tid, sid := NewIDs()
	ctx := RemoteParent(context.Background(), tid.String(), sid.String())
	require.Equal(t, tid.String(), TraceIDFromContext(ctx))
	require.Equal(t, sid.String(), SpanIDFromContext(ctx))
}

func TestNewIDs_AreValidAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 128 {
		tid, sid := NewIDs()
		require.True(t, tid.IsValid())
		require.True(t, sid.IsValid())
		require.False(t, seen[tid.String()], "a repeated trace id would merge two runs")
		require.False(t, seen[sid.String()], "a repeated span id would corrupt the tree")
		seen[tid.String()] = true
		seen[sid.String()] = true
	}
}

// errorMessage pulls the message off a recorded exception event, which is
// how RecordError stores it.
func errorMessage(t *testing.T, ev sdktrace.Event) string {
	t.Helper()
	require.Equal(t, "exception", ev.Name)
	for _, kv := range ev.Attributes {
		if kv.Key == "exception.message" {
			return kv.Value.AsString()
		}
	}
	t.Fatalf("event %q carries no exception.message: %v", ev.Name, ev.Attributes)
	return ""
}
