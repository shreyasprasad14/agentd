// Package courtlistener fetches court opinions from the CourtListener REST
// API (v4) and converts their HTML to plain text for chunking.
package courtlistener

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// blockTags end a paragraph when they open or close.
var blockTags = map[string]bool{
	"p": true, "div": true, "blockquote": true, "li": true, "ul": true,
	"ol": true, "table": true, "tr": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "center": true, "section": true,
	"article": true, "aside": true, "header": true, "footer": true, "pre": true,
}

// dropTags are removed with their contents: scripts, styles, and footnote
// markers (CourtListener renders them as <sup> or footnote-classed spans;
// the marker numbers are noise in retrieval text).
var dropTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "sup": true, "head": true,
}

// HTMLToText converts an opinion's html_with_citations to plain text with
// paragraph breaks preserved as blank lines. Citation anchors keep their
// text; entities are decoded by the parser. Input that is not HTML passes
// through with its whitespace normalised, so a plain_text fallback can take
// the same path.
func HTMLToText(src string) string {
	if !strings.Contains(src, "<") {
		return normalise(src)
	}
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return normalise(src)
	}
	var b strings.Builder
	walk(doc, &b)
	return normalise(b.String())
}

func walk(n *html.Node, b *strings.Builder) {
	switch n.Type {
	case html.TextNode:
		b.WriteString(n.Data)
		return
	case html.ElementNode:
		if dropTags[n.Data] {
			return
		}
		if isFootnote(n) {
			return
		}
		if n.Data == "br" {
			b.WriteString("\n")
			return
		}
		if blockTags[n.Data] {
			b.WriteString("\n\n")
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, b)
	}
	if n.Type == html.ElementNode && blockTags[n.Data] {
		b.WriteString("\n\n")
	}
}

// isFootnote reports whether an element is footnote markup by class or id.
func isFootnote(n *html.Node) bool {
	for _, a := range n.Attr {
		if a.Key == "class" || a.Key == "id" {
			v := strings.ToLower(a.Val)
			if strings.Contains(v, "footnote") || strings.Contains(v, "star_pagination") {
				return true
			}
		}
	}
	return false
}

var (
	spaceRun = regexp.MustCompile(`[ \t]+`)
	blankRun = regexp.MustCompile(`\n{3,}`)
)

// normalise collapses horizontal whitespace runs, trims line edges, and
// bounds blank-line runs to one (a single paragraph break).
func normalise(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, " ", " ")
	s = spaceRun.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = blankRun.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
