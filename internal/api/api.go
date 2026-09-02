// Package api is the HTTP control plane. It never calls a model: it writes
// intent to Postgres and tails the event log.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
)

// Options tunes the SSE tailing behaviour.
type Options struct {
	// PollInterval is how often a live SSE stream checks for new events.
	// M0 polls; LISTEN/NOTIFY is the obvious upgrade once event volume matters.
	PollInterval time.Duration
	// KeepAlive is how often an idle stream emits an SSE comment so proxies
	// do not reap the connection.
	KeepAlive time.Duration
}

func (o Options) withDefaults() Options {
	if o.PollInterval <= 0 {
		o.PollInterval = 200 * time.Millisecond
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = 15 * time.Second
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
func NewServer(st *store.Store, log *slog.Logger, opts Options) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{store: st, log: log, opts: opts.withDefaults()}
}

// Router returns the mounted HTTP routes.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Route("/v1/runs", func(r chi.Router) {
		r.Post("/", s.createRun)
		r.Get("/{id}", s.getRun)
		r.Get("/{id}/events", s.listEvents)
		r.Get("/{id}/stream", s.streamRun)
	})
	return r
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

	run, err := s.store.CreateRun(r.Context(), req.Goal, req.AgentConfig, req.MaxSteps, req.BudgetUSD)
	if err != nil {
		s.log.Error("create run", "error", err)
		writeError(w, http.StatusInternalServerError, "could not create run")
		return
	}
	s.log.Info("run submitted", "run_id", run.ID)
	writeJSON(w, http.StatusCreated, CreateRunResponse{ID: run.ID, Status: run.Status})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.lookupRun(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, run)
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
