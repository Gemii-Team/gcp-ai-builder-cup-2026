package meetings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
	"kiwi/internal/chat"
	"kiwi/internal/meetings/internal/store"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/google"
	"kiwi/internal/platform/outbox"
)

// Summarizer turns a transcript into a summary. It is implemented over the model; the
// consumers here only need this much of it.
type Summarizer interface {
	Summarize(ctx context.Context, in SummarizeInput) (Summary, error)
}

// SummarizeInput is what the summarizer reads: the meeting and every transcript entry.
type SummarizeInput struct {
	MeetingTitle string
	MeetingDate  time.Time
	Entries      []*meetingsv1.Entry
}

// Summary is what the model found in a transcript. EntryRefs are the idx of the
// transcript entries each finding comes from.
type Summary struct {
	Summary       string
	Decisions     []Item
	OpenQuestions []Item
	ActionItems   []ActionItem
}

// Item is a decision or an open question.
type Item struct {
	Text      string
	EntryRefs []int
}

// ActionItem is something someone should do. The hints are what the transcript said
// about who and when; code, not the model, resolves them.
type ActionItem struct {
	Title, OwnerHint, DueHint string
	EntryRefs                 []int
}

const (
	consumerIngest          = "meeting.ingest"
	consumerTranscriptCheck = "meeting.transcript_check"

	// finalAttempt is the delivery the bus dead-letters a message after: a read or a
	// summary that still fails on it is recorded as a failed meeting, because nothing
	// will deliver the message again and the meeting would show a spinner for ever.
	finalAttempt = 5
	// rereadDelay is how long after a read that disagrees with the last one the
	// transcript is read again.
	rereadDelay = 2 * time.Minute
)

// pipeline is what the consumers of the Meet path run with.
type pipeline struct {
	db         *db.DB
	chat       chat.Service
	google     google.Client
	summarizer Summarizer
	clock      func() time.Time
}

func newPipeline(d Deps) *pipeline {
	return &pipeline{db: d.DB, chat: d.Chat, google: d.Google, summarizer: d.Summarizer, clock: clockOf(d)}
}

// decodeEvent reads a bus message into ev.
func decodeEvent(m bus.Message, ev proto.Message) error {
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(m.Data, ev); err != nil {
		return fmt.Errorf("meetings: decode %s: %v: %w", m.Topic, err, errs.ErrInvalid)
	}
	return nil
}

// waiting reads the Meet meeting an event names and reports whether it is still in
// status, the one the event is for. A meeting that is gone, has moved on (a retry
// leaves the event of the first run behind) or never had a Meet space has nothing for
// the event to do, and the event is acknowledged.
func (p *pipeline) waiting(ctx context.Context, workspaceID, meetingID, status string) (store.MeetingsMeeting, bool, error) {
	ws, err := parseID(workspaceID, "workspace id")
	if err != nil {
		return store.MeetingsMeeting{}, false, err
	}
	id, err := parseID(meetingID, "meeting id")
	if err != nil {
		return store.MeetingsMeeting{}, false, err
	}
	row, err := load(ctx, p.db, ws, id)
	if errors.Is(err, errs.ErrNotFound) {
		return store.MeetingsMeeting{}, false, nil
	}
	if err != nil {
		return store.MeetingsMeeting{}, false, err
	}
	return row, row.Status == status && row.MeetSpace != nil, nil
}

// prepareIngest is the meeting.ingest job: it reads a transcript as the meeting's
// owner. Meet can serve a transcript that is still incomplete just after it is
// generated, so a read only counts once the next one finds the same number of
// entries; until then the entry count is remembered and the transcript is read again
// later (the job enqueues itself with a not-before time, it never sleeps). Two equal
// reads go to the summarizer and then to one transaction that writes everything.
//
// An empty transcript is judged the same way: two reads of zero entries end the
// meeting as no_speech. Losing the owner's access, or a read or a summary that still
// fails on the final attempt, also ends it with a reason; any other error leaves the
// meeting ingesting and the message to be delivered again.
func (p *pipeline) prepareIngest(ctx context.Context, m bus.Message) (outbox.Apply, error) {
	var ev eventsv1.MeetingIngest
	if err := decodeEvent(m, &ev); err != nil {
		return nil, err
	}
	row, ok, err := p.waiting(ctx, ev.GetWorkspaceId(), ev.GetMeetingId(), statusIngesting)
	if err != nil || !ok {
		return nil, err
	}

	name := ev.GetTranscriptName()
	if name == "" {
		// A retry does not know which transcript failed: ask Meet which one there is.
		var found bool
		if name, found, err = p.findTranscript(ctx, row); err != nil {
			return p.readFailed(ctx, row, m, err)
		}
		if !found {
			return p.noTranscript(row, statusIngesting), nil
		}
	}

	read, err := p.google.ListTranscriptEntries(ctx, row.OwnerUserID, name)
	if err != nil {
		return p.readFailed(ctx, row, m, err)
	}
	count := int32(len(read))
	if row.LastEntryCount == nil || *row.LastEntryCount != count {
		return p.reread(row, name, ev.GetAttempt(), count), nil
	}
	if count == 0 {
		return p.fail(ctx, row, reasonNoSpeech, m.Attempt, errors.New("the transcript has no entries")), nil
	}

	entries := make([]*meetingsv1.Entry, 0, len(read))
	for _, e := range read {
		entries = append(entries, entryOf(e))
	}
	sum, err := p.summarizer.Summarize(ctx, SummarizeInput{MeetingTitle: row.Title, MeetingDate: meetingDate(row), Entries: entries})
	switch {
	case err == nil:
		return p.ready(row, entries, sum), nil
	case ctx.Err() != nil:
		// Not the meeting's fault: a process that is shutting down will find the
		// message again.
		return nil, err
	case m.Attempt >= finalAttempt:
		return p.fail(ctx, row, reasonModelUnavailable, m.Attempt, err), nil
	}
	return nil, err
}

// readFailed decides what an error from Meet means for the meeting: its owner's access
// is gone (fail now, retrying cannot help), Meet kept failing until the final attempt
// (fail), or it may pass (nack).
func (p *pipeline) readFailed(ctx context.Context, row store.MeetingsMeeting, m bus.Message, err error) (outbox.Apply, error) {
	switch {
	case ctx.Err() != nil:
		return nil, err
	case google.IsAccessDenied(err):
		return p.fail(ctx, row, reasonMeetAccess, m.Attempt, err), nil
	case m.Attempt >= finalAttempt:
		return p.fail(ctx, row, reasonMeetUnavailable, m.Attempt, err), nil
	}
	return nil, err
}

// findTranscript asks Meet for the transcript the poll would have picked.
func (p *pipeline) findTranscript(ctx context.Context, row store.MeetingsMeeting) (name string, found bool, err error) {
	confs, err := p.google.ListConferences(ctx, row.OwnerUserID, *row.MeetSpace)
	if err != nil {
		return "", false, err
	}
	conf, ok := latestConference(confs)
	if !ok {
		return "", false, nil
	}
	name, found = earliestGenerated(conf)
	return name, found, nil
}

// reread returns the write that remembers how many entries the transcript has and
// asks for another read in rereadDelay.
func (p *pipeline) reread(row store.MeetingsMeeting, transcript string, attempt, count int32) outbox.Apply {
	return func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.LockMeeting(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("meetings: lock meeting: %w", err)
		}
		if cur.Status != statusIngesting {
			return nil
		}
		if _, err := q.SetLastEntryCount(ctx, store.SetLastEntryCountParams{ID: row.ID, LastEntryCount: &count}); err != nil {
			return fmt.Errorf("meetings: remember entry count: %w", err)
		}
		return enqueue(ctx, tx, events.TopicMeetingIngest, cur, p.clock().Add(rereadDelay), &eventsv1.MeetingIngest{
			WorkspaceId: cur.WorkspaceID.String(), MeetingId: cur.ID.String(), TranscriptName: transcript, Attempt: attempt + 1,
		})
	}
}

// fail returns the write that records the failure of a meeting.
func (p *pipeline) fail(ctx context.Context, row store.MeetingsMeeting, reason string, attempt int, cause error) outbox.Apply {
	slog.WarnContext(ctx, "meetings: ingest failed", "meeting_id", row.ID, "reason", reason, "attempt", attempt, "error", cause)
	return func(ctx context.Context, tx pgx.Tx) error { return failMeeting(ctx, tx, row.ID, reason) }
}

// ready returns the write that records a summary, in one transaction: the entries, the
// summary and the status ready, the slip in the channel, and the events that start the
// drafting of tasks, the indexing and the refresh of the channel.
func (p *pipeline) ready(row store.MeetingsMeeting, entries []*meetingsv1.Entry, sum Summary) outbox.Apply {
	return func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.LockMeeting(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("meetings: lock meeting: %w", err)
		}
		if cur.Status != statusIngesting {
			return nil
		}
		rows, err := entriesJSON(entries)
		if err != nil {
			return err
		}
		if err := q.InsertTranscriptEntries(ctx, store.InsertTranscriptEntriesParams{
			MeetingID: cur.ID, WorkspaceID: cur.WorkspaceID, Entries: rows,
		}); err != nil {
			return fmt.Errorf("meetings: write transcript: %w", err)
		}
		decisions, err := itemsJSON(sum.Decisions)
		if err != nil {
			return err
		}
		questions, err := itemsJSON(sum.OpenQuestions)
		if err != nil {
			return err
		}
		if err := q.InsertSummary(ctx, store.InsertSummaryParams{
			MeetingID: cur.ID, WorkspaceID: cur.WorkspaceID, Summary: sum.Summary, Decisions: decisions, OpenQuestions: questions,
		}); err != nil {
			return fmt.Errorf("meetings: write summary: %w", err)
		}
		ready, err := q.MarkMeetingReady(ctx, cur.ID)
		if err != nil {
			return fmt.Errorf("meetings: mark meeting ready: %w", err)
		}

		post := agentPost(ready, "meeting-summary-"+ready.ID.String(), deref(ready.Ref)+" · "+ready.Title)
		post.Artifact = p.slip(ready, sum, entries)
		if _, err := p.chat.PostMessageTx(ctx, tx, systemActor(ready), post); err != nil {
			return fmt.Errorf("meetings: post summary: %w", err)
		}
		if items := draftItems(sum.ActionItems, entries); len(items) > 0 {
			if err := enqueue(ctx, tx, events.TopicTaskDraft, ready, time.Time{}, &eventsv1.TaskDraftRequested{
				WorkspaceId: ready.WorkspaceID.String(), MeetingId: ready.ID.String(), MeetingTitle: ready.Title,
				ChannelId: ready.ChannelID.String(), OwnerUserId: ready.OwnerUserID.String(), Items: items,
			}); err != nil {
				return err
			}
		}
		if err := enqueue(ctx, tx, events.TopicEmbedIndex, ready, time.Time{}, &eventsv1.EmbedIndex{
			WorkspaceId: ready.WorkspaceID.String(), SourceKind: "meeting", SourceId: ready.ID.String(), ChannelId: ready.ChannelID.String(),
		}); err != nil {
			return err
		}
		return enqueueStatus(ctx, tx, ready)
	}
}

// slip is the MEETING_SUMMARY artifact: the summary, with its decisions and open
// questions as tracks and the meeting as its only source. A track links to the
// transcript only when the entry it comes from has timing; a transcript without
// timing gets no offset to link to.
func (p *pipeline) slip(row store.MeetingsMeeting, sum Summary, entries []*meetingsv1.Entry) *chatv1.Artifact {
	byIdx := indexEntries(entries)
	tracks := make([]*chatv1.Track, 0, len(sum.Decisions)+len(sum.OpenQuestions))
	for _, it := range sum.Decisions {
		tracks = append(tracks, track(chatv1.TrackKind_TRACK_KIND_DECISION, it, byIdx))
	}
	for _, it := range sum.OpenQuestions {
		tracks = append(tracks, track(chatv1.TrackKind_TRACK_KIND_OPEN_QUESTION, it, byIdx))
	}
	return &chatv1.Artifact{
		Kind:         chatv1.ArtifactKind_ARTIFACT_KIND_MEETING_SUMMARY,
		Ref:          deref(row.Ref),
		Title:        row.Title,
		Meta:         meetingDate(row).Format(time.DateOnly),
		BodyMarkdown: sum.Summary,
		Tracks:       tracks,
		Sources: []*chatv1.Source{{
			Kind: chatv1.SourceKind_SOURCE_KIND_MEETING, Id: row.ID.String(), Label: row.Title, ChannelId: row.ChannelID.String(),
		}},
		GeneratedAt: timestamppb.New(p.clock()),
		MeetingId:   proto.String(row.ID.String()),
	}
}

func track(kind chatv1.TrackKind, it Item, byIdx map[int32]*meetingsv1.Entry) *chatv1.Track {
	t := &chatv1.Track{Kind: kind, Text: it.Text}
	if e := anchorEntry(it.EntryRefs, byIdx); e != nil && e.StartMs != nil {
		t.EntryIdx = proto.Int32(e.GetIdx())
		t.OffsetMs = proto.Int64(e.GetStartMs())
	}
	return t
}

// draftItems turns the action items into what the task drafter reads. Each points at
// the first transcript entry it names; offset_ms is that entry's start and stays unset
// for an entry without timing. An item that names no entry points at the first one.
func draftItems(items []ActionItem, entries []*meetingsv1.Entry) []*eventsv1.TaskDraftRequested_Item {
	byIdx := indexEntries(entries)
	out := make([]*eventsv1.TaskDraftRequested_Item, 0, len(items))
	for _, ai := range items {
		it := &eventsv1.TaskDraftRequested_Item{Title: ai.Title, OwnerHint: ai.OwnerHint, DueHint: ai.DueHint}
		if e := anchorEntry(ai.EntryRefs, byIdx); e != nil {
			it.EntryIdx = e.GetIdx()
			if e.StartMs != nil {
				it.OffsetMs = proto.Int64(e.GetStartMs())
			}
		}
		out = append(out, it)
	}
	return out
}

// anchorEntry is the first entry that refs names and the transcript has; a reference
// to an entry that does not exist is the model's mistake and is ignored.
func anchorEntry(refs []int, byIdx map[int32]*meetingsv1.Entry) *meetingsv1.Entry {
	for _, r := range refs {
		if r < 0 || r > math.MaxInt32 {
			continue
		}
		if e, ok := byIdx[int32(r)]; ok {
			return e
		}
	}
	return nil
}

func indexEntries(entries []*meetingsv1.Entry) map[int32]*meetingsv1.Entry {
	byIdx := make(map[int32]*meetingsv1.Entry, len(entries))
	for _, e := range entries {
		byIdx[e.GetIdx()] = e
	}
	return byIdx
}

// entryOf maps a Meet entry. Postgres refuses NUL in text, so it is dropped here
// rather than failing the whole meeting; an empty speaker means Meet named none.
func entryOf(e google.Entry) *meetingsv1.Entry {
	out := &meetingsv1.Entry{Idx: int32(e.Idx), Text: cleanText(e.Text), StartMs: e.StartMS, EndMs: e.EndMS}
	if e.Speaker != "" {
		out.Speaker = proto.String(cleanText(e.Speaker))
	}
	return out
}

func cleanText(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "\uFFFD")
}

// entryJSON is a transcript entry as InsertTranscriptEntries reads it; a nil pointer is
// a JSON null and becomes a SQL NULL.
type entryJSON struct {
	Idx     int32   `json:"idx"`
	Speaker *string `json:"speaker"`
	StartMS *int64  `json:"start_ms"`
	EndMS   *int64  `json:"end_ms"`
	Text    string  `json:"text"`
}

func entriesJSON(entries []*meetingsv1.Entry) (json.RawMessage, error) {
	out := make([]entryJSON, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryJSON{Idx: e.GetIdx(), Speaker: e.Speaker, StartMS: e.StartMs, EndMS: e.EndMs, Text: e.GetText()})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("meetings: encode transcript: %w", err)
	}
	return raw, nil
}

// meetingDate is when the meeting took place: its end, else its start, else the
// moment it was made.
func meetingDate(row store.MeetingsMeeting) time.Time {
	switch {
	case row.EndedAt.Valid:
		return row.EndedAt.Time.UTC()
	case row.StartedAt.Valid:
		return row.StartedAt.Time.UTC()
	}
	return row.CreatedAt.Time.UTC()
}

// ---- no transcript ----------------------------------------------------------

// prepareCheck is the meeting.transcript_check job, due half an hour after a
// conference ended: a meeting that is still pending has produced no transcript the
// poll could read.
func (p *pipeline) prepareCheck(ctx context.Context, m bus.Message) (outbox.Apply, error) {
	var ev eventsv1.MeetingTranscriptCheck
	if err := decodeEvent(m, &ev); err != nil {
		return nil, err
	}
	row, ok, err := p.waiting(ctx, ev.GetWorkspaceId(), ev.GetMeetingId(), statusPending)
	if err != nil || !ok {
		return nil, err
	}
	return p.noTranscript(row, statusPending), nil
}

// noTranscript returns the write that ends a meeting that was in status from without a
// transcript: it tells people to upload a recording instead and announces the status.
func (p *pipeline) noTranscript(row store.MeetingsMeeting, from string) outbox.Apply {
	return func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.LockMeeting(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("meetings: lock meeting: %w", err)
		}
		if cur.Status != from {
			return nil
		}
		done, err := q.MarkMeetingNoTranscript(ctx, cur.ID)
		if err != nil {
			return fmt.Errorf("meetings: mark meeting without transcript: %w", err)
		}
		if _, err := p.chat.PostMessageTx(ctx, tx, systemActor(done), agentPost(done, "meeting-nudge-"+done.ID.String(),
			"Meet produced no transcript for "+done.Title+". Upload a recording of the meeting and I will summarize it.")); err != nil {
			return fmt.Errorf("meetings: post recording nudge: %w", err)
		}
		return enqueueStatus(ctx, tx, done)
	}
}
