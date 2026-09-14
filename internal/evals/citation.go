package evals

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/shreyasprasad/agentd/internal/store"
)

// Citation is one (source_id, ordinal) pair found in an answer.
type Citation struct {
	SourceID string `json:"source_id"`
	Ordinal  int    `json:"ordinal"`
	Text     string `json:"text"`
	Resolved bool   `json:"resolved"`
}

// CitationOutcome is what a case's answer cited and whether it was true.
type CitationOutcome struct {
	Found      []Citation `json:"found"`
	Resolved   int        `json:"resolved"`
	Unresolved []string   `json:"unresolved,omitempty"`
	// Unparsed is text that looks like a citation to a person and did not
	// parse. It is reported rather than ignored because a lax parser is the
	// failure mode that hides fabrication: a citation the parser cannot see
	// is a citation it cannot mark unresolved.
	Unparsed []string `json:"unparsed,omitempty"`
}

// citationRE matches the convention the corpus tools teach and the default
// system prompt asks for — (clop-0002 ¶14) — plus the spellings a model
// reaches for anyway: bare, without parentheses, with "para"/"P"/"#" for the
// pilcrow, and with arbitrary spacing.
//
// Parsing is permissive and resolution is strict, and that asymmetry is the
// whole design: the conservative direction for a fabrication check is to see
// more citation-shaped strings, not fewer.
var citationRE = regexp.MustCompile(`(?i)\b([a-z][a-z0-9]*-[0-9a-z_.\-]+)\s*(?:¶|para\.?|paragraph|p\.|#)\s*([0-9]+)`)

// looseCitationRE finds text that a reader would call a citation even when
// citationRE cannot make a pair of it: a source id next to something
// paragraph-shaped, or a pilcrow with no resolvable id in front of it.
var looseCitationRE = regexp.MustCompile(`(?i)(?:\(\s*)?([a-z][a-z0-9]*-[0-9a-z_.\-]+)[^)\n]{0,24}?(?:¶|paragraph|para\b)`)

// ParseCitations pulls every citation out of an answer, in order,
// de-duplicated, and separately reports text that looks like a citation and
// did not parse into a pair.
func ParseCitations(answer string) ([]Citation, []string) {
	var found []Citation
	seen := map[string]bool{}
	strict := citationRE.FindAllStringSubmatchIndex(answer, -1)
	for _, m := range strict {
		ordinal, err := strconv.Atoi(answer[m[4]:m[5]])
		if err != nil {
			continue
		}
		c := Citation{
			SourceID: strings.ToLower(answer[m[2]:m[3]]),
			Ordinal:  ordinal,
			Text:     strings.TrimSpace(answer[m[0]:m[1]]),
		}
		key := c.SourceID + "#" + strconv.Itoa(ordinal)
		if seen[key] {
			continue
		}
		seen[key] = true
		found = append(found, c)
	}

	// Anything citation-shaped the strict pattern did not cover. Overlap is
	// measured against positions in the answer rather than by re-running the
	// strict pattern on the loose match: the loose pattern stops at the
	// pilcrow, so its text never contains the ordinal and re-matching it would
	// report every correctly-parsed citation as unparsed.
	var unparsed []string
	for _, l := range looseCitationRE.FindAllStringIndex(answer, -1) {
		if overlapsAny(l, strict) {
			continue
		}
		unparsed = append(unparsed, strings.TrimSpace(answer[l[0]:l[1]]))
	}
	return found, dedupe(unparsed)
}

// overlapsAny reports whether span shares any byte with one of spans. Each
// entry of spans may carry submatch positions after the first pair; only the
// first pair is the whole match.
func overlapsAny(span []int, spans [][]int) bool {
	for _, s := range spans {
		if s[0] < span[1] && span[0] < s[1] {
			return true
		}
	}
	return false
}

// ChunkResolver is what citation checking needs from the store.
type ChunkResolver interface {
	GetChunkByCitation(ctx context.Context, sourceID string, ordinal int) (*store.SearchHit, error)
}

// ResolveCitations checks each citation against the chunks table. A citation
// that names a document that exists but a paragraph that does not is
// unresolved, which is the fabrication shape a document-level check would miss.
func ResolveCitations(ctx context.Context, r ChunkResolver, answer string) (*CitationOutcome, error) {
	found, unparsed := ParseCitations(answer)
	out := &CitationOutcome{Found: found, Unparsed: unparsed}
	for i := range out.Found {
		c := &out.Found[i]
		_, err := r.GetChunkByCitation(ctx, c.SourceID, c.Ordinal)
		switch {
		case err == nil:
			c.Resolved = true
			out.Resolved++
		case errors.Is(err, store.ErrNotFound):
			out.Unresolved = append(out.Unresolved, c.Text)
		default:
			return nil, err
		}
	}
	return out, nil
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
