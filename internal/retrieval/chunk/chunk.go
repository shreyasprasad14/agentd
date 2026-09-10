// Package chunk splits opinion text into retrieval chunks: structure first
// (paragraphs, section headings), size second (target/max/min with sentence
// splitting), per spec §9. Split is pure; the same input always yields the
// same chunks.
package chunk

import (
	"regexp"
	"strings"
	"unicode"
)

// Chunk is one retrieval unit. Content is what gets embedded and stored:
// the overlap prefix (the previous chunk's last sentence) followed by the
// body. CharStart and CharEnd span the body in the input text, overlap
// excluded, so Content without its overlap equals input[CharStart:CharEnd].
type Chunk struct {
	Ordinal   int
	Section   string
	Content   string
	CharStart int
	CharEnd   int
}

// Options are the size bounds, in bytes of UTF-8 text.
type Options struct {
	// Target is where a chunk stops accepting more paragraphs.
	Target int
	// Max is the hard cap; a single paragraph longer than this is split at
	// sentence boundaries.
	Max int
	// Min is the smallest chunk allowed to stand alone; a shorter trailing
	// fragment is merged backward when the merge stays under Max.
	Min int
	// Overlap caps the prepended last sentence of the previous chunk.
	Overlap int
}

// DefaultOptions matches the plan: ~300 tokens per chunk, comfortably inside
// mxbai-embed-large's 512-token window with the query prefix.
func DefaultOptions() Options {
	return Options{Target: 1200, Max: 1800, Min: 200, Overlap: 200}
}

// span is a half-open byte range in the input.
type span struct{ start, end int }

// Split chunks text with DefaultOptions.
func Split(text string) []Chunk { return SplitWith(text, DefaultOptions()) }

// SplitWith chunks text with explicit bounds.
func SplitWith(text string, opt Options) []Chunk {
	if opt.Target <= 0 || opt.Max <= 0 {
		opt = DefaultOptions()
	}
	paras := paragraphs(text)
	if len(paras) == 0 {
		return nil
	}

	// First pass: group paragraph spans into chunk bodies under a section.
	type body struct {
		section string
		span    span
	}
	var bodies []body
	section := ""
	open := -1 // index in bodies of the chunk being accumulated, -1 none

	for _, p := range paras {
		line := text[p.start:p.end]
		if isHeading(line) {
			section = strings.TrimSpace(line)
			open = -1
			continue
		}
		if p.end-p.start > opt.Max {
			// An oversize paragraph stands alone, split at sentences.
			open = -1
			for _, sp := range splitOversize(text, p, opt) {
				bodies = append(bodies, body{section, sp})
			}
			continue
		}
		if open >= 0 && (p.end-bodies[open].span.start) <= opt.Target {
			bodies[open].span.end = p.end
			continue
		}
		bodies = append(bodies, body{section, p})
		open = len(bodies) - 1
	}

	// Second pass: merge trailing fragments backward. A fragment merges into
	// the previous body only when both share a section (so the spans are
	// contiguous in the input) and the merge stays under Max.
	merged := bodies[:0]
	for _, b := range bodies {
		n := len(merged)
		if b.span.end-b.span.start < opt.Min && n > 0 &&
			merged[n-1].section == b.section &&
			b.span.end-merged[n-1].span.start <= opt.Max {
			merged[n-1].span.end = b.span.end
			continue
		}
		merged = append(merged, b)
	}

	// Third pass: ordinals and overlap.
	out := make([]Chunk, 0, len(merged))
	for i, b := range merged {
		c := Chunk{
			Ordinal:   i,
			Section:   b.section,
			CharStart: b.span.start,
			CharEnd:   b.span.end,
		}
		bodyText := text[b.span.start:b.span.end]
		if i > 0 {
			prev := merged[i-1]
			if ov := lastSentence(text[prev.span.start:prev.span.end], opt.Overlap); ov != "" {
				c.Content = ov + "\n" + bodyText
			}
		}
		if c.Content == "" {
			c.Content = bodyText
		}
		out = append(out, c)
	}
	return out
}

// paragraphs returns the spans of maximal runs of non-blank lines, trimmed to
// their text (no leading or trailing whitespace inside a span). A line of
// only whitespace counts as blank.
func paragraphs(text string) []span {
	var out []span
	paraStart := -1
	flush := func(end int) {
		if paraStart < 0 {
			return
		}
		if s := trimSpan(text, paraStart, end); s.start < s.end {
			out = append(out, s)
		}
		paraStart = -1
	}
	lineStart := 0
	for lineStart <= len(text) {
		lineEnd := len(text)
		if i := strings.IndexByte(text[lineStart:], '\n'); i >= 0 {
			lineEnd = lineStart + i
		}
		if strings.TrimSpace(text[lineStart:lineEnd]) == "" {
			flush(lineStart)
		} else if paraStart < 0 {
			paraStart = lineStart
		}
		if lineEnd == len(text) {
			break
		}
		lineStart = lineEnd + 1
	}
	flush(len(text))
	return out
}

const asciiSpace = " \t\r\n\v\f"

func trimSpan(text string, start, end int) span {
	seg := text[start:end]
	lt := strings.TrimLeft(seg, asciiSpace)
	newStart := start + (len(seg) - len(lt))
	rt := strings.TrimRight(lt, asciiSpace)
	return span{newStart, newStart + len(rt)}
}

// numberedHeading matches "II.", "IV. Discussion", "A. The standard", "1. ..."
var numberedHeading = regexp.MustCompile(`^(?:[IVXLCDM]+|[A-Z]|\d{1,2})\.(?:\s|$)`)

// isHeading reports whether a paragraph is a section heading: a single short
// line that is numbered, all caps, or title case. Ordinary short sentences
// end with a period and are excluded (numbered headings excepted).
func isHeading(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" || len(line) > 100 || strings.ContainsRune(line, '\n') {
		return false
	}
	if numberedHeading.MatchString(line) {
		return true
	}
	if strings.HasSuffix(line, ".") {
		return false
	}
	letters := 0
	upper := 0
	for _, r := range line {
		if unicode.IsLetter(r) {
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	if letters == 0 {
		return false
	}
	if upper == letters {
		return true // ALL CAPS
	}
	return isTitleCase(line)
}

// titleStopwords may stay lowercase inside a title-case heading.
var titleStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "as": true, "at": true, "but": true,
	"by": true, "for": true, "in": true, "of": true, "on": true, "or": true,
	"the": true, "to": true, "with": true,
}

func isTitleCase(line string) bool {
	words := strings.Fields(line)
	if len(words) == 0 || len(words) > 8 {
		return false
	}
	for i, w := range words {
		r := []rune(w)[0]
		if !unicode.IsLetter(r) {
			continue
		}
		if unicode.IsUpper(r) {
			continue
		}
		if i > 0 && titleStopwords[strings.ToLower(w)] {
			continue
		}
		return false
	}
	return true
}

// splitOversize cuts one paragraph into sentence-aligned pieces no longer
// than Max, aiming for Target. A single sentence longer than Max is cut hard
// at Max, which legal prose essentially never requires.
func splitOversize(text string, p span, opt Options) []span {
	var out []span
	start := p.start
	for start < p.end {
		if p.end-start <= opt.Max {
			out = append(out, span{start, p.end})
			break
		}
		cut := sentenceCut(text[start:p.end], opt.Target, opt.Max)
		out = append(out, span{start, start + cut})
		// Skip whitespace between sentences so the next piece starts on text.
		next := start + cut
		for next < p.end && (text[next] == ' ' || text[next] == '\n' || text[next] == '\t') {
			next++
		}
		start = next
	}
	return out
}

// sentenceCut returns the byte offset to cut s at: the last sentence end at
// or before target, else the first one before max, else max.
func sentenceCut(s string, target, max int) int {
	if max > len(s) {
		max = len(s)
	}
	best := -1
	for _, end := range sentenceEnds(s) {
		if end > max {
			break
		}
		if end <= target {
			best = end
		} else if best == -1 {
			best = end
		}
	}
	if best == -1 {
		return max
	}
	return best
}

// abbreviations that end with a period but do not end a sentence.
var abbreviations = map[string]bool{
	"v": true, "id": true, "no": true, "u.s": true, "cf": true, "e.g": true,
	"i.e": true, "j": true, "jr": true, "mr": true, "mrs": true, "ms": true,
	"dr": true, "st": true, "inc": true, "corp": true, "co": true, "art": true,
	"sec": true, "stat": true, "cal": true, "fed": true, "supp": true,
}

// sentenceEnds returns the byte offsets just past each sentence terminator
// in s, in order.
func sentenceEnds(s string) []int {
	var out []int
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '.' && c != '!' && c != '?' {
			continue
		}
		// Closing quotes or parens belong to the sentence.
		end := i + 1
		for end < len(s) && (s[end] == '"' || s[end] == '\'' || s[end] == ')' || s[end] == ']') {
			end++
		}
		if end < len(s) && !unicode.IsSpace(rune(s[end])) {
			continue // mid-token period ("3.14", "U.S")
		}
		if c == '.' && isAbbreviation(s[:i]) {
			continue
		}
		out = append(out, end)
	}
	return out
}

func isAbbreviation(prefix string) bool {
	j := len(prefix)
	for j > 0 {
		r := prefix[j-1]
		if r == ' ' || r == '\n' || r == '\t' || r == '(' || r == '"' {
			break
		}
		j--
	}
	word := strings.ToLower(strings.TrimRight(prefix[j:], "."))
	if word == "" {
		return false
	}
	if abbreviations[word] {
		return true
	}
	// A single letter followed by a period is an initial, not an end.
	return len([]rune(word)) == 1
}

// lastSentence returns the final sentence of s, capped at max bytes; the cap
// truncates from the front so the sentence's end (the part that straddles the
// boundary) is what survives.
func lastSentence(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" || max <= 0 {
		return ""
	}
	ends := sentenceEnds(s)
	start := 0
	if len(ends) > 1 {
		// The last complete sentence starts after the second-to-last end.
		prev := ends[len(ends)-1]
		if prev == len(s) && len(ends) >= 2 {
			prev = ends[len(ends)-2]
		}
		for prev < len(s) && unicode.IsSpace(rune(s[prev])) {
			prev++
		}
		start = prev
	}
	out := s[start:]
	if len(out) > max {
		cut := len(out) - max
		// Advance to a rune boundary, then to the next word.
		for cut < len(out) && (out[cut]&0xC0) == 0x80 {
			cut++
		}
		if sp := strings.IndexAny(out[cut:], " \n\t"); sp >= 0 {
			cut += sp + 1
		}
		out = out[cut:]
	}
	return strings.TrimSpace(out)
}
