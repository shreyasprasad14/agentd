package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shreyasprasad/agentd/internal/model/anthropic"
	"github.com/shreyasprasad/agentd/internal/model/local"
	"github.com/shreyasprasad/agentd/internal/retrieval"
	"github.com/shreyasprasad/agentd/internal/retrieval/courtlistener"
	"github.com/shreyasprasad/agentd/internal/retrieval/embed"
	"github.com/shreyasprasad/agentd/internal/retrieval/eval"
	"github.com/shreyasprasad/agentd/internal/retrieval/rerank"
	"github.com/shreyasprasad/agentd/internal/store"
)

const defaultEmbedModel = "mxbai-embed-large"

// embedFlags are shared by work, ingest, and eval.
type embedFlags struct {
	url   *string
	model *string
}

func addEmbedFlags(fs *flag.FlagSet) embedFlags {
	return embedFlags{
		url: fs.String("embed-url", os.Getenv("AGENTD_EMBED_URL"),
			"OpenAI-compatible embeddings base URL (default: the model URL)"),
		model: fs.String("embed-model", envOr("AGENTD_EMBED_MODEL", defaultEmbedModel),
			"embedding model; its width must match the schema's VECTOR(1024)"),
	}
}

// buildEmbedder wires the embeddings client. modelURL is the fallback base
// URL: Ollama serves chat and embeddings from the same port.
func (f embedFlags) buildEmbedder(modelURL string) *embed.Client {
	url := *f.url
	if url == "" {
		url = modelURL
	}
	return embed.New(embed.Config{BaseURL: url, Model: *f.model, Dimensions: store.EmbeddingDimensions})
}

// probeEmbedder embeds one string at boot. Unreachable is a warning, like
// the sandbox: runs that never search must keep working, and search_corpus
// calls fail retryably until the embedder is back. A wrong width is fatal,
// because it would otherwise surface as an insert error mid-run.
func probeEmbedder(ctx context.Context, e *embed.Client, log *slog.Logger) error {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	vecs, err := e.Embed(probeCtx, []string{"agentd boot probe"})
	if err != nil {
		log.Warn("embedder unreachable; search_corpus will fail until it is", "model", e.Model(), "error", err)
		return nil
	}
	if len(vecs) != 1 || len(vecs[0]) != store.EmbeddingDimensions {
		return fmt.Errorf("embedding model %s returns width %d, schema needs %d; use a %d-dimension model or re-migrate",
			e.Model(), len(vecs[0]), store.EmbeddingDimensions, store.EmbeddingDimensions)
	}
	log.Info("embedder ready", "model", e.Model(), "dimensions", store.EmbeddingDimensions)
	return nil
}

// rerankFlags are shared by work and eval.
type rerankFlags struct {
	on         *bool
	model      *string
	candidates *int
}

func addRerankFlags(fs *flag.FlagSet) rerankFlags {
	return rerankFlags{
		on:         fs.Bool("rerank", envOr("AGENTD_RERANK", "on") != "off", "LLM-rerank fused search results (-rerank=false to disable)"),
		model:      fs.String("rerank-model", os.Getenv("AGENTD_RERANK_MODEL"), "model for the LLM reranker (default: the worker model; must route to the local provider)"),
		candidates: fs.Int("rerank-candidates", 50, "fused candidates to rerank per query"),
	}
}

// buildReranker wires the LLM reranker on the local provider. A hosted
// rerank model is still refused, but not because the spend would be
// invisible: tool-internal model cost now lands on tool_succeeded and in
// spent_usd, so a paid reranker would be measured and bounded like any
// other call (ADR-23).
//
// The refusal is a price decision instead. Measured, a hosted pass runs
// about 11,000 input and 500 output tokens per search — roughly $0.04 at
// Sonnet-class rates, or ~20% on top of a six-step research run, so three
// searches is closer to +65%. That is affordable and was still declined, in
// exchange for runs that are cheap by construction and a restriction that
// needs no per-run reasoning. What it gives up is the latency win: M3
// measures the local reranker at 10–30s per search, the slowest step in a
// research run, where a hosted model running four batches at once would
// finish in seconds. Revisit if search latency becomes the complaint.
func (f rerankFlags) buildReranker(onBox *local.Provider, workerModel string, log *slog.Logger) (rerank.Reranker, string, error) {
	if !*f.on {
		return nil, "", nil
	}
	name := *f.model
	if name == "" {
		name = workerModel
	}
	if anthropic.IsClaudeModel(name) || strings.HasPrefix(name, "anthropic/") {
		return nil, "", fmt.Errorf("rerank model %q routes to Anthropic; the reranker is local-only by policy — a hosted pass measures at about $0.04 per search, which was declined to keep runs cheap by construction (set -rerank-model to a local model, or -rerank=false)", name)
	}
	name = strings.TrimPrefix(name, "local/")
	return rerank.NewLLM(onBox, name, log), name, nil
}

// fetchCmd pulls opinions from CourtListener into a JSONL file, resuming
// from whatever the file already holds.
func fetchCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	court := fs.String("court", "scotus", "CourtListener court id")
	filedAfter := fs.String("filed-after", "2010-01-01", "only opinions filed on or after this date")
	limit := fs.Int("limit", 2500, "stop after this many opinions (0 = no cap)")
	out := fs.String("out", "", "output JSONL path (default data/corpus/<court>.jsonl)")
	baseURL := fs.String("base-url", envOr("COURTLISTENER_URL", courtlistener.DefaultBaseURL), "API root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := newLogger("fetch")
	path := *out
	if path == "" {
		path = filepath.Join("data", "corpus", *court+".jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	// Resume: skip opinions already in the output file.
	skip := map[string]bool{}
	if f, err := os.Open(path); err == nil {
		docs, err := retrieval.ReadJSONL(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s exists but is not readable JSONL: %w", path, err)
		}
		for _, d := range docs {
			skip[d.SourceID] = true
		}
		if len(skip) > 0 {
			log.Info("resuming fetch", "already_have", len(skip), "file", path)
		}
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	token := os.Getenv("COURTLISTENER_TOKEN")
	if token == "" {
		log.Warn("COURTLISTENER_TOKEN not set; anonymous rate limits are much lower")
	}
	client := courtlistener.New(*baseURL, token, log)
	enc := json.NewEncoder(file)
	n := 0
	sum, err := client.Fetch(ctx, courtlistener.FetchOptions{
		Court: *court, FiledAfter: *filedAfter, Limit: *limit, Skip: skip,
	}, func(d retrieval.InputDoc) error {
		n++
		if n%50 == 0 {
			log.Info("fetching", "emitted", n)
		}
		return enc.Encode(d)
	})
	log.Info("fetch done", "emitted", sum.Emitted, "skipped_existing", sum.SkippedExisting,
		"skipped_empty", sum.SkippedEmpty, "skipped_no_lead", sum.SkippedNoLead, "file", path)
	return err
}

// dedupeCmd collapses revisions of the same case in a fetched JSONL file,
// writing a new file rather than editing one in place: the displaced ids stay
// recoverable from the input, and the corpus a measurement ran against stays
// on disk next to the one it was derived from. It runs between fetch and
// ingest because deduplication is corpus curation and ingest is a pipeline —
// see ADR-39 and the package comment on retrieval.Dedupe.
func dedupeCmd(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("dedupe", flag.ExitOnError)
	in := fs.String("in", "", "input JSONL from `agentd fetch` (required)")
	out := fs.String("out", "", "output JSONL path (default <in>-dedup.jsonl)")
	key := fs.String("key", "docket_number", "metadata field identifying the case; documents lacking it group by normalized title")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" {
		return fmt.Errorf("-in is required")
	}
	outPath := *out
	if outPath == "" {
		outPath = strings.TrimSuffix(*in, ".jsonl") + "-dedup.jsonl"
	}
	if outPath == *in {
		return fmt.Errorf("-out must differ from -in: dedupe never edits a corpus in place")
	}
	log := newLogger("dedupe")

	f, err := os.Open(*in)
	if err != nil {
		return err
	}
	docs, err := retrieval.ReadJSONL(f)
	f.Close()
	if err != nil {
		return err
	}

	kept, sum := retrieval.Dedupe(docs, retrieval.DedupeOptions{MetadataKey: *key})

	// Write to a temporary file and rename, so an interrupted run cannot leave
	// a half-written corpus that looks complete to the next ingest.
	tmp := outPath + ".tmp"
	wf, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(wf)
	for _, d := range kept {
		if err := enc.Encode(d); err != nil {
			wf.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := wf.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, outPath); err != nil {
		os.Remove(tmp)
		return err
	}

	log.Info("dedupe done", "in", sum.In, "out", sum.Out, "dropped", sum.Dropped,
		"collapsed_groups", sum.Collapsed, "by_key", sum.ByKey, "by_title", sum.ByTitle,
		"key", *key, "file", outPath)
	return nil
}

// ingestCmd chunks, embeds, and upserts a fetched JSONL file.
func ingestCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	dsn := fs.String("dsn", envOr("AGENTD_DSN", defaultDSN), "Postgres DSN")
	file := fs.String("file", "", "input JSONL from `agentd fetch` (required)")
	modelURL := fs.String("model-url", envOr("AGENTD_MODEL_URL", defaultModelURL), "fallback base URL for -embed-url")
	ef := addEmbedFlags(fs)
	force := fs.Bool("force", false, "re-chunk and re-embed documents whose content hash matches")
	concurrency := fs.Int("concurrency", 2, "documents embedded at once")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("-file is required")
	}
	log := newLogger("ingest")

	f, err := os.Open(*file)
	if err != nil {
		return err
	}
	docs, err := retrieval.ReadJSONL(f)
	f.Close()
	if err != nil {
		return err
	}

	embedder := ef.buildEmbedder(*modelURL)
	if err := embed.Probe(ctx, embedder); err != nil {
		return fmt.Errorf("embedder: %w (is the embedding model pulled? `ollama pull %s`)", err, embedder.Model())
	}
	st, err := open(ctx, *dsn, log, true)
	if err != nil {
		return err
	}
	defer st.Close()

	sum, err := retrieval.Ingest(ctx, st, embedder, docs, retrieval.IngestOptions{
		Force: *force, Concurrency: *concurrency, Log: log,
	})
	if err != nil {
		return err
	}
	perSec := 0.0
	if sum.Elapsed > 0 {
		perSec = float64(sum.Chunks) / sum.Elapsed.Seconds()
	}
	log.Info("ingest done", "seen", sum.Seen, "ingested", sum.Ingested, "skipped", sum.Skipped,
		"failed", sum.Failed, "chunks", sum.Chunks, "elapsed", sum.Elapsed.Round(time.Millisecond),
		"chunks_per_sec", fmt.Sprintf("%.1f", perSec), "embed_model", embedder.Model())
	if sum.Failed > 0 {
		return fmt.Errorf("%d documents failed to ingest", sum.Failed)
	}
	return nil
}

// evalCmd dispatches `agentd eval <suite>`. The two suites measure different
// things and share no code: retrieval is scored per query against hand labels,
// agent runs are scored per trajectory against the event log (spec §12).
func evalCmd(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: agentd eval <retrieval|suite|record> [flags]")
	}
	switch args[0] {
	case "retrieval":
		return evalRetrievalCmd(ctx, args[1:])
	case "suite":
		return evalSuiteCmd(ctx, args[1:], false)
	case "record":
		return evalSuiteCmd(ctx, args[1:], true)
	default:
		return fmt.Errorf("unknown eval suite %q (want retrieval, suite, or record)", args[0])
	}
}

func evalRetrievalCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval retrieval", flag.ExitOnError)
	dsn := fs.String("dsn", envOr("AGENTD_DSN", defaultDSN), "Postgres DSN")
	labelsPath := fs.String("labels", "evals/retrieval/labels.yaml", "label file")
	resultsPath := fs.String("results", "evals/retrieval/results.json", "where to write machine-readable results")
	modelURL := fs.String("model-url", envOr("AGENTD_MODEL_URL", defaultModelURL), "local model base URL (reranker and -embed-url fallback)")
	modelTimeout := fs.Duration("model-timeout", 10*time.Minute, "per-call model timeout")
	modes := fs.String("modes", "", "comma-separated subset of vector,bm25,hybrid,hybrid+rerank (default all)")
	labelQuery := fs.String("label", "", "labeling helper: print the top 20 hybrid hits for this query and exit")
	ef := addEmbedFlags(fs)
	rf := addRerankFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := newLogger("eval")

	embedder := ef.buildEmbedder(*modelURL)
	if err := embed.Probe(ctx, embedder); err != nil {
		return fmt.Errorf("embedder: %w", err)
	}
	st, err := open(ctx, *dsn, log, true)
	if err != nil {
		return err
	}
	defer st.Close()

	searcher := &retrieval.Searcher{Store: st, Embedder: embedder, RerankCandidates: *rf.candidates, Log: log}

	if *labelQuery != "" {
		return labelHelper(ctx, searcher, *labelQuery)
	}

	stats, err := st.CorpusStats(ctx)
	if err != nil {
		return err
	}
	if stats.Chunks == 0 {
		return fmt.Errorf("the corpus is empty; run `agentd ingest` first")
	}

	f, err := os.Open(*labelsPath)
	if err != nil {
		return err
	}
	labels, err := eval.ReadLabels(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", *labelsPath, err)
	}

	runModes, err := parseModes(*modes)
	if err != nil {
		return err
	}
	rerankModel := ""
	if includes(runModes, retrieval.ModeHybridRerank) {
		onBox := local.New(local.Config{BaseURL: *modelURL, APIKey: os.Getenv("AGENTD_MODEL_API_KEY"), Timeout: *modelTimeout})
		searcher.Reranker, rerankModel, err = rf.buildReranker(onBox, envOr("AGENTD_MODEL", defaultModel), log)
		if err != nil {
			return err
		}
		if searcher.Reranker == nil {
			return fmt.Errorf("the hybrid+rerank mode needs the reranker; drop it from -modes or remove -rerank=false")
		}
	}

	log.Info("evaluating", "cases", len(labels.Cases), "modes", runModes,
		"documents", stats.Documents, "chunks", stats.Chunks)
	rep, err := eval.Run(ctx, searcher, labels, runModes)
	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Print(eval.RenderTable(rep))
	fmt.Println()

	results := struct {
		*eval.Report
		Corpus         store.CorpusStats `json:"corpus"`
		EmbeddingModel string            `json:"embedding_model"`
		RerankModel    string            `json:"rerank_model,omitempty"`
		Labels         string            `json:"labels"`
		GeneratedAt    time.Time         `json:"generated_at"`
	}{rep, stats, embedder.Model(), rerankModel, *labelsPath, time.Now().UTC()}
	raw, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*resultsPath, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	log.Info("results written", "path", *resultsPath)

	if len(rep.Failures) > 0 {
		return fmt.Errorf("thresholds missed:\n  %s", strings.Join(rep.Failures, "\n  "))
	}
	return nil
}

// labelHelper prints the top 20 hybrid hits for one query so a person can
// mark the relevant ones and paste them into labels.yaml.
func labelHelper(ctx context.Context, s *retrieval.Searcher, query string) error {
	res, err := s.Search(ctx, retrieval.Query{Text: query, K: 20, Mode: retrieval.ModeHybrid})
	if err != nil {
		return err
	}
	fmt.Printf("query: %q  (%d candidates)\n\n", query, res.CandidatesConsidered)
	for i, h := range res.Hits {
		snippet := strings.Join(strings.Fields(h.Content), " ")
		if len(snippet) > 140 {
			snippet = snippet[:140] + "…"
		}
		fmt.Printf("%2d. %-14s ¶%-3d %s\n    %s\n", i+1, h.SourceID, h.Ordinal, h.Title, snippet)
	}
	fmt.Printf("\nlabel entry:\n  - query: %q\n    relevant:\n      - {source_id: \"...\", ordinals: [..]}   # ordinals optional\n", query)
	return nil
}

func parseModes(s string) ([]retrieval.Mode, error) {
	if s == "" {
		return eval.AllModes, nil
	}
	var out []retrieval.Mode
	for _, part := range strings.Split(s, ",") {
		m := retrieval.Mode(strings.TrimSpace(part))
		if !includes(eval.AllModes, m) {
			return nil, fmt.Errorf("unknown mode %q (want vector, bm25, hybrid, hybrid+rerank)", part)
		}
		out = append(out, m)
	}
	return out, nil
}

func includes(modes []retrieval.Mode, m retrieval.Mode) bool {
	for _, x := range modes {
		if x == m {
			return true
		}
	}
	return false
}
