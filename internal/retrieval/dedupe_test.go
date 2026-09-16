package retrieval

import (
	"encoding/json"
	"testing"
)

func doc(id, title, docket, text string) InputDoc {
	d := InputDoc{SourceID: id, Title: title, Text: text}
	if docket != "" {
		d.Metadata = json.RawMessage(`{"docket_number":"` + docket + `"}`)
	}
	return d
}

func ids(docs []InputDoc) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.SourceID
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDedupeKeepsLongestPerDocket(t *testing.T) {
	// The shape CourtListener actually produces: one case published three
	// times, each revision longer than the last, under three opinion ids.
	in := []InputDoc{
		doc("a", "Goldey v. Fields", "24-809", "short"),
		doc("b", "Other v. Case", "24-100", "unrelated"),
		doc("c", "Goldey v. Fields", "24-809", "much longer text"),
		doc("d", "Goldey v. Fields Revisions: 6/30/25", "24-809", "longest text of the three"),
	}
	out, sum := Dedupe(in, DedupeOptions{MetadataKey: "docket_number"})

	if got := ids(out); !equal(got, []string{"d", "b"}) {
		t.Fatalf("kept %v, want [d b] — longest per docket, in first-appearance order", got)
	}
	if sum.In != 4 || sum.Out != 2 || sum.Dropped != 2 || sum.Collapsed != 1 {
		t.Fatalf("summary = %+v, want in 4 / out 2 / dropped 2 / collapsed 1", sum)
	}
	if sum.ByKey != 4 || sum.ByTitle != 0 {
		t.Fatalf("summary = %+v, want every document grouped by docket", sum)
	}
}

// Every revision hashes differently, which is the reason dedupe is keyed on
// identity rather than content. If this ever fails, content-hash dedupe would
// have sufficed and Dedupe is carrying weight it does not need to.
func TestDedupeTargetsDocumentsContentHashingWouldMiss(t *testing.T) {
	in := []InputDoc{
		doc("a", "Trump v. CASA, Inc.", "24A884", "opinion text"),
		doc("b", "Trump v. CASA, Inc. Revisions: 6/27/25", "24A884", "opinion text, revised"),
	}
	if in[0].Text == in[1].Text {
		t.Fatal("fixture is wrong: revisions must differ in text")
	}
	out, sum := Dedupe(in, DedupeOptions{MetadataKey: "docket_number"})
	if len(out) != 1 || out[0].SourceID != "b" {
		t.Fatalf("kept %v, want [b]", ids(out))
	}
	if sum.Collapsed != 1 {
		t.Fatalf("summary = %+v, want the pair collapsed", sum)
	}
}

func TestDedupeFallsBackToNormalizedTitle(t *testing.T) {
	// No metadata at all, and a revision marker that makes two records of one
	// case look like two cases.
	in := []InputDoc{
		doc("a", "Trump v. CASA, Inc.", "", "text"),
		doc("b", "Trump  v. CASA, Inc. Revisions: 6/27/25", "", "longer text"),
	}
	out, sum := Dedupe(in, DedupeOptions{MetadataKey: "docket_number"})
	if len(out) != 1 || out[0].SourceID != "b" {
		t.Fatalf("kept %v, want [b]", ids(out))
	}
	if sum.ByTitle != 2 || sum.ByKey != 0 {
		t.Fatalf("summary = %+v, want both documents grouped by title", sum)
	}
}

// A document whose metadata is malformed or holds a non-string docket must not
// abort the corpus or vanish from it.
func TestDedupeToleratesBadMetadata(t *testing.T) {
	in := []InputDoc{
		{SourceID: "a", Title: "One v. Two", Text: "x", Metadata: json.RawMessage(`not json`)},
		{SourceID: "b", Title: "Three v. Four", Text: "y", Metadata: json.RawMessage(`{"docket_number":404}`)},
		{SourceID: "c", Title: "Five v. Six", Text: "z"},
	}
	out, sum := Dedupe(in, DedupeOptions{MetadataKey: "docket_number"})
	if got := ids(out); !equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("kept %v, want all three preserved", got)
	}
	if sum.ByTitle != 3 {
		t.Fatalf("summary = %+v, want all three to fall back to title", sum)
	}
}

// Distinct cases that share a docket number must not collapse into one. They
// do not share one in practice, but the guard is what makes "keep the longest"
// safe: a wrong key silently deletes an opinion.
func TestDedupeDistinctDocketsSurvive(t *testing.T) {
	in := []InputDoc{
		doc("a", "One v. Two", "24-1", "x"),
		doc("b", "Three v. Four", "24-2", "y"),
		doc("c", "Five v. Six", "24-3", "z"),
	}
	out, sum := Dedupe(in, DedupeOptions{MetadataKey: "docket_number"})
	if len(out) != 3 || sum.Dropped != 0 || sum.Collapsed != 0 {
		t.Fatalf("kept %v summary %+v, want all three and nothing collapsed", ids(out), sum)
	}
}

func TestDedupeIsIdempotent(t *testing.T) {
	in := []InputDoc{
		doc("a", "Goldey v. Fields", "24-809", "short"),
		doc("b", "Goldey v. Fields", "24-809", "longer text"),
		doc("c", "Other v. Case", "24-100", "unrelated"),
	}
	once, _ := Dedupe(in, DedupeOptions{MetadataKey: "docket_number"})
	twice, sum := Dedupe(once, DedupeOptions{MetadataKey: "docket_number"})
	if !equal(ids(once), ids(twice)) {
		t.Fatalf("second pass changed the corpus: %v then %v", ids(once), ids(twice))
	}
	if sum.Dropped != 0 {
		t.Fatalf("summary = %+v, want a second pass to drop nothing", sum)
	}
}

func TestNormalizeTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Trump v. CASA, Inc.", "trump v. casa, inc"},
		{"Trump v. CASA, Inc. Revisions: 6/27/25", "trump v. casa, inc"},
		{"Goldey  v.   Fields", "goldey v. fields"},
		{"Riley v. Bondi REVISIONS: 7/01/26", "riley v. bondi"},
	}
	for _, c := range cases {
		if got := NormalizeTitle(c.in); got != c.want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
