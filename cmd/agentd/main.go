// Command agentd is the single binary for the durable agent execution
// runtime. It runs in two modes:
//
//	agentd serve   # HTTP control plane
//	agentd work    # queue worker
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/shreyasprasad/agentd/internal/api"
	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/anthropic"
	"github.com/shreyasprasad/agentd/internal/model/local"
	"github.com/shreyasprasad/agentd/internal/retrieval"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/sandbox"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/telemetry"
	"github.com/shreyasprasad/agentd/internal/tools"
	"github.com/shreyasprasad/agentd/internal/tools/builtin"
	"github.com/shreyasprasad/agentd/internal/tools/corpus"
	"github.com/shreyasprasad/agentd/internal/tools/python"
)

const (
	defaultDSN          = "postgres://agentd:agentd@localhost:5432/agentd?sslmode=disable"
	defaultModelURL     = "http://localhost:11434/v1"
	defaultModel        = "qwen2.5:7b"
	defaultSandboxImage = "agentd/sandbox:python"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "agentd:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return fmt.Errorf("a subcommand is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "serve":
		return serve(ctx, os.Args[2:])
	case "work":
		return work(ctx, os.Args[2:])
	case "migrate":
		return migrate(ctx, os.Args[2:])
	case "fetch":
		return fetchCmd(ctx, os.Args[2:])
	case "fetch-cap":
		return fetchCAPCmd(ctx, os.Args[2:])
	case "dedupe":
		return dedupeCmd(ctx, os.Args[2:])
	case "ingest":
		return ingestCmd(ctx, os.Args[2:])
	case "eval":
		return evalCmd(ctx, os.Args[2:])
	case "healthz":
		return healthCmd(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: agentd <command> [flags]

commands:
  serve     run the HTTP API (POST /v1/runs, GET /v1/runs/:id, SSE stream)
  work      run a queue worker that executes claimed runs
  migrate   apply database migrations and exit
  fetch     pull court opinions from CourtListener into a JSONL corpus file
  fetch-cap pull opinions from the Caselaw Access Project (no quota; stops at 2014)
  dedupe    collapse revisions of the same case in a corpus file (run before ingest)
  ingest    chunk, embed, and upsert a fetched corpus into Postgres
  eval      run an eval suite (eval retrieval | eval suite | eval record)
  healthz   probe a serve or work process's /healthz and exit 0 or 1

`)
}

// registry is the tool set. serve and work must agree on its names: the API
// fills a run's default allowlist from it, the worker dispatches through it.
// Only the worker has a sandbox executor, an embedder, and a store; serve
// passes nils and gets tools it can list and allowlist but not run.
func registry(exec sandbox.Executor, limits sandbox.Limits, searcher corpus.Searcher, docs corpus.Store) *tools.Registry {
	return tools.NewRegistry().MustRegister(builtin.Finish{}, builtin.ComputeDeadline{}, python.New(exec, limits),
		corpus.NewSearch(searcher), corpus.NewFetch(docs))
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("AGENTD_ADDR", ":8080"), "listen address")
	dsn := fs.String("dsn", envOr("AGENTD_DSN", defaultDSN), "Postgres DSN")
	poll := fs.Duration("sse-poll", 200*time.Millisecond, "SSE event tail poll interval")
	jaegerUI := fs.String("jaeger-ui", envOr("AGENTD_JAEGER_UI", api.DefaultJaegerUI),
		"browser-facing Jaeger base URL that GET /v1/runs/:id/trace links into")
	mf := addMCPFlag(fs)
	tf := addTraceFlags(fs, "agentd-api")
	skipMigrate := fs.Bool("skip-migrate", false, "do not apply migrations at startup")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger("serve")
	tel, err := tf.start(ctx, log)
	if err != nil {
		return err
	}
	defer stopTracing(tel, log)

	st, err := open(ctx, *dsn, log, !*skipMigrate)
	if err != nil {
		return err
	}
	defer st.Close()

	// serve holds no sandbox, embedder or store handle, so its registry lists
	// and allowlists tools it cannot run. MCP is the exception it still has to
	// dial: the manifest is only obtainable from the server.
	mcpTools, mcpClose, err := mf.connect(ctx, log)
	if err != nil {
		return err
	}
	defer mcpClose.Close()
	reg := registry(nil, sandbox.DefaultLimits(), nil, nil)
	registerMCP(reg, mcpTools, log)

	// The API serves /metrics on its own listener, and NewServer registers
	// the Postgres-backed run gauges against this registry (ADR-26).
	srv := api.NewServer(st, log, api.Options{
		PollInterval: *poll,
		Registry:     reg,
		Tracer:       tel.Tracer(),
		JaegerUI:     *jaegerUI,
		Metrics:      telemetry.NewMetrics(),
	})
	return srv.ListenAndServe(ctx, *addr)
}

func work(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("work", flag.ExitOnError)
	dsn := fs.String("dsn", envOr("AGENTD_DSN", defaultDSN), "Postgres DSN")
	owner := fs.String("owner", os.Getenv("AGENTD_WORKER_OWNER"), "lease owner identity (default: hostname/random)")
	poll := fs.Duration("poll", 250*time.Millisecond, "queue poll interval")
	lease := fs.Duration("lease", 60*time.Second, "lease duration")
	reaper := fs.Duration("reaper-interval", 0, "expired-lease sweep interval (default: lease/2)")
	modelURL := fs.String("model-url", envOr("AGENTD_MODEL_URL", defaultModelURL), "OpenAI-compatible chat completions base URL (Ollama, llama.cpp)")
	modelName := fs.String("model", envOr("AGENTD_MODEL", defaultModel),
		"default model when a run's agent_config names none; \"anthropic/...\" or a bare \"claude-*\" name routes to Anthropic, anything else to the local runtime")
	modelTimeout := fs.Duration("model-timeout", 10*time.Minute, "per-call model timeout")
	anthropicPrices := fs.String("anthropic-prices", os.Getenv("AGENTD_ANTHROPIC_PRICES"),
		"extra Anthropic prices as model=input/output in USD per million tokens, comma separated (models without a price cannot be called)")
	thinkingDisplay := fs.String("anthropic-thinking-display", os.Getenv("AGENTD_ANTHROPIC_THINKING_DISPLAY"),
		"\"summarized\" records readable thinking summaries in the event log, \"omitted\" only signatures; empty takes the model default")
	sandboxImage := fs.String("sandbox-image", envOr("AGENTD_SANDBOX_IMAGE", defaultSandboxImage), "container image for sandboxed tools (build with `make sandbox-build`)")
	sandboxTimeout := fs.Duration("sandbox-timeout", envDurationOr("AGENTD_SANDBOX_TIMEOUT", sandbox.DefaultLimits().DefaultTimeout), "default wall-clock limit per sandboxed tool call")
	sandboxMaxTimeout := fs.Duration("sandbox-max-timeout", envDurationOr("AGENTD_SANDBOX_MAX_TIMEOUT", sandbox.DefaultLimits().MaxTimeout), "the most a tool call may ask for via timeout_seconds")
	cancelPoll := fs.Duration("cancel-poll", envDurationOr("AGENTD_CANCEL_POLL", runtime.DefaultCancelPoll),
		"how often a running run's cancel flag is checked; the floor on cancel latency (ADR-25)")
	mf := addMCPFlag(fs)
	ef := addEmbedFlags(fs)
	rf := addRerankFlags(fs)
	tf := addTraceFlags(fs, "agentd-worker")
	metricsAddr := addMetricsAddrFlag(fs)
	skipMigrate := fs.Bool("skip-migrate", false, "do not apply migrations at startup")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger("work")
	// One registry for this process, injected into everything that counts:
	// the loop and the worker through WorkerConfig, the sandbox and the
	// searcher through their own configs. No package-level instruments, so
	// two workers in one test process cannot fight over a registration.
	metrics := telemetry.NewMetrics()
	stopMetrics := serveMetrics(*metricsAddr, metrics, log)
	defer stopMetrics()
	// Before anything else that can block on a model, a daemon, or Postgres:
	// the tracer is what the rest of this function is threaded with, and its
	// startup line belongs above the slow ones an operator waits through.
	tel, err := tf.start(ctx, log)
	if err != nil {
		return err
	}
	defer stopTracing(tel, log)

	provider, onBox, err := buildProvider(*modelURL, *modelTimeout, *anthropicPrices, *thinkingDisplay, log)
	if err != nil {
		return err
	}
	exec, err := buildSandbox(ctx, *sandboxImage, sandbox.Limits{DefaultTimeout: *sandboxTimeout, MaxTimeout: *sandboxMaxTimeout},
		log, tel.Tracer(), metrics)
	if err != nil {
		return err
	}
	defer exec.Close()

	embedder := ef.buildEmbedder(*modelURL)
	if err := probeEmbedder(ctx, embedder, log); err != nil {
		return err
	}
	reranker, rerankModel, err := rf.buildReranker(onBox, *modelName, log)
	if err != nil {
		return err
	}
	if reranker != nil {
		log.Info("reranker ready", "model", rerankModel, "candidates", *rf.candidates)
	}

	st, err := open(ctx, *dsn, log, !*skipMigrate)
	if err != nil {
		return err
	}
	defer st.Close()

	searcher := &retrieval.Searcher{
		Store: st, Embedder: embedder, Reranker: reranker,
		RerankCandidates: *rf.candidates, Log: log, Tracer: tel.Tracer(), Metrics: metrics,
	}
	// Dialed after the store so a config typo is reported before anything
	// spawns a subprocess, and deferred immediately so every path out of this
	// function past here reaps the stdio children. A worker that returns an
	// error without closing them leaks one process per configured server.
	mcpTools, mcpClose, err := mf.connect(ctx, log)
	if err != nil {
		return err
	}
	defer mcpClose.Close()
	reg := registry(exec, exec.Limits(), searcher, st)
	registerMCP(reg, mcpTools, log)

	w := runtime.NewWorker(st, log, runtime.WorkerConfig{
		Owner:          *owner,
		PollInterval:   *poll,
		LeaseDuration:  *lease,
		ReaperInterval: *reaper,
		Provider:       provider,
		Registry:       reg,
		DefaultModel:   *modelName,
		CancelPoll:     *cancelPoll,
		Tracer:         tel.Tracer(),
		Metrics:        metrics,
	})
	return w.Run(ctx)
}

// buildSandbox wires the Docker executor. An unreachable daemon or a missing
// image is a warning, not a fatal error: runs that never call a sandboxed
// tool must keep working, and the ones that do get a clear tool_failed. When
// the daemon is reachable, leftovers from a previous worker that died
// mid-call are swept before any run is claimed.
func buildSandbox(ctx context.Context, image string, limits sandbox.Limits, log *slog.Logger,
	tracer trace.Tracer, metrics *telemetry.Metrics) (*sandbox.Docker, error) {
	exec, err := sandbox.NewDocker(sandbox.DockerConfig{Image: image, Limits: limits, Log: log, Tracer: tracer, Metrics: metrics})
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := exec.Ping(pingCtx); err != nil {
		log.Warn("sandbox unavailable; sandboxed tool calls will fail until it is", "error", err, "docker_host", exec.Host())
		return exec, nil
	}
	n, err := exec.SweepOrphans(pingCtx, exec.OrphanAge())
	if err != nil {
		log.Warn("sandbox orphan sweep failed", "error", err)
	}
	l := exec.Limits()
	log.Info("sandbox ready", "docker_host", exec.Host(), "image", image, "orphans_removed", n,
		"memory_mb", l.MemoryBytes>>20, "cpus", l.CPUs, "pids", l.PidsLimit, "timeout", l.DefaultTimeout, "max_timeout", l.MaxTimeout)
	return exec, nil
}

// buildProvider wires the local runtime and Anthropic behind one Router. The
// local backend is the default; Anthropic claims "anthropic/..." and bare
// "claude-*" model names. Anthropic is always registered: credentials are
// only needed once a run actually targets it, and a missing key then fails
// that run with a clear authentication error rather than the whole worker.
// The local provider is also returned bare for the reranker, which must not
// spend hosted-API money outside a run's budget.
func buildProvider(modelURL string, timeout time.Duration, prices, thinkingDisplay string, log *slog.Logger) (model.Provider, *local.Provider, error) {
	extra, err := anthropic.ParsePrices(prices)
	if err != nil {
		return nil, nil, fmt.Errorf("-anthropic-prices: %w", err)
	}
	switch thinkingDisplay {
	case "", "summarized", "omitted":
	default:
		return nil, nil, fmt.Errorf("-anthropic-thinking-display: want summarized or omitted, got %q", thinkingDisplay)
	}
	onBox := local.New(local.Config{
		BaseURL: modelURL,
		APIKey:  os.Getenv("AGENTD_MODEL_API_KEY"),
		Timeout: timeout,
	})
	hosted := anthropic.New(anthropic.Config{
		Timeout:         timeout,
		Price:           extra,
		ThinkingDisplay: thinkingDisplay,
	})
	log.Info("model backends", "local", modelURL, "anthropic_api_key_set", os.Getenv("ANTHROPIC_API_KEY") != "",
		"anthropic_extra_prices", len(extra))
	router := model.NewRouter().
		Register("local", onBox, nil).
		Register("anthropic", hosted, anthropic.IsClaudeModel)
	return router, onBox, nil
}

func migrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dsn := fs.String("dsn", envOr("AGENTD_DSN", defaultDSN), "Postgres DSN")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := newLogger("migrate")
	st, err := open(ctx, *dsn, log, true)
	if err != nil {
		return err
	}
	defer st.Close()
	log.Info("migrations applied")
	return nil
}

// open connects with a short retry loop so `docker compose up` ordering does
// not matter, then optionally applies migrations.
func open(ctx context.Context, dsn string, log *slog.Logger, doMigrate bool) (*store.Store, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := store.New(ctx, dsn)
		if err == nil {
			if doMigrate {
				if err := st.Migrate(ctx); err != nil {
					st.Close()
					return nil, fmt.Errorf("migrate: %w", err)
				}
			}
			return st, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("connect to postgres: %w", err)
		}
		log.Warn("postgres not ready, retrying", "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// newLogger builds the process logger. Every line goes through
// telemetry.NewHandler, which adds trace_id and span_id when the context a
// line was logged with carries a span, so a log line and a bar of the
// waterfall can be matched up. It wraps unconditionally: the wrapper adds
// nothing to a line that has no span, so the commands that never trace —
// migrate, fetch, ingest, eval — pay a context lookup and print what they
// always printed.
func newLogger(mode string) *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("AGENTD_DEBUG") != "" {
		level = slog.LevelDebug
	}
	h := telemetry.NewHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	log := slog.New(h).With("mode", mode)
	slog.SetDefault(log)
	return log
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
