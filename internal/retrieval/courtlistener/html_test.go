package courtlistener

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTMLToTextParagraphs(t *testing.T) {
	src := `<div><p>First paragraph of the opinion.</p><p>Second paragraph follows.</p></div>`
	got := HTMLToText(src)
	require.Equal(t, "First paragraph of the opinion.\n\nSecond paragraph follows.", got)
}

func TestHTMLToTextBreaksBecomeNewlines(t *testing.T) {
	src := `<p>Line one.<br>Line two.</p>`
	got := HTMLToText(src)
	require.Equal(t, "Line one.\nLine two.", got)
}

func TestHTMLToTextKeepsCitationAnchorText(t *testing.T) {
	src := `<p>See <a href="/opinion/12345/smith/">Smith v. Jones, 555 U.S. 100</a> (2009).</p>`
	got := HTMLToText(src)
	require.Equal(t, "See Smith v. Jones, 555 U.S. 100 (2009).", got)
}

func TestHTMLToTextDecodesEntities(t *testing.T) {
	src := `<p>The court&rsquo;s holding &amp; its scope&nbsp;are clear.</p>`
	got := HTMLToText(src)
	require.Equal(t, "The court’s holding & its scope are clear.", got)
}

func TestHTMLToTextDropsScriptsAndFootnotes(t *testing.T) {
	src := `<p>Text before<sup>1</sup> the note.</p>` +
		`<script>alert("x")</script>` +
		`<div class="footnote">1. A footnote body that should vanish.</div>` +
		`<p>Text after.</p>`
	got := HTMLToText(src)
	require.NotContains(t, got, "alert")
	require.NotContains(t, got, "footnote body")
	require.NotContains(t, got, ">1<")
	require.Contains(t, got, "Text before the note.")
	require.Contains(t, got, "Text after.")
}

func TestHTMLToTextPlainTextPassthrough(t *testing.T) {
	src := "SYLLABUS\n\nA plain text opinion with no markup.\n\nSecond paragraph."
	require.Equal(t, src, HTMLToText(src))
	// Excess blank lines and trailing spaces are normalised.
	messy := "One.   \n\n\n\nTwo."
	require.Equal(t, "One.\n\nTwo.", HTMLToText(messy))
}

func TestHTMLToTextNestedBlocks(t *testing.T) {
	src := `<div><blockquote><p>Quoted passage.</p></blockquote><p>After the quote.</p></div>`
	got := HTMLToText(src)
	parts := strings.Split(got, "\n\n")
	require.Equal(t, []string{"Quoted passage.", "After the quote."}, parts)
}
