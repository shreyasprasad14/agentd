package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/shreyasprasad/agentd/internal/evals"
	"github.com/shreyasprasad/agentd/internal/model"
	"github.com/shreyasprasad/agentd/internal/model/cassette"
	"github.com/shreyasprasad/agentd/internal/retrieval"
	"github.com/shreyasprasad/agentd/internal/retrieval/embed"
	"github.com/shreyasprasad/agentd/internal/sandbox"
	"github.com/shreyasprasad/agentd/internal/store"
	"github.com/shreyasprasad/agentd/internal/tools"
	"github.com/shreyasprasad/agentd/internal/tools/mcp"
	"github.com/shreyasprasad/agentd/internal/tools/mcp/testserver"
)

// defaultEvalDSN is a database of its own. The suite ingests with the fake
// embedder and its corpus includes documents that say "disregard your
// instructions"; neither belongs in the database the demos search.
const defaultEvalDSN = "postgres://agentd:agentd@localhost:5432/agentd_eval?sslmode=disable"

const (
	defaultSuiteDir     = "evals/cases"
	defaultCassetteDir  = "evals/cassettes"
	defaultEvalResults  = "evals/results.json"
	defaultEvalEmbedder = "fake"
)

// evalSuiteCmd runs the agent eval suite. Replay is the default and needs
// nothing but Postgres: no model, no API key, no network.
func evalSuiteCmd(ctx context.Context, args []string, record bool) error {
	name := "eval suite"
	if record {
		name = "eval record"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	dsn := fs.String("dsn", envOr("AGENTD_EVAL_DSN", defaultEvalDSN), "Postgres DSN for the eval database")
	suiteDir := fs.String("suite", defaultSuiteDir, "suite file or directory of *.yaml")
	cassetteDir := fs.String("cassettes", defaultCassetteDir, "directory of per-case cassettes")
	resultsPath := fs.String("results", defaultEvalResults, "where to write the machine-readable scorecard")
	only := fs.String("case", "", "comma-separated case ids to run (default all)")
	live := fs.Bool("live", false, "answer from a real provider instead of a cassette")
	onMiss := fs.String("on-miss", string(cassette.MissFail),
		"what a replay miss does: fail (the default, and the only safe value for CI) or record")
	from := fs.String("from", "script",
		"where `eval record` takes answers from: \"script\" replays each case's declared trajectory and needs no model, \"model\" asks a real one")
	modelURL := fs.String("model-url", envOr("AGENTD_MODEL_URL", defaultModelURL), "OpenAI-compatible base URL for the live provider")
	modelName := fs.String("model", envOr("AGENTD_MODEL", defaultModel), "model for -live and for recording")
	modelTimeout := fs.Duration("model-timeout", 10*time.Minute, "per-call model timeout")
	embedder := fs.String("embedder", defaultEvalEmbedder,
		"\"fake\" for the deterministic model-free embedder, or an embedding model name served by -model-url")
	sandboxImage := fs.String("sandbox-image", envOr("AGENTD_SANDBOX_IMAGE", defaultSandboxImage), "container image for sandboxed tools")
	noSandbox := fs.Bool("no-sandbox", false, "skip the Docker-dependent cases without probing the daemon")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if record {
		*onMiss = string(cassette.MissRecord)
	}
	if *onMiss != string(cassette.MissFail) && *onMiss != string(cassette.MissRecord) {
		return fmt.Errorf("-on-miss: want %q or %q, got %q", cassette.MissFail, cassette.MissRecord, *onMiss)
	}
	if *from != "script" && *from != "model" {
		return fmt.Errorf("-from: want \"script\" or \"model\", got %q", *from)
	}
	fromScript := record && *from == "script"
	log := newLogger("eval")

	suites, err := evals.LoadSuites(*suiteDir)
	if err != nil {
		return err
	}

	if err := ensureDatabase(ctx, *dsn, log); err != nil {
		return err
	}
	st, err := open(ctx, *dsn, log, true)
	if err != nil {
		return err
	}
	defer st.Close()

	emb, err := buildEvalEmbedder(ctx, *embedder, *modelURL)
	if err != nil {
		return err
	}
	if err := evals.Prepare(ctx, st, emb, suites, log); err != nil {
		return err
	}

	// Only the live and model-recording paths need a real model. Neither
	// replay nor a scripted re-record may probe one, so that `make eval` and
	// `make eval-record` both work with Ollama stopped and no key set.
	var liveProvider model.Provider
	if *live || (record && !fromScript) {
		liveProvider, _, err = buildProvider(*modelURL, *modelTimeout, "", "", log)
		if err != nil {
			return err
		}
	}

	backend, mode := buildEvalBackend(evalBackendOptions{
		Live:         *live,
		Record:       record,
		FromScript:   fromScript,
		CassetteDir:  *cassetteDir,
		OnMiss:       cassette.OnMiss(*onMiss),
		LiveProvider: liveProvider,
		Model:        *modelName,
	})

	exec, dockerOK := buildEvalSandbox(ctx, *sandboxImage, *noSandbox, log)
	if exec != nil {
		defer exec.Close()
	}
	searcher := &retrieval.Searcher{Store: st, Embedder: emb, Log: log}
	registry := registry(exec, sandbox.DefaultLimits(), searcher, st)
	stopMCP, mcpOK := buildEvalMCP(ctx, registry, log)
	defer stopMCP()

	runner := evals.NewRunner(evals.Config{
		Store:        st,
		Registry:     registry,
		Backend:      backend,
		Log:          log,
		DefaultModel: *modelName,
		Capabilities: map[string]bool{
			evals.CapDocker: dockerOK,
			evals.CapCorpus: evals.HasCorpus(ctx, st),
			evals.CapMCP:    mcpOK,
		},
		Only: parseOnly(*only),
	})

	log.Info("running the eval suite", "mode", mode, "suites", len(suites),
		"cases", countCases(suites), "docker", dockerOK, "embed_model", emb.Model())

	start := time.Now()
	results, err := runner.Run(ctx, suites)
	if err != nil {
		return err
	}
	rep := evals.Summarize(mode, results, mergeThresholds(suites), time.Since(start))

	fmt.Println()
	fmt.Print(evals.RenderTable(rep, mergeThresholds(suites)))
	fmt.Println()
	if s := evals.RenderFailures(rep); s != "" {
		fmt.Print(s)
		fmt.Println()
	}
	fmt.Println("  retrieval quality is scored separately: make eval-retrieval (writes evals/retrieval/results-scotus-cap.json)")
	fmt.Println()

	if err := writeEvalResults(*resultsPath, rep, emb.Model(), *cassetteDir); err != nil {
		return err
	}
	log.Info("results written", "path", *resultsPath)

	if !rep.OK() {
		return fmt.Errorf("thresholds missed:\n  %s", strings.Join(rep.Failures, "\n  "))
	}
	return nil
}

type evalBackendOptions struct {
	Live         bool
	Record       bool
	FromScript   bool
	CassetteDir  string
	OnMiss       cassette.OnMiss
	LiveProvider model.Provider
	Model        string
}

// invalidCatalogName is the SQLSTATE Postgres returns for "database does not
// exist".
const invalidCatalogName = "3D000"

// ensureDatabase creates the eval database if it is missing, by connecting to
// the maintenance database on the same server.
//
// It exists because the suite runs against a database of its own (the corpus
// includes documents that say "disregard your instructions", and the demos
// must never search them), and a `make eval` whose first failure is "create
// this database by hand" is one step of setup away from never being run.
// Nothing outside the eval path calls it: `serve` and `work` connect to a
// database an operator provisioned.
func ensureDatabase(ctx context.Context, dsn string, log *slog.Logger) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" || name == "postgres" {
		return nil
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err == nil {
		conn.Close(ctx)
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != invalidCatalogName {
		// Unreachable server, bad credentials, anything else: let open()'s
		// retry loop report it, since it has the better message for a
		// Postgres that is merely still starting.
		return nil
	}

	admin := *u
	admin.Path = "/postgres"
	adminConn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		return fmt.Errorf("connect to the maintenance database to create %q: %w", name, err)
	}
	defer adminConn.Close(ctx)
	// The identifier comes from a DSN the operator typed, but quoting it is
	// free and CREATE DATABASE takes no parameters.
	if _, err := adminConn.Exec(ctx, `CREATE DATABASE "`+strings.ReplaceAll(name, `"`, `""`)+`"`); err != nil {
		return fmt.Errorf("create database %q: %w", name, err)
	}
	log.Info("created the eval database", "database", name)
	return nil
}

func buildEvalBackend(o evalBackendOptions) (evals.Backend, string) {
	if o.Live {
		return &evals.LiveBackend{Provider: o.LiveProvider, Model: o.Model}, "live"
	}
	mode := "replay"
	switch {
	case o.Record && o.FromScript:
		mode = "record (scripted)"
	case o.Record:
		mode = "record (live model)"
	}
	return &evals.CassetteBackend{
		Dir:    o.CassetteDir,
		OnMiss: o.OnMiss,
		Live:   o.LiveProvider,
		Model:  o.Model,
		Script: o.FromScript,
		// A scripted re-record rebuilds each file from a trajectory that is
		// fully known, so leaving stale entries behind would serve no purpose.
		// A model re-record appends, which is what makes re-recording after a
		// prompt edit re-ask only the calls that actually changed.
		Fresh: o.FromScript,
	}, mode
}

// buildEvalEmbedder defaults to the deterministic fake. Replay freezes the
// model and nothing else, but retrieval still has to be reproducible for a
// cassette key to match twice, and a bag-of-words embedder gives a small
// corpus real nearest-neighbour structure with no model anywhere.
func buildEvalEmbedder(ctx context.Context, name, modelURL string) (embed.Embedder, error) {
	if name == "" || name == defaultEvalEmbedder {
		return embed.NewFake(), nil
	}
	e := embed.New(embed.Config{BaseURL: modelURL, Model: name, Dimensions: store.EmbeddingDimensions})
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := embed.Probe(probeCtx, e); err != nil {
		return nil, fmt.Errorf("embedder %s: %w", name, err)
	}
	return e, nil
}

// buildEvalSandbox reports whether the Docker-dependent cases can run. An
// unreachable daemon is a skip, never a failure: `make eval` has to work on a
// machine without Docker and say so in the table.
func buildEvalSandbox(ctx context.Context, image string, disabled bool, log *slog.Logger) (*sandbox.Docker, bool) {
	if disabled {
		return nil, false
	}
	exec, err := sandbox.NewDocker(sandbox.DockerConfig{Image: image, Log: log})
	if err != nil {
		log.Warn("sandbox unavailable; SAFETY cases will skip", "error", err)
		return nil, false
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := exec.Ping(pingCtx); err != nil {
		log.Warn("sandbox unavailable; SAFETY cases will skip", "error", err, "docker_host", exec.Host())
		return exec, false
	}
	return exec, true
}

// buildEvalMCP starts the in-repo MCP server with a poisoned tool description
// and registers what it advertises, so the INJECTION category can score the
// one attack the <tool_result> envelope does not cover: a server that writes
// an instruction into a tool's *description*, which rides in the model's tool
// definitions outside every envelope (ADR-35).
//
// It runs in-process over HTTP on a loopback port rather than spawning the
// fixture binary. The suite is not testing the transport — the adapter's own
// tests do that over a real pipe — and an in-process server means `make eval`
// gains no build step and no child to reap.
//
// A failure here is a skip, like Docker: the rest of the suite must still run.
func buildEvalMCP(ctx context.Context, reg *tools.Registry, log *slog.Logger) (func(), bool) {
	noop := func() {}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Warn("mcp fixture unavailable; INJECTION mcp cases will skip", "error", err)
		return noop, false
	}
	srv := &http.Server{Handler: testserver.Handler(testserver.Options{PoisonedDescription: true})}
	go func() { _ = srv.Serve(ln) }()
	shutdown := func() { _ = srv.Close() }

	ts, closer, err := mcp.Connect(ctx, mcp.Config{Servers: []mcp.ServerConfig{{
		Name:      testserver.DefaultName,
		Transport: mcp.TransportHTTP,
		URL:       "http://" + ln.Addr().String(),
	}}}, log)
	if err != nil {
		log.Warn("mcp fixture unavailable; INJECTION mcp cases will skip", "error", err)
		shutdown()
		return noop, false
	}
	registerMCP(reg, ts, log)
	return func() { _ = closer.Close(); shutdown() }, len(ts) > 0
}

func parseOnly(s string) map[string]bool {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := map[string]bool{}
	for _, id := range strings.Split(s, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
	return out
}

func countCases(suites []*evals.Suite) int {
	n := 0
	for _, s := range suites {
		n += len(s.Cases)
	}
	return n
}

// mergeThresholds flattens every suite's thresholds into one map. Cases of one
// category are scored together whichever file they came from, so their
// thresholds have to be too; a later file overriding an earlier one is the
// same rule Go's flag package uses and is the one nobody is surprised by.
func mergeThresholds(suites []*evals.Suite) map[string]map[string]float64 {
	out := map[string]map[string]float64{}
	for _, s := range suites {
		for cat, metrics := range s.Thresholds {
			if out[cat] == nil {
				out[cat] = map[string]float64{}
			}
			for k, v := range metrics {
				out[cat][k] = v
			}
		}
	}
	return out
}

// writeEvalResults saves the scorecard next to what produced it, the way
// evals/retrieval/results.json already does, so a table in a README can be
// traced back to a run rather than taken on trust.
func writeEvalResults(path string, rep *evals.Report, embedModel, cassetteDir string) error {
	payload := struct {
		*evals.Report
		EmbeddingModel string    `json:"embedding_model"`
		Cassettes      string    `json:"cassettes,omitempty"`
		GeneratedAt    time.Time `json:"generated_at"`
	}{rep, embedModel, cassetteDir, time.Now().UTC()}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}
