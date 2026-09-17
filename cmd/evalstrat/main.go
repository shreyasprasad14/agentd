// Command evalstrat is a one-off helper for scoring the draft stratified label
// set (evals/retrieval/labels-scotus-cap-stratified.yaml) with the existing
// retrieval eval. It lives outside the retrieval packages on purpose: nothing
// here changes how retrieval runs or how it is scored.
//
//	evalstrat convert   paragraph labels -> chunk-ordinal label files the scorer reads
//	evalstrat percase   run every case once per mode and record per-case ranks and hits
//
// Run from the repository root.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"

	"github.com/shreyasprasad/agentd/internal/model/local"
	"github.com/shreyasprasad/agentd/internal/retrieval"
	"github.com/shreyasprasad/agentd/internal/retrieval/embed"
	"github.com/shreyasprasad/agentd/internal/retrieval/eval"
	"github.com/shreyasprasad/agentd/internal/retrieval/rerank"
	"github.com/shreyasprasad/agentd/internal/store"
)

const defaultDSN = "postgres://agentd:agentd@localhost:5432/agentd?sslmode=disable"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: evalstrat <convert|percase> [flags]")
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "convert":
		err = convertCmd(ctx, os.Args[2:])
	case "percase":
		err = percaseCmd(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "evalstrat:", err)
		os.Exit(1)
	}
}

// Source format, mirroring stratified_labels_test.go.
type stratifiedFile struct {
	Thresholds map[string]map[string]float64 `yaml:"thresholds"`
	Cases      []stratifiedCase              `yaml:"cases"`
}

type stratifiedCase struct {
	Query       string               `yaml:"query"`
	Category    string               `yaml:"category"`
	Relevant    []stratifiedRelevant `yaml:"relevant"`
	LabelStatus string               `yaml:"label_status"`
	Notes       string               `yaml:"notes"`
}

type stratifiedRelevant struct {
	SourceID         string `yaml:"source_id"`
	ParagraphOrdinal *int   `yaml:"paragraph_ordinal"`
	Evidence         string `yaml:"evidence"`
}

var categories = []string{"paraphrase", "citation", "case_name", "term_of_art", "mixed"}

var spaceRun = regexp.MustCompile(`\s+`)

func normalizeSpace(s string) string { return strings.TrimSpace(spaceRun.ReplaceAllString(s, " ")) }

// paragraphSpans is the label file's paragraph definition (text split on
// "\n", blank lines skipped — the same as paragraphs() in
// stratified_labels_test.go), returned as byte spans into text.
func paragraphSpans(text string) [][2]int {
	var out [][2]int
	pos := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, [2]int{pos, pos + len(line)})
		}
		pos += len(line) + 1
	}
	return out
}

type dbChunk struct {
	Ordinal   int
	CharStart int
	CharEnd   int
	Content   string
}

// Sidecar written next to the label files: what each generated relevant
// entry was derived from. The report reads it; the scorer does not.
type caseInfo struct {
	Index       int         `json:"index"`
	Query       string      `json:"query"`
	Category    string      `json:"category"`
	LabelStatus string      `json:"label_status,omitempty"`
	Notes       string      `json:"notes,omitempty"`
	Labels      []labelInfo `json:"labels"`
}

type labelInfo struct {
	SourceID         string `json:"source_id"`
	ParagraphOrdinal *int   `json:"paragraph_ordinal,omitempty"`
	ParagraphStart   int    `json:"paragraph_byte_start,omitempty"`
	ParagraphEnd     int    `json:"paragraph_byte_end,omitempty"`
	ChunkOrdinals    []int  `json:"chunk_ordinals,omitempty"`
	Evidence         string `json:"evidence,omitempty"`
}

type checkReport struct {
	GeneratedAt         time.Time `json:"generated_at"`
	Source              string    `json:"source"`
	Corpus              string    `json:"corpus"`
	Cases               int       `json:"cases"`
	DocumentsReferenced int       `json:"documents_referenced"`
	ParagraphLabels     int       `json:"paragraph_labels"`
	DocumentLabels      int       `json:"document_labels"`
	SHAChecked          int       `json:"sha_checked"`
	SHAMismatches       []string  `json:"sha_mismatches"`
	MissingDocuments    []string  `json:"missing_documents"`
	OrdinalOutOfRange   []string  `json:"ordinal_out_of_range"`
	NoChunkMapped       []string  `json:"no_chunk_mapped"`
	EvidenceNotFound    []string  `json:"evidence_not_found_in_mapped_chunks"`
	// EvidenceNotInParagraph repeats stratified_labels_test.go's check, to
	// separate a bad ordinal from a chunk-mapping problem.
	EvidenceNotInParagraph []string    `json:"evidence_not_in_paragraph"`
	ChunksPerLabel         map[int]int `json:"chunks_per_paragraph_label_histogram"`
}

func (r *checkReport) failures() int {
	return len(r.SHAMismatches) + len(r.MissingDocuments) + len(r.OrdinalOutOfRange) + len(r.NoChunkMapped) + len(r.EvidenceNotFound)
}

func convertCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("convert", flag.ExitOnError)
	src := fs.String("labels", "evals/retrieval/labels-scotus-cap-stratified.yaml", "stratified label file")
	corpus := fs.String("corpus", "data/corpus/scotus-cap-dedup.jsonl", "corpus JSONL the labels were written against")
	dsn := fs.String("dsn", envOr("AGENTD_DSN", defaultDSN), "Postgres DSN of the ingested corpus")
	outDir := fs.String("out", "evals/retrieval/generated", "output directory")
	maxFailures := fs.Int("max-failures", 3, "refuse to write label files above this many mapping failures")
	if err := fs.Parse(args); err != nil {
		return err
	}
	command := "go run ./cmd/evalstrat convert " + strings.Join(args, " ")

	f, err := os.Open(*src)
	if err != nil {
		return err
	}
	var sf stratifiedFile
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	err = dec.Decode(&sf)
	f.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", *src, err)
	}

	wanted := map[string]bool{}
	for _, c := range sf.Cases {
		for _, r := range c.Relevant {
			wanted[r.SourceID] = true
		}
	}
	ids := make([]string, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	shas := map[string]string{}
	chunks := map[string][]dbChunk{}
	rows, err := conn.Query(ctx, `
		SELECT d.source_id, d.content_sha256, c.ordinal, c.char_start, c.char_end, c.content
		FROM documents d JOIN chunks c ON c.document_id = d.id
		WHERE d.source_id = ANY($1)
		ORDER BY d.source_id, c.ordinal`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, sha string
		var ch dbChunk
		if err := rows.Scan(&id, &sha, &ch.Ordinal, &ch.CharStart, &ch.CharEnd, &ch.Content); err != nil {
			return err
		}
		shas[id] = sha
		chunks[id] = append(chunks[id], ch)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	jf, err := os.Open(*corpus)
	if err != nil {
		return err
	}
	docs, err := retrieval.ReadJSONL(jf)
	jf.Close()
	if err != nil {
		return err
	}
	texts := map[string]string{}
	for _, d := range docs {
		if wanted[d.SourceID] {
			texts[d.SourceID] = d.Text
		}
	}

	rep := &checkReport{
		GeneratedAt: time.Now().UTC(), Source: *src, Corpus: *corpus, Cases: len(sf.Cases),
		DocumentsReferenced: len(ids), ChunksPerLabel: map[int]int{},
		SHAMismatches: []string{}, MissingDocuments: []string{}, OrdinalOutOfRange: []string{},
		NoChunkMapped: []string{}, EvidenceNotFound: []string{}, EvidenceNotInParagraph: []string{},
	}
	for _, id := range ids {
		text, inJSONL := texts[id]
		_, inDB := shas[id]
		if !inJSONL || !inDB {
			rep.MissingDocuments = append(rep.MissingDocuments, fmt.Sprintf("%s (jsonl=%v db=%v)", id, inJSONL, inDB))
			continue
		}
		sum := sha256.Sum256([]byte(text))
		rep.SHAChecked++
		if got := hex.EncodeToString(sum[:]); got != shas[id] {
			rep.SHAMismatches = append(rep.SHAMismatches, fmt.Sprintf("%s jsonl=%s db=%s", id, got, shas[id]))
		}
	}

	infos := make([]caseInfo, len(sf.Cases))
	for i, c := range sf.Cases {
		info := caseInfo{Index: i, Query: c.Query, Category: c.Category, LabelStatus: c.LabelStatus, Notes: c.Notes}
		for _, r := range c.Relevant {
			li := labelInfo{SourceID: r.SourceID, ParagraphOrdinal: r.ParagraphOrdinal, Evidence: r.Evidence}
			if r.ParagraphOrdinal == nil {
				rep.DocumentLabels++
				info.Labels = append(info.Labels, li)
				continue
			}
			rep.ParagraphLabels++
			name := fmt.Sprintf("case %d %s ¶%d", i, r.SourceID, *r.ParagraphOrdinal)
			text, ok := texts[r.SourceID]
			if !ok {
				info.Labels = append(info.Labels, li)
				continue
			}
			spans := paragraphSpans(text)
			if *r.ParagraphOrdinal >= len(spans) {
				rep.OrdinalOutOfRange = append(rep.OrdinalOutOfRange, fmt.Sprintf("%s (document has %d)", name, len(spans)))
				info.Labels = append(info.Labels, li)
				continue
			}
			sp := spans[*r.ParagraphOrdinal]
			li.ParagraphStart, li.ParagraphEnd = sp[0], sp[1]
			if !strings.Contains(normalizeSpace(text[sp[0]:sp[1]]), normalizeSpace(r.Evidence)) {
				rep.EvidenceNotInParagraph = append(rep.EvidenceNotInParagraph, fmt.Sprintf("%s: %q", name, r.Evidence))
			}
			var content strings.Builder
			for _, ch := range chunks[r.SourceID] {
				// Half-open overlap: [char_start, char_end) ∩ [start, end) ≠ ∅.
				if ch.CharStart < sp[1] && sp[0] < ch.CharEnd {
					li.ChunkOrdinals = append(li.ChunkOrdinals, ch.Ordinal)
					content.WriteString(ch.Content)
					content.WriteString(" ")
				}
			}
			rep.ChunksPerLabel[len(li.ChunkOrdinals)]++
			if len(li.ChunkOrdinals) == 0 {
				rep.NoChunkMapped = append(rep.NoChunkMapped, name)
			} else if !strings.Contains(normalizeSpace(content.String()), normalizeSpace(r.Evidence)) {
				rep.EvidenceNotFound = append(rep.EvidenceNotFound, fmt.Sprintf("%s chunks %v: %q", name, li.ChunkOrdinals, r.Evidence))
			}
			info.Labels = append(info.Labels, li)
		}
		infos[i] = info
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "mapping-checks.json"), append(raw, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("cases=%d docs=%d paragraph_labels=%d document_labels=%d sha_checked=%d\n",
		rep.Cases, rep.DocumentsReferenced, rep.ParagraphLabels, rep.DocumentLabels, rep.SHAChecked)
	fmt.Printf("sha_mismatches=%d missing_documents=%d out_of_range=%d no_chunk=%d evidence_not_in_chunks=%d evidence_not_in_paragraph=%d\n",
		len(rep.SHAMismatches), len(rep.MissingDocuments), len(rep.OrdinalOutOfRange), len(rep.NoChunkMapped), len(rep.EvidenceNotFound), len(rep.EvidenceNotInParagraph))
	for _, l := range [][]string{rep.SHAMismatches, rep.MissingDocuments, rep.OrdinalOutOfRange, rep.NoChunkMapped, rep.EvidenceNotFound, rep.EvidenceNotInParagraph} {
		for _, s := range l {
			fmt.Println("  FAIL", s)
		}
	}
	fmt.Printf("chunks per paragraph label: %v\n", rep.ChunksPerLabel)
	if n := rep.failures(); n > *maxFailures {
		return fmt.Errorf("%d mapping failures (limit %d); not writing label files", n, *maxFailures)
	}

	header := func(desc string) string {
		return fmt.Sprintf(`# GENERATED FILE — do not edit by hand.
#
# %s
#
# Source:  %s (all labels are unreviewed drafts; see that file's header)
# Corpus:  %s, chunk offsets from the agentd database
# Command: %s
#
# Paragraph-level labels were mapped to every chunk whose [char_start, char_end)
# overlaps the paragraph's byte span; each paragraph label is one relevant
# entry, so recall counts paragraphs, not chunks. Document-level labels keep
# source_id only. No thresholds: this file reports numbers, it does not gate.
# Mapping checks: mapping-checks.json in this directory.

`, desc, *src, *corpus, command)
	}

	type outFile struct {
		name, desc string
		pick       func(caseInfo) bool
		docLevel   bool
	}
	isNew := func(c caseInfo) bool { return c.Category != "paraphrase" }
	files := []outFile{
		{"stratified-all.yaml", "All 103 cases, paragraph labels mapped to chunk ordinals.", func(caseInfo) bool { return true }, false},
		{"stratified-new-doclevel.yaml", "The 63 non-paraphrase cases at document level: ordinals stripped, one entry per distinct source_id.", isNew, true},
	}
	for _, cat := range categories {
		cat := cat
		files = append(files, outFile{"stratified-" + cat + ".yaml", "Category " + cat + ", paragraph labels mapped to chunk ordinals.",
			func(c caseInfo) bool { return c.Category == cat }, false})
		if cat != "paraphrase" {
			files = append(files, outFile{"stratified-" + cat + "-doclevel.yaml", "Category " + cat + " at document level: ordinals stripped, one entry per distinct source_id.",
				func(c caseInfo) bool { return c.Category == cat }, true})
		}
	}
	for _, of := range files {
		var labels eval.Labels
		for _, c := range infos {
			if !of.pick(c) {
				continue
			}
			labels.Cases = append(labels.Cases, toCase(c, of.docLevel))
		}
		var buf bytes.Buffer
		buf.WriteString(header(of.desc))
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(struct {
			Cases []eval.Case `yaml:"cases"`
		}{labels.Cases}); err != nil {
			return err
		}
		enc.Close()
		// The scorer's own loader must accept what we wrote.
		back, err := eval.ReadLabels(bytes.NewReader(buf.Bytes()))
		if err != nil {
			return fmt.Errorf("%s does not load: %w", of.name, err)
		}
		if err := os.WriteFile(filepath.Join(*outDir, of.name), buf.Bytes(), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d cases)\n", of.name, len(back.Cases))
	}
	raw, _ = json.MarshalIndent(infos, "", "  ")
	return os.WriteFile(filepath.Join(*outDir, "cases.json"), append(raw, '\n'), 0o644)
}

func toCase(c caseInfo, docLevel bool) eval.Case {
	out := eval.Case{Query: c.Query}
	seen := map[string]bool{}
	for _, l := range c.Labels {
		if docLevel || l.ParagraphOrdinal == nil {
			if seen[l.SourceID] {
				continue
			}
			seen[l.SourceID] = true
			out.Relevant = append(out.Relevant, eval.Relevant{SourceID: l.SourceID})
			continue
		}
		out.Relevant = append(out.Relevant, eval.Relevant{SourceID: l.SourceID, Ordinals: l.ChunkOrdinals})
	}
	return out
}

// replaySearcher serves recorded results so eval.Run scores one case at a
// time with its own, unchanged scoring code.
type replaySearcher map[string]*retrieval.Result

func (r replaySearcher) Search(_ context.Context, q retrieval.Query) (*retrieval.Result, error) {
	res, ok := r[string(q.Mode)+"\x00"+q.Text]
	if !ok {
		return nil, fmt.Errorf("no recorded result for %s %q", q.Mode, q.Text)
	}
	return res, nil
}

type hitRecord struct {
	SourceID string `json:"source_id"`
	Ordinal  int    `json:"ordinal"`
	Title    string `json:"title"`
	Content  string `json:"content"`
}

type caseModeResult struct {
	Mode string `json:"mode"`
	// Paragraph/chunk-level (as in stratified-all.yaml).
	RecallAt8  float64 `json:"recall_at_8"`
	RecallAt20 float64 `json:"recall_at_20"`
	RecallAt50 float64 `json:"recall_at_50"`
	MRR        float64 `json:"mrr"`
	// Document-level (ordinals stripped).
	DocRecallAt8  float64     `json:"doc_recall_at_8"`
	DocRecallAt20 float64     `json:"doc_recall_at_20"`
	DocMRR        float64     `json:"doc_mrr"`
	Hits          []hitRecord `json:"hits"`
}

type caseResult struct {
	caseInfo
	Modes []caseModeResult `json:"modes"`
}

func percaseCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("percase", flag.ExitOnError)
	dir := fs.String("generated", "evals/retrieval/generated", "directory written by convert")
	out := fs.String("out", "evals/retrieval/results-scotus-cap-stratified/percase.json", "output")
	dsn := fs.String("dsn", envOr("AGENTD_DSN", defaultDSN), "Postgres DSN")
	modelURL := fs.String("model-url", envOr("AGENTD_MODEL_URL", "http://localhost:11434/v1"), "local model base URL")
	embedModel := fs.String("embed-model", envOr("AGENTD_EMBED_MODEL", "mxbai-embed-large"), "embedding model")
	rerankModel := fs.String("rerank-model", envOr("AGENTD_RERANK_MODEL", envOr("AGENTD_MODEL", "qwen2.5:7b")), "rerank model (the eval command's default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	raw, err := os.ReadFile(filepath.Join(*dir, "cases.json"))
	if err != nil {
		return err
	}
	var infos []caseInfo
	if err := json.Unmarshal(raw, &infos); err != nil {
		return err
	}

	embedder := embed.New(embed.Config{BaseURL: *modelURL, Model: *embedModel, Dimensions: store.EmbeddingDimensions})
	if err := embed.Probe(ctx, embedder); err != nil {
		return fmt.Errorf("embedder: %w", err)
	}
	st, err := store.New(ctx, *dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	onBox := local.New(local.Config{BaseURL: *modelURL, APIKey: os.Getenv("AGENTD_MODEL_API_KEY"), Timeout: 10 * time.Minute})
	// Same wiring as `agentd eval retrieval`: RerankCandidates 50, default Candidates.
	searcher := &retrieval.Searcher{Store: st, Embedder: embedder, Reranker: rerank.NewLLM(onBox, *rerankModel, log), RerankCandidates: 50, Log: log}

	// Depth 50 matches eval.evalDepth.
	const depth = 50
	recorded := replaySearcher{}
	results := make([]caseResult, len(infos))
	for i, info := range infos {
		results[i].caseInfo = info
		for _, mode := range eval.AllModes {
			start := time.Now()
			res, err := searcher.Search(ctx, retrieval.Query{Text: info.Query, K: depth, Mode: mode})
			if err != nil {
				return fmt.Errorf("case %d mode %s: %w", i, mode, err)
			}
			if mode == retrieval.ModeHybridRerank && res.Mode != retrieval.ModeHybridRerank {
				return fmt.Errorf("case %d: rerank degraded to %s", i, res.Mode)
			}
			recorded[string(mode)+"\x00"+info.Query] = res
			one := func(docLevel bool) eval.ModeResult {
				rep, err := eval.Run(ctx, recorded, &eval.Labels{Cases: []eval.Case{toCase(info, docLevel)}}, []retrieval.Mode{mode})
				if err != nil {
					panic(err)
				}
				return rep.Modes[0]
			}
			p, d := one(false), one(true)
			cm := caseModeResult{Mode: string(mode), RecallAt8: p.RecallAt8, RecallAt20: p.RecallAt20, RecallAt50: p.RecallAt50, MRR: p.MRR,
				DocRecallAt8: d.RecallAt8, DocRecallAt20: d.RecallAt20, DocMRR: d.MRR}
			for _, h := range res.Hits {
				cm.Hits = append(cm.Hits, hitRecord{SourceID: h.SourceID, Ordinal: h.Ordinal, Title: h.Title, Content: h.Content})
			}
			results[i].Modes = append(results[i].Modes, cm)
			fmt.Fprintf(os.Stderr, "case %3d %-13s %-14s r@8=%.2f mrr=%.3f (%s)\n", i, info.Category, mode, p.RecallAt8, p.MRR, time.Since(start).Round(time.Millisecond))
		}
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	raw, _ = json.MarshalIndent(results, "", "  ")
	return os.WriteFile(*out, append(raw, '\n'), 0o644)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
