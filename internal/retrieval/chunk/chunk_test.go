package chunk

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// invariants asserts the properties every chunking must satisfy: contiguous
// ordinals, bodies that are exact slices of the input, bodies under Max, and
// coverage of every non-whitespace, non-heading character.
func invariants(t *testing.T, text string, chunks []Chunk, opt Options) {
	t.Helper()
	covered := make([]bool, len(text))
	for i, c := range chunks {
		require.Equal(t, i, c.Ordinal, "ordinals must be 0..n-1")
		require.LessOrEqual(t, c.CharStart, c.CharEnd)
		body := text[c.CharStart:c.CharEnd]
		require.True(t, strings.HasSuffix(c.Content, body),
			"content must end with the input slice [%d:%d]", c.CharStart, c.CharEnd)
		require.LessOrEqual(t, len(body), opt.Max, "chunk body exceeds max")
		for j := c.CharStart; j < c.CharEnd; j++ {
			covered[j] = true
		}
	}
	// Every non-whitespace char outside a heading paragraph must be covered.
	for _, p := range paragraphs(text) {
		if isHeading(text[p.start:p.end]) {
			continue
		}
		for j := p.start; j < p.end; j++ {
			if !unicode.IsSpace(rune(text[j])) {
				require.True(t, covered[j], "char %d (%q) not covered by any chunk", j, text[j])
			}
		}
	}
}

func TestEmptyAndWhitespaceInput(t *testing.T) {
	require.Nil(t, Split(""))
	require.Nil(t, Split("   \n\n \t \n"))
}

func TestHeadingsBecomeSections(t *testing.T) {
	text := "SYLLABUS\n\n" +
		"The petitioner sought review of the judgment below, and this Court granted certiorari to resolve the question presented by the parties in their briefs.\n\n" +
		"II. Discussion\n\n" +
		"The doctrine at issue requires that the law be clearly established at the time of the conduct, such that every reasonable official would have understood the conduct to be unlawful.\n\n" +
		"CONCLUSION\n\n" +
		"The judgment of the Court of Appeals is reversed, and the case is remanded for further proceedings consistent with this opinion."

	chunks := SplitWith(text, Options{Target: 300, Max: 600, Min: 50, Overlap: 200})
	invariants(t, text, chunks, Options{Target: 300, Max: 600, Min: 50, Overlap: 200})
	require.Len(t, chunks, 3)
	require.Equal(t, "SYLLABUS", chunks[0].Section)
	require.Equal(t, "II. Discussion", chunks[1].Section)
	require.Equal(t, "CONCLUSION", chunks[2].Section)
	for _, c := range chunks {
		require.NotContains(t, text[c.CharStart:c.CharEnd], "SYLLABUS", "headings must not be chunk bodies")
	}
}

func TestIsHeading(t *testing.T) {
	for _, line := range []string{
		"SYLLABUS", "II.", "II. Discussion", "A. The Standard", "IV. Remedies",
		"The Qualified Immunity Question", "1. Background",
	} {
		require.True(t, isHeading(line), "%q should be a heading", line)
	}
	for _, line := range []string{
		"Reversed.", "The court disagrees with this contention.",
		"So ordered.", "", "It is so ordered.",
		"the parties dispute whether the statute applies",
		strings.Repeat("LONG CAPS ", 20),
	} {
		require.False(t, isHeading(line), "%q should not be a heading", line)
	}
}

func TestParagraphMerging(t *testing.T) {
	// Three short paragraphs that fit one Target-sized chunk together.
	text := "First short paragraph about the holding.\n\nSecond short paragraph continues the reasoning.\n\nThird short paragraph concludes the section."
	opt := Options{Target: 1200, Max: 1800, Min: 200, Overlap: 200}
	chunks := SplitWith(text, opt)
	invariants(t, text, chunks, opt)
	require.Len(t, chunks, 1)
	require.Equal(t, text, chunks[0].Content)
}

func TestOversizeParagraphSplitAtSentences(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteString("This sentence pads the paragraph well past the maximum chunk size to force sentence splitting. ")
	}
	text := strings.TrimSpace(sb.String())
	opt := Options{Target: 1200, Max: 1800, Min: 200, Overlap: 200}
	chunks := SplitWith(text, opt)
	invariants(t, text, chunks, opt)
	require.Greater(t, len(chunks), 1)
	for _, c := range chunks {
		body := text[c.CharStart:c.CharEnd]
		require.True(t, strings.HasSuffix(body, "."), "each piece should end on a sentence boundary: %q", body[len(body)-20:])
	}
}

func TestOverlapIsPreviousLastSentence(t *testing.T) {
	para1 := "The first paragraph makes a point at length, describing the procedural history of the case in detail so that it stands as its own chunk under a small target. The holding straddles this boundary."
	para2 := "The second paragraph relies on the holding announced above and applies it to the facts, which is why the last sentence of the previous chunk must be retrievable from this one as well."
	text := para1 + "\n\n" + para2
	opt := Options{Target: 200, Max: 400, Min: 50, Overlap: 200}
	chunks := SplitWith(text, opt)
	invariants(t, text, chunks, opt)
	require.Len(t, chunks, 2)
	require.True(t, strings.HasPrefix(chunks[1].Content, "The holding straddles this boundary."),
		"chunk 1 must start with chunk 0's last sentence, got %q", chunks[1].Content[:60])
}

func TestOverlapCapped(t *testing.T) {
	long := "This single enormous sentence " + strings.Repeat("keeps going and going ", 30) + "and finally ends here."
	text := long + "\n\nA short second paragraph that needs an overlap prefix from the previous chunk."
	opt := Options{Target: 300, Max: 900, Min: 50, Overlap: 100}
	chunks := SplitWith(text, opt)
	require.GreaterOrEqual(t, len(chunks), 2)
	last := chunks[len(chunks)-1]
	overlap := strings.TrimSuffix(last.Content, text[last.CharStart:last.CharEnd])
	require.LessOrEqual(t, len(overlap), 101, "overlap must respect the cap") // +1 for the joining newline
	require.Contains(t, overlap, "ends here.")
}

func TestTrailingFragmentMergedBackward(t *testing.T) {
	// A normal paragraph followed by a tiny one in the same section.
	text := "This paragraph is comfortably long enough to stand on its own as a chunk because it exceeds the minimum size for the options used in this test case by a fair margin.\n\nToo small."
	opt := Options{Target: 120, Max: 400, Min: 50, Overlap: 100}
	chunks := SplitWith(text, opt)
	invariants(t, text, chunks, opt)
	require.Len(t, chunks, 1, "the trailing fragment should merge backward")
	require.True(t, strings.HasSuffix(chunks[0].Content, "Too small."))
}

func TestSectionCarriedAcrossChunks(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("III. Analysis\n\n")
	for i := 0; i < 6; i++ {
		sb.WriteString("A reasonably sized paragraph that will be grouped with its neighbours until the running chunk reaches the configured target size for this table-driven test case.\n\n")
	}
	text := strings.TrimSpace(sb.String())
	opt := Options{Target: 300, Max: 600, Min: 50, Overlap: 100}
	chunks := SplitWith(text, opt)
	invariants(t, text, chunks, opt)
	require.Greater(t, len(chunks), 1)
	for _, c := range chunks {
		require.Equal(t, "III. Analysis", c.Section, "every chunk under a heading carries it")
	}
}

func TestSentenceEndsSkipLegalAbbreviations(t *testing.T) {
	s := "See Smith v. Jones, 555 U.S. 100 (2009). The court agreed. Id. at 105."
	ends := sentenceEnds(s)
	// "v." and "U.S." and "Id." must not end sentences; "(2009)." and "agreed." do.
	require.Len(t, ends, 3)
	require.Equal(t, "See Smith v. Jones, 555 U.S. 100 (2009).", s[:ends[0]])
	require.Equal(t, " The court agreed.", s[ends[0]:ends[1]])
}

func TestLastSentence(t *testing.T) {
	require.Equal(t, "Second sentence here.", lastSentence("First sentence. Second sentence here.", 200))
	require.Equal(t, "Only one.", lastSentence("Only one.", 200))
	require.Equal(t, "", lastSentence("", 200))
	got := lastSentence("Alpha beta gamma delta epsilon zeta eta theta.", 20)
	require.LessOrEqual(t, len(got), 20)
	require.True(t, strings.HasSuffix(got, "theta."))
}

// TestOversizeParagraphCutsOnRuneBoundary covers the defect that stopped
// United States v. Booker from ingesting: a paragraph longer than Max with no
// sentence terminator inside it falls through to the max-byte cut, and cutting
// an arbitrary byte offset splits a multi-byte rune. The chunk is then not
// valid UTF-8, Postgres rejects the insert, and the document is lost with an
// error that points at the database instead of the chunker.
func TestOversizeParagraphCutsOnRuneBoundary(t *testing.T) {
	opt := DefaultOptions()
	// Byte 1800 (opt.Max) lands on the middle byte of the first em-dash:
	// 1798 ASCII bytes, then "—" occupying 1798, 1799, 1800.
	text := strings.Repeat("a", opt.Max-2) + strings.Repeat("— und so weiter ", 20)
	require.Greater(t, len(text), opt.Max, "the paragraph must be oversize to take the split path")
	require.NotContains(t, text, ".", "and must hold no sentence end, to reach the max-byte fallback")

	chunks := Split(text)

	require.NotEmpty(t, chunks)
	for i, c := range chunks {
		require.True(t, utf8.ValidString(c.Content), "chunk %d is not valid UTF-8", i)
	}
	invariants(t, text, chunks, opt)
}

func TestRuneBoundary(t *testing.T) {
	s := "ab—cd" // '—' is 3 bytes at offsets 2,3,4
	require.Equal(t, 2, runeBoundary(s, 2), "already a boundary")
	require.Equal(t, 2, runeBoundary(s, 3), "mid-rune walks back")
	require.Equal(t, 2, runeBoundary(s, 4), "mid-rune walks back")
	require.Equal(t, 5, runeBoundary(s, 5), "boundary after the rune")
	require.Equal(t, len(s), runeBoundary(s, len(s)+10), "past the end clamps")
	require.Equal(t, 0, runeBoundary("", 0))
}
