package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	filesv1 "kiwi/gen/kiwi/files/v1"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/blob"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/llm"
	"kiwi/internal/testkit/fakellm"
)

const summaryJSON = `{"summary":"The Bang Na warehouse holds 12,000 pallets.","key_points":["12,000 pallets","Bang Na","opens in Q4"]}`

func reply(text string) fakellm.GenerateReply {
	return fakellm.GenerateReply{Result: llm.Result{Text: text}}
}

func replyErr(err error) fakellm.GenerateReply { return fakellm.GenerateReply{Err: err} }

// deliver hands the newest doc.uploaded event to the doc.analyze handler as the relay
// would, as delivery number attempt.
func (f *fixture) deliver(attempt int) error {
	f.t.Helper()
	return f.deliverTo(f.analyze, "", attempt)
}

// deliverTo is deliver to another handler; a non-empty id replaces the message id, to
// send the same event again as a different delivery.
func (f *fixture) deliverTo(h bus.Handler, id string, attempt int) error {
	f.t.Helper()
	var rowID int64
	var payload string
	f.asOwner(func(ctx context.Context, tx pgx.Tx) {
		err := tx.QueryRow(ctx, `SELECT id, payload::text FROM platform.outbox WHERE topic = $1 ORDER BY id DESC LIMIT 1`,
			events.TopicDocUploaded).Scan(&rowID, &payload)
		if err != nil {
			f.t.Fatalf("no doc.uploaded event to deliver: %v", err)
		}
	})
	if id == "" {
		id = strconv.FormatInt(rowID, 10)
	}
	return h(f.t.Context(), bus.Message{
		ID: id, Topic: events.TopicDocUploaded, Data: []byte(payload),
		Attrs: map[string]string{"workspace_id": f.ws.String()}, Attempt: attempt,
	})
}

func (f *fixture) processed() int {
	return f.count(`SELECT count(*) FROM platform.processed_events WHERE consumer = $1`, consumerAnalyze)
}

// lastStatusEvent is the payload of the newest file.status_changed event.
func (f *fixture) lastStatusEvent() string {
	f.t.Helper()
	all := f.payloads(events.TopicFileStatusChanged)
	return all[len(all)-1]
}

func hasStatus(payload string, status filesv1.FileStatus, reason string) bool {
	return strings.Contains(payload, `"`+status.String()+`"`) && (reason == "" || strings.Contains(payload, `"`+reason+`"`))
}

// ---- instant summary --------------------------------------------------------

func TestAnalyze_TextFile_PostsReadySlipWithDOCRef(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	id := f.attach(ch, "warehouse.txt", "text/plain", []byte("Bang Na warehouse capacity: 12,000 pallets. คลังสินค้าบางนา เปิดไตรมาส 4"))
	f.llm.QueueGenerate(reply(summaryJSON))
	created := f.topicCount(events.TopicChatMessageCreated)

	if err := f.deliver(1); err != nil {
		t.Fatalf("doc.analyze: %v", err)
	}

	// One call, shaped as asked, carrying the file.
	calls := f.llm.GenerateCalls()
	if len(calls) != 1 {
		t.Fatalf("%d model calls, want 1", len(calls))
	}
	req := calls[0]
	if req.Tier != llm.TierFlash || req.Schema == nil || req.Schema.Name != "file_summary" || !strings.Contains(req.System, "document's own language") {
		t.Fatalf("request = tier %q, schema %v, system %q", req.Tier, req.Schema, req.System)
	}
	var sent strings.Builder
	for _, p := range req.Parts {
		sent.WriteString(p.Text)
	}
	if !strings.Contains(sent.String(), "warehouse.txt") || !strings.Contains(sent.String(), "คลังสินค้าบางนา") {
		t.Fatalf("the model was sent %q, want the file name and its text", sent.String())
	}

	file := f.file(id)
	if file.GetStatus() != filesv1.FileStatus_FILE_STATUS_READY || file.GetRef() != "DOC-1001" || file.GetPages() != 1 || file.GetFailureReason() != "" {
		t.Fatalf("file = %v, want READY with DOC-1001 and 1 page", file)
	}

	// The slip follows the attachment, from the assistant, citing the file.
	msgs := f.messages(ch)
	if len(msgs) != 2 {
		t.Fatalf("%d messages, want the attachment and the slip", len(msgs))
	}
	slip := msgs[1]
	if slip.GetAuthorKind() != chatv1.AuthorKind_AUTHOR_KIND_AGENT || slip.GetAuthorId() != authz.AgentID("assistant").String() {
		t.Fatalf("slip author = %v %s, want the assistant", slip.GetAuthorKind(), slip.GetAuthorId())
	}
	art := slip.GetArtifact()
	if art.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_DOC_SUMMARY || art.GetRef() != "DOC-1001" || art.GetTitle() != "warehouse.txt" ||
		art.GetFileId() != id.String() || art.GetGeneratedAt() == nil || art.GetMeta() != "TXT" {
		t.Fatalf("artifact = %v", art)
	}
	for _, want := range []string{"The Bang Na warehouse holds 12,000 pallets.", "- 12,000 pallets", "- opens in Q4"} {
		if !strings.Contains(art.GetBodyMarkdown(), want) {
			t.Fatalf("slip body = %q, missing %q", art.GetBodyMarkdown(), want)
		}
	}
	if len(art.GetSources()) != 1 {
		t.Fatalf("slip sources = %v, want the file", art.GetSources())
	}
	if src := art.GetSources()[0]; src.GetKind() != chatv1.SourceKind_SOURCE_KIND_FILE || src.GetId() != id.String() ||
		src.GetLabel() != "warehouse.txt" || src.GetChannelId() != ch.String() {
		t.Fatalf("slip source = %v, want the file", src)
	}

	// Exactly one new message event, the index request and the status change.
	if n := f.topicCount(events.TopicChatMessageCreated) - created; n != 1 {
		t.Fatalf("%d chat.message.created events from the analysis, want 1", n)
	}
	idx := f.payloads(events.TopicEmbedIndex)
	if len(idx) != 1 || !strings.Contains(idx[0], `"doc"`) || !strings.Contains(idx[0], id.String()) || !strings.Contains(idx[0], ch.String()) {
		t.Fatalf("embed.index = %v, want one for the doc", idx)
	}
	if !hasStatus(f.lastStatusEvent(), filesv1.FileStatus_FILE_STATUS_READY, "") {
		t.Fatalf("last file.status_changed = %s, want READY", f.lastStatusEvent())
	}
	if n := f.processed(); n != 1 {
		t.Fatalf("%d processed events, want 1", n)
	}
}

func TestAnalyze_ScannedPDFOnOpenAI_FailsWithReason(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.attach(ch, "scan.pdf", "application/pdf", imageOnlyPDF())
	created := f.topicCount(events.TopicChatMessageCreated)

	// Acknowledged, not retried: nothing will make a scan readable.
	if err := f.deliver(1); err != nil {
		t.Fatalf("doc.analyze = %v, want it acknowledged", err)
	}

	file := f.file(id)
	if file.GetStatus() != filesv1.FileStatus_FILE_STATUS_FAILED || file.GetFailureReason() != reasonNoTextLayer || file.GetRef() != "" {
		t.Fatalf("file = %v, want FAILED %s without a ref", file, reasonNoTextLayer)
	}
	if len(f.messages(ch)) != 1 || f.topicCount(events.TopicChatMessageCreated) != created {
		t.Fatal("a failed analysis posted a slip")
	}
	if n := f.count(`SELECT count(*) FROM platform.ref_counters WHERE kind = 'DOC'`); n != 0 {
		t.Fatal("a failed analysis consumed a DOC reference")
	}
	if !hasStatus(f.lastStatusEvent(), filesv1.FileStatus_FILE_STATUS_FAILED, reasonNoTextLayer) {
		t.Fatalf("last file.status_changed = %s, want FAILED with the reason", f.lastStatusEvent())
	}
	if calls := f.llm.GenerateCalls(); len(calls) != 0 {
		t.Fatalf("%d model calls for a file that cannot be read, want 0", len(calls))
	}
	if f.processed() != 1 {
		t.Fatal("the delivery was not recorded as processed")
	}
	// The failure is logged with the file and the reason, so an operator can find it.
	if logged := f.logs.String(); !strings.Contains(logged, "file_id="+id.String()) || !strings.Contains(logged, "reason="+reasonNoTextLayer) {
		t.Fatalf("log = %q, want the file and the reason", logged)
	}
	if f.topicCount(events.TopicEmbedIndex) != 0 {
		t.Fatal("a failed file was queued for indexing")
	}
}

func TestAnalyze_FinalAttemptError_MarksFailed(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.attach(ch, "notes.txt", "text/plain", []byte("hello team"))
	f.llm.QueueGenerate(replyErr(errors.New("upstream exploded")))
	created := f.topicCount(events.TopicChatMessageCreated)

	if err := f.deliver(finalAttempt); err != nil {
		t.Fatalf("doc.analyze on the final attempt = %v, want it acknowledged", err)
	}
	file := f.file(id)
	if file.GetStatus() != filesv1.FileStatus_FILE_STATUS_FAILED || file.GetFailureReason() != reasonModelUnavailable || file.GetRef() != "" {
		t.Fatalf("file = %v, want FAILED %s without a ref", file, reasonModelUnavailable)
	}
	if f.topicCount(events.TopicChatMessageCreated) != created || f.processed() != 1 {
		t.Fatal("the failure posted a slip or was not recorded")
	}
	if !hasStatus(f.lastStatusEvent(), filesv1.FileStatus_FILE_STATUS_FAILED, reasonModelUnavailable) {
		t.Fatalf("last file.status_changed = %s", f.lastStatusEvent())
	}

	// Retry starts over and a failure left no reference behind: the first success is DOC-1001.
	if err := f.svc.RetryAnalysis(t.Context(), f.user(f.alice), id); err != nil {
		t.Fatalf("RetryAnalysis: %v", err)
	}
	f.llm.QueueGenerate(reply(summaryJSON))
	if err := f.deliver(1); err != nil {
		t.Fatalf("doc.analyze after the retry: %v", err)
	}
	if file := f.file(id); file.GetStatus() != filesv1.FileStatus_FILE_STATUS_READY || file.GetRef() != "DOC-1001" {
		t.Fatalf("file after the retry = %v, want READY with DOC-1001", file)
	}
	if len(f.messages(ch)) != 2 {
		t.Fatal("the retried analysis did not post exactly one slip")
	}
}

func TestAnalyze_TransientErrorBeforeFinalAttempt_Nacks(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.attach(ch, "notes.txt", "text/plain", []byte("hello team"))
	boom := errors.New("upstream exploded")
	topics := f.topics()

	for attempt := 1; attempt < finalAttempt; attempt++ {
		f.llm.QueueGenerate(replyErr(boom))
		if err := f.deliver(attempt); !errors.Is(err, boom) {
			t.Fatalf("attempt %d: doc.analyze = %v, want the error returned to nack", attempt, err)
		}
		if got := f.file(id); got.GetStatus() != filesv1.FileStatus_FILE_STATUS_ANALYZING || got.GetFailureReason() != "" {
			t.Fatalf("attempt %d: file = %v, want still analyzing", attempt, got)
		}
		if f.processed() != 0 || !slices.Equal(f.topics(), topics) {
			t.Fatalf("attempt %d: a nacked delivery left effects behind", attempt)
		}
	}

	// The redelivery that works is not held back by the nacks before it, not even on the
	// last attempt.
	f.llm.QueueGenerate(reply(summaryJSON))
	if err := f.deliver(finalAttempt); err != nil {
		t.Fatalf("final attempt that succeeds: %v", err)
	}
	if got := f.file(id); got.GetStatus() != filesv1.FileStatus_FILE_STATUS_READY || got.GetRef() != "DOC-1001" {
		t.Fatalf("file = %v, want READY", got)
	}
	if len(f.messages(ch)) != 2 {
		t.Fatal("the slip was not posted exactly once")
	}
}

func TestAnalyze_PermanentAndBudgetErrors(t *testing.T) {
	cases := []struct {
		name    string
		reply   fakellm.GenerateReply
		attempt int
		// acked is the file's failure reason when the message is acknowledged; empty when
		// it is nacked and the file waits.
		acked string
	}{
		{"unsupported by the provider is permanent", replyErr(fmt.Errorf("adapter: %w", llm.ErrUnsupportedByProvider)), 1, reasonUnsupportedByProvider},
		{"budget exhausted waits, even on the last attempt", replyErr(llm.ErrBudgetExhausted), finalAttempt, ""},
		{"output that is not JSON is retried", reply("Sure! Here is a summary."), 1, ""},
		{"an empty summary is retried", reply(`{"summary":"  ","key_points":[]}`), 1, ""},
		{"truncated output is retried", replyErr(llm.ErrTruncated), 2, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			ch := f.channel("public", f.alice)
			id := f.attach(ch, "notes.txt", "text/plain", []byte("hello team"))
			f.llm.QueueGenerate(c.reply)

			err := f.deliver(c.attempt)
			got := f.file(id)
			if c.acked == "" {
				if err == nil {
					t.Fatal("doc.analyze acknowledged a delivery that should be retried")
				}
				if got.GetStatus() != filesv1.FileStatus_FILE_STATUS_ANALYZING {
					t.Fatalf("file = %v, want still analyzing", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("doc.analyze = %v, want it acknowledged", err)
			}
			if got.GetStatus() != filesv1.FileStatus_FILE_STATUS_FAILED || got.GetFailureReason() != c.acked {
				t.Fatalf("file = %v, want FAILED %s", got, c.acked)
			}
		})
	}
}

func TestAnalyze_RedeliveryAppliesOnce(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.attach(ch, "notes.txt", "text/plain", []byte("hello team"))
	f.llm.HandleGenerate(func(context.Context, llm.Request) (llm.Result, error) {
		return llm.Result{Text: summaryJSON}, nil
	})

	if err := f.deliver(1); err != nil {
		t.Fatal(err)
	}
	// The same message again, as at-least-once delivery will: acknowledged, not redone.
	if err := f.deliver(2); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if n := len(f.llm.GenerateCalls()); n != 1 {
		t.Fatalf("%d model calls for one message delivered twice, want 1", n)
	}
	if n := len(f.messages(ch)); n != 2 {
		t.Fatalf("%d messages after a redelivery, want the attachment and one slip", n)
	}

	// The same event delivered under another id once the file is settled: the file is no
	// longer analyzing, so there is nothing to do.
	if err := f.deliverTo(f.analyze, "stale-1", 1); err != nil {
		t.Fatalf("event for a settled file: %v", err)
	}
	if n := len(f.llm.GenerateCalls()); n != 1 || len(f.messages(ch)) != 2 {
		t.Fatalf("a settled file was analysed again (%d calls, %d messages)", n, len(f.messages(ch)))
	}
	if got := f.file(id); got.GetRef() != "DOC-1001" || f.count(`SELECT last FROM platform.ref_counters WHERE kind = 'DOC'`) != 1001 {
		t.Fatalf("file ref = %q, want DOC-1001 and one reference consumed", got.GetRef())
	}
}

func TestAnalyze_ConcurrentDeliveryAppliesOnce(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.attach(ch, "notes.txt", "text/plain", []byte("hello team"))
	f.llm.HandleGenerate(func(context.Context, llm.Request) (llm.Result, error) {
		return llm.Result{Text: summaryJSON}, nil
	})

	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = f.deliver(1)
		}()
	}
	wg.Wait()
	for _, err := range results {
		if err != nil {
			t.Fatalf("doc.analyze: %v", err)
		}
	}
	if n := len(f.messages(ch)); n != 2 {
		t.Fatalf("%d messages after concurrent deliveries, want the attachment and one slip", n)
	}
	if got := f.file(id); got.GetStatus() != filesv1.FileStatus_FILE_STATUS_READY || got.GetRef() != "DOC-1001" {
		t.Fatalf("file = %v", got)
	}
	if n := f.topicCount(events.TopicEmbedIndex); n != 1 {
		t.Fatalf("%d embed.index events, want 1", n)
	}
}

func TestAnalyze_SetsPagesFromThePDFAndOneForTextFiles(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)

	pdf := f.attach(ch, "three.pdf", "application/pdf", minimalPDF("alpha page", "beta page", "gamma page"))
	f.llm.QueueGenerate(reply(summaryJSON))
	if err := f.deliver(1); err != nil {
		t.Fatal(err)
	}
	if got := f.file(pdf); got.GetStatus() != filesv1.FileStatus_FILE_STATUS_READY || got.GetPages() != 3 {
		t.Fatalf("pdf = %v, want READY with 3 pages", got)
	}
	// The text layer is what the model reads, and the slip says how long the file is.
	var sent strings.Builder
	for _, p := range f.llm.GenerateCalls()[0].Parts {
		sent.WriteString(p.Text)
	}
	for _, want := range []string{"three.pdf", "alpha page", "beta page", "gamma page"} {
		if !strings.Contains(sent.String(), want) {
			t.Fatalf("the model was sent %q, missing %q", sent.String(), want)
		}
	}
	msgs := f.messages(ch)
	if meta := msgs[len(msgs)-1].GetArtifact().GetMeta(); meta != "PDF · 3 pages" {
		t.Fatalf("slip meta = %q, want %q", meta, "PDF · 3 pages")
	}

	txt := f.attach(ch, "notes.md", "text/markdown", []byte("# Notes\n\nfive\nlines\nof\ntext"))
	f.llm.QueueGenerate(reply(summaryJSON))
	if err := f.deliver(1); err != nil {
		t.Fatal(err)
	}
	if got := f.file(txt); got.GetStatus() != filesv1.FileStatus_FILE_STATUS_READY || got.GetPages() != 1 || got.GetRef() != "DOC-1002" {
		t.Fatalf("text file = %v, want READY, 1 page, DOC-1002", got)
	}

	// An image goes to the model as an image and counts as one page.
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	img := f.attach(ch, "photo.png", "image/png", png)
	f.llm.QueueGenerate(reply(summaryJSON))
	if err := f.deliver(1); err != nil {
		t.Fatal(err)
	}
	if got := f.file(img); got.GetStatus() != filesv1.FileStatus_FILE_STATUS_READY || got.GetPages() != 1 {
		t.Fatalf("image = %v, want READY with 1 page", got)
	}
	parts := f.llm.GenerateCalls()[2].Parts
	if len(parts) != 2 || !bytes.Equal(parts[1].Data, png) || parts[1].MIME != "image/png" || parts[1].URI != "" {
		t.Fatalf("image request parts = %+v, want the file name and the image bytes", parts)
	}
}

// withURI is a store that, like GCS, has an address of its own for every object.
type withURI struct{ blob.Store }

func (withURI) URI(key string) string { return "gs://kiwi-test/" + key }

func TestAnalyze_PDFWithProviderURI_PassesTheURI(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.attach(ch, "scan.pdf", "application/pdf", minimalPDF("one", "two"))
	f.llm.QueueGenerate(reply(summaryJSON))

	deps := f.deps
	deps.Blob = withURI{f.local}
	if err := f.deliverTo(Jobs(deps)[0].Handler, "", 1); err != nil {
		t.Fatal(err)
	}
	parts := f.llm.GenerateCalls()[0].Parts
	if len(parts) != 2 || parts[1].URI != "gs://kiwi-test/"+blobKey(f.ws, id) || parts[1].MIME != "application/pdf" || parts[1].Text != "" || len(parts[1].Data) != 0 {
		t.Fatalf("request parts = %+v, want the provider's URI and no text", parts)
	}
	// The page count still comes from the bytes.
	if got := f.file(id); got.GetPages() != 2 || got.GetStatus() != filesv1.FileStatus_FILE_STATUS_READY {
		t.Fatalf("file = %v, want READY with 2 pages", got)
	}
}

func TestAnalyze_UnreadableInputs_FailWithReason(t *testing.T) {
	cases := []struct {
		name   string
		file   string
		mime   string
		body   []byte
		mutate func(f *fixture, id uuid.UUID)
		reason string
	}{
		{"a PDF that is not one", "broken.pdf", "application/pdf", []byte("this is not a pdf at all"), nil, reasonUnreadable},
		{"a truncated PDF", "cut.pdf", "application/pdf", minimalPDF("one", "two")[:80], nil, reasonUnreadable},
		{"a text file of blanks", "blank.txt", "text/plain", []byte("  \n\t \n"), nil, reasonEmpty},
		{"an object that is gone", "gone.txt", "text/plain", []byte("hello"), func(f *fixture, id uuid.UUID) {
			if err := f.local.Delete(f.t.Context(), blobKey(f.ws, id)); err != nil {
				f.t.Fatal(err)
			}
		}, reasonUnreadable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			ch := f.channel("public", f.alice)
			id := f.attach(ch, c.file, c.mime, c.body)
			if c.mutate != nil {
				c.mutate(f, id)
			}
			if err := f.deliver(1); err != nil {
				t.Fatalf("doc.analyze = %v, want it acknowledged", err)
			}
			got := f.file(id)
			if got.GetStatus() != filesv1.FileStatus_FILE_STATUS_FAILED || got.GetFailureReason() != c.reason || got.GetRef() != "" {
				t.Fatalf("file = %v, want FAILED %s", got, c.reason)
			}
			if n := len(f.llm.GenerateCalls()); n != 0 {
				t.Fatalf("%d model calls for an unreadable file", n)
			}
		})
	}
}

func TestAnalyze_LongTextIsCapped(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	f.attach(ch, "long.txt", "text/plain", []byte(strings.Repeat("ก", 250_000)))
	f.llm.QueueGenerate(reply(summaryJSON))
	if err := f.deliver(1); err != nil {
		t.Fatal(err)
	}
	text := f.llm.GenerateCalls()[0].Parts[1].Text
	if n := strings.Count(text, "ก"); n != maxSummaryChars {
		t.Fatalf("the model was sent %d characters, want %d", n, maxSummaryChars)
	}
	if !strings.Contains(text, "only its first 200000 characters") {
		t.Fatal("the cut is not mentioned to the model")
	}
}

// ---- ExtractPages -----------------------------------------------------------

// ready completes an attachment as Alice and marks it ready, as a finished analysis
// would, so the extraction tests do not depend on the summary.
func (f *fixture) ready(ch uuid.UUID, name, mime string, body []byte) uuid.UUID {
	f.t.Helper()
	id := f.attach(ch, name, mime, body)
	f.exec(`UPDATE files.files SET status = 'ready' WHERE id = $1`, id)
	return id
}

func TestExtractPages_PDFTextAndImage(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice)
	indexer := f.system(f.alice)

	t.Run("a PDF gives the text layer of each page, keeping the numbers of the blank ones", func(t *testing.T) {
		id := f.ready(ch, "q4.pdf", "application/pdf", minimalPDF("first page", "", "third page"))
		pages, err := f.svc.ExtractPages(t.Context(), indexer, id)
		if err != nil {
			t.Fatal(err)
		}
		want := []Page{{N: 1, Markdown: "first page"}, {N: 3, Markdown: "third page"}}
		if !slices.Equal(pages, want) {
			t.Fatalf("pages = %v, want %v", pages, want)
		}
	})
	t.Run("a text file is one page", func(t *testing.T) {
		id := f.ready(ch, "notes.txt", "text/plain", []byte("\xEF\xBB\xBFสวัสดี team\x00"))
		pages, err := f.svc.ExtractPages(t.Context(), indexer, id)
		if err != nil || !slices.Equal(pages, []Page{{N: 1, Markdown: "สวัสดี team"}}) {
			t.Fatalf("pages = %v, %v, want one page without the BOM and the NUL", pages, err)
		}
	})
	t.Run("an empty text file has no pages", func(t *testing.T) {
		id := f.ready(ch, "blank.txt", "text/plain", []byte(" \n"))
		if pages, err := f.svc.ExtractPages(t.Context(), indexer, id); err != nil || len(pages) != 0 {
			t.Fatalf("pages = %v, %v, want none", pages, err)
		}
	})
	t.Run("an image is read by the model", func(t *testing.T) {
		png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
		id := f.ready(ch, "invoice.png", "image/png", png)
		f.llm.QueueGenerate(reply("  Invoice 42\nTotal 1,000 THB\n"))
		pages, err := f.svc.ExtractPages(t.Context(), indexer, id)
		if err != nil || len(pages) != 1 || pages[0].N != 1 || pages[0].Markdown != "Invoice 42\nTotal 1,000 THB" {
			t.Fatalf("pages = %v, %v, want the model's text as page 1", pages, err)
		}
		calls := f.llm.GenerateCalls()
		req := calls[len(calls)-1]
		if len(req.Parts) != 1 || !bytes.Equal(req.Parts[0].Data, png) || req.Parts[0].MIME != "image/png" || req.Schema != nil {
			t.Fatalf("vision request = %+v, want the image and no schema", req)
		}
	})
	t.Run("a scanned PDF cannot be read here", func(t *testing.T) {
		id := f.ready(ch, "scan.pdf", "application/pdf", imageOnlyPDF())
		if _, err := f.svc.ExtractPages(t.Context(), indexer, id); !errors.Is(err, llm.ErrUnsupportedByProvider) {
			t.Fatalf("ExtractPages of a scan = %v, want llm.ErrUnsupportedByProvider", err)
		}
	})
	t.Run("a model that fails fails the extraction", func(t *testing.T) {
		id := f.ready(ch, "photo.png", "image/png", append([]byte("\x89PNG\r\n\x1a\n"), 1))
		boom := errors.New("vision exploded")
		f.llm.QueueGenerate(replyErr(boom))
		if _, err := f.svc.ExtractPages(t.Context(), indexer, id); !errors.Is(err, boom) {
			t.Fatalf("ExtractPages = %v, want the model's error", err)
		}
	})
}

func TestExtractPages_Authorization(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice)
	id := f.ready(ch, "notes.txt", "text/plain", []byte("hello"))
	pending := f.attach(ch, "pending.txt", "text/plain", []byte("hello"))

	// The indexer is a system actor and needs no membership; people need it.
	if _, err := f.svc.ExtractPages(t.Context(), f.system(f.bob), id); err != nil {
		t.Fatalf("ExtractPages as the indexer: %v", err)
	}
	if _, err := f.svc.ExtractPages(t.Context(), f.user(f.alice), id); err != nil {
		t.Fatalf("ExtractPages by a member: %v", err)
	}
	if _, err := f.svc.ExtractPages(t.Context(), f.user(f.bob), id); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("ExtractPages by a non-member = %v, want ErrForbidden", err)
	}
	if _, err := f.svc.ExtractPages(t.Context(), authz.Actor{}, id); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("ExtractPages without an actor = %v, want ErrForbidden", err)
	}
	if _, err := f.svc.ExtractPages(t.Context(), f.system(f.alice), uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("ExtractPages of an unknown file = %v, want ErrNotFound", err)
	}
	// A file still being analysed is not read for the index.
	if _, err := f.svc.ExtractPages(t.Context(), f.system(f.alice), pending); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("ExtractPages of an analyzing file = %v, want ErrConflict", err)
	}
}

// ---- PDF fixtures -----------------------------------------------------------

// minimalPDF is a valid PDF with a text layer: one Helvetica line per page.
func minimalPDF(pages ...string) []byte {
	contents := make([]string, len(pages))
	for i, text := range pages {
		contents[i] = fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(text))
	}
	return buildPDF(contents...)
}

// imageOnlyPDF is a valid one-page PDF with a picture and no text, like a scan.
func imageOnlyPDF() []byte {
	return buildPDF("q 400 0 0 400 100 200 cm /Im0 Do Q")
}

// buildPDF writes a PDF whose pages draw the given content streams, with a Helvetica
// font as /F1 and a 1x1 grey image as /Im0 in each page's resources.
func buildPDF(contents ...string) []byte {
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n")
	var kids strings.Builder
	for i := range contents {
		fmt.Fprintf(&kids, "%d 0 R ", 5+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), len(contents)))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	obj("<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray /BitsPerComponent 8 /Length 1 >>\nstream\n\x80\nendstream")
	for i, c := range contents {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> /XObject << /Im0 4 0 R >> >> >>", 6+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(c), c))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}
