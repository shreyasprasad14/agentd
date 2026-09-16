package retrieval

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Deduplication is corpus curation, not ingestion.
//
// A source that versions its documents — CourtListener publishes each revision
// of an opinion under its own id — hands the same case to the pipeline several
// times over. That inflates any retrieval measurement taken against the corpus
// and wastes an agent's context window on near-identical hits, but it is not
// something ingest can fix: ingest is idempotent per source_id by content hash,
// which answers "do I already have this exact document" and not "are these two
// documents the same case". The second question needs a key the generic
// pipeline does not have, and answering it wrongly silently drops an opinion.
//
// Note that content hashing would not catch this at all. Revisions differ in
// text by construction — that is what makes them revisions — so every one of
// them hashes differently. The key has to be identity, not content.
//
// So it runs here, over the fetched JSONL, before anything is embedded: a pure
// function whose input and output are both inspectable files. See ADR-39.

// DedupeOptions selects the key documents are grouped by.
type DedupeOptions struct {
	// MetadataKey is a top-level string field of each document's metadata
	// holding a stable identifier for the thing the document is a version of
	// — "docket_number" for CourtListener. Documents whose metadata lacks it
	// fall back to their normalized title.
	MetadataKey string
}

// DedupeSummary reports what a Dedupe call collapsed.
type DedupeSummary struct {
	In        int `json:"in"`
	Out       int `json:"out"`
	Dropped   int `json:"dropped"`
	ByKey     int `json:"by_key"`    // documents grouped using MetadataKey
	ByTitle   int `json:"by_title"`  // documents that fell back to the title
	Collapsed int `json:"collapsed"` // groups holding more than one document
}

// revisionSuffix matches the marker CourtListener appends to a re-published
// opinion — "Trump v. CASA, Inc. Revisions: 6/27/25" — which otherwise makes a
// revision look like a different case than the opinion it revises.
var revisionSuffix = regexp.MustCompile(`(?i)\s*revisions?:.*$`)

// NormalizeTitle reduces a title to a comparable form: the revision marker
// removed, whitespace collapsed, case and trailing punctuation dropped.
func NormalizeTitle(title string) string {
	t := revisionSuffix.ReplaceAllString(title, "")
	t = strings.Join(strings.Fields(t), " ")
	return strings.TrimRight(strings.ToLower(t), ". ")
}

// Dedupe keeps one document per group, choosing the longest text because a
// revision supersedes what it revises and is the more complete document of the
// two. Input order is preserved by first appearance, so the output of a resumed
// fetch stays stable. Documents are never merged: the winner is returned
// verbatim, and the ids of the ones it displaced are recoverable from the
// input file, which is why this writes a new file rather than editing one.
func Dedupe(docs []InputDoc, opt DedupeOptions) ([]InputDoc, DedupeSummary) {
	sum := DedupeSummary{In: len(docs)}
	order := make([]string, 0, len(docs))
	winner := make(map[string]InputDoc, len(docs))
	count := make(map[string]int, len(docs))

	for _, d := range docs {
		key := ""
		if opt.MetadataKey != "" {
			key = metadataString(d.Metadata, opt.MetadataKey)
		}
		if key != "" {
			key = opt.MetadataKey + "=" + key
			sum.ByKey++
		} else {
			key = "title=" + NormalizeTitle(d.Title)
			sum.ByTitle++
		}

		prev, seen := winner[key]
		if !seen {
			order = append(order, key)
			winner[key] = d
			count[key] = 1
			continue
		}
		count[key]++
		if len(d.Text) > len(prev.Text) {
			winner[key] = d
		}
	}

	out := make([]InputDoc, 0, len(order))
	for _, k := range order {
		out = append(out, winner[k])
		if count[k] > 1 {
			sum.Collapsed++
		}
	}
	sum.Out = len(out)
	sum.Dropped = sum.In - sum.Out
	return out, sum
}

// metadataString reads one top-level string field out of a document's raw
// metadata. A document whose metadata is absent, malformed, or holds a
// non-string under the key reports no key rather than failing the run: the
// caller's fallback is a normalized title, which is strictly better than
// dropping the document or aborting the corpus.
func metadataString(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}
