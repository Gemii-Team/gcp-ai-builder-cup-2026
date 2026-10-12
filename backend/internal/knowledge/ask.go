package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"google.golang.org/protobuf/types/known/timestamppb"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	knowledgev1 "kiwi/gen/kiwi/knowledge/v1"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/llm"
)

const (
	// answerTimeout bounds the call that writes an answer; the adapter has no timeout of
	// its own and a runaway model would hold the question for ever.
	answerTimeout = 90 * time.Second
	// answerMaxTokens leaves room for an answer and its citations; the adapter adds its
	// own headroom for the model's reasoning.
	answerMaxTokens = 2048
)

// notFound is the text of an answer that found nothing, by language: it is what a person
// reads, so it is written in the language of the question.
var notFound = map[string]string{
	"en": "Not found in channels you can access.",
	"th": "ไม่พบข้อมูลนี้ในช่องที่คุณเข้าถึงได้",
}

// Ask implements Service. It searches the channels the actor can read and has the model
// answer from what it found. An answer is never returned without a citation that names
// a chunk of that search: when the search finds nothing, or the model's answer cites no
// retrieved chunk, the answer is the honest not-found one, and the model is not called
// at all for the first. A failed call to the model is an error, never a not-found.
func (s *service) Ask(ctx context.Context, a authz.Actor, in AskInput) (*knowledgev1.Answer, error) {
	question := strings.TrimSpace(in.Question)
	lang := DetectLang(question)
	r, err := s.retrieve(ctx, a, Query{Text: question, Lang: lang, ChannelID: in.ChannelID})
	if err != nil {
		return nil, err
	}
	scope := make([]string, len(r.scope))
	for i, c := range r.scope {
		scope[i] = c.String()
	}
	if len(r.hits) == 0 {
		return notFoundAnswer(lang, scope), nil
	}

	out, err := s.generate(ctx, answerRequest(lang, question, r.hits))
	if err != nil {
		return nil, err
	}
	markdown, citations := validate(out, r)
	if len(citations) == 0 {
		return notFoundAnswer(lang, scope), nil
	}
	return &knowledgev1.Answer{Language: lang, Markdown: markdown, Citations: citations, Found: true, ScopeChannelIds: scope}, nil
}

func notFoundAnswer(lang string, scope []string) *knowledgev1.Answer {
	return &knowledgev1.Answer{Language: lang, Markdown: notFound[lang], Found: false, ScopeChannelIds: scope}
}

// generate runs the answer call and reads its JSON.
func (s *service) generate(ctx context.Context, req llm.Request) (modelAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, answerTimeout)
	defer cancel()
	res, err := s.llm.Generate(ctx, req)
	if err != nil {
		return modelAnswer{}, fmt.Errorf("knowledge: answer: %w", err)
	}
	var out modelAnswer
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		return modelAnswer{}, fmt.Errorf("knowledge: the model's answer is not the requested JSON: %w", err)
	}
	return out, nil
}

// keptCitation is a citation of the model that names a chunk of the search: the chunk, and
// the quote it is shown with.
type keptCitation struct {
	hit   Hit
	quote string
}

// sourceAnchor names where a citation points: a source and an anchor in it, which is
// what one chip of the answer stands for.
type sourceAnchor struct {
	kind, anchor string
	source       uuid.UUID
}

// validate keeps of the model's answer what the search backs. A citation survives only if
// its chunk_id is a chunk the search retrieved, so an id the model made up, or took from
// a chunk's text, or that names a chunk the asker cannot read, is dropped. Citations of
// one source and anchor are one: a page or an entry too long for a chunk is cut into
// pieces that share their anchor, and a person sees one place cited once, so the first
// piece cited stands for all of them, with its quote, and their markers all name it. The
// quote of a survivor is the model's when its chunk says it (see citedQuote). The
// survivors are numbered by first appearance, in the order the answer first uses their
// marker and then, for those it never uses, in the model's order; the markers in the text
// are rewritten to match, and a marker of a dropped citation is removed. It returns no
// citation when none survives or the answer has no text.
func validate(out modelAnswer, r retrieval) (string, []*knowledgev1.Citation) {
	markdown := strings.TrimSpace(cleanText(out.AnswerMarkdown))
	if markdown == "" {
		return "", nil
	}
	retrieved := make(map[uuid.UUID]Hit, len(r.hits))
	for _, h := range r.hits {
		retrieved[h.ChunkID] = h
	}

	// kept holds the distinct valid citations in the model's order; kept index k-1 is
	// what the model's citations[i] resolves to in valid[i] = k, 0 for a dropped one.
	var kept []keptCitation
	keptAt := make(map[sourceAnchor]int)
	valid := make([]int, len(out.Citations))
	for i, c := range out.Citations {
		id, err := uuid.Parse(strings.TrimSpace(c.ChunkID))
		h, ok := retrieved[id]
		if err != nil || !ok {
			continue
		}
		key := sourceAnchor{kind: h.SourceKind, anchor: citedAnchor(h.SourceAnchor), source: h.SourceID}
		k, seen := keptAt[key]
		if !seen {
			kept = append(kept, keptCitation{hit: h, quote: citedQuote(c.Quote, h.Content)})
			k = len(kept) - 1
			keptAt[key] = k
		}
		valid[i] = k + 1
	}
	if len(kept) == 0 {
		return "", nil
	}

	markers := citationMarkers(markdown, len(valid))
	number := make([]int, len(kept)+1) // number[k] is the n of kept index k-1, 0 until given
	var order []int
	give := func(k int) {
		if k > 0 && number[k] == 0 {
			order = append(order, k)
			number[k] = len(order)
		}
	}
	for _, m := range markers {
		for _, i := range m.cited {
			give(valid[i-1])
		}
	}
	for k := 1; k <= len(kept); k++ {
		give(k)
	}

	var text strings.Builder
	pos := 0
	for _, m := range markers {
		text.WriteString(markdown[pos:m.start])
		pos = m.end
		var shown []int
		for _, i := range m.cited {
			if k := valid[i-1]; k > 0 && !slices.Contains(shown, number[k]) {
				shown = append(shown, number[k])
			}
		}
		if len(shown) == 0 {
			continue // a marker of dropped citations goes, with the space before it
		}
		text.WriteString(m.space)
		for _, n := range shown {
			text.WriteString("[" + strconv.Itoa(n) + "]")
		}
	}
	text.WriteString(markdown[pos:])

	citations := make([]*knowledgev1.Citation, len(order))
	for i, k := range order {
		kc := kept[k-1]
		citations[i] = &knowledgev1.Citation{
			N: int32(i + 1), ChunkId: kc.hit.ChunkID.String(), Quote: kc.quote,
			Source: sourceOf(kc.hit, r.meta[kc.hit.ChunkID]),
		}
	}
	return strings.TrimSpace(text.String()), citations
}

// ---- markers ----------------------------------------------------------------

// markerGroup is a bracketed run of numbers: [2], and also [1, 2] and [1-3], which the
// model writes though it is told not to. Group 1 is the space before it.
var markerGroup = regexp.MustCompile(`([ \t]?)\[\s*(\d+(?:\s*[,;\-–]\s*\d+)*)\s*\]`)

// marker is a citation marker of the answer's text: a bracketed group, with the space
// before it, that names citations of the model's list. cited holds their numbers in the
// list, counted from 1, each once.
type marker struct {
	start, end int
	space      string
	cited      []int
}

// citationMarkers finds the markers in markdown that cite one of the n citations of the
// model's list. Only text the model wrote as prose is read: the inside of code (a fence
// or a span) and the forms of a link, [3](url), and of a reference, [3]: url at the start
// of a line, are not markers, and neither is a group that names no number of the list,
// such as [2024] or retries[0]. A number of a group beyond the list is no citation and is
// left out of the group.
func citationMarkers(markdown string, n int) []marker {
	code := codeRanges(markdown)
	var out []marker
	for _, loc := range markerGroup.FindAllStringSubmatchIndex(markdown, -1) {
		start, end := loc[0], loc[1]
		bracket := loc[3] // the "[" follows the space of group 1
		if slices.ContainsFunc(code, func(c [2]int) bool { return bracket >= c[0] && bracket < c[1] }) {
			continue
		}
		if next := byteAt(markdown, end); next == '(' || (next == ':' && startsLine(markdown, bracket)) {
			continue
		}
		if cited := groupNumbers(markdown[loc[4]:loc[5]], n); len(cited) > 0 {
			out = append(out, marker{start: start, end: end, space: markdown[loc[2]:loc[3]], cited: cited})
		}
	}
	return out
}

func byteAt(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// startsLine reports whether only up to three spaces lie between the start of the line
// and i.
func startsLine(s string, i int) bool {
	from := strings.LastIndexByte(s[:i], '\n') + 1
	return i-from <= 3 && strings.Trim(s[from:i], " ") == ""
}

var (
	listSeparator  = regexp.MustCompile(`[,;]`)
	rangeSeparator = regexp.MustCompile(`[-–]`)
)

// groupNumbers is the numbers of 1 to n that the inside of a marker group names, in
// order and each once; "1-3" names 1, 2 and 3.
func groupNumbers(inner string, n int) []int {
	var out []int
	add := func(i int) {
		if i >= 1 && i <= n && !slices.Contains(out, i) {
			out = append(out, i)
		}
	}
	for _, item := range listSeparator.Split(inner, -1) {
		var bounds []int
		for _, digits := range rangeSeparator.Split(item, -1) {
			i, _ := strconv.Atoi(strings.TrimSpace(digits)) // too many digits read as 0, which is no number of the list
			bounds = append(bounds, i)
		}
		if len(bounds) == 2 && bounds[0] <= bounds[1] {
			for i := max(bounds[0], 1); i <= min(bounds[1], n); i++ {
				add(i)
			}
			continue
		}
		for _, i := range bounds {
			add(i)
		}
	}
	return out
}

// codeRanges returns the byte ranges of markdown that are code: fenced blocks, which
// run to the closing fence or to the end of the text, and inline spans, which a run of
// backticks opens and the next run of as many closes within the paragraph. A run of
// backticks that closes nothing is plain text.
func codeRanges(markdown string) [][2]int {
	var out [][2]int
	for i := 0; i < len(markdown); {
		if i == 0 || markdown[i-1] == '\n' {
			if end, ok := fencedBlock(markdown, i); ok {
				out = append(out, [2]int{i, end})
				i = end
				continue
			}
		}
		if markdown[i] != '`' {
			i++
			continue
		}
		run := backtickRun(markdown, i)
		limit := len(markdown)
		if blank := strings.Index(markdown[i:], "\n\n"); blank >= 0 {
			limit = i + blank
		}
		closing := closingRun(markdown[:limit], i+run, run)
		if closing < 0 {
			i += run
			continue
		}
		out = append(out, [2]int{i, closing + run})
		i = closing + run
	}
	return out
}

// backtickRun is the length of the run of backticks that starts at s[i].
func backtickRun(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}

// closingRun finds, from index from on, the next run of exactly n backticks and returns
// where it starts, or -1.
func closingRun(s string, from, n int) int {
	for j := from; j < len(s); {
		if s[j] != '`' {
			j++
			continue
		}
		m := backtickRun(s, j)
		if m == n {
			return j
		}
		j += m
	}
	return -1
}

// fenceAt reads a code fence at the start of the line at i: at most three spaces and a
// run of backticks or tildes. It returns the character, the length of the run (0 when
// there is none) and where the run ends.
func fenceAt(s string, i int) (char byte, n, end int) {
	end = i
	for end < len(s) && end-i < 3 && s[end] == ' ' {
		end++
	}
	if end >= len(s) || (s[end] != '`' && s[end] != '~') {
		return 0, 0, end
	}
	char = s[end]
	for end+n < len(s) && s[end+n] == char {
		n++
	}
	return char, n, end + n
}

// lineEnd is the index of the newline that ends the line holding s[i], or len(s).
func lineEnd(s string, i int) int {
	if k := strings.IndexByte(s[i:], '\n'); k >= 0 {
		return i + k
	}
	return len(s)
}

// fencedBlock reports whether the line at line opens a fenced block, that is three or
// more backticks or tildes after at most three spaces, and returns where the block ends:
// after the closing fence, a line of the same character and at least as long with nothing
// else on it, or at the end of the text when there is none.
func fencedBlock(s string, line int) (end int, ok bool) {
	char, n, after := fenceAt(s, line)
	if n < 3 {
		return 0, false
	}
	if char == '`' && strings.Contains(s[after:lineEnd(s, after)], "`") {
		return 0, false // a span such as ```code``` on one line, not a fence
	}
	for next := lineEnd(s, line) + 1; next < len(s); next = lineEnd(s, next) + 1 {
		c, m, rest := fenceAt(s, next)
		if c == char && m >= n && strings.TrimSpace(s[rest:lineEnd(s, rest)]) == "" {
			return lineEnd(s, rest), true
		}
	}
	return len(s), true
}

// ---- quotes -----------------------------------------------------------------

// excerptRunes is the length of the excerpt that stands in for a quote the chunk does not
// hold.
const excerptRunes = 160

// citedQuote is the quote a citation shows. The model's own when the chunk says it, so
// that it is evidence a person can find in the source: the chunk contains the quote once
// both are read alike (see foldText). Otherwise the start of the chunk, because the model
// paraphrased, or made the quote up, and a passage the source does not hold must never
// pass for its words. The citation itself stays: its chunk_id is a chunk of the search.
func citedQuote(quote, content string) string {
	quote = strings.TrimSpace(cleanText(quote))
	if q := foldText(quote); q != "" && strings.Contains(foldText(content), q) {
		return quote
	}
	return excerpt(content)
}

var zeroWidth = strings.NewReplacer("\u200b", "", "\u200c", "")

// foldText is text read alike whatever its case, its white space, its Unicode form and
// the zero-width characters in it: a model copies Thai and accented text with those
// differences, and the prompt itself puts a zero-width space into a tag it defangs.
func foldText(s string) string {
	s = norm.NFC.String(cases.Fold().String(zeroWidth.Replace(s)))
	return strings.Join(strings.Fields(s), " ")
}

// excerpt is the start of content on one line, of about excerptRunes runes. A longer text
// is cut on a rune boundary that does not part a Thai consonant from its marks or a
// leading vowel from its consonant, and ends with an ellipsis.
func excerpt(content string) string {
	text := strings.Join(strings.Fields(content), " ")
	runes := []rune(text)
	if len(runes) <= excerptRunes {
		return text
	}
	cut := excerptRunes
	for cut > 1 && (noBreakBefore(runes[cut]) || noBreakAfter(runes[cut-1])) {
		cut--
	}
	return strings.TrimSpace(string(runes[:cut])) + "…"
}

// sourceOf is the typed reference behind a citation, built from the chunk and the
// display snapshot it keeps of its source alone, so that no other domain is asked at
// query time. The id of a chat chunk's source is its channel, the chat being indexed
// per channel; the anchor is where the source begins to speak (see citedAnchor).
func sourceOf(h Hit, m sourceMeta) *chatv1.Source {
	src := &chatv1.Source{Id: h.SourceID.String(), Anchor: citedAnchor(h.SourceAnchor), Label: m.Label}
	switch h.SourceKind {
	case kindChat:
		src.Kind = chatv1.SourceKind_SOURCE_KIND_MESSAGE
	case kindMeeting:
		src.Kind = chatv1.SourceKind_SOURCE_KIND_MEETING
	case kindDoc:
		src.Kind = chatv1.SourceKind_SOURCE_KIND_FILE
	}
	if h.ChannelID != nil {
		src.ChannelId = h.ChannelID.String()
	}
	if m.At != nil {
		src.At = timestamppb.New(*m.At)
	}
	if m.OffsetMS != nil {
		src.OffsetMs = new(*m.OffsetMS)
	}
	if n, ok := strings.CutPrefix(h.SourceAnchor, "page:"); ok {
		if page, err := strconv.ParseInt(n, 10, 32); err == nil {
			src.Page = new(int32(page))
		}
	}
	return src
}

// citedAnchor is a chunk's anchor as a Source carries it: a range of messages or of
// transcript entries is named by its first, "seq:10-14" by "10" and "entries:3-9" by
// "3"; a page ("page:2") and the summary of a meeting ("summary") stay as they are.
func citedAnchor(anchor string) string {
	for _, prefix := range []string{"seq:", "entries:"} {
		if rest, ok := strings.CutPrefix(anchor, prefix); ok {
			first, _, _ := strings.Cut(rest, "-")
			return first
		}
	}
	return anchor
}
