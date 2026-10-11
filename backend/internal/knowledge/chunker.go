package knowledge

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
	"kiwi/internal/docs"
	"kiwi/internal/platform/llm"
)

const (
	// chunkMaxTokens is the size of a chunk in estimated tokens (llm.EstimateTokens).
	// Ollama truncates an embedding input above 2048 tokens without a word, and the
	// estimate is conservative for Thai, so a chunk this size is always embedded whole.
	chunkMaxTokens = 512
	// chunkOverlapTokens is how much of a chunk's end the next chunk of the same text
	// repeats, so a sentence cut by a boundary is whole in one of the two.
	chunkOverlapTokens = 64
	// chatWindow is the span of one chat source: the messages of a channel sent in the
	// same five minutes of the clock are indexed together.
	chatWindow = 5 * time.Minute
)

// ChunkText cuts text into chunks of at most maxTokens estimated tokens
// (llm.EstimateTokens), each repeating about overlapTokens of the end of the one
// before it. It is rune-aware and made for Thai, which has no spaces: a chunk ends at
// the best boundary in reach (after a blank line, a line, a sentence, a word, a change
// between Thai and other script, and only then anywhere), and never at a place that
// would split a character cluster, so no chunk starts with a combining mark such as
// U+0E31, U+0E34-U+0E3A or U+0E47-U+0E4E, a vowel that follows its consonant, or ends
// on a vowel that leads it. Surrounding whitespace is trimmed; text without anything
// but whitespace yields no chunk. maxTokens below 1 is taken as 1 and the overlap is
// held to half of maxTokens.
func ChunkText(text string, maxTokens, overlapTokens int) []string {
	maxTokens = max(maxTokens, 1)
	overlapTokens = min(max(overlapTokens, 0), maxTokens/2)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if llm.EstimateTokens(text) <= maxTokens {
		return []string{strings.TrimSpace(text)}
	}

	c := newCutter(text)
	n := len(c.runes)
	var chunks []string
	for start, floor := c.skipSpace(0), 0; start < n; {
		end := c.fit(start, maxTokens)
		if end == n {
			chunks = append(chunks, c.slice(start, n))
			break
		}
		cut := c.cut(start, end, floor)
		chunks = append(chunks, c.slice(start, cut))
		if c.skipSpace(cut) == n {
			break // only whitespace is left: a chunk of nothing but the overlap would repeat the last
		}
		floor = cut
		start = c.skipSpace(c.next(start, cut, overlapTokens))
	}
	return chunks
}

// Boundary classes, best last. A chunk ends at the best class in reach.
const (
	classSafe      = iota // anywhere that splits no character cluster
	classScript           // between Thai and other letters or digits
	classWord             // after whitespace
	classSentence         // after sentence-ending punctuation and whitespace
	classLine             // after a line break
	classParagraph        // after a blank line
)

// cutter finds the places where one text may be cut. positions are rune indexes.
type cutter struct {
	text  string
	runes []rune
	// offs[i] is the byte offset of runes[i] in text; offs[len(runes)] is len(text).
	offs []int
}

func newCutter(text string) *cutter {
	n := utf8.RuneCountInString(text)
	c := &cutter{text: text, runes: make([]rune, 0, n), offs: make([]int, 0, n+1)}
	for i, r := range text {
		c.runes = append(c.runes, r)
		c.offs = append(c.offs, i)
	}
	c.offs = append(c.offs, len(text))
	return c
}

// tokens estimates runes[a:b], by the same rule as everything else that sizes text.
func (c *cutter) tokens(a, b int) int { return llm.EstimateTokens(c.text[c.offs[a]:c.offs[b]]) }

func (c *cutter) slice(a, b int) string { return strings.TrimSpace(c.text[c.offs[a]:c.offs[b]]) }

func (c *cutter) skipSpace(p int) int {
	for p < len(c.runes) && unicode.IsSpace(c.runes[p]) {
		p++
	}
	return p
}

// fit returns the largest end for which runes[start:end] is at most maxTokens. One
// rune is at most one token, so end is always past start, and a rune is at least a
// quarter of a token, so the search need not look further than 4*maxTokens runes.
func (c *cutter) fit(start, maxTokens int) int {
	lo, hi := start+1, min(len(c.runes), start+4*maxTokens)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if c.tokens(start, mid) <= maxTokens {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// cut picks where the chunk that starts at start and may reach end (end is before the
// end of the text) really stops: the best class of boundary in the second half of its
// reach, else in all of it, else any safe one, else end. It stops only after floor, the
// end of the chunk before, so that a chunk that begins in the overlap still brings text
// of its own instead of ending where the last one did.
func (c *cutter) cut(start, end, floor int) int {
	upper := start + (end-start)/2
	var anywhere, late [classParagraph + 1]int // the last position of each class; 0 = none
	for p := end; p > max(start, floor); p-- {
		class, ok := c.boundary(p)
		if !ok {
			continue
		}
		if anywhere[class] == 0 {
			anywhere[class] = p
		}
		if p > upper && late[class] == 0 {
			late[class] = p
		}
	}
	for _, found := range [2][classParagraph + 1]int{late, anywhere} {
		for class := classParagraph; class > classSafe; class-- {
			if found[class] != 0 {
				return found[class]
			}
		}
	}
	if anywhere[classSafe] != 0 {
		return anywhere[classSafe]
	}
	return end
}

// next is where the chunk after runes[start:cut] starts: the repeated tail of about
// overlapTokens, begun at the first boundary of any class in it (a word, a line, a
// script change) and only without one at a safe cut, which can fall inside a word. A
// chunk of no more than overlapTokens has nothing to repeat that would not be the whole
// chunk again, so the next one starts where it stopped.
func (c *cutter) next(start, cut, overlapTokens int) int {
	if overlapTokens == 0 || c.tokens(start, cut) <= overlapTokens {
		return cut
	}
	lo, hi := start+1, cut // the earliest o with a tail of at most overlapTokens
	for lo < hi {
		mid := lo + (hi-lo)/2
		if c.tokens(mid, cut) <= overlapTokens {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	safe := 0
	for p := lo; p < cut; p++ {
		class, ok := c.boundary(p)
		switch {
		case !ok:
		case class > classSafe:
			return p
		case safe == 0:
			safe = p
		}
	}
	if safe != 0 {
		return safe
	}
	return cut
}

// boundary reports whether a chunk may end before runes[p] and how good a place that
// is. It is not allowed to when that would cut a character cluster apart.
func (c *cutter) boundary(p int) (class int, ok bool) {
	n := len(c.runes)
	prev := c.runes[p-1]
	if noBreakAfter(prev) || (p < n && noBreakBefore(c.runes[p])) {
		return 0, false
	}
	if p == n {
		return classParagraph, true
	}
	next := c.runes[p]
	if unicode.IsSpace(prev) {
		if unicode.IsSpace(next) {
			return classSafe, true // inside a run of whitespace; its end is the better place
		}
		return c.afterSpace(p), true
	}
	if !unicode.IsSpace(next) && isThai(prev) != isThai(next) && isWordRune(prev) && isWordRune(next) {
		return classScript, true
	}
	return classSafe, true
}

// afterSpace classifies the end of the whitespace run before position p.
func (c *cutter) afterSpace(p int) int {
	lines, q := 0, p-1
	for ; q >= 0 && unicode.IsSpace(c.runes[q]); q-- {
		if c.runes[q] == '\n' {
			lines++
		}
	}
	switch {
	case lines >= 2:
		return classParagraph
	case lines == 1:
		return classLine
	case q >= 0 && isSentenceEnd(c.runes[q]):
		return classSentence
	}
	return classWord
}

func isThai(r rune) bool { return r >= 0x0E00 && r <= 0x0E7F }

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) }

func isSentenceEnd(r rune) bool {
	switch r {
	case '.', '!', '?', '…', '。', '！', '？':
		return true
	}
	return false
}

// noBreakBefore is true for a rune that belongs to the rune before it: a combining
// mark (the Thai ones are U+0E31, U+0E34-U+0E3A and U+0E47-U+0E4E), a Thai vowel that
// follows its consonant, or a joiner.
func noBreakBefore(r rune) bool {
	switch r {
	case 0x0E30, 0x0E32, 0x0E33, 0x0E45, 0x200D:
		return true
	}
	return unicode.IsMark(r)
}

// noBreakAfter is true for a rune that belongs to the rune after it: a Thai vowel
// written before its consonant (U+0E40-U+0E44), or a joiner.
func noBreakAfter(r rune) bool { return (r >= 0x0E40 && r <= 0x0E44) || r == 0x200D }

// group is a run of consecutive lines packed into one chunk, or one piece of a line
// that was too long to be packed.
type group struct {
	first, last int // the lines it covers, as indexes
	piece       int // 0, or n for the nth piece of a line cut apart
	text        string
}

// packLines packs consecutive lines into chunks of at most maxTokens, in order and
// without overlap: a line stays whole while it fits, and a line that cannot fit even
// alone is cut by ChunkText into chunks of its own. Every line must hold text.
func packLines(lines []string, maxTokens, overlapTokens int) []group {
	var out []group
	var cur strings.Builder
	first := 0
	flush := func(last int) {
		if cur.Len() > 0 {
			out = append(out, group{first: first, last: last, text: cur.String()})
			cur.Reset()
		}
	}
	for i, line := range lines {
		if llm.EstimateTokens(line) > maxTokens {
			flush(i - 1)
			for k, piece := range ChunkText(line, maxTokens, overlapTokens) {
				out = append(out, group{first: i, last: i, piece: k, text: piece})
			}
			continue
		}
		if cur.Len() > 0 && llm.EstimateTokens(cur.String()+"\n"+line) > maxTokens {
			flush(i - 1)
		}
		if cur.Len() == 0 {
			first = i
		} else {
			cur.WriteByte('\n')
		}
		cur.WriteString(line)
	}
	flush(len(lines) - 1)
	return out
}

// sourceMeta is the display snapshot a chunk keeps of its source, so that a citation
// renders without a call into another domain. At is the time of a chat chunk's first
// message and OffsetMS the start of a meeting chunk's first entry; each is left out
// when it is unknown.
type sourceMeta struct {
	Label    string     `json:"label"`
	At       *time.Time `json:"at,omitempty"`
	OffsetMS *int64     `json:"offset_ms,omitempty"`
}

// draft is a chunk before it is embedded: where it sits in its source and what it says.
type draft struct {
	anchor  string // source_anchor, for example seq:10-14, entries:3-9, page:2 or summary
	piece   int    // see pieceID
	content string
	meta    sourceMeta
}

// chatDrafts returns the chunks of the messages of one window, oldest first. A message
// is one "author: body" line; one that carries an artifact is left out, as it is
// indexed from its own source, and so is one without text. author names a message's
// author.
func chatDrafts(msgs []*chatv1.Message, author func(*chatv1.Message) string) []draft {
	kept := make([]*chatv1.Message, 0, len(msgs))
	lines := make([]string, 0, len(msgs))
	for _, m := range msgs {
		body := strings.TrimSpace(m.GetBody())
		if m.GetArtifact() != nil || body == "" {
			continue
		}
		kept = append(kept, m)
		lines = append(lines, author(m)+": "+body)
	}
	var out []draft
	for _, g := range packLines(lines, chunkMaxTokens, chunkOverlapTokens) {
		first, last := kept[g.first], kept[g.last]
		out = append(out, draft{
			anchor:  fmt.Sprintf("seq:%d-%d", first.GetSeq(), last.GetSeq()),
			piece:   g.piece,
			content: g.text,
			meta:    sourceMeta{Label: author(first), At: new(first.GetCreatedAt().AsTime().UTC())},
		})
	}
	return out
}

// entryDrafts returns the chunks of a transcript: runs of consecutive entries as
// "speaker: text" lines, anchored by the index of their first and last entry. Each
// carries the start of its first entry, unset for a transcript without timing.
func entryDrafts(label string, entries []*meetingsv1.Entry) []draft {
	kept := make([]*meetingsv1.Entry, 0, len(entries))
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		text := strings.TrimSpace(e.GetText())
		if text == "" {
			continue
		}
		if speaker := strings.TrimSpace(e.GetSpeaker()); speaker != "" {
			text = speaker + ": " + text
		}
		kept = append(kept, e)
		lines = append(lines, text)
	}
	var out []draft
	for _, g := range packLines(lines, chunkMaxTokens, chunkOverlapTokens) {
		first, last := kept[g.first], kept[g.last]
		meta := sourceMeta{Label: label}
		if first.StartMs != nil {
			meta.OffsetMS = new(*first.StartMs)
		}
		out = append(out, draft{
			anchor:  fmt.Sprintf("entries:%d-%d", first.GetIdx(), last.GetIdx()),
			piece:   g.piece,
			content: g.text,
			meta:    meta,
		})
	}
	return out
}

// summaryAnchor is the anchor of the chunk that holds a meeting's summary.
const summaryAnchor = "summary"

// meetingSummary is what a meeting's summary chunk says.
type meetingSummary struct {
	Summary       string
	Decisions     []string
	OpenQuestions []string
}

// summaryDrafts returns the chunk of a meeting's summary, its decisions and its open
// questions, so a question about what was decided finds the decision as one passage.
func summaryDrafts(label string, s meetingSummary) []draft {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(s.Summary))
	for _, list := range []struct {
		heading string
		items   []string
	}{{"Decisions", s.Decisions}, {"Open questions", s.OpenQuestions}} {
		var items []string
		for _, it := range list.items {
			if it = strings.TrimSpace(it); it != "" {
				items = append(items, "- "+it)
			}
		}
		if len(items) > 0 {
			b.WriteString("\n\n" + list.heading + ":\n" + strings.Join(items, "\n"))
		}
	}
	var out []draft
	for k, piece := range ChunkText(b.String(), chunkMaxTokens, chunkOverlapTokens) {
		out = append(out, draft{anchor: summaryAnchor, piece: k, content: piece, meta: sourceMeta{Label: label}})
	}
	return out
}

// pageDrafts returns the chunks of a document, page by page; a page longer than a
// chunk is cut into several, all anchored at its page.
func pageDrafts(label string, pages []docs.Page) []draft {
	var out []draft
	for _, p := range pages {
		for k, piece := range ChunkText(cleanText(p.Markdown), chunkMaxTokens, chunkOverlapTokens) {
			out = append(out, draft{anchor: fmt.Sprintf("page:%d", p.N), piece: k, content: piece, meta: sourceMeta{Label: label}})
		}
	}
	return out
}

// cleanText makes text something Postgres stores: valid UTF-8 without NUL.
func cleanText(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "\uFFFD")
}

// chunkLang is the language of a chunk: "th" when any rune is Thai (U+0E00-U+0E7F),
// otherwise "en".
func chunkLang(content string) string {
	for _, r := range content {
		if isThai(r) {
			return "th"
		}
	}
	return "en"
}
