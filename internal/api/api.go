// Package api is the HTTP control plane. It never calls a model: it writes
// intent to Postgres and tails the event log.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/shreyasprasad/agentd/internal/api/ui"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/telemetry"
	"github.com/shreyasprasad/agentd/internal/tools"
)

// DefaultJaegerUI is where Jaeger sits in deploy/docker-compose.yml as seen
// from a browser on the host. It is deliberately not derived from the OTLP
// endpoint: the collector address the process exports to (http://jaeger:4318
// inside compose) is not an address the reader of the deep link can open.
//
// It is exported so cmd/agentd can print it as the -jaeger-ui default instead
// of repeating the literal, which is how the flag and the fallback drift.
const DefaultJaegerUI = "http://localhost:16686"

// Options tunes the server.
type Options struct {
	// PollInterval is how often a live SSE stream checks for new events.
	// M0 polls; LISTEN/NOTIFY is the obvious upgrade once event volume matters.
	PollInterval time.Duration
	// KeepAlive is how often an idle stream emits an SSE comment so proxies
	// do not reap the connection.
	KeepAlive time.Duration
	// Registry lists the tools workers offer. The API uses it to fill a
	// run's default allowlist at submission and to serve GET /v1/tools.
	// It must match what the workers register.
	Registry *tools.Registry
	// Tracer records the API's spans. Nil means tracing is off; withDefaults
	// swaps in a no-op tracer so no handler branches on it.
	Tracer trace.Tracer
	// JaegerUI is the browser-facing base URL that GET /v1/runs/:id/trace
	// builds its deep link against. Empty means DefaultJaegerUI.
	JaegerUI string
	// Metrics backs GET /metrics. Nil turns the endpoint into a 404 and
	// records nothing, which is what a test that does not care gets.
	Metrics *telemetry.Metrics
}

func (o Options) withDefaults() Options {
	if o.PollInterval <= 0 {
		o.PollInterval = 200 * time.Millisecond
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = 15 * time.Second
	}
	if o.Registry == nil {
		o.Registry = tools.NewRegistry()
	}
	if o.Tracer == nil {
		o.Tracer = noop.NewTracerProvider().Tracer("")
	}
	if o.JaegerUI == "" {
		o.JaegerUI = DefaultJaegerUI
	}
	return o
}

// Server holds the handler dependencies.
type Server struct {
	store *store.Store
	log   *slog.Logger
	opts  Options
}

// NewServer builds a Server. A nil logger falls back to the default.
//
// When metrics are on it also registers the run-state collector, because the
// API process is the one that should answer questions about the runs table:
// it already holds the store, and unlike a worker there is exactly one kind
// of it, so the same gauge is not reported by every replica (ADR-26).
func NewServer(st *store.Store, log *slog.Logger, opts Options) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{store: st, log: log, opts: opts.withDefaults()}
	if s.opts.Metrics != nil && st != nil {
		if err := s.opts.Metrics.Register(s.runStateCollector()); err != nil {
			// Not fatal: a control plane that cannot report a gauge is still
			// a control plane. The only way to get here is registering two
			// servers against one registry, which is a wiring bug rather
			// than a runtime condition.
			s.log.Warn("run state collector not registered", "error", err)
		}
	}
	return s
}

// runStateCollector adapts the store to the collector's one-query interface.
// The two reads are one closure because they are one scrape of one table, and
// splitting them would mean two failure paths to report the same outage.
func (s *Server) runStateCollector() *telemetry.RunStateCollector {
	return telemetry.NewRunStateCollector(runtime.Statuses(), func(ctx context.Context) (telemetry.RunState, error) {
		counts, err := s.store.RunCounts(ctx)
		if err != nil {
			return telemetry.RunState{}, err
		}
		state := telemetry.RunState{Counts: make(map[string]int64, len(counts))}
		for _, c := range counts {
			state.Counts[c.Status] = c.Count
		}
		if state.OldestQueued, state.HasQueued, err = s.store.OldestQueuedAge(ctx); err != nil {
			return telemetry.RunState{}, err
		}
		return state, nil
	}, s.log)
}

// Router returns the mounted HTTP routes.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(s.count)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Served on the API's existing listener, unauthenticated like everything
	// else here (spec §2 puts auth out of scope) and with no run ids in any
	// label, so exposing it leaks nothing a scrape should not see.
	r.Handle("/metrics", s.opts.Metrics.Handler())
	r.Get("/v1/tools", s.listTools)
	r.Route("/v1/runs", func(r chi.Router) {
		r.Post("/", s.createRun)
		r.Get("/", s.listRuns)
		r.Get("/{id}", s.getRun)
		r.Get("/{id}/events", s.listEvents)
		r.Get("/{id}/stream", s.streamRun)
		r.Get("/{id}/trace", s.traceRun)
		r.Post("/{id}/cancel", s.cancelRun)
		r.Post("/{id}/resume", s.resumeRun)
	})
	// Last, so the viewer's catch-all reads as the fallback it is. chi matches
	// static segments ahead of a wildcard regardless of registration order, so
	// this does not shadow anything above it (ui.Mount documents that, and
	// ui's own test pins it in both orders).
	ui.Mount(r)
	return r
}

// count records every request as agentd_http_requests_total.
//
// The label is chi's matched route *pattern*, never the path: r.URL.Path is
// "/v1/runs/4f3c.../events", which would mint a new time series per run and
// eventually take the process down with it. That is the cardinality rule the
// whole metrics layer is written against, and this is the one place in the
// codebase where breaking it would be easy and invisible.
//
// The pattern is read after the handler returns because chi fills it in while
// routing; a request that matched nothing has none, and is counted as "other".
func (s *Server) count(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		defer func() {
			code := ww.Status()
			if code == 0 {
				// A handler that returned without writing anything: Go sends
				// 200 for it, so that is what the client saw.
				code = http.StatusOK
			}
			s.opts.Metrics.HTTPRequest(chi.RouteContext(r.Context()).RoutePattern(), r.Method, code)
		}()
		next.ServeHTTP(ww, r)
	})
}

// CreateRunRequest is the POST /v1/runs body.
type CreateRunRequest struct {
	Goal        string          `json:"goal"`
	AgentConfig json.RawMessage `json:"agent_config,omitempty"`
	MaxSteps    int32           `json:"max_steps,omitempty"`
	BudgetUSD   string          `json:"budget_usd,omitempty"`
}

// CreateRunResponse is the POST /v1/runs response.
type CreateRunResponse struct {
	ID     uuid.UUID `json:"id"`
	Status string    `json:"status"`
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var req CreateRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Goal == "" {
		writeError(w, http.StatusBadRequest, "goal is required")
		return
	}
	if req.MaxSteps <= 0 {
		req.MaxSteps = 30
	}
	if req.BudgetUSD == "" {
		req.BudgetUSD = "1.00"
	}
	cfg, err := s.normalizeConfig(req.AgentConfig)
	if err != nil {
		writeError(w, http.StatusBadRequest, "agent_config: "+err.Error())
		return
	}
	rawCfg, err := json.Marshal(cfg)
	if err != nil {
		s.log.Error("encode agent config", "error", err)
		writeError(w, http.StatusInternalServerError, "could not create run")
		return
	}

	// The span starts only once the request is known to be valid: a rejected
	// submission creates no run, so there is no run trace for it to belong
	// to, and inventing one would put spans in Jaeger for runs that do not
	// exist.
	//
	// The run's trace identity is minted here rather than by the worker
	// because the row is the only place it can live. A context does not
	// survive the queue between this process and the worker, let alone the
	// kill -9 between two attempts (ADR-24).
	traceID, rootSpanID := telemetry.NewIDs()

	// RemoteParent is the right tool even though the span it names as parent
	// does not exist yet. It asserts where this span belongs, not that a
	// parent has already been exported: the worker that writes the terminal
	// event emits agent.run later with exactly this span id, back-dated to
	// created_at, and api.create_run is then its child. Until that happens —
	// and forever, for a run that never finishes — Jaeger renders
	// api.create_run as a parentless span of the run's trace, which is the
	// same degradation the plan already documents for an unfinished run.
	// Starting a fresh trace instead is the one genuinely wrong answer: the
	// submission would land in a different waterfall from the run it
	// submitted.
	ctx := telemetry.RemoteParent(r.Context(), traceID.String(), rootSpanID.String())
	ctx, span := s.opts.Tracer.Start(ctx, telemetry.SpanCreateRun)
	span.SetAttributes(
		telemetry.AttrBudgetUSD.String(req.BudgetUSD),
		telemetry.AttrMaxSteps.Int(int(req.MaxSteps)),
		telemetry.AttrToolCount.Int(len(cfg.Tools)),
	)

	// Whether tracing is on is a question this handler can only ask of the
	// span it just started. The server holds a trace.Tracer, never a
	// *telemetry.Telemetry with its Enabled method: only cmd/agentd owns a
	// provider, which is what lets a test hand this package a recorder
	// instead. IsRecording is the honest discriminator among what a Tracer
	// offers; SpanContext().IsValid() is not, because the no-op tracer
	// faithfully propagates the remote parent planted above and so hands
	// back a valid span context whether tracing is on or off.
	//
	// A run submitted with tracing off stores NULL rather than the ids just
	// minted, because GET /v1/runs/:id/trace 404s on exactly that, and a
	// deep link to a trace no collector ever received is worse than no link.
	var storedTraceID, storedRootSpanID string
	if span.IsRecording() {
		storedTraceID, storedRootSpanID = traceID.String(), rootSpanID.String()
	}

	run, err := s.store.CreateRun(ctx, store.NewRun{
		Goal:        req.Goal,
		AgentConfig: rawCfg,
		MaxSteps:    req.MaxSteps,
		BudgetUSD:   req.BudgetUSD,
		TraceID:     storedTraceID,
		RootSpanID:  storedRootSpanID,
	})
	if err != nil {
		telemetry.End(span, err)
		s.log.Error("create run", "error", err)
		writeError(w, http.StatusInternalServerError, "could not create run")
		return
	}
	span.SetAttributes(telemetry.AttrRunID.String(run.ID.String()))
	telemetry.End(span, nil)

	s.log.InfoContext(ctx, "run submitted", "run_id", run.ID)
	writeJSON(w, http.StatusCreated, CreateRunResponse{ID: run.ID, Status: run.Status})
}

// normalizeConfig validates agent_config strictly and fixes the tool
// allowlist at submission time (spec §10): a run that names no tools gets
// every registered tool, and one that names unknown tools is rejected.
//
// It returns the parsed config rather than its JSON so the caller can both
// store it and count its tools; marshalling it back is the caller's job.
func (s *Server) normalizeConfig(raw json.RawMessage) (runtime.AgentConfig, error) {
	var cfg runtime.AgentConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, err
		}
	}
	if cfg.Tools == nil {
		cfg.Tools = s.opts.Registry.Names()
	}
	for _, name := range cfg.Tools {
		if _, ok := s.opts.Registry.Get(name); !ok {
			return cfg, fmt.Errorf("unknown tool %q", name)
		}
	}
	if cfg.ToolDelayMS < 0 {
		return cfg, errors.New("tool_delay_ms must not be negative")
	}
	return cfg, nil
}

// RunResponse is GET /v1/runs/:id: the row plus the reduced event log.
type RunResponse struct {
	Run   *store.Run     `json:"run"`
	State *runtime.State `json:"state"`
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.lookupRun(w, r)
	if !ok {
		return
	}
	events, err := s.store.ListEvents(r.Context(), run.ID, 0)
	if err != nil {
		s.log.Error("list events", "run_id", run.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not load events")
		return
	}
	state, err := runtime.Reduce(events)
	if err != nil {
		s.log.Error("reduce", "run_id", run.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not reduce event log")
		return
	}
	writeJSON(w, http.StatusOK, RunResponse{Run: run, State: &state})
}

// TraceResponse is GET /v1/runs/:id/trace: the run's trace id and the Jaeger
// deep link that opens it.
type TraceResponse struct {
	TraceID string `json:"trace_id"`
	URL     string `json:"url"`
}

// traceRun returns the deep link to the run's trace, or 404 when there is no
// trace to link to — a run created before M4, or one submitted while tracing
// was off.
//
// The 404 is the whole point of the handler. Rendering a link for a trace no
// collector ever received would be worse than having no endpoint: the link
// resolves to an empty Jaeger page, which reads as "your trace was lost"
// rather than "this run was never traced", and the first is a bug report.
func (s *Server) traceRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.lookupRun(w, r)
	if !ok {
		return
	}
	if run.TraceID == nil || *run.TraceID == "" {
		writeError(w, http.StatusNotFound, "run has no trace")
		return
	}
	writeJSON(w, http.StatusOK, TraceResponse{
		TraceID: *run.TraceID,
		URL:     traceURL(s.opts.JaegerUI, *run.TraceID),
	})
}

// traceURL joins the Jaeger UI base with the trace path, tolerating a base
// that was configured with or without a trailing slash. Both spellings are
// what an operator actually types into -jaeger-ui, and a doubled slash makes
// a link that still works but looks broken enough to be reported as one.
func traceURL(base, traceID string) string {
	return strings.TrimRight(base, "/") + "/trace/" + traceID
}

// cancelRun sets the cooperative cancel flag; the worker finishes the run
// as cancelled at its next loop iteration.
func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.lookupRun(w, r)
	if !ok {
		return
	}
	ok, err := s.store.RequestCancel(r.Context(), run.ID)
	if err != nil {
		s.log.Error("cancel run", "run_id", run.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not cancel run")
		return
	}
	if !ok {
		writeError(w, http.StatusConflict, "run already finished")
		return
	}
	s.log.Info("cancel requested", "run_id", run.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"id": run.ID, "cancel_requested": true})
}

// resumeRun force-releases a run's lease so any worker can pick it up. It
// is the manual override for a run stuck on a worker that still heartbeats.
func (s *Server) resumeRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.lookupRun(w, r)
	if !ok {
		return
	}
	ok, err := s.store.ReleaseLease(r.Context(), run.ID)
	if err != nil {
		s.log.Error("resume run", "run_id", run.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not resume run")
		return
	}
	if !ok {
		writeError(w, http.StatusConflict, "run already finished")
		return
	}
	s.log.Info("run released for resume", "run_id", run.ID, "previous_owner", run.LeaseOwner)
	writeJSON(w, http.StatusAccepted, map[string]any{"id": run.ID, "status": runtime.StatusQueued})
}

// ToolInfo is one entry of GET /v1/tools.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	TrustTier   tools.TrustTier `json:"trust_tier"`
	Schema      json.RawMessage `json:"input_schema"`
}

func (s *Server) listTools(w http.ResponseWriter, r *http.Request) {
	list := s.opts.Registry.List()
	out := make([]ToolInfo, 0, len(list))
	for _, t := range list {
		out = append(out, ToolInfo{Name: t.Name(), Description: t.Description(), TrustTier: t.TrustTier(), Schema: t.Schema()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": out})
}

// listRuns serves GET /v1/runs, the newest runs first, optionally narrowed to
// one status. It exists for the viewer, which would otherwise open on a box
// wanting a run id pasted into it.
//
// An unknown ?status= is rejected here rather than in the store, which matches
// nothing for one instead (see store.ListRuns). The two answers are both
// defensible and the difference is who is asking: a caller that typo'd a status
// is a client bug worth naming, and an empty list would read as "no runs are
// queued" rather than "queued is not spelled that way".
func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	opts := store.ListRunsOptions{Status: r.URL.Query().Get("status")}
	if opts.Status != "" && !slices.Contains(runtime.Statuses(), opts.Status) {
		writeError(w, http.StatusBadRequest,
			"unknown status "+strconv.Quote(opts.Status)+"; want one of "+strings.Join(runtime.Statuses(), ", "))
		return
	}
	// An unparseable limit is a client bug for the same reason. Out-of-range
	// values are not: the store clamps to MaxRunLimit, so asking for too much
	// gets the most it may have.
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "limit must be an integer")
			return
		}
		opts.Limit = n
	}

	runs, err := s.store.ListRuns(r.Context(), opts)
	if err != nil {
		s.log.Error("list runs", "error", err)
		writeError(w, http.StatusInternalServerError, "could not list runs")
		return
	}
	if runs == nil {
		runs = []store.Run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	run, ok := s.lookupRun(w, r)
	if !ok {
		return
	}
	events, err := s.store.ListEvents(r.Context(), run.ID, 0)
	if err != nil {
		s.log.Error("list events", "run_id", run.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not list events")
		return
	}
	if events == nil {
		events = []store.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID, "events": events})
}

// streamRun tails the event log as SSE. Each event's SSE id is its seq, so a
// reconnecting client sends Last-Event-ID and gets every event it missed
// before the live tail resumes. The stream closes after run_finished.
func (s *Server) streamRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.lookupRun(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	lastSeq := lastEventID(r)

	// The gauge is the one number that says whether streams are being left
	// open: every other SSE signal is a rate, and a client that connects and
	// never disconnects shows up in none of them.
	s.opts.Metrics.SSEStreamOpened()
	defer s.opts.Metrics.SSEStreamClosed()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	poll := time.NewTicker(s.opts.PollInterval)
	defer poll.Stop()
	keepAlive := time.NewTicker(s.opts.KeepAlive)
	defer keepAlive.Stop()

	for {
		events, err := s.store.ListEvents(ctx, run.ID, lastSeq)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Error("stream: list events", "run_id", run.ID, "error", err)
			return
		}
		for _, ev := range events {
			if err := writeSSEEvent(w, ev); err != nil {
				return
			}
			lastSeq = ev.Seq
			flusher.Flush()
			if runtime.IsTerminal(ev.Type) {
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-poll.C:
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, ev store.Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, data)
	return err
}

// lastEventID reads the replay cursor from the standard header, falling back
// to a query parameter for clients (curl, EventSource polyfills) that cannot
// set headers.
func lastEventID(r *http.Request) int32 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("last_event_id")
	}
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n < 0 {
		return 0
	}
	return int32(n)
}

func (s *Server) lookupRun(w http.ResponseWriter, r *http.Request) (*store.Run, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return nil, false
	}
	run, err := s.store.GetRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "run not found")
		return nil, false
	}
	if err != nil {
		s.log.Error("get run", "run_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not load run")
		return nil, false
	}
	return run, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ListenAndServe runs the HTTP server until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Router(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE streams are long-lived by design.
	}
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("api listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
