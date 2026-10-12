package meetings

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
)

func launchEntries() []*meetingsv1.Entry {
	return []*meetingsv1.Entry{
		{Idx: 0, Speaker: proto.String("Alice"), StartMs: proto.Int64(5_000), EndMs: proto.Int64(20_000), Text: "Let's start."},
		{Idx: 1, Speaker: proto.String("Bob"), StartMs: proto.Int64(21_000), EndMs: proto.Int64(40_000), Text: "We ship on Friday."},
		{Idx: 2, Speaker: proto.String("Alice"), StartMs: proto.Int64(41_000), EndMs: proto.Int64(60_000), Text: "Who owns QA?"},
	}
}

func (f *fixture) imported(ch uuid.UUID) Import {
	return Import{
		ID: uuid.New(), ChannelID: ch, Title: "Launch sync", Entries: launchEntries(),
		Summary: Summary{
			Summary:       "We agreed to ship on Friday.",
			Decisions:     []Item{{Text: "Ship on Friday", EntryRefs: []int{1}}},
			OpenQuestions: []Item{{Text: "Who owns QA?", EntryRefs: []int{2}}},
		},
	}
}

func TestImportMeeting_RecordsAReadyMeetingTheWayTheIngestDoes(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	in := f.imported(ch)

	if err := ImportMeeting(t.Context(), f.deps, f.user(f.alice), in); err != nil {
		t.Fatalf("ImportMeeting: %v", err)
	}

	row := f.meeting(in.ID)
	if row.Status != statusReady || row.Owner != f.alice || row.Ref == nil || *row.Ref != "MTG-1001" || row.MeetSpace != nil || row.RecordingID != nil {
		t.Fatalf("meeting = %+v, want a ready MTG-1001 of Alice's with no Meet space and no recording", row)
	}
	got, err := f.svc.GetMeeting(t.Context(), f.user(f.bob), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetTitle() != "Launch sync" || got.GetSummary() != "We agreed to ship on Friday." || len(got.GetDecisions()) != 1 || len(got.GetOpenQuestions()) != 1 {
		t.Errorf("GetMeeting = %v, want the title and the summary with its decisions and open questions", got)
	}
	entries, err := f.svc.TranscriptEntries(t.Context(), f.user(f.bob), in.ID)
	if err != nil || len(entries) != 3 || entries[1].GetSpeaker() != "Bob" || entries[1].GetStartMs() != 21_000 || entries[2].GetText() != "Who owns QA?" {
		t.Fatalf("TranscriptEntries = %v, %v, want the 3 entries with speakers and timing", entries, err)
	}

	// The slip is the one the pipeline posts: an assistant message id'd by the meeting.
	msgs := f.messages(ch)
	if len(msgs) != 1 || msgs[0].GetClientMsgId() != "meeting-summary-"+in.ID.String() || msgs[0].GetAuthorKind() != chatv1.AuthorKind_AUTHOR_KIND_AGENT {
		t.Fatalf("channel = %v, want the one summary slip", msgs)
	}
	a := msgs[0].GetArtifact()
	if a.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_MEETING_SUMMARY || a.GetRef() != "MTG-1001" || a.GetMeetingId() != in.ID.String() || len(a.GetTracks()) != 2 {
		t.Errorf("artifact = %v, want the MEETING_SUMMARY of MTG-1001 with a decision and an open question", a)
	}
	if tr := a.GetTracks()[0]; tr.GetEntryIdx() != 1 || tr.GetOffsetMs() != 21_000 {
		t.Errorf("decision track = %v, want entry 1 at 21000 ms (the transcript has timing)", tr)
	}

	// Indexing and the channel's status are announced; with no action items no draft is.
	embeds := f.outbox(events.TopicEmbedIndex)
	if len(embeds) != 1 {
		t.Fatalf("%d embed.index events, want 1", len(embeds))
	}
	var ei eventsv1.EmbedIndex
	f.payload(embeds[0], &ei)
	if ei.GetSourceKind() != "meeting" || ei.GetSourceId() != in.ID.String() {
		t.Errorf("embed.index = %v, want the meeting as a source", &ei)
	}
	if n := f.topicCount(events.TopicMeetingSummaryReady); n != 1 {
		t.Errorf("%d meeting.summary_ready events, want 1", n)
	}
	if n := f.topicCount(events.TopicTaskDraft); n != 0 {
		t.Errorf("%d task.draft events, want none for a summary without action items", n)
	}
}

func TestImportMeeting_ImportingTheSameMeetingAgainChangesNothing(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	in := f.imported(ch)
	var hooks int
	in.InTx = func(context.Context, pgx.Tx) error { hooks++; return nil }

	if err := ImportMeeting(t.Context(), f.deps, f.user(f.alice), in); err != nil {
		t.Fatalf("ImportMeeting: %v", err)
	}
	counts := func() (n [5]int) {
		for i, q := range []string{
			`SELECT count(*) FROM meetings.meetings`,
			`SELECT count(*) FROM meetings.transcript_entries`,
			`SELECT count(*) FROM meetings.summaries`,
			`SELECT count(*) FROM chat.messages`,
			`SELECT count(*) FROM platform.outbox`,
		} {
			n[i] = f.count(q)
		}
		return n
	}
	first := counts()
	if err := ImportMeeting(t.Context(), f.deps, f.user(f.alice), in); err != nil {
		t.Fatalf("ImportMeeting again: %v", err)
	}

	if second := counts(); second != first {
		t.Errorf("row counts %v after the second import, want %v", second, first)
	}
	if first[0] != 1 || first[1] != 3 || first[2] != 1 || first[3] != 1 {
		t.Errorf("row counts %v, want 1 meeting, 3 entries, 1 summary, 1 message", first)
	}
	if hooks != 1 {
		t.Errorf("InTx ran %d times, want once: only a new meeting runs it", hooks)
	}
}

func TestImportMeeting_InTxCommitsWithTheMeetingOrNotAtAll(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	in := f.imported(ch)
	boom := errors.New("boom")
	in.InTx = func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO platform.processed_events (consumer, event_id) VALUES ('seed-test', 'x')`); err != nil {
			return err
		}
		return boom
	}

	if err := ImportMeeting(t.Context(), f.deps, f.user(f.alice), in); !errors.Is(err, boom) {
		t.Fatalf("ImportMeeting = %v, want the hook's error", err)
	}

	for _, q := range []string{
		`SELECT count(*) FROM meetings.meetings`,
		`SELECT count(*) FROM meetings.summaries`,
		`SELECT count(*) FROM chat.messages`,
		`SELECT count(*) FROM platform.processed_events WHERE consumer = 'seed-test'`,
	} {
		if n := f.count(q); n != 0 {
			t.Errorf("%s = %d, want 0: a failing hook rolls the whole import back", q, n)
		}
	}
	if n := f.count(`SELECT count(*) FROM platform.ref_counters WHERE kind = 'MTG' AND last > 0`); n != 0 {
		t.Errorf("a rolled-back import consumed a REF")
	}
}

func TestImportMeeting_RefusesWhatItCannotRecord(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	empty := f.imported(ch)
	empty.Entries = nil
	gap := f.imported(ch)
	gap.Entries[1].Idx = 5
	untitled := f.imported(ch)
	untitled.Title = "  "
	noSummary := f.imported(ch)
	noSummary.Summary.Summary = ""
	noID := f.imported(ch)
	noID.ID = uuid.Nil

	for name, in := range map[string]Import{"no entries": empty, "entries that skip an index": gap, "a blank title": untitled, "an empty summary": noSummary, "no id": noID} {
		if err := ImportMeeting(t.Context(), f.deps, f.user(f.alice), in); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("%s: ImportMeeting = %v, want errs.ErrInvalid", name, err)
		}
	}
	if n := f.count(`SELECT count(*) FROM meetings.meetings`); n != 0 {
		t.Errorf("%d meetings after refused imports, want 0", n)
	}
}

func TestImportMeeting_NeedsAMemberOfTheChannel(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice)
	in := f.imported(ch)

	if err := ImportMeeting(t.Context(), f.deps, f.user(f.bob), in); !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("a non-member's ImportMeeting = %v, want errs.ErrForbidden", err)
	}
	if err := ImportMeeting(t.Context(), f.deps, f.system(f.alice), in); !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("a job's ImportMeeting = %v, want errs.ErrForbidden: only a person starts a meeting", err)
	}
	if n := f.count(`SELECT count(*) FROM meetings.meetings`); n != 0 {
		t.Errorf("%d meetings, want 0", n)
	}
}

func TestImportMeeting_ActionItemsAreAnnouncedForTheDrafter(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	in := f.imported(ch)
	in.Summary.ActionItems = []ActionItem{
		{Title: "Write the notes", OwnerHint: "Alice", DueHint: "+3d", EntryRefs: []int{1}},
		{Title: "Pick a QA owner", EntryRefs: []int{9}},
	}

	if err := ImportMeeting(t.Context(), f.deps, f.user(f.alice), in); err != nil {
		t.Fatal(err)
	}

	drafts := f.outbox(events.TopicTaskDraft)
	if len(drafts) != 1 {
		t.Fatalf("%d task.draft events, want 1", len(drafts))
	}
	var td eventsv1.TaskDraftRequested
	f.payload(drafts[0], &td)
	items := td.GetItems()
	if td.GetMeetingId() != in.ID.String() || td.GetOwnerUserId() != f.alice.String() || len(items) != 2 ||
		items[0].GetEntryIdx() != 1 || items[0].GetOffsetMs() != 21_000 || items[1].GetEntryIdx() != NoSourceEntry {
		t.Errorf("task.draft = %v, want the two items, the second without a source entry", &td)
	}
	if td.GetMeetingTitle() != "Launch sync" {
		t.Errorf("task.draft title = %q, want the meeting's", td.GetMeetingTitle())
	}
}
