package evals

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/shreyasprasad/agentd/internal/retrieval"
	"github.com/shreyasprasad/agentd/internal/retrieval/embed"
	"github.com/shreyasprasad/agentd/internal/store"
)

// Prepare ingests every suite's corpus, once, before any case runs.
//
// It runs against the eval database rather than the one the demos use, and
// that separation is not tidiness (Decision 2 of docs/plans/m5.md). Two
// reasons, of which the second is the load-bearing one:
//
//   - The suite ingests with a deterministic fake embedder, and two
//     embedding models under one HNSW index rank wrongly and silently.
//   - The injection set is documents that say "disregard your instructions".
//     Landing them in the corpus `make demo-legal` searches would leave one
//     waiting to surface in a screenshot.
//
// The first of those is checked here rather than assumed, because a corpus
// mixed by accident produces a scorecard rather than an error.
func Prepare(ctx context.Context, st *store.Store, emb embed.Embedder, suites []*Suite, log *slog.Logger) error {
	files := corpusFiles(suites)
	if len(files) == 0 {
		return nil
	}
	if err := checkEmbeddingModel(ctx, st, emb); err != nil {
		return err
	}

	var docs []retrieval.InputDoc
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("corpus %s: %w", path, err)
		}
		batch, err := retrieval.ReadJSONL(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("corpus %s: %w", path, err)
		}
		docs = append(docs, batch...)
	}

	sum, err := retrieval.Ingest(ctx, st, emb, docs, retrieval.IngestOptions{Log: log})
	if err != nil {
		return err
	}
	if sum.Failed > 0 {
		return fmt.Errorf("%d of %d corpus documents failed to ingest", sum.Failed, sum.Seen)
	}
	log.Info("eval corpus ready", "files", len(files), "documents", sum.Seen,
		"ingested", sum.Ingested, "unchanged", sum.Skipped, "chunks", sum.Chunks, "embed_model", emb.Model())
	return nil
}

// HasCorpus reports whether anything is ingested, which is what the `corpus`
// capability means for a case that needs to search.
func HasCorpus(ctx context.Context, st *store.Store) bool {
	stats, err := st.CorpusStats(ctx)
	return err == nil && stats.Chunks > 0
}

func corpusFiles(suites []*Suite) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range suites {
		for _, f := range s.CorpusFiles() {
			if seen[f] {
				continue
			}
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// checkEmbeddingModel refuses to run against a corpus embedded by a different
// model, with the fix in the message. Vector search over a mixed corpus is not
// wrong loudly; it is wrong quietly, which is worse for a tool whose whole job
// is to tell you when something is wrong.
func checkEmbeddingModel(ctx context.Context, st *store.Store, emb embed.Embedder) error {
	stats, err := st.CorpusStats(ctx)
	if err != nil {
		return err
	}
	var foreign []string
	for _, m := range stats.EmbeddingModels {
		if m != "" && m != emb.Model() {
			foreign = append(foreign, m)
		}
	}
	if len(foreign) == 0 {
		return nil
	}
	return fmt.Errorf(
		"this database holds chunks embedded by %s and the suite embeds with %s; "+
			"one index over two embedding models ranks wrongly and silently. "+
			"Point -dsn at the eval database (the default) or re-ingest it",
		strings.Join(foreign, ", "), emb.Model())
}
