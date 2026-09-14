package evals_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shreyasprasad/agentd/internal/evals"
)

func TestParseCitationSpellings(t *testing.T) {
	for _, tc := range []struct {
		answer   string
		sourceID string
		ordinal  int
	}{
		{"as held in (clop-0002 ¶14).", "clop-0002", 14},
		{"see clop-0002 ¶14", "clop-0002", 14},
		{"(clop-0002 ¶ 14)", "clop-0002", 14},
		{"(CLOP-0002 ¶14)", "clop-0002", 14},
		{"clop-0002 para. 14", "clop-0002", 14},
		{"clop-0002 paragraph 14", "clop-0002", 14},
		{"clop-0002 p. 14", "clop-0002", 14},
		{"clop-0002 #14", "clop-0002", 14},
	} {
		found, unparsed := evals.ParseCitations(tc.answer)
		require.Len(t, found, 1, "answer %q", tc.answer)
		require.Equal(t, tc.sourceID, found[0].SourceID, "answer %q", tc.answer)
		require.Equal(t, tc.ordinal, found[0].Ordinal, "answer %q", tc.answer)
		require.Empty(t, unparsed, "answer %q", tc.answer)
	}
}

func TestParseCitationsDeduplicates(t *testing.T) {
	found, _ := evals.ParseCitations("(clop-0002 ¶1) ... and again (clop-0002 ¶1), plus (clop-0002 ¶4).")
	require.Len(t, found, 2)
	require.Equal(t, 1, found[0].Ordinal)
	require.Equal(t, 4, found[1].Ordinal)
}

func TestParseCitationsIgnoresOrdinaryProse(t *testing.T) {
	found, unparsed := evals.ParseCitations("The Court held that the warrant requirement applies.")
	require.Empty(t, found)
	require.Empty(t, unparsed)
}

// TestParseCitationsReportsUnparsedText covers the way this check goes quietly
// blind: a citation the parser cannot see is a citation it cannot mark
// unresolved, so a lax parser hides fabrication instead of catching it.
func TestParseCitationsReportsUnparsedText(t *testing.T) {
	found, unparsed := evals.ParseCitations("see clop-0002 ¶ the second paragraph")
	require.Empty(t, found)
	require.Len(t, unparsed, 1)
	require.Contains(t, unparsed[0], "clop-0002")
}

// TestParseCitationsDoesNotReportParsedOnesAsUnparsed is the regression test
// for the bug this parser shipped with: the loose pattern stops at the
// pilcrow, so re-matching its text against the strict pattern reported every
// correctly-parsed citation as unparsed. Overlap is measured by position.
func TestParseCitationsDoesNotReportParsedOnesAsUnparsed(t *testing.T) {
	_, unparsed := evals.ParseCitations(
		"The acquisition is a search (clop-0002 ¶1), and see also (clop-0011 ¶3).")
	require.Empty(t, unparsed)
}

func TestResolveCitations(t *testing.T) {
	resolver := stubResolver{"clop-0002#1": true, "clop-0011#3": true}
	out, err := evals.ResolveCitations(context.Background(), resolver,
		"held in (clop-0002 ¶1); compare (clop-0011 ¶3).")
	require.NoError(t, err)
	require.Len(t, out.Found, 2)
	require.Equal(t, 2, out.Resolved)
	require.Empty(t, out.Unresolved)
}

// TestResolveCitationsCatchesAFabricatedParagraph is the whole point of the
// CITATION category: the document is real and the paragraph is not, which is
// the shape a document-level check would wave through.
func TestResolveCitationsCatchesAFabricatedParagraph(t *testing.T) {
	resolver := stubResolver{"clop-0002#1": true}
	out, err := evals.ResolveCitations(context.Background(), resolver,
		"held in (clop-0002 ¶1), and also (clop-0002 ¶999).")
	require.NoError(t, err)
	require.Len(t, out.Found, 2)
	require.Equal(t, 1, out.Resolved)
	require.Len(t, out.Unresolved, 1)
	require.Contains(t, out.Unresolved[0], "999")
}

func TestResolveCitationsCatchesAFabricatedDocument(t *testing.T) {
	out, err := evals.ResolveCitations(context.Background(), stubResolver{},
		"held in (clop-9999 ¶1).")
	require.NoError(t, err)
	require.Len(t, out.Unresolved, 1)
}

func TestCitationAssertionFailsOnAnUncheckableAnswer(t *testing.T) {
	ev := newLog(t, []string{"finish"}).finish("The Court held that a warrant is required.").evidence()
	fail := check(t, evals.Assertions{Citations: &evals.CitationAssert{Min: 1}}, ev, stubResolver{})
	require.Len(t, fail, 1)
	require.Contains(t, fail[0], "says nothing checkable")
}

func TestCitationAssertionFailsOnAFabricatedCite(t *testing.T) {
	ev := newLog(t, []string{"finish"}).finish("As held in (clop-0002 ¶999).").evidence()
	fail := check(t, evals.Assertions{
		Citations: &evals.CitationAssert{Min: 1, AllResolve: true},
	}, ev, stubResolver{"clop-0002#1": true})
	require.Len(t, fail, 1)
	require.Contains(t, fail[0], "do not resolve")
}

func TestCitationAssertionRequiresANamedSource(t *testing.T) {
	ev := newLog(t, []string{"finish"}).finish("As held in (clop-0011 ¶3).").evidence()
	fail := check(t, evals.Assertions{
		Citations: &evals.CitationAssert{Min: 1, AllResolve: true, MustInclude: []string{"clop-0002"}},
	}, ev, stubResolver{"clop-0011#3": true})
	require.Len(t, fail, 1)
	require.Contains(t, fail[0], "does not cite clop-0002")
}
