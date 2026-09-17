package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/shreyasprasad/agentd/internal/retrieval"
)

// stratifiedLabelsPath is the draft stratified eval set. It is not read by
// ReadLabels: its cases carry fields (category, paragraph_ordinal, evidence,
// label_status, notes) that the scorer's strict decoder rejects. This test is
// its only consumer until the scorer learns paragraph-level labels.
const stratifiedLabelsPath = "../../../evals/retrieval/labels-scotus-cap-stratified.yaml"

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

// stratifiedRelevant is a document-level label when ParagraphOrdinal is nil
// (the original paraphrase cases) and a paragraph-level label otherwise.
type stratifiedRelevant struct {
	SourceID         string `yaml:"source_id"`
	ParagraphOrdinal *int   `yaml:"paragraph_ordinal"`
	Evidence         string `yaml:"evidence"`
}

var validCategories = map[string]bool{
	"paraphrase": true, "citation": true, "case_name": true, "term_of_art": true, "mixed": true,
}

func loadStratified(t *testing.T) *stratifiedFile {
	t.Helper()
	f, err := os.Open(stratifiedLabelsPath)
	require.NoError(t, err)
	defer f.Close()
	var sf stratifiedFile
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // a misspelled field is a silently dropped label
	require.NoError(t, dec.Decode(&sf))
	require.NotEmpty(t, sf.Cases)
	return &sf
}

var spaceRun = regexp.MustCompile(`\s+`)

func normalizeSpace(s string) string { return strings.TrimSpace(spaceRun.ReplaceAllString(s, " ")) }

// TestStratifiedLabelsWellFormed needs nothing but the file.
func TestStratifiedLabelsWellFormed(t *testing.T) {
	sf := loadStratified(t)
	seen := map[string]int{}
	for i, c := range sf.Cases {
		name := fmt.Sprintf("case %d (%.60q)", i, c.Query)
		if strings.TrimSpace(c.Query) == "" {
			t.Errorf("case %d: empty query", i)
		}
		if !validCategories[c.Category] {
			t.Errorf("%s: invalid category %q", name, c.Category)
		}
		if len(c.Relevant) == 0 {
			t.Errorf("%s: no relevant labels", name)
		}
		if c.LabelStatus != "" && c.LabelStatus != "draft" && c.LabelStatus != "reviewed" {
			t.Errorf("%s: label_status %q, want draft or reviewed", name, c.LabelStatus)
		}
		for j, r := range c.Relevant {
			if r.SourceID == "" {
				t.Errorf("%s relevant %d: missing source_id", name, j)
			}
			if r.ParagraphOrdinal != nil && *r.ParagraphOrdinal < 0 {
				t.Errorf("%s relevant %d: negative paragraph_ordinal", name, j)
			}
			// Paragraph labels must say why; document labels predate evidence.
			if r.ParagraphOrdinal != nil && strings.TrimSpace(r.Evidence) == "" {
				t.Errorf("%s relevant %d: paragraph label without evidence", name, j)
			}
		}
		key := strings.ToLower(normalizeSpace(c.Query))
		if prev, ok := seen[key]; ok {
			t.Errorf("%s: duplicate of case %d", name, prev)
		}
		seen[key] = i
	}
}

// paragraphs is the paragraph-ordinal definition the label file documents:
// text split on "\n", blank lines skipped.
func paragraphs(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestStratifiedLabelsResolveInCorpus checks every label against the ingested
// corpus: the source_id is a row in documents, the JSONL text is the text that
// was ingested (content_sha256 matches), the paragraph ordinal is in range,
// and the evidence excerpt appears in that paragraph. Paragraph text comes
// from the JSONL because chunks overlap and do not preserve paragraphs.
//
// It needs the ingested corpus: Postgres at AGENTD_EVAL_CORPUS_DSN (default:
// the Makefile's DSN) and the JSONL at AGENTD_EVAL_CORPUS_FILE (default:
// data/corpus/scotus-cap-dedup.jsonl). data/ is not checked in, so a missing
// file skips; a missing database with the file present fails.
func TestStratifiedLabelsResolveInCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("needs the ingested corpus in Postgres")
	}
	corpusFile := os.Getenv("AGENTD_EVAL_CORPUS_FILE")
	if corpusFile == "" {
		corpusFile = filepath.Join("..", "..", "..", "data", "corpus", "scotus-cap-dedup.jsonl")
	}
	jf, err := os.Open(corpusFile)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("corpus file %s not present (fetch and dedupe it, or set AGENTD_EVAL_CORPUS_FILE)", corpusFile)
	}
	require.NoError(t, err)
	defer jf.Close()

	dsn := os.Getenv("AGENTD_EVAL_CORPUS_DSN")
	if dsn == "" {
		dsn = "postgres://agentd:agentd@localhost:5432/agentd?sslmode=disable"
	}

	sf := loadStratified(t)
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err, "connect to the corpus database (make up, or run with -short)")
	defer conn.Close(context.Background())

	rows, err := conn.Query(ctx, `SELECT source_id, content_sha256 FROM documents WHERE source_id = ANY($1)`, ids)
	require.NoError(t, err)
	ingestedSHA := map[string]string{}
	for rows.Next() {
		var id, sha string
		require.NoError(t, rows.Scan(&id, &sha))
		ingestedSHA[id] = sha
	}
	require.NoError(t, rows.Err())

	docs, err := retrieval.ReadJSONL(jf)
	require.NoError(t, err)
	paras := map[string][]string{}
	for _, d := range docs {
		if !wanted[d.SourceID] {
			continue
		}
		sum := sha256.Sum256([]byte(d.Text))
		if got, want := hex.EncodeToString(sum[:]), ingestedSHA[d.SourceID]; want != "" && got != want {
			t.Errorf("%s: JSONL text sha %s differs from ingested content_sha256 %s; ordinals may not match what was ingested", d.SourceID, got, want)
		}
		paras[d.SourceID] = paragraphs(d.Text)
	}

	for i, c := range sf.Cases {
		name := fmt.Sprintf("case %d (%.60q)", i, c.Query)
		for _, r := range c.Relevant {
			if _, ok := ingestedSHA[r.SourceID]; !ok {
				t.Errorf("%s: source_id %s is not in documents", name, r.SourceID)
				continue
			}
			if r.ParagraphOrdinal == nil {
				continue
			}
			ps, ok := paras[r.SourceID]
			if !ok {
				t.Errorf("%s: source_id %s is ingested but missing from %s", name, r.SourceID, corpusFile)
				continue
			}
			ord := *r.ParagraphOrdinal
			if ord >= len(ps) {
				t.Errorf("%s: %s paragraph_ordinal %d out of range (document has %d)", name, r.SourceID, ord, len(ps))
				continue
			}
			if !strings.Contains(normalizeSpace(ps[ord]), normalizeSpace(r.Evidence)) {
				t.Errorf("%s: evidence not found in %s paragraph %d: %q", name, r.SourceID, ord, r.Evidence)
			}
		}
	}
}
