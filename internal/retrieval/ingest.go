package retrieval

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/shreyasprasad/agentd/internal/retrieval/chunk"
	"github.com/shreyasprasad/agentd/internal/retrieval/embed"
	"github.com/shreyasprasad/agentd/internal/store"
)

// InputDoc is one line of the fetch JSONL. Ingest reads only this format, so
// the corpus source is swappable (another court, the bulk CSVs, a synthetic
// poisoned set in M5) without touching the pipeline.
type InputDoc struct {
	SourceID  string          `json:"source_id"`
	Title     string          `json:"title"`
	Court     string          `json:"court,omitempty"`
	DecidedOn string          `json:"decided_on,omitempty"` // YYYY-MM-DD
	Text      string          `json:"text"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

// ReadJSONL parses one InputDoc per line, skipping blank lines.
func ReadJSONL(r io.Reader) ([]InputDoc, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20) // opinions can be long lines
	var out []InputDoc
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var d InputDoc
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if d.SourceID == "" {
			return nil, fmt.Errorf("line %d: missing source_id", line)
		}
		out = append(out, d)
	}
	return out, sc.Err()
}

// IngestOptions tunes an ingest run.
type IngestOptions struct {
	// Force re-chunks and re-embeds documents whose hash already matches.
	Force bool
	// Concurrency is documents embedded at once (default 2; Ollama
	// serialises embedding calls anyway, more only adds queueing).
	Concurrency int
	// ChunkOptions defaults to chunk.DefaultOptions.
	ChunkOptions chunk.Options
	Log          *slog.Logger
}

// IngestSummary is what an ingest run did.
type IngestSummary struct {
	Seen     int           `json:"seen"`
	Skipped  int           `json:"skipped"`
	Ingested int           `json:"ingested"`
	Failed   int           `json:"failed"`
	Chunks   int           `json:"chunks"`
	Elapsed  time.Duration `json:"elapsed"`
}

// Ingest chunks, embeds, and upserts docs. Per document it is atomic (one
// transaction in the store), so a crash mid-corpus leaves completed documents
// whole and unstarted ones absent; re-running skips what is already there.
// A document is skipped when its content hash, chunk count, and embedding
// model all match what is stored.
func Ingest(ctx context.Context, st *store.Store, emb embed.Embedder, docs []InputDoc, opt IngestOptions) (IngestSummary, error) {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	if opt.Concurrency <= 0 {
		opt.Concurrency = 2
	}
	if opt.ChunkOptions.Max <= 0 {
		opt.ChunkOptions = chunk.DefaultOptions()
	}

	start := time.Now()
	var mu sync.Mutex
	sum := IngestSummary{Seen: len(docs)}

	var wg sync.WaitGroup
	sem := make(chan struct{}, opt.Concurrency)
	for _, doc := range docs {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(doc InputDoc) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			n, err := ingestOne(ctx, st, emb, doc, opt)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, errSkipped):
				sum.Skipped++
			case err != nil:
				sum.Failed++
				log.Warn("ingest failed", "source_id", doc.SourceID, "error", err)
			default:
				sum.Ingested++
				sum.Chunks += n
			}
		}(doc)
	}
	wg.Wait()
	if ctx.Err() != nil {
		sum.Elapsed = time.Since(start)
		return sum, ctx.Err()
	}
	// Lexical ranking reads corpus statistics that only a refresh updates.
	// It runs even when every document was skipped, so a run interrupted
	// before this point is repaired by the next one rather than leaving
	// stale statistics that no later no-op ingest would fix.
	if err := st.RefreshLexicalStats(ctx); err != nil {
		sum.Elapsed = time.Since(start)
		return sum, fmt.Errorf("refresh lexical stats: %w", err)
	}
	sum.Elapsed = time.Since(start)
	return sum, nil
}

// errSkipped marks a document already ingested in this exact form.
var errSkipped = errors.New("skipped")

func ingestOne(ctx context.Context, st *store.Store, emb embed.Embedder, doc InputDoc, opt IngestOptions) (int, error) {
	if doc.Text == "" {
		return 0, errSkipped
	}
	sha := sha256.Sum256([]byte(doc.Text))
	contentSHA := hex.EncodeToString(sha[:])

	chunks := chunk.SplitWith(doc.Text, opt.ChunkOptions)
	if len(chunks) == 0 {
		return 0, errSkipped
	}

	if !opt.Force {
		existing, err := st.GetDocumentBySourceID(ctx, doc.SourceID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return 0, err
		}
		if existing != nil && existing.ContentSHA256 == contentSHA && existing.ChunkCount == len(chunks) {
			// The hash and count match; check the vectors are from the same
			// model before deciding nothing needs doing.
			first, err := st.ListChunks(ctx, existing.ID, 0, 0)
			if err != nil {
				return 0, err
			}
			if len(first) == 1 && first[0].EmbeddingModel == emb.Model() {
				return 0, errSkipped
			}
		}
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
	}
	vecs, err := emb.Embed(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("embed: %w", err)
	}

	var decided *time.Time
	if doc.DecidedOn != "" {
		d, err := time.Parse("2006-01-02", doc.DecidedOn)
		if err != nil {
			return 0, fmt.Errorf("decided_on: %w", err)
		}
		decided = &d
	}

	rows := make([]store.Chunk, len(chunks))
	for i, c := range chunks {
		rows[i] = store.Chunk{
			Ordinal:        c.Ordinal,
			Section:        c.Section,
			Content:        c.Content,
			CharStart:      c.CharStart,
			CharEnd:        c.CharEnd,
			EmbeddingModel: emb.Model(),
			Embedding:      vecs[i],
		}
	}
	_, err = st.IngestDocument(ctx, store.Document{
		SourceID:      doc.SourceID,
		Title:         doc.Title,
		Court:         doc.Court,
		DecidedOn:     decided,
		Metadata:      doc.Metadata,
		ContentSHA256: contentSHA,
	}, rows)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}
