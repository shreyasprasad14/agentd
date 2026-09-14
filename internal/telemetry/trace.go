// Package telemetry wires agentd to OpenTelemetry. Today that is tracing:
// one trace per run, stitched together across the API process that submitted
// it, every worker that claimed it, and every attempt a crash interrupted
// (ADR-24). The ids that do the stitching live on the run row, not in a
// context, because a context cannot cross a queue or a kill -9.
//
// Everything here degrades to nothing when no collector is configured, which
// is the common case: `make test`, `agentd ingest`, `agentd eval`, and anyone
// running the binary outside compose must not need a Jaeger to work.
package telemetry

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// scopeName is the instrumentation scope every agentd span is created under.
// One scope for the whole binary is right here: what distinguishes spans in
// the waterfall is the span name, not which of our packages emitted it.
const scopeName = "github.com/shreyasprasad/agentd"

// defaultServiceName is what an unnamed process calls itself. The flags set
// this per binary (agentd-api, agentd-worker); the fallback exists so a
// caller that forgot still shows up under a name rather than OTel's
// "unknown_service:agentd".
const defaultServiceName = "agentd"

// Config configures tracing. An empty Endpoint disables it, which is what
// make test, agentd ingest, agentd eval, and anyone running the binary
// outside compose get.
type Config struct {
	// Endpoint is the OTLP/HTTP collector: a URL ("http://jaeger:4318",
	// which is what deploy/docker-compose.yml passes) or a bare host:port.
	// Empty turns tracing off.
	Endpoint string
	// ServiceName is how this process appears in the waterfall, and is the
	// only attribute Jaeger's UI groups by.
	ServiceName string
	// Version is the build version, recorded on the resource so a trace can
	// be tied to the code that produced it. Optional.
	Version string
}

// Telemetry owns the tracer provider and its shutdown.
//
// Its zero value is a disabled Telemetry, and every method is safe on that
// and on a nil pointer. Callers therefore hold one unconditionally and start
// spans without guarding them; a disabled Telemetry hands out spans that do
// nothing rather than making every call site branch.
type Telemetry struct {
	// provider is nil exactly when tracing is disabled, and is the single
	// field the other methods branch on.
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
}

// Init builds a tracer provider. With no endpoint it returns a no-op
// Telemetry that records nothing and costs nothing; an unreachable
// collector is a warning rather than a fatal, because the OTLP exporter
// retries in the background on its own.
func Init(ctx context.Context, cfg Config) (*Telemetry, error) {
	if cfg.Endpoint == "" {
		return &Telemetry{}, nil
	}

	opts, err := exporterOptions(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}
	// Deliberately, this does not contact the collector: otlptracehttp
	// builds its HTTP client here and makes its first request when the
	// first batch is ready. A Jaeger that is down, slow, or simply started
	// after the worker must never stop the worker from working, and the
	// exporter's own retry handles the gap. So Init fails only on an
	// endpoint we could not parse, which is a misconfiguration the operator
	// wants to hear about at startup.
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: otlp exporter: %w", err)
	}

	res, err := newResource(cfg)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		// Without this the run's root span could not exist: it is emitted
		// after the fact under an id the API minted and every attempt has
		// already stored a parent link to, and the generator is the only hook
		// the SDK offers for choosing a span id (ADR-24).
		sdktrace.WithIDGenerator(NewIDGenerator()),
		// Runs are minutes long and arrive a few per minute, so sampling
		// would buy nothing and would guarantee that the one run someone
		// asks about is the one that was dropped. Tail sampling is
		// production work the README describes rather than ships.
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	// Nothing in agentd reads the global provider — every package that
	// emits a span is handed a tracer explicitly — but library code the
	// loop calls (the Docker client, pgx instrumentation) creates spans off
	// the global, and those belong in the run's trace rather than nowhere.
	// A single-purpose binary that owns its process can own the global; the
	// explicit tracer means no test and no library depends on it.
	otel.SetTracerProvider(tp)

	return newTelemetry(tp), nil
}

// Tracer returns the named tracer. Safe on a nil or disabled Telemetry,
// where it returns a tracer whose spans never record, so a call site can
// start and end spans without asking whether tracing is on.
func (t *Telemetry) Tracer() trace.Tracer {
	if t == nil || t.tracer == nil {
		return noop.Tracer{}
	}
	return t.tracer
}

// Enabled reports whether spans are exported anywhere. It is for deciding
// whether to do work *for* the trace — minting ids for a new run, rendering
// a deep link — never for guarding a span, which already costs nothing when
// tracing is off.
func (t *Telemetry) Enabled() bool { return t != nil && t.provider != nil }

// Shutdown flushes pending spans. It is a no-op on a nil or disabled
// Telemetry.
//
// It leaves the global tracer provider pointing at the provider it just shut
// down. That provider hands out non-recording spans from here on, which is
// exactly what a process on its way out wants, and restoring the previous
// global would only open a window in which a late span went somewhere
// surprising.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil || t.provider == nil {
		return nil
	}
	if err := t.provider.Shutdown(ctx); err != nil {
		return fmt.Errorf("telemetry: shut down tracer provider: %w", err)
	}
	return nil
}

// newTelemetry wraps a provider and resolves its tracer once: the provider
// returns the same tracer for the same scope on every call anyway, and doing
// it here keeps Tracer's nil check down to one comparison.
func newTelemetry(tp *sdktrace.TracerProvider) *Telemetry {
	return &Telemetry{provider: tp, tracer: tp.Tracer(scopeName)}
}

// newRecording builds a Telemetry that feeds sp instead of a collector. It is
// the seam this package's tests use to assert on real spans through
// tracetest.SpanRecorder, so `make test` needs neither Jaeger nor a network.
// Unlike Init it never touches the global provider, because two tests in one
// process must not fight over it.
//
// It installs the id generator for the same reason Init does: a provider
// without it ignores a planted span id, and a test against such a provider
// would pass while the trace it is standing in for came out wrong.
func newRecording(sp sdktrace.SpanProcessor) *Telemetry {
	return newTelemetry(sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sp),
		sdktrace.WithIDGenerator(NewIDGenerator()),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	))
}

// exporterOptions turns an endpoint into OTLP/HTTP options.
//
// The scheme decides the transport: http:// is plaintext, https:// is TLS.
// A bare "jaeger:4318" is the other spelling people reach for, and it means
// the plaintext collector next door, so it is parsed as if it had said
// http:// — one parse path rather than two. Any path is treated as a base
// and the signal path appended, matching what OTEL_EXPORTER_OTLP_ENDPOINT
// means to every other OTLP client.
func exporterOptions(endpoint string) ([]otlptracehttp.Option, error) {
	raw := endpoint
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse otlp endpoint %q: %w", endpoint, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("otlp endpoint %q: scheme %q is not http or https", endpoint, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("otlp endpoint %q: no host", endpoint)
	}

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(u.Host),
		otlptracehttp.WithURLPath(strings.TrimSuffix(u.Path, "/") + "/v1/traces"),
	}
	if u.Scheme == "http" {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	return opts, nil
}

// newResource describes this process to the collector. service.name is the
// attribute the UI groups by, so it is the one that has to be right; the
// rest (host, process, SDK version) comes from resource.Default.
func newResource(cfg Config) (*resource.Resource, error) {
	name := cfg.ServiceName
	if name == "" {
		name = defaultServiceName
	}
	attrs := []attribute.KeyValue{semconv.ServiceName(name)}
	if cfg.Version != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.Version))
	}
	// Ours is the second argument so it wins over the OTEL_SERVICE_NAME the
	// default resource may have picked up: the flag is the more specific
	// statement of intent.
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL, attrs...))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}
	return res, nil
}
