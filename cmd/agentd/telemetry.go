package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/shreyasprasad/agentd/internal/telemetry"
)

// shutdownGrace bounds the flush of pending spans on the way out. Shutdown
// pushes the last batch to the collector over HTTP, and a Jaeger that has gone
// away — or was never there, because the endpoint was a typo — would otherwise
// hold a clean Ctrl-C open for as long as the exporter keeps retrying. A few
// spans lost from a process that is already leaving is the cheaper end of that
// trade.
const shutdownGrace = 5 * time.Second

// traceFlags are the tracing flags serve and work share. Both processes export
// spans of the same run to the same collector (ADR-24); they differ only in the
// name they report themselves under, which is what the Jaeger UI groups by.
type traceFlags struct {
	endpoint *string
	service  *string
}

// addTraceFlags registers -otlp-endpoint and -otel-service. defaultService is
// the per-binary service.name: agentd-api for serve, agentd-worker for work.
//
// The env names are OTel's own rather than AGENTD_-prefixed, because every
// other OTLP client in a deployment already reads them and an operator setting
// one expects it to reach all of them.
func addTraceFlags(fs *flag.FlagSet, defaultService string) traceFlags {
	return traceFlags{
		endpoint: fs.String("otlp-endpoint", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
			"OTLP/HTTP collector for traces, e.g. http://localhost:4318 (empty: tracing off)"),
		service: fs.String("otel-service", envOr("OTEL_SERVICE_NAME", defaultService),
			"service.name reported on every span; the Jaeger UI groups by it"),
	}
}

// start builds the tracer provider and reports what it did in the first lines
// of the log.
//
// The log line is the point of this function existing rather than a bare Init
// call. Init deliberately does not contact the collector — the exporter makes
// its first request when the first batch is ready and retries in the
// background — so a mistyped host is otherwise silent until someone opens
// Jaeger an hour later and finds it empty. One line at startup turns that into
// something an operator can check immediately.
func (f traceFlags) start(ctx context.Context, log *slog.Logger) (*telemetry.Telemetry, error) {
	tel, err := telemetry.Init(ctx, telemetry.Config{Endpoint: *f.endpoint, ServiceName: *f.service})
	if err != nil {
		return nil, err
	}
	if tel.Enabled() {
		log.Info("tracing on", "otlp_endpoint", *f.endpoint, "service", *f.service)
	} else {
		log.Info("tracing off", "hint", "set -otlp-endpoint or OTEL_EXPORTER_OTLP_ENDPOINT to export spans")
	}
	return tel, nil
}

// defaultMetricsAddr is where the worker serves /metrics and /healthz. The
// API has a listener already and serves both on it; the worker had none,
// which is also why compose could never health-check it.
const defaultMetricsAddr = ":9091"

// healthTimeout bounds the `agentd healthz` probe. It is the whole budget for
// connect, request, and response against a listener on the same host, so a
// process that has not answered in a second is not answering.
const healthTimeout = time.Second

// addMetricsAddrFlag registers -metrics-addr for the worker. An empty value
// turns the listener off, which is what the crash demo passes for its second
// worker: two workers on one host cannot both hold :9091.
func addMetricsAddrFlag(fs *flag.FlagSet) *string {
	return fs.String("metrics-addr", envOr("AGENTD_METRICS_ADDR", defaultMetricsAddr),
		"address for the worker's /metrics and /healthz listener (empty: off)")
}

// serveMetrics starts the worker's metrics listener and returns its shutdown.
//
// A port that is already taken is a warning, not a fatal error. The worker's
// job is executing runs, and refusing to start one because a second worker on
// the same host got to :9091 first would trade the thing that matters for the
// thing that observes it — and two workers on one host is not a corner case
// here, it is `make crash-demo` and the two-worker lease demo in spec §2.
//
// Inside compose the port is deliberately not published to the host, because
// `docker compose up --scale worker=2` collides on the second replica the
// moment a fixed host port is mapped. Prometheus scrapes it on the compose
// network instead, where the service name resolves to every replica.
func serveMetrics(addr string, m *telemetry.Metrics, log *slog.Logger) func() {
	if addr == "" {
		log.Info("metrics off", "hint", "set -metrics-addr to serve /metrics and /healthz")
		return func() {}
	}
	// Listening here rather than inside the goroutine so a bad address is
	// reported at startup, next to the tracing line, instead of appearing in
	// the log some time later with no request to explain it.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Warn("metrics listener unavailable; continuing without it", "addr", addr, "error", err)
		return func() {}
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		// Liveness, not readiness: it says this process is up and answering,
		// which is what a compose healthcheck and a restart policy act on. A
		// worker that cannot reach Postgres is not unhealthy — it retries,
		// and restarting it would only make the retry slower.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("metrics listener stopped", "error", err)
		}
	}()
	log.Info("metrics listening", "addr", ln.Addr().String())

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// healthCmd is `agentd healthz`, the container healthcheck. It exists because
// the runtime image is distroless: there is no curl, no wget, and no shell to
// run them from, so the only executable available to a HEALTHCHECK is this
// binary.
func healthCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("healthz", flag.ExitOnError)
	addr := fs.String("addr", envOr("AGENTD_HEALTH_ADDR", "localhost"+defaultMetricsAddr),
		"host:port or URL to probe; the worker's -metrics-addr or the API's -addr")
	if err := fs.Parse(args); err != nil {
		return err
	}

	url := *addr
	if !strings.Contains(url, "://") {
		url = "http://" + url
	}
	url = strings.TrimSuffix(url, "/") + "/healthz"

	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return nil
}

// stopTracing flushes pending spans. Callers defer it.
//
// It builds its own context instead of taking the command's: by the time this
// runs, the command's context is usually the one a Ctrl-C just cancelled, and
// an already-cancelled context makes Shutdown return without exporting
// anything — which would drop precisely the spans of the run that was
// interrupted, the ones most worth looking at.
func stopTracing(tel *telemetry.Telemetry, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		// Losing the last batch does not make the command's own work wrong,
		// so this is a warning and never the process's exit status.
		log.Warn("telemetry shutdown", "error", err)
	}
}
