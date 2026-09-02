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

	"github.com/shreyasprasad/agentd/internal/api"
	"github.com/shreyasprasad/agentd/internal/runtime"
	"github.com/shreyasprasad/agentd/internal/store"
)

const defaultDSN = "postgres://agentd:agentd@localhost:5432/agentd?sslmode=disable"

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

`)
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("AGENTD_ADDR", ":8080"), "listen address")
	dsn := fs.String("dsn", envOr("AGENTD_DSN", defaultDSN), "Postgres DSN")
	poll := fs.Duration("sse-poll", 200*time.Millisecond, "SSE event tail poll interval")
	skipMigrate := fs.Bool("skip-migrate", false, "do not apply migrations at startup")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger("serve")
	st, err := open(ctx, *dsn, log, !*skipMigrate)
	if err != nil {
		return err
	}
	defer st.Close()

	srv := api.NewServer(st, log, api.Options{PollInterval: *poll})
	return srv.ListenAndServe(ctx, *addr)
}

func work(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("work", flag.ExitOnError)
	dsn := fs.String("dsn", envOr("AGENTD_DSN", defaultDSN), "Postgres DSN")
	owner := fs.String("owner", os.Getenv("AGENTD_WORKER_OWNER"), "lease owner identity (default: hostname/random)")
	poll := fs.Duration("poll", 250*time.Millisecond, "queue poll interval")
	lease := fs.Duration("lease", 60*time.Second, "lease duration")
	stepDelay := fs.Duration("step-delay", time.Second, "stub agent delay between events")
	skipMigrate := fs.Bool("skip-migrate", false, "do not apply migrations at startup")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger("work")
	st, err := open(ctx, *dsn, log, !*skipMigrate)
	if err != nil {
		return err
	}
	defer st.Close()

	w := runtime.NewWorker(st, log, runtime.WorkerConfig{
		Owner:         *owner,
		PollInterval:  *poll,
		LeaseDuration: *lease,
		StepDelay:     *stepDelay,
	})
	return w.Run(ctx)
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

func newLogger(mode string) *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("AGENTD_DEBUG") != "" {
		level = slog.LevelDebug
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
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
