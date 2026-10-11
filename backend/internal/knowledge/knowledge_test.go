package knowledge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
	"kiwi/internal/chat"
	"kiwi/internal/docs"
	"kiwi/internal/knowledge/internal/store"
	"kiwi/internal/meetings"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/llm"
	"kiwi/internal/testkit/dbtest"
	"kiwi/internal/testkit/fakeblob"
	"kiwi/internal/testkit/fakellm"
)

func TestMain(m *testing.M) {
	// Reindex and the jobs log what they did; a test run stays silent.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// ---- chunker ----------------------------------------------------------------

// isCombining is the set the brief names: U+0E31, U+0E34-U+0E3A and U+0E47-U+0E4E.
func isCombining(r rune) bool {
	return r == 0x0E31 || (r >= 0x0E34 && r <= 0x0E3A) || (r >= 0x0E47 && r <= 0x0E4E)
}

// thaiText is n runes of Thai without a single space: clusters of an optional leading
// vowel, a consonant, an optional vowel mark above or below, an optional tone mark and
// an optional following vowel.
func thaiText(rng *rand.Rand, n int) string {
	var b strings.Builder
	for count := 0; count < n; {
		var cluster []rune
		if rng.IntN(5) == 0 {
			cluster = append(cluster, rune(0x0E40+rng.IntN(5)))
		}
		cluster = append(cluster, rune(0x0E01+rng.IntN(0x2E)))
		switch rng.IntN(4) {
		case 0:
			cluster = append(cluster, []rune{0x0E31, 0x0E34, 0x0E35, 0x0E36, 0x0E37}[rng.IntN(5)])
		case 1:
			cluster = append(cluster, []rune{0x0E38, 0x0E39}[rng.IntN(2)])
		}
		if rng.IntN(4) == 0 {
			cluster = append(cluster, rune(0x0E48+rng.IntN(4)))
		}
		if rng.IntN(6) == 0 {
			cluster = append(cluster, []rune{0x0E30, 0x0E32, 0x0E33}[rng.IntN(3)])
		}
		for _, r := range cluster {
			if count < n {
				b.WriteRune(r)
				count++
			}
		}
	}
	return b.String()
}

// overlapRunes is the length of the longest tail of prev that next begins with.
func overlapRunes(prev, next string) int {
	p, n := []rune(prev), []rune(next)
	for k := min(len(p), len(n)); k > 0; k-- {
		if slices.Equal(p[len(p)-k:], n[:k]) {
			return k
		}
	}
	return 0
}

func TestChunker_ThaiWithoutSpaces_RespectsMaxTokens(t *testing.T) {
	text := thaiText(rand.New(rand.NewPCG(7, 11)), 20_000)
	if strings.ContainsAny(text, " \n\t") {
		t.Fatal("the fixture must have no whitespace")
	}

	chunks := ChunkText(text, 512, 64)
	if len(chunks) < 15 {
		t.Fatalf("20 000 Thai runes became %d chunks, want a chunk per ~1000 runes", len(chunks))
	}
	for i, c := range chunks {
		if got := llm.EstimateTokens(c); got > 512 {
			t.Errorf("chunk %d is %d estimated tokens, want at most 512", i, got)
		}
		runes := []rune(c)
		if first := runes[0]; isCombining(first) || first == 0x0E30 || first == 0x0E32 || first == 0x0E33 {
			t.Errorf("chunk %d starts with U+%04X, which belongs to the rune before it", i, first)
		}
		if last := runes[len(runes)-1]; last >= 0x0E40 && last <= 0x0E44 {
			t.Errorf("chunk %d ends with U+%04X, a vowel that leads the consonant after it", i, last)
		}
		if i > 0 {
			if k := overlapRunes(chunks[i-1], c); k < 64 || k > 128 {
				t.Errorf("chunks %d and %d overlap by %d runes, want about 64 tokens (64 to 128 runes)", i-1, i, k)
			}
		}
	}
	if !strings.HasPrefix(text, chunks[0]) || !strings.HasSuffix(text, chunks[len(chunks)-1]) {
		t.Error("the chunks do not begin and end where the text does")
	}
}

func TestChunker_ThaiAndEnglishMixed_DoesNotSplitMidWord(t *testing.T) {
	latinWord := regexp.MustCompile(`[A-Za-z]+`)
	cases := map[string]string{
		"spaced": strings.Repeat("ทีมเราย้ายระบบ Kubernetes cluster ไปใช้ deployment pipeline ใหม่ เสร็จแล้ว. Docker แทน VirtualMachine ได้ผลดี\n", 200),
		"glued":  strings.Repeat("ระบบใหม่Kubernetesทำงานบนclusterขององค์กรผ่านpipelineและDockerเรียบร้อยVirtualMachineเดิม", 200),
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			whole := map[string]bool{}
			for _, w := range latinWord.FindAllString(text, -1) {
				whole[w] = true
			}
			chunks := ChunkText(text, 512, 64)
			if len(chunks) < 3 {
				t.Fatalf("got %d chunks, want several", len(chunks))
			}
			for i, c := range chunks {
				for _, w := range latinWord.FindAllString(c, -1) {
					if !whole[w] {
						t.Fatalf("chunk %d holds %q, a piece of a word, near %q", i, w, c[:min(len(c), 40)])
					}
				}
				if got := llm.EstimateTokens(c); got > 512 {
					t.Errorf("chunk %d is %d estimated tokens", i, got)
				}
			}
		})
	}
}

// TestChunkText_ShortHeadingBeforeUnspacedThaiDoesNotStartTheNextChunkMidWord: a heading
// ends its chunk early, at the line break, and what follows has no space to cut at. The
// next chunk must begin where the Thai does, not repeat "bstract" from a heading that was
// shorter than the overlap.
func TestChunkText_ShortHeadingBeforeUnspacedThaiDoesNotStartTheNextChunkMidWord(t *testing.T) {
	thai := thaiText(rand.New(rand.NewPCG(21, 22)), 3000)
	for name, heading := range map[string]string{
		"abstract":        "Abstract",
		"two words":       "Executive summary",
		"numbered":        "3.2 Results",
		"thai then latin": "สรุปผล Results",
	} {
		t.Run(name, func(t *testing.T) {
			text := heading + "\n" + thai
			chunks := ChunkText(text, 512, 64)
			if len(chunks) < 3 {
				t.Fatalf("got %d chunks, want the heading and at least two of Thai", len(chunks))
			}
			if !strings.HasPrefix(chunks[0], heading) {
				t.Fatalf("the first chunk = %.30q, want it to begin with the heading %q", chunks[0], heading)
			}
			for i, c := range chunks[1:] {
				if first := []rune(c)[0]; first < 0x0E00 || first > 0x0E7F {
					t.Errorf("chunk %d starts with %q, a piece of the heading, not with the Thai", i+1, first)
				}
				if llm.EstimateTokens(c) > 512 {
					t.Errorf("chunk %d is %d tokens", i+1, llm.EstimateTokens(c))
				}
			}
			var words []string
			for _, c := range chunks {
				words = append(words, regexp.MustCompile(`[A-Za-z0-9.]+`).FindAllString(c, -1)...)
			}
			for _, w := range regexp.MustCompile(`[A-Za-z0-9.]+`).FindAllString(heading, -1) {
				if n := slices.Index(words, w); n < 0 {
					t.Errorf("the word %q of the heading is not whole in any chunk (got %q)", w, words)
				}
			}
			for _, w := range words {
				if !strings.Contains(heading, w) {
					t.Errorf("chunks hold %q, a piece of the heading %q", w, heading)
				}
			}
		})
	}
}

// TestChunkText_EarlyCutDoesNotEmitAChunkInsideThePreviousOne: a paragraph longer than the
// overlap ends its chunk early, at the line break, and the Thai after it has no boundary
// to cut at. The next chunk starts in the overlap, a word start of the paragraph, and must
// carry on into the Thai rather than stop where the paragraph stopped, which would be a
// chunk that is nothing but the tail of the one before.
func TestChunkText_EarlyCutDoesNotEmitAChunkInsideThePreviousOne(t *testing.T) {
	thai := thaiText(rand.New(rand.NewPCG(21, 22)), 3000)
	para := strings.Repeat("The abstract describes the method and results in some detail. ", 7) // ~100 tokens
	text := strings.TrimSpace(para) + "\n" + thai

	chunks := ChunkText(text, 512, 64)
	if len(chunks) < 4 {
		t.Fatalf("got %d chunks, want the paragraph and at least three of Thai", len(chunks))
	}
	for i := 1; i < len(chunks); i++ {
		if strings.Contains(chunks[i-1], chunks[i]) {
			t.Errorf("chunk %d (%d tokens) is a piece of chunk %d: %.30q", i, llm.EstimateTokens(chunks[i]), i-1, chunks[i])
		}
		if llm.EstimateTokens(chunks[i]) > 512 {
			t.Errorf("chunk %d is %d tokens", i, llm.EstimateTokens(chunks[i]))
		}
	}
	if !strings.HasPrefix(chunks[0], "The abstract") || !strings.Contains(chunks[1], "\n") {
		t.Errorf("chunk 1 = %.40q, want the overlap's tail of the paragraph followed by the Thai", chunks[1])
	}
	if pos := strings.Index(text, chunks[1]); pos <= 0 || text[pos-1] != ' ' {
		t.Errorf("chunk 1 starts at %d, not at the start of a word of the paragraph: %.20q", pos, chunks[1])
	}
}

// TestChunkText_ChunkShorterThanTheOverlapIsNotRepeated: a chunk of fewer tokens than the
// overlap has nothing to carry over that the next chunk would not repeat whole; the same
// holds for a Latin run without a space.
func TestChunkText_ChunkShorterThanTheOverlapIsNotRepeated(t *testing.T) {
	text := "Heading\n" + strings.Repeat("x", 5000)
	chunks := ChunkText(text, 128, 32)
	if len(chunks) < 3 || chunks[0] != "Heading" {
		t.Fatalf("chunks = %d, first %.20q; want the heading alone first", len(chunks), chunks[0])
	}
	if strings.Contains(chunks[1], "eading") || !strings.HasPrefix(chunks[1], "xxxx") {
		t.Errorf("the chunk after the heading repeats it: %.30q", chunks[1])
	}
}

func TestChunkText_ShortTextIsOneTrimmedChunk(t *testing.T) {
	if got := ChunkText("  hello world \n", 512, 64); !slices.Equal(got, []string{"hello world"}) {
		t.Errorf("got %q, want one trimmed chunk", got)
	}
	for _, blank := range []string{"", "   ", "\n\t "} {
		if got := ChunkText(blank, 512, 64); got != nil {
			t.Errorf("ChunkText(%q) = %q, want no chunk", blank, got)
		}
	}
}

func TestChunkText_PrefersParagraphThenSentenceBoundaries(t *testing.T) {
	sentence := strings.Repeat("word ", 59) + "end. " // 300 runes: 75 tokens
	paragraph := strings.TrimSpace(strings.Repeat(sentence, 4))
	text := paragraph + "\n\n" + paragraph + "\n\n" + paragraph

	chunks := ChunkText(text, 400, 0) // 1600 runes of room: a paragraph of 1200 fits, two do not
	if len(chunks) != 3 || chunks[0] != paragraph || chunks[1] != paragraph || chunks[2] != paragraph {
		t.Fatalf("chunks = %d, first ends %q: want the three paragraphs", len(chunks), chunks[0][max(0, len(chunks[0])-30):])
	}

	// With no paragraph breaks the cut falls after a sentence, not inside one.
	flat := strings.ReplaceAll(text, "\n\n", " ")
	for i, c := range ChunkText(flat, 400, 0) {
		if !strings.HasSuffix(c, "end.") {
			t.Errorf("chunk %d ends %q, want the end of a sentence", i, c[max(0, len(c)-20):])
		}
	}
}

// TestChunkText_PropertyOverRandomMixedText holds every invariant at once over text
// that mixes Thai, Latin, digits, punctuation, line breaks and emoji.
func TestChunkText_PropertyOverRandomMixedText(t *testing.T) {
	for seed := uint64(1); seed <= 12; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*977))
		var b strings.Builder
		for b.Len() < 40_000 {
			switch rng.IntN(7) {
			case 0, 1:
				b.WriteString(thaiText(rng, 1+rng.IntN(300)))
			case 2:
				fmt.Fprintf(&b, "%s ", strings.Repeat("Kubernetes ", 1+rng.IntN(8))+strconv.Itoa(b.Len()))
			case 3:
				fmt.Fprintf(&b, " 2026-10-%d. ", b.Len())
			case 4:
				b.WriteString("\n")
			case 5:
				fmt.Fprintf(&b, "🙂 👩‍💻 %d ", b.Len())
			default:
				b.WriteString(" ")
			}
		}
		text := b.String()
		maxTokens, overlap := 120+int(seed)*40, int(seed)*7

		chunks := ChunkText(text, maxTokens, overlap)
		prevStart, prevEnd := -1, 0
		for i, c := range chunks {
			if got := llm.EstimateTokens(c); got > maxTokens {
				t.Fatalf("seed %d chunk %d: %d tokens > %d", seed, i, got, maxTokens)
			}
			if c != strings.TrimSpace(c) || c == "" {
				t.Fatalf("seed %d chunk %d is not trimmed", seed, i)
			}
			first := []rune(c)[0]
			if isCombining(first) || first == 0x200D {
				t.Fatalf("seed %d chunk %d starts with U+%04X", seed, i, first)
			}
			pos := strings.Index(text[prevStart+1:], c)
			if pos < 0 {
				t.Fatalf("seed %d chunk %d is not in the text after the previous chunk's start", seed, i)
			}
			pos += prevStart + 1
			if i == 0 && pos != len(text)-len(strings.TrimLeft(text, " \n")) {
				t.Fatalf("seed %d: the first chunk does not begin the text", seed)
			}
			if i > 0 && strings.TrimSpace(text[prevEnd:max(prevEnd, pos)]) != "" {
				t.Fatalf("seed %d chunk %d leaves text between %d and %d uncovered", seed, i, prevEnd, pos)
			}
			prevStart, prevEnd = pos, pos+len(c)
		}
		if prevEnd != len(strings.TrimRight(text, " \n")) && strings.TrimSpace(text[prevEnd:]) != "" {
			t.Fatalf("seed %d: the last chunk stops before the end of the text", seed)
		}
	}
}

func message(seq int64, authorKind chatv1.AuthorKind, author uuid.UUID, body string, at time.Time) *chatv1.Message {
	return &chatv1.Message{
		Id: uuid.NewString(), Seq: seq, AuthorKind: authorKind, AuthorId: author.String(),
		Body: body, CreatedAt: timestamppb.New(at),
	}
}

func TestChunker_ChatWindowsExcludeArtifactMessages(t *testing.T) {
	alice, bob := uuid.New(), uuid.New()
	names := map[string]string{alice.String(): "Alice", bob.String(): "Bob"}
	author := func(m *chatv1.Message) string {
		if m.GetAuthorKind() == chatv1.AuthorKind_AUTHOR_KIND_AGENT {
			return "assistant"
		}
		return names[m.GetAuthorId()]
	}
	at := time.Date(2026, 10, 11, 10, 1, 0, 0, time.UTC)

	slip := message(2, chatv1.AuthorKind_AUTHOR_KIND_AGENT, authz.AgentID("assistant"), "DOC-1001 · plan.pdf", at.Add(time.Minute))
	slip.Artifact = &chatv1.Artifact{Kind: chatv1.ArtifactKind_ARTIFACT_KIND_DOC_SUMMARY, Ref: "DOC-1001"}
	msgs := []*chatv1.Message{
		message(1, chatv1.AuthorKind_AUTHOR_KIND_USER, alice, "ship it on Friday", at),
		slip,
		message(3, chatv1.AuthorKind_AUTHOR_KIND_USER, bob, "   ", at.Add(2*time.Minute)), // an attachment with no words
		message(4, chatv1.AuthorKind_AUTHOR_KIND_AGENT, authz.AgentID("assistant"), "noted", at.Add(3*time.Minute)),
		message(5, chatv1.AuthorKind_AUTHOR_KIND_USER, bob, "ตกลงครับ", at.Add(4*time.Minute)),
	}

	got := chatDrafts(msgs, author)
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want the window in one", len(got))
	}
	d := got[0]
	if want := "Alice: ship it on Friday\nassistant: noted\nBob: ตกลงครับ"; d.content != want {
		t.Errorf("content = %q, want %q", d.content, want)
	}
	if strings.Contains(d.content, "DOC-1001") {
		t.Error("the artifact message was indexed with the chat")
	}
	if d.anchor != "seq:1-5" || d.meta.Label != "Alice" || d.meta.At == nil || !d.meta.At.Equal(at) {
		t.Errorf("anchor = %q, meta = %+v, want seq:1-5, label Alice, at %s", d.anchor, d.meta, at)
	}

	if got := chatDrafts(msgs[1:2], author); len(got) != 0 {
		t.Errorf("a window of nothing but an artifact gave %d chunks", len(got))
	}
}

func TestChunker_TranscriptRangeAnchors(t *testing.T) {
	long := strings.Repeat("the launch plan depends on the vendor contract and the budget review ", 6) // ~100 tokens
	var entries []*meetingsv1.Entry
	for i := range 40 {
		entries = append(entries, &meetingsv1.Entry{Idx: int32(i), Speaker: new(fmt.Sprintf("S%d", i%3)), StartMs: new(int64(i) * 5000), Text: long})
	}
	got := entryDrafts("Launch review", entries)

	if len(got) < 6 {
		t.Fatalf("40 entries of ~100 tokens made %d chunks, want several", len(got))
	}
	next := 0
	for i, d := range got {
		var a, b int
		if _, err := fmt.Sscanf(d.anchor, "entries:%d-%d", &a, &b); err != nil {
			t.Fatalf("chunk %d anchor %q: %v", i, d.anchor, err)
		}
		if a != next || b < a {
			t.Errorf("chunk %d anchor %s: want it to start at entry %d", i, d.anchor, next)
		}
		next = b + 1
		if tokens := llm.EstimateTokens(d.content); tokens > 512 {
			t.Errorf("chunk %d is %d tokens", i, tokens)
		}
		if d.meta.OffsetMS == nil || *d.meta.OffsetMS != int64(a)*5000 {
			t.Errorf("chunk %d offset = %v, want the start of entry %d", i, d.meta.OffsetMS, a)
		}
		if d.meta.Label != "Launch review" {
			t.Errorf("chunk %d label = %q", i, d.meta.Label)
		}
		if !strings.HasPrefix(d.content, fmt.Sprintf("S%d: ", a%3)) {
			t.Errorf("chunk %d does not begin with its first entry's speaker: %.20q", i, d.content)
		}
	}
	if next != 40 {
		t.Errorf("the anchors end at entry %d, want every one of 40 covered", next-1)
	}

	t.Run("an entry too long for a chunk is cut at its own anchor", func(t *testing.T) {
		huge := &meetingsv1.Entry{Idx: 7, Text: thaiText(rand.New(rand.NewPCG(3, 4)), 5000)}
		got := entryDrafts("m", []*meetingsv1.Entry{huge})
		if len(got) < 4 {
			t.Fatalf("got %d chunks for 5000 Thai runes", len(got))
		}
		for k, d := range got {
			if d.anchor != "entries:7-7" || d.piece != k || d.meta.OffsetMS != nil {
				t.Errorf("piece %d: anchor %q piece %d offset %v", k, d.anchor, d.piece, d.meta.OffsetMS)
			}
		}
	})

	t.Run("without timing there is no offset", func(t *testing.T) {
		got := entryDrafts("m", []*meetingsv1.Entry{{Idx: 0, Text: "hello"}, {Idx: 1, Text: "world"}})
		if len(got) != 1 || got[0].meta.OffsetMS != nil || got[0].content != "hello\nworld" {
			t.Errorf("got %+v", got)
		}
	})
}

func TestChunkID_Deterministic(t *testing.T) {
	ws, other, src := uuid.New(), uuid.New(), uuid.New()
	id := ChunkID(ws, "chat", src, "seq:1-2")

	if again := ChunkID(ws, "chat", src, "seq:1-2"); again != id {
		t.Errorf("the same arguments gave %s and %s", id, again)
	}
	if want := uuid.NewSHA1(ws, []byte("chat/"+src.String()+"/seq:1-2")); id != want {
		t.Errorf("id = %s, want UUIDv5 over kind/source/anchor in the workspace namespace (%s)", id, want)
	}
	if id.Version() != 5 {
		t.Errorf("version = %d, want 5", id.Version())
	}
	for name, differs := range map[string]uuid.UUID{
		"workspace": ChunkID(other, "chat", src, "seq:1-2"),
		"kind":      ChunkID(ws, "doc", src, "seq:1-2"),
		"source":    ChunkID(ws, "chat", uuid.New(), "seq:1-2"),
		"anchor":    ChunkID(ws, "chat", src, "seq:1-3"),
	} {
		if differs == id {
			t.Errorf("a different %s gave the same id", name)
		}
	}
	if pieceID(ws, "chat", src, "seq:1-2", 0) != id {
		t.Error("piece 0 must be the anchor's own id")
	}
	if p1 := pieceID(ws, "chat", src, "seq:1-2", 1); p1 == id || p1 != ChunkID(ws, "chat", src, "seq:1-2#1") {
		t.Error("piece 1 must be the id of anchor#1")
	}
}

func TestKeyedLock_SerialisesOneKeyAndNotOthers(t *testing.T) {
	k := newKeyedLock()
	unlock, err := k.lock(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := k.lock(t.Context(), "b")
	if err != nil {
		t.Fatalf("another key had to be free: %v", err)
	}
	other()

	acquired := make(chan struct{})
	go func() {
		u, err := k.lock(t.Context(), "a")
		if err == nil {
			close(acquired)
			u()
		}
	}()
	select {
	case <-acquired:
		t.Fatal("the key was taken twice")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never got the key after it was released")
	}

	held, _ := k.lock(t.Context(), "c")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := k.lock(ctx, "c"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait = %v, want context.Canceled", err)
	}
	held()
	if len(k.entries) != 0 {
		t.Errorf("%d keys are still tracked after every lock was released", len(k.entries))
	}
}

// ---- fixture ----------------------------------------------------------------

// members is the authz.Membership of the tests: who belongs to which channel.
type members struct {
	mu   sync.Mutex
	list map[[2]uuid.UUID]bool
}

func (m *members) add(channel, user uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.list == nil {
		m.list = map[[2]uuid.UUID]bool{}
	}
	m.list[[2]uuid.UUID{channel, user}] = true
}

func (m *members) IsMember(_ context.Context, channel, user uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list[[2]uuid.UUID{channel, user}], nil
}

// stubDocs is a docs.Service that only reads pages, so a test decides what a document
// says; every other method panics on the nil embedded interface.
type stubDocs struct {
	docs.Service
	mu     sync.Mutex
	pages  []docs.Page
	err    error
	actors []authz.Actor
}

func (s *stubDocs) ExtractPages(_ context.Context, a authz.Actor, _ uuid.UUID) ([]docs.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actors = append(s.actors, a)
	return slices.Clone(s.pages), s.err
}

func (s *stubDocs) set(pages ...docs.Page) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pages = pages
}

type fixture struct {
	t     *testing.T
	env   *dbtest.Env
	db    *db.DB
	llm   *fakellm.Client
	blob  *fakeblob.Store
	stub  *stubDocs
	mem   *members
	deps  Deps // with the real docs service
	svc   Service
	stubd Service // the same, reading documents from stub

	ws, alice, bob uuid.UUID
	public         uuid.UUID // a public channel; alice and bob are members
	private        uuid.UUID // a private channel; only alice is a member
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := dbtest.Start(t)
	d, err := db.Open(t.Context(), config.Config{DatabaseURL: env.AppURL, DatabaseSystemURL: env.SystemURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(d.Close)

	f := &fixture{t: t, env: env, db: d, llm: fakellm.New(), blob: fakeblob.New(), stub: &stubDocs{}, mem: &members{}}
	az := authz.New(f.mem)
	f.deps = Deps{
		DB: d, LLM: f.llm, Authz: az,
		Chat:     chat.New(chat.Deps{DB: d, Authz: az}),
		Meetings: meetings.New(meetings.Deps{DB: d, Authz: az}),
		Docs:     docs.New(docs.Deps{DB: d, Authz: az, Blob: f.blob, LLM: f.llm}),
	}
	f.svc = New(f.deps)
	stubbed := f.deps
	stubbed.Docs = f.stub
	f.stubd = New(stubbed)

	f.ws = f.workspace("acme.example")
	f.alice = f.user(f.ws, "Alice")
	f.bob = f.user(f.ws, "Bob")
	f.public = f.channel(f.ws, "public", f.alice, f.bob)
	f.private = f.channel(f.ws, "private", f.alice)
	return f
}

// system is the actor of the embed.index job: a workspace and no user.
func system(ws uuid.UUID) authz.Actor { return authz.Actor{Kind: authz.ActorSystem, WorkspaceID: ws} }

// asOwner runs fn as kiwi_owner in a transaction bound to ws; the tests seed and read
// tenant rows this way.
func (f *fixture) asOwner(ws uuid.UUID, fn func(ctx context.Context, tx pgx.Tx)) {
	f.t.Helper()
	ctx := f.t.Context()
	conn, err := pgx.Connect(ctx, f.env.OwnerURL)
	if err != nil {
		f.t.Fatalf("connect as owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('kiwi.workspace_id', $1, true)`, ws.String()); err != nil {
		f.t.Fatal(err)
	}
	fn(ctx, tx)
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) exec(ws uuid.UUID, sql string, args ...any) {
	f.t.Helper()
	f.asOwner(ws, func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			f.t.Fatalf("%s: %v", sql, err)
		}
	})
}

func (f *fixture) scalar(ws uuid.UUID, dst any, sql string, args ...any) {
	f.t.Helper()
	f.asOwner(ws, func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			f.t.Fatalf("%s: %v", sql, err)
		}
	})
}

func (f *fixture) count(ws uuid.UUID, sql string, args ...any) int {
	f.t.Helper()
	var n int
	f.scalar(ws, &n, sql, args...)
	return n
}

func (f *fixture) workspace(domain string) uuid.UUID {
	f.t.Helper()
	ws := uuid.New()
	f.exec(ws, `INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, $2, $2)`, ws, domain)
	return ws
}

func (f *fixture) user(ws uuid.UUID, name string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(ws, `INSERT INTO identity.users (id, workspace_id, google_sub, email, name) VALUES ($1, $2, $3, $4, $5)`,
		id, ws, "sub-"+id.String(), strings.ToLower(name)+"@example.test", name)
	return id
}

func (f *fixture) channel(ws uuid.UUID, kind string, member ...uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(ws, `INSERT INTO chat.channels (id, workspace_id, name, kind) VALUES ($1, $2, $3, $4)`, id, ws, "ch-"+id.String()[:8], kind)
	for _, m := range member {
		f.exec(ws, `INSERT INTO chat.channel_members (channel_id, user_id, workspace_id) VALUES ($1, $2, $3)`, id, m, ws)
		f.mem.add(id, m)
	}
	return id
}

// say writes a message straight into the table, so a test chooses its seq and time.
func (f *fixture) say(ws, channel, author uuid.UUID, seq int, body string, at time.Time) {
	f.t.Helper()
	f.exec(ws, `INSERT INTO chat.messages (id, workspace_id, channel_id, seq, author_kind, author_id, body, client_msg_id, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3, 'user', $4, $5, $6, $7)`, ws, channel, seq, author, body, uuid.NewString(), at)
	f.exec(ws, `UPDATE chat.channels SET last_seq = GREATEST(last_seq, $2) WHERE id = $1`, channel, seq)
}

// slip writes the message of an AI slip: an agent's message that carries an artifact.
func (f *fixture) slip(ws, channel uuid.UUID, seq int, at time.Time) {
	f.t.Helper()
	f.exec(ws, `INSERT INTO chat.messages (id, workspace_id, channel_id, seq, author_kind, author_id, body, artifact, client_msg_id, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3, 'agent', $4, 'DOC-1001 · plan.pdf',
		        '{"kind":"ARTIFACT_KIND_DOC_SUMMARY","ref":"DOC-1001","title":"plan.pdf"}'::jsonb, $5, $6)`,
		ws, channel, seq, authz.AgentID("assistant"), uuid.NewString(), at)
	f.exec(ws, `UPDATE chat.channels SET last_seq = GREATEST(last_seq, $2) WHERE id = $1`, channel, seq)
}

type entry struct {
	speaker string
	startMS *int64
	text    string
}

// meeting writes a meeting of the fixture workspace with its transcript and, when
// summary is not nil, its summary with decisions and open questions as the meetings
// domain stores them.
func (f *fixture) meeting(channel, owner uuid.UUID, title, status string, entries []entry, summary *string, decisions, questions []string) uuid.UUID {
	f.t.Helper()
	return f.meetingIn(f.ws, channel, owner, title, status, entries, summary, decisions, questions)
}

func (f *fixture) meetingIn(ws, channel, owner uuid.UUID, title, status string, entries []entry, summary *string, decisions, questions []string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(ws, `INSERT INTO meetings.meetings (id, workspace_id, channel_id, owner_user_id, title, status) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, ws, channel, owner, title, status)
	for i, e := range entries {
		var speaker *string
		if e.speaker != "" {
			speaker = &e.speaker
		}
		f.exec(ws, `INSERT INTO meetings.transcript_entries (meeting_id, workspace_id, idx, speaker, start_ms, text) VALUES ($1, $2, $3, $4, $5, $6)`,
			id, ws, i, speaker, e.startMS, e.text)
	}
	if summary != nil {
		f.exec(ws, `INSERT INTO meetings.summaries (meeting_id, workspace_id, summary, decisions, open_questions) VALUES ($1, $2, $3, $4::jsonb, $5::jsonb)`,
			id, ws, *summary, items(decisions), items(questions))
	}
	return id
}

func items(texts []string) string {
	type item struct {
		Text      string `json:"text"`
		EntryRefs []int  `json:"entry_refs"`
	}
	out := make([]item, len(texts))
	for i, tx := range texts {
		out[i] = item{Text: tx, EntryRefs: []int{0}}
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

// file writes a ready file row; with body it also stores the bytes the real docs
// service reads.
func (f *fixture) file(ws, channel, uploader uuid.UUID, name string, body []byte) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	key := "uploads/" + ws.String() + "/" + id.String()
	sum := sha256.Sum256(body)
	f.blob.Put(key, "text/plain", body)
	f.exec(ws, `INSERT INTO files.files (id, workspace_id, channel_id, uploaded_by, name, blob_key, mime, size, sha256, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'text/plain', $7, $8, 'ready')`,
		id, ws, channel, uploader, name, key, len(body), hex.EncodeToString(sum[:]))
	return id
}

// storedChunk is one row of knowledge.chunks as a test reads it.
type storedChunk struct {
	ID        uuid.UUID
	ChannelID *uuid.UUID
	Kind      string
	SourceID  uuid.UUID
	Anchor    string
	Lang      string
	Content   string
	SHA       string
	Model     string
	Meta      map[string]any
	Dims      int
	TsvOK     bool
}

func (f *fixture) chunks(ws uuid.UUID, where string, args ...any) []storedChunk {
	f.t.Helper()
	var out []storedChunk
	f.asOwner(ws, func(ctx context.Context, tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT id, channel_id, source_kind, source_id, source_anchor, lang, content, content_sha256,
			embedding_model, source_meta::text, vector_dims(embedding), tsv = to_tsvector('simple', content)
			FROM knowledge.chunks `+where+` ORDER BY source_anchor, id`, args...)
		if err != nil {
			f.t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var c storedChunk
			var meta string
			if err := rows.Scan(&c.ID, &c.ChannelID, &c.Kind, &c.SourceID, &c.Anchor, &c.Lang, &c.Content, &c.SHA, &c.Model, &meta, &c.Dims, &c.TsvOK); err != nil {
				f.t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(meta), &c.Meta); err != nil {
				f.t.Fatalf("source_meta %q: %v", meta, err)
			}
			out = append(out, c)
		}
		if err := rows.Err(); err != nil {
			f.t.Fatal(err)
		}
	})
	return out
}

func (f *fixture) sourceChunks(source uuid.UUID) []storedChunk {
	f.t.Helper()
	return f.chunks(f.ws, `WHERE source_id = $1`, source)
}

func (f *fixture) vectorText(id uuid.UUID) string {
	f.t.Helper()
	var s string
	f.scalar(f.ws, &s, `SELECT embedding::text FROM knowledge.chunks WHERE id = $1`, id)
	return s
}

// embedded returns every text the fake model was asked to embed, in call order.
func (f *fixture) embedded() []string {
	var texts []string
	for _, c := range f.llm.EmbedCalls() {
		texts = append(texts, c.Texts...)
	}
	return texts
}

func (f *fixture) index(a authz.Actor, kind string, source, channel uuid.UUID, windowStart *time.Time) error {
	req := IndexRequest{SourceKind: kind, SourceID: source, WindowStart: windowStart}
	if channel != uuid.Nil {
		req.ChannelID = &channel
	}
	return f.svc.Index(f.t.Context(), a, req)
}

func (f *fixture) indexStubbed(a authz.Actor, source, channel uuid.UUID) error {
	return f.stubd.Index(f.t.Context(), a, IndexRequest{SourceKind: kindDoc, SourceID: source, ChannelID: &channel})
}

func at(h, m, s int) time.Time { return time.Date(2026, 10, 11, h, m, s, 0, time.UTC) }

func pages(texts ...string) []docs.Page {
	out := make([]docs.Page, len(texts))
	for i, tx := range texts {
		out[i] = docs.Page{N: i + 1, Markdown: tx}
	}
	return out
}

// ---- Index: meetings --------------------------------------------------------

func TestIndex_MeetingSourceIncludesSummaryChunk(t *testing.T) {
	f := newFixture(t)
	sum := "The team chose the vendor and fixed the launch for November."
	id := f.meeting(f.public, f.alice, "Launch review", "ready", []entry{
		{"Alice", new(int64(1000)), "We should pick the vendor today."},
		{"Bob", new(int64(9000)), "Agreed, and launch in November."},
	}, &sum, []string{"Use the cheaper vendor"}, []string{"Who signs the contract?"})

	// The embed.index job is a system actor with a workspace and no user.
	if err := f.index(system(f.ws), kindMeeting, id, f.public, nil); err != nil {
		t.Fatalf("Index: %v", err)
	}

	got := f.sourceChunks(id)
	if len(got) != 2 || got[0].Anchor != "entries:0-1" || got[1].Anchor != "summary" {
		t.Fatalf("anchors = %v, want entries:0-1 and summary", anchors(got))
	}
	if want := "Alice: We should pick the vendor today.\nBob: Agreed, and launch in November."; got[0].Content != want {
		t.Errorf("transcript chunk = %q, want %q", got[0].Content, want)
	}
	want := sum + "\n\nDecisions:\n- Use the cheaper vendor\n\nOpen questions:\n- Who signs the contract?"
	if got[1].Content != want {
		t.Errorf("summary chunk = %q, want %q", got[1].Content, want)
	}
	for _, c := range got {
		if c.Kind != "meeting" || c.SourceID != id {
			t.Errorf("chunk %s is %s of %s", c.Anchor, c.Kind, c.SourceID)
		}
	}
	calls := f.llm.EmbedCalls()
	if len(calls) != 1 || len(calls[0].Texts) != 2 || calls[0].Task != llm.EmbedDocument {
		t.Errorf("embed calls = %+v, want one call of two chunks embedded as documents", calls)
	}
}

func anchors(cs []storedChunk) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Anchor
	}
	return out
}

func TestIndex_MeetingChunkStoresLabelAndFirstEntryOffset(t *testing.T) {
	f := newFixture(t)
	timed := f.meeting(f.public, f.alice, "Timed call", "ready", []entry{
		{"Alice", new(int64(4500)), "first words"}, {"Bob", new(int64(9000)), "second words"},
	}, nil, nil, nil)
	untimed := f.meeting(f.public, f.alice, "Recorded call", "ready", []entry{{"", nil, "only words"}}, nil, nil, nil)

	for _, id := range []uuid.UUID{timed, untimed} {
		if err := f.index(system(f.ws), kindMeeting, id, f.public, nil); err != nil {
			t.Fatalf("Index: %v", err)
		}
	}

	c := f.sourceChunks(timed)[0]
	if c.Meta["label"] != "Timed call" || c.Meta["offset_ms"] != float64(4500) {
		t.Errorf("timed meta = %v, want label Timed call and offset_ms 4500", c.Meta)
	}
	u := f.sourceChunks(untimed)[0]
	if u.Meta["label"] != "Recorded call" {
		t.Errorf("untimed label = %v", u.Meta["label"])
	}
	if _, has := u.Meta["offset_ms"]; has {
		t.Errorf("an untimed transcript stored an offset: %v", u.Meta)
	}
	if n := f.count(f.ws, `SELECT count(*) FROM knowledge.chunks WHERE source_id = $1 AND source_meta ? 'offset_ms'`, untimed); n != 0 {
		t.Errorf("%d chunks of the untimed meeting have an offset_ms key", n)
	}
}

// TestIndex_UnreadableSummaryItemsAreWarnedAboutNotSilentlyDropped: decisions that cannot
// be read leave the rest of the meeting indexable, and the log says which meeting lost them.
func TestIndex_UnreadableSummaryItemsAreWarnedAboutNotSilentlyDropped(t *testing.T) {
	f := newFixture(t)
	id := f.meeting(f.public, f.alice, "Weekly", "ready", []entry{{"Alice", nil, "we decided"}},
		new("The team decided."), []string{"Ship on Friday"}, []string{"Who deploys?"})
	f.exec(f.ws, `UPDATE meetings.summaries SET decisions = '{"not":"a list"}'::jsonb WHERE meeting_id = $1`, id)

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	if err := f.index(system(f.ws), kindMeeting, id, f.public, nil); err != nil {
		t.Fatalf("Index: %v", err)
	}

	var summary string
	for _, c := range f.sourceChunks(id) {
		if c.Anchor == "summary" {
			summary = c.Content
		}
	}
	if want := "The team decided.\n\nOpen questions:\n- Who deploys?"; summary != want {
		t.Errorf("summary chunk = %q, want the summary and the readable open questions only", summary)
	}
	out := logs.String()
	for _, want := range []string{"level=WARN", "meeting_id=" + id.String(), "list=decisions"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "list=open_questions") {
		t.Errorf("a readable list was reported as unreadable: %q", out)
	}
}

func TestIndex_MeetingMustBeReady(t *testing.T) {
	f := newFixture(t)
	id := f.meeting(f.public, f.alice, "Half done", "ingesting", []entry{{"Alice", nil, "so far"}}, nil, nil, nil)
	if err := f.index(system(f.ws), kindMeeting, id, f.public, nil); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("Index of an ingesting meeting = %v, want ErrConflict", err)
	}
	if len(f.llm.EmbedCalls()) != 0 {
		t.Error("a meeting that is not ready was embedded")
	}
}

// ---- Index: chat ------------------------------------------------------------

func TestIndex_ChatChunkStoresAuthorLabelAndFirstMessageTime(t *testing.T) {
	f := newFixture(t)
	first := at(10, 7, 30).Add(123456 * time.Microsecond)
	f.say(f.ws, f.public, f.alice, 1, "ship the release on Friday", first)
	f.say(f.ws, f.public, f.bob, 2, "ok, I will tag it", at(10, 8, 0))

	w := at(10, 5, 0)
	if err := f.index(system(f.ws), kindChat, f.public, f.public, &w); err != nil {
		t.Fatalf("Index: %v", err)
	}

	got := f.sourceChunks(f.public)
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want the window in one", len(got))
	}
	c := got[0]
	if c.Anchor != "seq:1-2" || c.Kind != "chat" || c.Content != "Alice: ship the release on Friday\nBob: ok, I will tag it" {
		t.Errorf("chunk = %s %s %q", c.Kind, c.Anchor, c.Content)
	}
	if c.Meta["label"] != "Alice" {
		t.Errorf("label = %v, want the first message's author", c.Meta["label"])
	}
	stored, err := time.Parse(time.RFC3339Nano, c.Meta["at"].(string))
	if err != nil || !stored.Equal(first) {
		t.Errorf("at = %v (%v), want the first message's time %s", c.Meta["at"], err, first)
	}
}

func TestIndex_ChatWindowCoversOnlyItsFiveMinutes(t *testing.T) {
	f := newFixture(t)
	f.say(f.ws, f.public, f.alice, 1, "before the window", at(10, 4, 59))
	f.say(f.ws, f.public, f.alice, 2, "first in the window", at(10, 5, 0))
	f.say(f.ws, f.public, f.bob, 3, "last in the window", at(10, 9, 59))
	f.say(f.ws, f.public, f.bob, 4, "after the window", at(10, 10, 0))

	// An unaligned start still means the window it falls in.
	mid := at(10, 7, 12)
	if err := f.index(system(f.ws), kindChat, f.public, f.public, &mid); err != nil {
		t.Fatalf("Index: %v", err)
	}
	got := f.sourceChunks(f.public)
	if len(got) != 1 || got[0].Anchor != "seq:2-3" || got[0].Content != "Alice: first in the window\nBob: last in the window" {
		t.Fatalf("chunks = %+v, want only the two messages of 10:05-10:10", got)
	}

	// Indexing the neighbours must not delete this window's chunk as stale.
	for _, w := range []time.Time{at(10, 0, 0), at(10, 10, 0)} {
		if err := f.index(system(f.ws), kindChat, f.public, f.public, &w); err != nil {
			t.Fatalf("Index %s: %v", w, err)
		}
	}
	if got := anchors(f.sourceChunks(f.public)); !slices.Equal(got, []string{"seq:1-1", "seq:2-3", "seq:4-4"}) {
		t.Errorf("anchors = %v, want one chunk per window", got)
	}
}

// TestIndex_ChatWindowReadsPastNewerMessages is Ruling 14: the history is paged newest
// first, so a window behind more messages than one page holds still has to be found.
func TestIndex_ChatWindowReadsPastNewerMessages(t *testing.T) {
	f := newFixture(t)
	f.exec(f.ws, `INSERT INTO chat.messages (id, workspace_id, channel_id, seq, author_kind, author_id, body, client_msg_id, created_at)
		SELECT gen_random_uuid(), $1, $2, g, 'user', $3, 'message ' || g, 'm-' || g,
		       CASE WHEN g <= 10 THEN $4::timestamptz + g * interval '1 second' ELSE $5::timestamptz + g * interval '1 second' END
		FROM generate_series(1, 450) AS g`, f.ws, f.public, f.alice, at(10, 1, 0), at(11, 0, 0))
	f.exec(f.ws, `UPDATE chat.channels SET last_seq = 450 WHERE id = $1`, f.public)

	w := at(10, 0, 0)
	if err := f.index(system(f.ws), kindChat, f.public, f.public, &w); err != nil {
		t.Fatalf("Index: %v", err)
	}
	got := f.sourceChunks(f.public)
	if len(got) != 1 || got[0].Anchor != "seq:1-10" || !strings.Contains(got[0].Content, "message 10") || strings.Contains(got[0].Content, "message 11\n") {
		t.Fatalf("chunks = %v, want the first ten messages only, found behind 440 newer ones", anchors(got))
	}
}

func TestIndex_ChatArtifactMessagesAreNotIndexedWithTheChat(t *testing.T) {
	f := newFixture(t)
	f.say(f.ws, f.public, f.alice, 1, "here is the plan", at(10, 1, 0))
	f.slip(f.ws, f.public, 2, at(10, 2, 0))
	w := at(10, 0, 0)
	if err := f.index(system(f.ws), kindChat, f.public, f.public, &w); err != nil {
		t.Fatalf("Index: %v", err)
	}
	got := f.sourceChunks(f.public)
	if len(got) != 1 || strings.Contains(got[0].Content, "DOC-1001") || got[0].Anchor != "seq:1-1" {
		t.Fatalf("chunks = %+v, want the slip left out", got)
	}
}

func TestIndex_StoresEmbeddingModelAndChannelID(t *testing.T) {
	f := newFixture(t)
	f.llm.SetEmbeddingModel("embeddinggemma:300m")
	f.say(f.ws, f.private, f.alice, 1, "ประชุมเรื่องงบประมาณพรุ่งนี้ คุยกับ finance ด้วย", at(10, 1, 0))
	f.say(f.ws, f.public, f.alice, 1, "lunch at noon", at(10, 1, 0))
	w := at(10, 0, 0)
	for _, ch := range []uuid.UUID{f.private, f.public} {
		if err := f.index(system(f.ws), kindChat, ch, ch, &w); err != nil {
			t.Fatalf("Index: %v", err)
		}
	}

	priv := f.sourceChunks(f.private)[0]
	if priv.ChannelID == nil || *priv.ChannelID != f.private {
		t.Errorf("private chunk channel_id = %v, want %s", priv.ChannelID, f.private)
	}
	if priv.Model != "embeddinggemma:300m" || priv.Dims != 768 || priv.Lang != "th" || !priv.TsvOK {
		t.Errorf("private chunk: model %q, %d dims, lang %q, tsv = to_tsvector(simple, content) %v", priv.Model, priv.Dims, priv.Lang, priv.TsvOK)
	}
	pub := f.sourceChunks(f.public)[0]
	if pub.ChannelID == nil || *pub.ChannelID != f.public || pub.Lang != "en" {
		t.Errorf("public chunk channel_id = %v, lang %q", pub.ChannelID, pub.Lang)
	}
	sum := sha256.Sum256([]byte(pub.Content))
	if pub.SHA != hex.EncodeToString(sum[:]) {
		t.Errorf("content_sha256 = %s, want the SHA-256 of the content", pub.SHA)
	}
	if want := ChunkID(f.ws, "chat", f.public, "seq:1-1"); pub.ID != want {
		t.Errorf("id = %s, want the deterministic chunk id %s", pub.ID, want)
	}
}

// ---- Index: documents -------------------------------------------------------

func TestIndex_DocSourceThroughTheRealDocsService(t *testing.T) {
	f := newFixture(t)
	id := f.file(f.ws, f.public, f.alice, "นโยบาย.txt", []byte("นโยบายการลา: พนักงานลาได้ 10 วันต่อปี\n\nHR policy: ten days of leave a year."))
	if err := f.index(system(f.ws), kindDoc, id, f.public, nil); err != nil {
		t.Fatalf("Index: %v", err)
	}
	got := f.sourceChunks(id)
	if len(got) != 1 || got[0].Anchor != "page:1" || got[0].Meta["label"] != "นโยบาย.txt" || got[0].Lang != "th" {
		t.Fatalf("chunks = %+v, want one page chunk labelled with the file name", got)
	}
	if !strings.Contains(got[0].Content, "ten days of leave") {
		t.Errorf("content = %q", got[0].Content)
	}
}

func TestIndex_DocPagesAreAnchoredAndALongPageIsCutAtItsOwnAnchor(t *testing.T) {
	f := newFixture(t)
	id := f.file(f.ws, f.public, f.alice, "book.pdf", nil)
	long := thaiText(rand.New(rand.NewPCG(5, 6)), 6000)
	f.stub.set(pages("short first page", long, "last page")...)

	if err := f.indexStubbed(system(f.ws), id, f.public); err != nil {
		t.Fatalf("Index: %v", err)
	}
	got := f.sourceChunks(id)
	seen := map[uuid.UUID]bool{}
	perAnchor := map[string]int{}
	for _, c := range got {
		if seen[c.ID] {
			t.Fatalf("two chunks share the id %s", c.ID)
		}
		seen[c.ID] = true
		perAnchor[c.Anchor]++
		if llm.EstimateTokens(c.Content) > 512 {
			t.Errorf("chunk %s has %d tokens", c.Anchor, llm.EstimateTokens(c.Content))
		}
	}
	if perAnchor["page:1"] != 1 || perAnchor["page:3"] != 1 || perAnchor["page:2"] < 6 {
		t.Errorf("chunks per anchor = %v, want 1 for page 1 and 3 and at least 6 pieces of page 2", perAnchor)
	}
	if len(f.stub.actors) != 1 || f.stub.actors[0].Kind != authz.ActorSystem || f.stub.actors[0].UserID != f.alice {
		t.Errorf("ExtractPages was called as %+v, want the system actor acting for the uploader", f.stub.actors)
	}

	// The pieces survive a second run: nothing is embedded again.
	before := len(f.embedded())
	if err := f.indexStubbed(system(f.ws), id, f.public); err != nil {
		t.Fatalf("second Index: %v", err)
	}
	if after := len(f.embedded()); after != before || len(f.sourceChunks(id)) != len(got) {
		t.Errorf("a second run embedded %d texts and left %d chunks, want none and %d", after-before, len(f.sourceChunks(id)), len(got))
	}
}

// ---- Index: idempotence, replacement, batching ------------------------------

func TestIndex_SkipsReembedWhenShaAndModelUnchanged(t *testing.T) {
	f := newFixture(t)
	id := f.meeting(f.public, f.alice, "Weekly", "ready", []entry{{"Alice", nil, "numbers are up"}}, new("Numbers are up."), nil, nil)

	for run := 1; run <= 2; run++ {
		if err := f.index(system(f.ws), kindMeeting, id, f.public, nil); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	if calls := len(f.llm.EmbedCalls()); calls != 1 {
		t.Fatalf("%d Embed calls after two runs, want the second to make none", calls)
	}

	// A different model is a different vector: the same text is embedded again.
	f.llm.SetEmbeddingModel("another-model")
	if err := f.index(system(f.ws), kindMeeting, id, f.public, nil); err != nil {
		t.Fatalf("run with a new model: %v", err)
	}
	if calls := len(f.llm.EmbedCalls()); calls != 2 {
		t.Errorf("%d Embed calls after a model change, want one more", calls)
	}
	for _, c := range f.sourceChunks(id) {
		if c.Model != "another-model" {
			t.Errorf("chunk %s kept model %q", c.Anchor, c.Model)
		}
	}
}

func TestIndex_ReplacesChangedChunksAndDeletesStale(t *testing.T) {
	f := newFixture(t)
	id := f.file(f.ws, f.public, f.alice, "notes.pdf", nil)
	f.stub.set(pages("page one stays", "page two as first written", "page three goes away")...)
	if err := f.indexStubbed(system(f.ws), id, f.public); err != nil {
		t.Fatal(err)
	}
	p1 := f.chunkByAnchor(id, "page:1")
	oldP2 := f.vectorText(f.chunkByAnchor(id, "page:2").ID)

	f.stub.set(pages("page one stays", "page two rewritten")...)
	before := len(f.embedded())
	if err := f.indexStubbed(system(f.ws), id, f.public); err != nil {
		t.Fatal(err)
	}

	if got := anchors(f.sourceChunks(id)); !slices.Equal(got, []string{"page:1", "page:2"}) {
		t.Errorf("anchors = %v, want page 3 deleted as stale", got)
	}
	if got := f.embedded()[before:]; !slices.Equal(got, []string{"page two rewritten"}) {
		t.Errorf("embedded %q on the second run, want only the changed page", got)
	}
	if again := f.chunkByAnchor(id, "page:1"); again.ID != p1.ID || again.SHA != p1.SHA {
		t.Error("page 1 changed although its text did not")
	}
	p2 := f.chunkByAnchor(id, "page:2")
	if p2.Content != "page two rewritten" || f.vectorText(p2.ID) == oldP2 {
		t.Error("page 2 kept its old text or its old vector")
	}

	// A source that now says nothing keeps no chunk; this is the case a nil keep list
	// would get wrong, because deleting where id <> ALL(NULL) deletes nothing.
	f.stub.set()
	if err := f.indexStubbed(system(f.ws), id, f.public); err != nil {
		t.Fatal(err)
	}
	if n := len(f.sourceChunks(id)); n != 0 {
		t.Errorf("%d chunks remain of a source that is now empty", n)
	}
}

func (f *fixture) chunkByAnchor(source uuid.UUID, anchor string) storedChunk {
	f.t.Helper()
	for _, c := range f.sourceChunks(source) {
		if c.Anchor == anchor {
			return c
		}
	}
	f.t.Fatalf("source %s has no chunk at %s", source, anchor)
	return storedChunk{}
}

func TestIndex_ChatWindowReplacesItsChunkWhenAMessageArrives(t *testing.T) {
	f := newFixture(t)
	f.say(f.ws, f.public, f.alice, 1, "first", at(10, 1, 0))
	f.say(f.ws, f.public, f.bob, 2, "second", at(10, 2, 0))
	w := at(10, 0, 0)
	if err := f.index(system(f.ws), kindChat, f.public, f.public, &w); err != nil {
		t.Fatal(err)
	}
	f.say(f.ws, f.public, f.alice, 3, "third", at(10, 3, 0))
	if err := f.index(system(f.ws), kindChat, f.public, f.public, &w); err != nil {
		t.Fatal(err)
	}
	got := f.sourceChunks(f.public)
	if len(got) != 1 || got[0].Anchor != "seq:1-3" {
		t.Fatalf("anchors = %v, want the window's chunk replaced by seq:1-3", anchors(got))
	}
}

func TestIndex_EmbedsInBatchesWithADeadline(t *testing.T) {
	f := newFixture(t)
	id := f.file(f.ws, f.public, f.alice, "twenty.pdf", nil)
	texts := make([]string, 20)
	for i := range texts {
		texts[i] = fmt.Sprintf("page number %d says something of its own", i+1)
	}
	f.stub.set(pages(texts...)...)
	var deadlines int
	f.llm.HandleEmbed(func(ctx context.Context, in []string, task llm.EmbedTask) ([][]float32, error) {
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) > 0 {
			deadlines++
		}
		out := make([][]float32, len(in))
		for i, tx := range in {
			out[i] = fakellm.Vector(tx)
		}
		return out, nil
	})

	if err := f.indexStubbed(system(f.ws), id, f.public); err != nil {
		t.Fatal(err)
	}
	var sizes []int
	for _, c := range f.llm.EmbedCalls() {
		sizes = append(sizes, len(c.Texts))
		if c.Task != llm.EmbedDocument {
			t.Errorf("a call embedded as task %v, want documents", c.Task)
		}
	}
	if !slices.Equal(sizes, []int{8, 8, 4}) {
		t.Errorf("call sizes = %v, want batches of 8", sizes)
	}
	if deadlines != 3 {
		t.Errorf("%d of 3 calls carried a deadline", deadlines)
	}
}

func TestIndex_FailedBatchKeepsTheFinishedOnesAndTheNextRunContinues(t *testing.T) {
	f := newFixture(t)
	id := f.file(f.ws, f.public, f.alice, "twenty.pdf", nil)
	texts := make([]string, 20)
	for i := range texts {
		texts[i] = fmt.Sprintf("page number %d says something of its own", i+1)
	}
	f.stub.set(pages(texts...)...)
	boom := errors.New("model went away")
	var calls int
	f.llm.HandleEmbed(func(_ context.Context, in []string, _ llm.EmbedTask) ([][]float32, error) {
		calls++
		if calls == 2 {
			return nil, boom
		}
		out := make([][]float32, len(in))
		for i, tx := range in {
			out[i] = fakellm.Vector(tx)
		}
		return out, nil
	})

	if err := f.indexStubbed(system(f.ws), id, f.public); !errors.Is(err, boom) {
		t.Fatalf("Index = %v, want the model's error", err)
	}
	if n := len(f.sourceChunks(id)); n != 8 {
		t.Fatalf("%d chunks stored after the failure, want the first batch's 8", n)
	}

	before := len(f.embedded())
	if err := f.indexStubbed(system(f.ws), id, f.public); err != nil {
		t.Fatalf("second Index: %v", err)
	}
	if got := len(f.embedded()) - before; got != 12 {
		t.Errorf("the second run embedded %d chunks, want only the 12 that were missing", got)
	}
	if n := len(f.sourceChunks(id)); n != 20 {
		t.Errorf("%d chunks stored, want 20", n)
	}
}

// TestIndex_RunsOfOneSourceDoNotOverlap: two runs on one source would each write what
// they read and delete what they did not produce, so the one that read earlier could
// undo the other. The second waits for the first and then finds nothing to do.
func TestIndex_RunsOfOneSourceDoNotOverlap(t *testing.T) {
	f := newFixture(t)
	id := f.file(f.ws, f.public, f.alice, "one.pdf", nil)
	f.stub.set(pages("the only page")...)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	f.llm.HandleEmbed(func(_ context.Context, in []string, _ llm.EmbedTask) ([][]float32, error) {
		entered <- struct{}{}
		<-release
		out := make([][]float32, len(in))
		for i, tx := range in {
			out[i] = fakellm.Vector(tx)
		}
		return out, nil
	})

	done := make(chan error, 2)
	for range 2 {
		go func() { done <- f.indexStubbed(system(f.ws), id, f.public) }()
	}
	<-entered // one run is inside the model, holding the source
	select {
	case <-entered:
		t.Error("a second run reached the model while the first held the source")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Errorf("Index: %v", err)
		}
	}
	if calls := len(f.llm.EmbedCalls()); calls != 1 {
		t.Errorf("%d Embed calls, want the second run to find the first one's work done", calls)
	}
	if n := len(f.sourceChunks(id)); n != 1 {
		t.Errorf("%d chunks, want 1", n)
	}
}

func TestIndex_RejectsWrongVectors(t *testing.T) {
	f := newFixture(t)
	id := f.meeting(f.public, f.alice, "Weekly", "ready", []entry{{"Alice", nil, "hello"}}, nil, nil, nil)
	f.llm.HandleEmbed(func(_ context.Context, in []string, _ llm.EmbedTask) ([][]float32, error) {
		return [][]float32{make([]float32, 3)}, nil
	})
	err := f.index(system(f.ws), kindMeeting, id, f.public, nil)
	if err == nil || !strings.Contains(err.Error(), "3 dimensions") {
		t.Fatalf("Index = %v, want the wrong width named", err)
	}
	if n := len(f.sourceChunks(id)); n != 0 {
		t.Errorf("%d chunks stored from a vector of the wrong width", n)
	}
}

// ---- Index: who may ---------------------------------------------------------

func TestIndex_NeedsKnowledgeIndexOnTheSourcesChannel(t *testing.T) {
	f := newFixture(t)
	f.say(f.ws, f.private, f.alice, 1, "secret plan", at(10, 1, 0))
	w := at(10, 0, 0)

	bob := authz.Actor{Kind: authz.ActorUser, WorkspaceID: f.ws, UserID: f.bob}
	if err := f.index(bob, kindChat, f.private, f.private, &w); !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("a non-member indexing a private channel = %v, want ErrForbidden", err)
	}
	agent := authz.Actor{Kind: authz.ActorAgent, WorkspaceID: f.ws, UserID: f.alice, OnBehalfOf: f.alice, Agent: "assistant"}
	if err := f.index(agent, kindChat, f.private, f.private, &w); !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("an agent indexing = %v, want ErrForbidden", err)
	}
	if err := f.index(authz.Actor{Kind: authz.ActorSystem}, kindChat, f.private, f.private, &w); err == nil {
		t.Error("a system actor without a workspace was allowed")
	}
	if len(f.llm.EmbedCalls()) != 0 {
		t.Error("something was embedded for an actor that may not index")
	}

	alice := authz.Actor{Kind: authz.ActorUser, WorkspaceID: f.ws, UserID: f.alice}
	if err := f.index(alice, kindChat, f.private, f.private, &w); err != nil {
		t.Errorf("a member indexing their channel = %v", err)
	}
}

func TestIndex_RejectsRequestsThatNameNoSource(t *testing.T) {
	f := newFixture(t)
	id := f.meeting(f.public, f.alice, "Weekly", "ready", []entry{{"Alice", nil, "hello"}}, nil, nil, nil)
	w := at(10, 0, 0)
	sys := system(f.ws)

	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"unknown kind":      {f.index(sys, "email", id, f.public, nil), errs.ErrInvalid},
		"nil source":        {f.index(sys, kindMeeting, uuid.Nil, f.public, nil), errs.ErrInvalid},
		"chat without time": {f.index(sys, kindChat, f.public, f.public, nil), errs.ErrInvalid},
		"chat in a channel": {f.index(sys, kindChat, f.public, f.private, &w), errs.ErrInvalid},
		"other channel":     {f.index(sys, kindMeeting, id, f.private, nil), errs.ErrInvalid},
		"no such meeting":   {f.index(sys, kindMeeting, uuid.New(), f.public, nil), errs.ErrNotFound},
		"no such file":      {f.index(sys, kindDoc, uuid.New(), f.public, nil), errs.ErrNotFound},
	} {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: Index = %v, want %v", name, tc.err, tc.want)
		}
	}
	if err := New(Deps{DB: f.db, LLM: f.llm}).Index(t.Context(), sys, IndexRequest{SourceKind: kindMeeting, SourceID: id}); !errors.Is(err, errs.ErrInvalid) {
		t.Errorf("Index on a Service built to reindex = %v, want ErrInvalid rather than a nil dereference", err)
	}
}

// ---- tenancy ----------------------------------------------------------------

// foreign is a second workspace with a user, a channel, a meeting, a file and a chat
// window of its own, all indexed there, so a test can try to reach them from the fixture
// workspace.
type foreign struct {
	ws, user, channel, meeting, doc uuid.UUID
	window                          time.Time
	chunks                          int
}

func (f *fixture) foreignWorkspace() foreign {
	f.t.Helper()
	ws := f.workspace("mallory.example")
	user := f.user(ws, "Mallory")
	channel := f.channel(ws, "public", user)
	o := foreign{
		ws: ws, user: user, channel: channel, window: at(10, 0, 0),
		meeting: f.meetingIn(ws, channel, user, "Secret merger", "ready",
			[]entry{{"Mallory", nil, "the merger closes in March"}}, new("The merger closes in March."), nil, nil),
		doc: f.file(ws, channel, user, "merger.txt", []byte("the merger closes in March")),
	}
	f.say(ws, channel, user, 1, "the merger closes in March", at(10, 1, 0))
	for _, req := range []IndexRequest{
		{SourceKind: kindMeeting, SourceID: o.meeting, ChannelID: &channel},
		{SourceKind: kindDoc, SourceID: o.doc, ChannelID: &channel},
		{SourceKind: kindChat, SourceID: channel, ChannelID: &channel, WindowStart: &o.window},
	} {
		if err := f.svc.Index(f.t.Context(), system(ws), req); err != nil {
			f.t.Fatalf("index the other workspace's %s: %v", req.SourceKind, err)
		}
	}
	o.chunks = f.count(ws, `SELECT count(*) FROM knowledge.chunks`)
	if o.chunks != 4 { // the transcript, the summary, the page and the chat window
		f.t.Fatalf("the other workspace holds %d chunks, want 4", o.chunks)
	}
	return o
}

// TestIndex_SourceOfAnotherWorkspaceIsNotFoundAndWritesNothing: the indexer reads rows
// of other domains with plain queries that name no workspace; row-level security is what
// keeps them to the actor's. A job for workspace A that names a meeting, a file or a
// channel of workspace B finds nothing, embeds nothing and leaves both workspaces as
// they were.
func TestIndex_SourceOfAnotherWorkspaceIsNotFoundAndWritesNothing(t *testing.T) {
	f := newFixture(t)
	other := f.foreignWorkspace()
	embedsBefore := len(f.llm.EmbedCalls())
	sys := system(f.ws)

	for _, tc := range []struct {
		name string
		req  IndexRequest
	}{
		{"meeting with its channel", IndexRequest{SourceKind: kindMeeting, SourceID: other.meeting, ChannelID: &other.channel}},
		{"meeting without a channel", IndexRequest{SourceKind: kindMeeting, SourceID: other.meeting}},
		{"doc with its channel", IndexRequest{SourceKind: kindDoc, SourceID: other.doc, ChannelID: &other.channel}},
		{"doc without a channel", IndexRequest{SourceKind: kindDoc, SourceID: other.doc}},
	} {
		if err := f.svc.Index(t.Context(), sys, tc.req); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("%s: Index = %v, want ErrNotFound", tc.name, err)
		}
	}

	// A chat window of a channel this workspace does not have is an empty window: no
	// message is visible, so there is nothing to embed and nothing to delete.
	if err := f.svc.Index(t.Context(), sys, IndexRequest{SourceKind: kindChat, SourceID: other.channel, ChannelID: &other.channel, WindowStart: &other.window}); err != nil {
		t.Errorf("chat window of another workspace's channel: %v", err)
	}

	if got := len(f.llm.EmbedCalls()) - embedsBefore; got != 0 {
		t.Errorf("%d Embed calls for sources of another workspace, want none", got)
	}
	if n := f.count(f.ws, `SELECT count(*) FROM knowledge.chunks`); n != 0 {
		t.Errorf("the requesting workspace holds %d chunks, want none", n)
	}
	if n := f.count(other.ws, `SELECT count(*) FROM knowledge.chunks`); n != other.chunks {
		t.Errorf("the other workspace holds %d chunks, want its %d untouched", n, other.chunks)
	}

	// The same requests in the right workspace do find their sources: the decoys above
	// fail because of the workspace and for no other reason.
	for _, req := range []IndexRequest{
		{SourceKind: kindMeeting, SourceID: other.meeting, ChannelID: &other.channel},
		{SourceKind: kindDoc, SourceID: other.doc, ChannelID: &other.channel},
	} {
		if err := f.svc.Index(t.Context(), system(other.ws), req); err != nil {
			t.Errorf("%s in its own workspace: %v", req.SourceKind, err)
		}
	}
}

// TestIndex_AuthorOfAnotherWorkspaceIsUnknown: a message whose author_id is a user of
// another workspace is labelled with no name from there.
func TestIndex_AuthorOfAnotherWorkspaceIsUnknown(t *testing.T) {
	f := newFixture(t)
	other := f.foreignWorkspace()
	f.say(f.ws, f.public, f.alice, 1, "good morning", at(10, 1, 0))
	f.say(f.ws, f.public, other.user, 2, "I know a secret", at(10, 2, 0))

	w := at(10, 0, 0)
	if err := f.index(system(f.ws), kindChat, f.public, f.public, &w); err != nil {
		t.Fatalf("Index: %v", err)
	}
	got := f.sourceChunks(f.public)
	if len(got) != 1 || got[0].Content != "Alice: good morning\nunknown: I know a secret" {
		t.Fatalf("chunks = %+v, want the foreign author labelled unknown", got)
	}
	if strings.Contains(got[0].Content, "Mallory") {
		t.Error("a name from another workspace reached the chunk")
	}
}

// TestCrossSchemaReads_AreBoundByRowLevelSecurity runs every query that reads a table of
// another domain from the wrong workspace and from the right one. The queries carry no
// workspace predicate, so what answers "no rows" is row-level security. Run as kiwi_system,
// which bypasses it, the same queries would answer: the control that shows the test could
// fail.
func TestCrossSchemaReads_AreBoundByRowLevelSecurity(t *testing.T) {
	f := newFixture(t)
	other := f.foreignWorkspace()

	read := func(ws uuid.UUID, fn func(q *store.Queries) error) error {
		return f.db.InWorkspace(t.Context(), ws, func(ctx context.Context, tx pgx.Tx) error { return fn(store.New(tx)) })
	}
	for _, tc := range []struct {
		name string
		fn   func(q *store.Queries) error
	}{
		{"GetMeetingSource", func(q *store.Queries) error { _, err := q.GetMeetingSource(t.Context(), other.meeting); return err }},
		{"GetMeetingSummary", func(q *store.Queries) error { _, err := q.GetMeetingSummary(t.Context(), other.meeting); return err }},
		{"GetFileSource", func(q *store.Queries) error { _, err := q.GetFileSource(t.Context(), other.doc); return err }},
	} {
		if err := read(f.ws, tc.fn); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("%s from the wrong workspace = %v, want no rows", tc.name, err)
		}
		if err := read(other.ws, tc.fn); err != nil {
			t.Errorf("%s from its own workspace = %v, want the row", tc.name, err)
		}
	}

	names := func(ws uuid.UUID) (out []string) {
		err := read(ws, func(q *store.Queries) error {
			rows, err := q.ListUserNames(t.Context(), []string{other.user.String(), f.alice.String()})
			for _, r := range rows {
				out = append(out, r.Name)
			}
			return err
		})
		if err != nil {
			t.Fatalf("ListUserNames: %v", err)
		}
		return out
	}
	if got := names(f.ws); !slices.Equal(got, []string{"Alice"}) {
		t.Errorf("ListUserNames from the wrong workspace = %v, want only its own user", got)
	}
	if got := names(other.ws); !slices.Equal(got, []string{"Mallory"}) {
		t.Errorf("ListUserNames from its own workspace = %v, want only Mallory", got)
	}

	// Control: row-level security is the only thing in the way. kiwi_system bypasses it and
	// sees the foreign user and the foreign meeting through the same queries.
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, f.env.SystemURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	q := store.New(conn)
	rows, err := q.ListUserNames(ctx, []string{other.user.String(), f.alice.String()})
	if err != nil || len(rows) != 2 {
		t.Errorf("control: ListUserNames without row-level security = %d rows, %v, want both users", len(rows), err)
	}
	if _, err := q.GetMeetingSource(ctx, other.meeting); err != nil {
		t.Errorf("control: GetMeetingSource without row-level security = %v, want the foreign meeting", err)
	}
}

// ---- jobs -------------------------------------------------------------------

func embedMessage(t *testing.T, id string, ev *eventsv1.EmbedIndex) bus.Message {
	t.Helper()
	data, err := protoJSON.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return bus.Message{ID: id, Topic: events.TopicEmbedIndex, Data: data, Attrs: map[string]string{"workspace_id": ev.WorkspaceId}, Attempt: 1}
}

func TestJobs_RegisterOneConsumerOnTwoTopics(t *testing.T) {
	f := newFixture(t)
	jobs := Jobs(f.deps)
	if len(jobs) != 1 || jobs[0].Consumer != "embed.index" || jobs[0].Handler == nil ||
		!slices.Equal(jobs[0].Topics, []string{"embed.index", "chat.message.created"}) {
		t.Fatalf("jobs = %+v, want consumer embed.index on embed.index and chat.message.created", jobs)
	}
	defer func() {
		if recover() == nil {
			t.Error("Jobs without Chat did not panic")
		}
	}()
	Jobs(Deps{DB: f.db, LLM: f.llm})
}

func TestEmbedIndexJob_RedeliveryExactlyOnce(t *testing.T) {
	f := newFixture(t)
	id := f.meeting(f.public, f.alice, "Weekly", "ready", []entry{{"Alice", nil, "we shipped"}}, new("Shipped."), nil, nil)
	handle := Jobs(f.deps)[0].Handler
	msg := embedMessage(t, "4711", &eventsv1.EmbedIndex{
		WorkspaceId: f.ws.String(), SourceKind: "meeting", SourceId: id.String(), ChannelId: f.public.String(),
	})

	if err := handle(t.Context(), msg); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if n := len(f.sourceChunks(id)); n != 2 {
		t.Fatalf("%d chunks after the first delivery, want 2", n)
	}
	calls := len(f.llm.EmbedCalls())

	msg.Attempt = 2
	if err := handle(t.Context(), msg); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if got := len(f.llm.EmbedCalls()); got != calls {
		t.Errorf("the redelivery made %d more Embed calls, want none", got-calls)
	}
	if n := f.count(f.ws, `SELECT count(*) FROM platform.processed_events WHERE consumer = 'embed.index' AND event_id = '4711'`); n != 1 {
		t.Errorf("%d processed_events rows for the message, want 1", n)
	}
}

func TestEmbedIndexJob_FailureNacksAndLeavesNothingRecorded(t *testing.T) {
	f := newFixture(t)
	id := f.meeting(f.public, f.alice, "Weekly", "ready", []entry{{"Alice", nil, "we shipped"}}, nil, nil, nil)
	handle := Jobs(f.deps)[0].Handler
	boom := errors.New("model went away")
	f.llm.QueueEmbed(fakellm.EmbedReply{Err: boom})
	msg := embedMessage(t, "4712", &eventsv1.EmbedIndex{WorkspaceId: f.ws.String(), SourceKind: "meeting", SourceId: id.String(), ChannelId: f.public.String()})

	if err := handle(t.Context(), msg); !errors.Is(err, boom) {
		t.Fatalf("handler = %v, want the model's error so the bus redelivers", err)
	}
	if n := f.count(f.ws, `SELECT count(*) FROM platform.processed_events WHERE consumer = 'embed.index'`); n != 0 {
		t.Errorf("%d processed_events rows after a failure", n)
	}
	if err := handle(t.Context(), msg); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if n := len(f.sourceChunks(id)); n != 1 {
		t.Errorf("%d chunks after the redelivery, want 1", n)
	}
}

func TestEmbedIndexJob_ChatWindowEventIndexesThatWindow(t *testing.T) {
	f := newFixture(t)
	f.say(f.ws, f.public, f.alice, 1, "in the window", at(10, 6, 0))
	handle := Jobs(f.deps)[0].Handler
	msg := embedMessage(t, "4713", &eventsv1.EmbedIndex{
		WorkspaceId: f.ws.String(), SourceKind: "chat", SourceId: f.public.String(), ChannelId: f.public.String(),
		WindowStart: timestamppb.New(at(10, 5, 0)),
	})
	if err := handle(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	if got := f.sourceChunks(f.public); len(got) != 1 || got[0].Anchor != "seq:1-1" {
		t.Errorf("chunks = %+v", got)
	}
	bad := embedMessage(t, "4714", &eventsv1.EmbedIndex{WorkspaceId: f.ws.String(), SourceKind: "meeting", SourceId: "not-a-uuid"})
	if err := handle(t.Context(), bad); !errors.Is(err, errs.ErrInvalid) {
		t.Errorf("a malformed event = %v, want ErrInvalid", err)
	}
}

type outboxRow struct {
	topic, key, payload string
	notBefore           time.Time
}

func (f *fixture) outbox() []outboxRow {
	f.t.Helper()
	var out []outboxRow
	f.asOwner(f.ws, func(ctx context.Context, tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT topic, key, payload::text, not_before FROM platform.outbox ORDER BY id`)
		if err != nil {
			f.t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var r outboxRow
			if err := rows.Scan(&r.topic, &r.key, &r.payload, &r.notBefore); err != nil {
				f.t.Fatal(err)
			}
			out = append(out, r)
		}
	})
	return out
}

func (f *fixture) chatCreated(id string, msg *chatv1.Message) bus.Message {
	f.t.Helper()
	ev := &eventsv1.ChatMessageCreated{WorkspaceId: f.ws.String(), ChannelId: f.public.String(), Message: msg}
	data, err := protoJSON.Marshal(ev)
	if err != nil {
		f.t.Fatal(err)
	}
	return bus.Message{ID: id, Topic: events.TopicChatMessageCreated, Data: data, Attrs: map[string]string{"workspace_id": f.ws.String()}, Attempt: 1}
}

func TestChatMessageCreated_DebouncedWindowEnqueue(t *testing.T) {
	f := newFixture(t)
	handle := Jobs(f.deps)[0].Handler
	msg := f.chatCreated("900", message(5, chatv1.AuthorKind_AUTHOR_KIND_USER, f.alice, "hello", at(10, 7, 30)))

	before := time.Now()
	if err := handle(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	rows := f.outbox()
	if len(rows) != 1 {
		t.Fatalf("%d outbox rows, want one embed.index event", len(rows))
	}
	r := rows[0]
	if r.topic != "embed.index" || r.key != "chat:"+f.public.String()+":2026-10-11T10:05:00Z" {
		t.Errorf("event = %s %s, want embed.index on key chat:<channel>:<window start>", r.topic, r.key)
	}
	if r.notBefore.Before(before.Add(chatDebounce-time.Second)) || r.notBefore.After(after.Add(chatDebounce+time.Second)) {
		t.Errorf("not_before = %s, want about %s after the message", r.notBefore, chatDebounce)
	}
	var payload eventsv1.EmbedIndex
	if err := protojson.Unmarshal([]byte(r.payload), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.GetWorkspaceId() != f.ws.String() || payload.GetSourceKind() != "chat" || payload.GetSourceId() != f.public.String() ||
		payload.GetChannelId() != f.public.String() || !payload.GetWindowStart().AsTime().Equal(at(10, 5, 0)) {
		t.Errorf("payload = %v", &payload)
	}

	// The same delivery twice enqueues once.
	if err := handle(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	if n := len(f.outbox()); n != 1 {
		t.Errorf("a redelivered message left %d rows, want 1", n)
	}

	// A message of an artifact, or without words, makes no window to index; a message of
	// another window makes its own.
	slip := message(6, chatv1.AuthorKind_AUTHOR_KIND_AGENT, authz.AgentID("assistant"), "DOC-1001", at(10, 8, 0))
	slip.Artifact = &chatv1.Artifact{Ref: "DOC-1001"}
	for i, m := range []*chatv1.Message{slip, message(7, chatv1.AuthorKind_AUTHOR_KIND_USER, f.bob, " ", at(10, 8, 0))} {
		if err := handle(t.Context(), f.chatCreated(fmt.Sprintf("91%d", i), m)); err != nil {
			t.Fatal(err)
		}
	}
	if err := handle(t.Context(), f.chatCreated("930", message(8, chatv1.AuthorKind_AUTHOR_KIND_USER, f.bob, "later", at(10, 10, 0)))); err != nil {
		t.Fatal(err)
	}
	rows = f.outbox()
	if len(rows) != 2 || rows[1].key != "chat:"+f.public.String()+":2026-10-11T10:10:00Z" {
		t.Errorf("rows = %+v, want the artifact and the blank message ignored and a second window", rows)
	}
}

// ---- reindex ----------------------------------------------------------------

func TestReindex_ReembedsEverythingWithNewModel(t *testing.T) {
	f := newFixture(t)
	f.llm.SetEmbeddingModel("model-one")

	docID := f.file(f.ws, f.public, f.alice, "plan.pdf", nil)
	f.stub.set(pages("page one of the plan", "page two of the plan", "page three of the plan")...)
	if err := f.indexStubbed(system(f.ws), docID, f.public); err != nil {
		t.Fatal(err)
	}
	f.say(f.ws, f.private, f.alice, 1, "a private note", at(10, 1, 0))
	w := at(10, 0, 0)
	if err := f.index(system(f.ws), kindChat, f.private, f.private, &w); err != nil {
		t.Fatal(err)
	}
	// A second workspace: reindex enumerates every one.
	ws2 := f.workspace("other.example")
	u2 := f.user(ws2, "Dana")
	ch2 := f.channel(ws2, "public", u2)
	doc2 := f.file(ws2, ch2, u2, "other.pdf", nil)
	f.stub.set(pages("the other workspace's only page")...)
	if err := f.stubd.Index(t.Context(), system(ws2), IndexRequest{SourceKind: kindDoc, SourceID: doc2, ChannelID: &ch2}); err != nil {
		t.Fatal(err)
	}

	type snapshot struct{ vector, content, sha string }
	snap := map[uuid.UUID]snapshot{}
	all := func(ws uuid.UUID) []storedChunk { return f.chunks(ws, ``) }
	for _, ws := range []uuid.UUID{f.ws, ws2} {
		for _, c := range all(ws) {
			var v string
			f.scalar(ws, &v, `SELECT embedding::text FROM knowledge.chunks WHERE id = $1`, c.ID)
			snap[c.ID] = snapshot{v, c.Content, c.SHA}
			if c.Model != "model-one" {
				t.Fatalf("chunk %s has model %q before the reindex", c.Anchor, c.Model)
			}
		}
	}
	if len(snap) != 5 {
		t.Fatalf("%d chunks before the reindex, want 5 over two workspaces", len(snap))
	}

	f.llm.SetEmbeddingModel("model-two")
	var batches []int
	f.llm.HandleEmbed(func(_ context.Context, in []string, task llm.EmbedTask) ([][]float32, error) {
		batches = append(batches, len(in))
		if task != llm.EmbedDocument {
			t.Errorf("reindex embedded as task %v", task)
		}
		out := make([][]float32, len(in))
		for i, tx := range in {
			out[i] = fakellm.Vector("model-two:" + tx)
		}
		return out, nil
	})
	if err := f.svc.Reindex(t.Context()); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	embeddedAfter := 0
	for _, n := range batches {
		embeddedAfter += n
		if n > embedBatch {
			t.Errorf("a batch of %d, want at most %d", n, embedBatch)
		}
	}
	if embeddedAfter != 5 {
		t.Errorf("%d chunks embedded, want all 5", embeddedAfter)
	}
	for _, ws := range []uuid.UUID{f.ws, ws2} {
		for _, c := range all(ws) {
			var v string
			f.scalar(ws, &v, `SELECT embedding::text FROM knowledge.chunks WHERE id = $1`, c.ID)
			old := snap[c.ID]
			if c.Model != "model-two" || v == old.vector {
				t.Errorf("chunk %s: model %q, vector changed %v", c.Anchor, c.Model, v != old.vector)
			}
			if c.Content != old.content || c.SHA != old.sha {
				t.Errorf("chunk %s: the text changed under a reindex", c.Anchor)
			}
		}
	}

	// A second run finds every chunk on the current model and embeds nothing.
	batches = nil
	if err := f.svc.Reindex(t.Context()); err != nil {
		t.Fatalf("second Reindex: %v", err)
	}
	if len(batches) != 0 {
		t.Errorf("the second Reindex embedded %v, want nothing", batches)
	}
}

func TestReindex_ContinuesWhereAFailedRunStopped(t *testing.T) {
	f := newFixture(t)
	f.llm.SetEmbeddingModel("model-one")
	id := f.file(f.ws, f.public, f.alice, "twenty.pdf", nil)
	texts := make([]string, 20)
	for i := range texts {
		texts[i] = fmt.Sprintf("page number %d says something of its own", i+1)
	}
	f.stub.set(pages(texts...)...)
	if err := f.indexStubbed(system(f.ws), id, f.public); err != nil {
		t.Fatal(err)
	}

	f.llm.SetEmbeddingModel("model-two")
	boom := errors.New("model went away")
	var calls, embedded int
	f.llm.HandleEmbed(func(_ context.Context, in []string, _ llm.EmbedTask) ([][]float32, error) {
		calls++
		if calls == 2 {
			return nil, boom
		}
		embedded += len(in)
		out := make([][]float32, len(in))
		for i, tx := range in {
			out[i] = fakellm.Vector(tx)
		}
		return out, nil
	})
	if err := f.svc.Reindex(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("Reindex = %v, want the model's error", err)
	}
	if n := f.count(f.ws, `SELECT count(*) FROM knowledge.chunks WHERE embedding_model = 'model-two'`); n != 8 {
		t.Fatalf("%d chunks moved to the new model before the failure, want the first batch's 8", n)
	}
	embedded = 0
	if err := f.svc.Reindex(t.Context()); err != nil {
		t.Fatalf("second Reindex: %v", err)
	}
	if embedded != 12 {
		t.Errorf("the second run embedded %d chunks, want only the 12 left", embedded)
	}
}

func TestReindex_NeedsADatabaseAndAModel(t *testing.T) {
	if err := New(Deps{}).Reindex(t.Context()); !errors.Is(err, errs.ErrInvalid) {
		t.Errorf("Reindex without dependencies = %v, want ErrInvalid", err)
	}
}
