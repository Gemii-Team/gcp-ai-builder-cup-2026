package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
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

// marker is a citation marker in an answer, with the space before it.
var marker = regexp.MustCompile(`([ \t]?)\[(\d+)\]`)

// keptCitation is a citation of the model that names a chunk of the search.
type keptCitation struct {
	hit   Hit
	quote string
}

// validate keeps of the model's answer what the search backs. A citation survives only if
// its chunk_id is a chunk the search retrieved, so an id the model made up, or took from
// a chunk's text, or that names a chunk the asker cannot read, is dropped; a chunk the
// model cites twice is cited once. The survivors are numbered by first appearance, in the
// order the answer first uses their marker and then, for those it never uses, in the
// model's order; the markers in the text are rewritten to match, and a marker of a
// dropped citation is removed. It returns no citation when none survives or the answer
// has no text.
func validate(out modelAnswer, r retrieval) (string, []*knowledgev1.Citation) {
	markdown := strings.TrimSpace(cleanText(out.AnswerMarkdown))
	if markdown == "" {
		return "", nil
	}
	retrieved := make(map[uuid.UUID]Hit, len(r.hits))
	for _, h := range r.hits {
		retrieved[h.ChunkID] = h
	}

	// kept holds the distinct valid citations in the model's order; kept index k+1 is
	// what the model's citations[i] resolves to in valid[i], 0 for a dropped one.
	var kept []keptCitation
	valid := make([]int, len(out.Citations))
	for i, c := range out.Citations {
		id, err := uuid.Parse(strings.TrimSpace(c.ChunkID))
		h, ok := retrieved[id]
		if err != nil || !ok {
			continue
		}
		at := -1
		for k, kc := range kept {
			if kc.hit.ChunkID == id {
				at = k
				break
			}
		}
		if at < 0 {
			kept = append(kept, keptCitation{hit: h, quote: strings.TrimSpace(cleanText(c.Quote))})
			at = len(kept) - 1
		}
		valid[i] = at + 1
	}
	if len(kept) == 0 {
		return "", nil
	}
	resolve := func(digits string) int {
		i, err := strconv.Atoi(digits)
		if err != nil || i < 1 || i > len(valid) {
			return 0
		}
		return valid[i-1]
	}

	number := make([]int, len(kept)+1) // number[k] is the n of kept index k, 0 until given
	var order []int
	give := func(k int) {
		if k > 0 && number[k] == 0 {
			order = append(order, k)
			number[k] = len(order)
		}
	}
	for _, m := range marker.FindAllStringSubmatch(markdown, -1) {
		give(resolve(m[2]))
	}
	for k := 1; k <= len(kept); k++ {
		give(k)
	}
	markdown = marker.ReplaceAllStringFunc(markdown, func(text string) string {
		m := marker.FindStringSubmatch(text)
		k := resolve(m[2])
		if k == 0 {
			return ""
		}
		return m[1] + "[" + strconv.Itoa(number[k]) + "]"
	})

	citations := make([]*knowledgev1.Citation, len(order))
	for i, k := range order {
		kc := kept[k-1]
		citations[i] = &knowledgev1.Citation{
			N: int32(i + 1), ChunkId: kc.hit.ChunkID.String(), Quote: kc.quote,
			Source: sourceOf(kc.hit, r.meta[kc.hit.ChunkID]),
		}
	}
	return strings.TrimSpace(markdown), citations
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
