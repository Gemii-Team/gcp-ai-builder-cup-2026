package meetings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
	"kiwi/internal/chat"
	"kiwi/internal/meetings/internal/store"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/blob"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/google"
	"kiwi/internal/platform/llm"
	"kiwi/internal/platform/outbox"
	"kiwi/internal/platform/refs"
)

// Values of meetings.status.
const (
	statusPending      = "pending"
	statusIngesting    = "ingesting"
	statusReady        = "ready"
	statusFailed       = "failed"
	statusNoTranscript = "no_transcript"
)

// Failure reasons a meeting ends with.
const (
	// reasonMeetAccess: the owner's Google grant for Meet is gone.
	reasonMeetAccess = "meet_access"
	// reasonMeetUnavailable: Meet kept failing until the last attempt.
	reasonMeetUnavailable = "meet_unavailable"
	// reasonNoSpeech: the transcript holds no entries.
	reasonNoSpeech = "no_speech"
	// reasonModelUnavailable: the summarizer kept failing until the last attempt.
	reasonModelUnavailable = "model_unavailable"
	// reasonInternal: a write of the pipeline kept failing until the last delivery.
	reasonInternal = "internal"
	// reasonTranscriptTooLong: the transcript is longer than one call to the summarizer takes.
	reasonTranscriptTooLong = "transcript_too_long"
	// reasonUnsupportedAudio: the recording is no longer a PCM 16-bit WAV file with audio
	// in it, or is over the size an upload may have.
	reasonUnsupportedAudio = "unsupported_audio"
	// reasonUnreadable: the recording's object is gone from the store.
	reasonUnreadable = "unreadable"
)

// bangkok is the zone in which people read the dates of a meeting. Thailand has had no
// daylight saving time since 1920, so a fixed offset is the same as Asia/Bangkok and
// the image needs no zone database.
var bangkok = time.FixedZone("Asia/Bangkok", 7*60*60)

const (
	// slipAuthor is the agent whose name the meeting messages are posted under.
	slipAuthor = "assistant"
	// maxExcerptRadius caps how many entries each side of the focus an excerpt returns.
	maxExcerptRadius = 50
	// maxTitleRunes is the longest title CreateFromRecording accepts.
	maxTitleRunes = 200
)

// protoJSON is how events are carried on the outbox: field names as in the .proto,
// which is what outbox.Enqueue reads workspace_id by.
var protoJSON = protojson.MarshalOptions{UseProtoNames: true}

// Deps are what New and Jobs build the meetings domain from.
type Deps struct {
	DB *db.DB
	// Authz decides every channel-scoped action.
	Authz *authz.Authz
	// Google talks to Meet as the meeting's owner.
	Google google.Client
	// Chat posts the meeting's messages, in the transaction that records them.
	Chat chat.Service
	// Summarizer turns a transcript into a summary.
	Summarizer Summarizer
	// Blob keeps uploaded recordings.
	Blob blob.Store
	// LLM transcribes recordings.
	LLM llm.Client
	// FileChecker verifies the file a recording meeting is made from; CreateFromRecording
	// refuses a recording without it.
	FileChecker FileChecker
	// Clock tells the time; nil means time.Now.
	Clock func() time.Time
}

type service struct {
	db    *db.DB
	authz *authz.Authz
	g     google.Client
	chat  chat.Service
	files FileChecker
	clock func() time.Time
}

// New returns the meetings domain.
func New(d Deps) Service {
	return &service{db: d.DB, authz: d.Authz, g: d.Google, chat: d.Chat, files: d.FileChecker, clock: clockOf(d)}
}

var _ Service = (*service)(nil)

func clockOf(d Deps) func() time.Time {
	if d.Clock == nil {
		return time.Now
	}
	return d.Clock
}

// can asks authz whether a may perform act on the channel.
func (s *service) can(ctx context.Context, a authz.Actor, act authz.Action, channelID uuid.UUID) error {
	return s.authz.Can(ctx, a, act, authz.Resource{ChannelID: channelID})
}

// requireActor refuses a principal that has no workspace to read a meeting in.
func requireActor(a authz.Actor) error {
	switch a.Kind {
	case authz.ActorUser, authz.ActorAgent, authz.ActorSystem:
	default:
		return fmt.Errorf("meetings: unknown actor kind %q: %w", a.Kind, errs.ErrForbidden)
	}
	if a.WorkspaceID == uuid.Nil || a.EffectiveUserID() == uuid.Nil {
		return fmt.Errorf("meetings: actor has no workspace or user: %w", errs.ErrForbidden)
	}
	return nil
}

func parseID(raw, what string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("meetings: %s %q is not a UUID: %w", what, raw, errs.ErrInvalid)
	}
	return id, nil
}

// load reads a meeting; a meeting of another workspace is as good as missing.
func load(ctx context.Context, d *db.DB, ws, id uuid.UUID) (row store.MeetingsMeeting, err error) {
	err = d.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) (err error) {
		row, err = store.New(tx).GetMeeting(ctx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.MeetingsMeeting{}, fmt.Errorf("meetings: meeting %s: %w", id, errs.ErrNotFound)
	}
	if err != nil {
		return store.MeetingsMeeting{}, fmt.Errorf("meetings: read meeting: %w", err)
	}
	return row, nil
}

// authorize reads meeting id and checks that a may perform act on its channel, which
// never changes, so the check holds for the work that follows.
func (s *service) authorize(ctx context.Context, a authz.Actor, id uuid.UUID, act authz.Action) (store.MeetingsMeeting, error) {
	if err := requireActor(a); err != nil {
		return store.MeetingsMeeting{}, err
	}
	row, err := load(ctx, s.db, a.WorkspaceID, id)
	if err != nil {
		return store.MeetingsMeeting{}, err
	}
	if err := s.can(ctx, a, act, row.ChannelID); err != nil {
		return store.MeetingsMeeting{}, err
	}
	return row, nil
}

// systemActor is the job that acts for the user who owns the meeting.
func systemActor(row store.MeetingsMeeting) authz.Actor {
	return authz.Actor{Kind: authz.ActorSystem, WorkspaceID: row.WorkspaceID, UserID: row.OwnerUserID}
}

// ---- start and recording ----------------------------------------------------

// StartMeeting implements Service. The Meet space is created first, with the
// owner's own Google grant; the meeting and the message that carries its link are
// then committed together. There is nothing to subscribe to: the poll finds the
// meeting's transcript by asking Meet about its space.
func (s *service) StartMeeting(ctx context.Context, a authz.Actor, channelID uuid.UUID) (*meetingsv1.Meeting, error) {
	if err := requireActor(a); err != nil {
		return nil, err
	}
	if err := s.can(ctx, a, authz.ActMeetingStart, channelID); err != nil {
		return nil, err
	}
	space, err := s.g.CreateMeetSpace(ctx, a.EffectiveUserID(), true)
	if err != nil {
		// A *google.ConsentRequiredError reaches the caller as it is.
		return nil, err
	}

	var row store.MeetingsMeeting
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		ref, err := refs.Next(ctx, tx, "MTG")
		if err != nil {
			return fmt.Errorf("meetings: %w", err)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("meetings: new meeting id: %w", err)
		}
		row, err = store.New(tx).InsertMeeting(ctx, store.InsertMeetingParams{
			ID: id, WorkspaceID: a.WorkspaceID, ChannelID: channelID, OwnerUserID: a.EffectiveUserID(),
			Ref: &ref, Title: "Meeting " + s.clock().In(bangkok).Format(time.DateOnly),
			MeetSpace: &space.Name, MeetUri: &space.MeetingURI,
		})
		if err != nil {
			return fmt.Errorf("meetings: insert meeting: %w", err)
		}
		_, err = s.chat.PostMessageTx(ctx, tx, systemActor(row), agentPost(row, "meeting-link-"+row.ID.String(),
			"Meeting started: "+space.MeetingURI))
		if err != nil {
			return fmt.Errorf("meetings: post meeting link: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return meetingProto(row, nil), nil
}

// CreateFromRecording implements Service. The file must be an uploaded recording of this
// workspace and channel that the actor may use (see FileChecker). The meeting has no Meet
// space: the meeting.transcribe event it announces is what turns the uploaded recording
// into its transcript. A recording that already has a meeting that has not failed gets
// that meeting back, unchanged and announced no more than once.
func (s *service) CreateFromRecording(ctx context.Context, a authz.Actor, channelID, fileID uuid.UUID, title string) (*meetingsv1.Meeting, error) {
	if err := requireActor(a); err != nil {
		return nil, err
	}
	if err := s.can(ctx, a, authz.ActMeetingStart, channelID); err != nil {
		return nil, err
	}
	title = strings.TrimSpace(title)
	if title == "" || utf8.RuneCountInString(title) > maxTitleRunes || !utf8.ValidString(title) || strings.ContainsRune(title, 0) {
		return nil, fmt.Errorf("meetings: a meeting title must be 1-%d characters: %w", maxTitleRunes, errs.ErrInvalid)
	}
	if fileID == uuid.Nil {
		return nil, fmt.Errorf("meetings: a recording meeting needs its file: %w", errs.ErrInvalid)
	}
	if err := s.checkRecordingFile(ctx, a, channelID, fileID); err != nil {
		return nil, err
	}

	var row store.MeetingsMeeting
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		// A double click, or a retry after a timeout, names the same recording again: it
		// gets the meeting the first request made, not a second REF, a second pass of the
		// transcription model and a second slip. A meeting that failed is not that
		// meeting: it has Retry, and its recording can be made into a new one.
		if err := q.LockRecording(ctx, "recording/"+fileID.String()); err != nil {
			return fmt.Errorf("meetings: lock recording: %w", err)
		}
		existing, err := q.GetMeetingByRecording(ctx, store.GetMeetingByRecordingParams{ChannelID: channelID, RecordingFileID: fileID})
		if err == nil {
			row = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("meetings: look up the recording's meeting: %w", err)
		}
		ref, err := refs.Next(ctx, tx, "MTG")
		if err != nil {
			return fmt.Errorf("meetings: %w", err)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("meetings: new meeting id: %w", err)
		}
		row, err = q.InsertMeeting(ctx, store.InsertMeetingParams{
			ID: id, WorkspaceID: a.WorkspaceID, ChannelID: channelID, OwnerUserID: a.EffectiveUserID(),
			Ref: &ref, Title: title, RecordingFileID: &fileID,
		})
		if err != nil {
			return fmt.Errorf("meetings: insert meeting: %w", err)
		}
		return enqueueTranscribe(ctx, tx, row)
	})
	if err != nil {
		return nil, err
	}
	return meetingProto(row, nil), nil
}

// ---- read -------------------------------------------------------------------

// GetMeeting implements Service.
func (s *service) GetMeeting(ctx context.Context, a authz.Actor, id uuid.UUID) (*meetingsv1.Meeting, error) {
	row, err := s.authorize(ctx, a, id, authz.ActMeetingRead)
	if err != nil {
		return nil, err
	}
	var sum *store.MeetingsSummary
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		got, err := store.New(tx).GetSummary(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		sum = &got
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("meetings: read summary: %w", err)
	}
	return meetingProto(row, sum), nil
}

// GetTranscriptExcerpt implements Service. radius is capped at maxExcerptRadius; an
// entry that does not exist, such as any entry of a meeting that is not ready, is
// errs.ErrNotFound.
func (s *service) GetTranscriptExcerpt(ctx context.Context, a authz.Actor, meetingID uuid.UUID, entryIdx, radius int32) (*meetingsv1.Excerpt, error) {
	if entryIdx < 0 || radius < 0 {
		return nil, fmt.Errorf("meetings: entry index and radius must not be negative: %w", errs.ErrInvalid)
	}
	row, err := s.authorize(ctx, a, meetingID, authz.ActMeetingRead)
	if err != nil {
		return nil, err
	}
	radius = min(radius, maxExcerptRadius)
	lo := entryIdx - radius
	hi := int32(min(int64(entryIdx)+int64(radius), math.MaxInt32))

	var window []store.MeetingsTranscriptEntry
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		window, err = store.New(tx).ListTranscriptWindow(ctx, store.ListTranscriptWindowParams{
			MeetingID: meetingID, Lo: max(lo, 0), Hi: hi,
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("meetings: read transcript window: %w", err)
	}
	hasFocus := false
	for _, e := range window {
		hasFocus = hasFocus || e.Idx == entryIdx
	}
	if !hasFocus {
		return nil, fmt.Errorf("meetings: meeting %s has no transcript entry %d: %w", meetingID, entryIdx, errs.ErrNotFound)
	}
	return &meetingsv1.Excerpt{
		MeetingId: meetingID.String(), Title: row.Title, FocusIdx: entryIdx, Entries: entryProtos(window),
	}, nil
}

// TranscriptEntries implements Service. The indexer is a system actor, which is
// authorized by what it does (authz.ActKnowledgeIndex); a person or an agent reads
// the transcript as a member of the channel.
func (s *service) TranscriptEntries(ctx context.Context, a authz.Actor, meetingID uuid.UUID) ([]*meetingsv1.Entry, error) {
	act := authz.ActMeetingRead
	if a.Kind == authz.ActorSystem {
		act = authz.ActKnowledgeIndex
	}
	if _, err := s.authorize(ctx, a, meetingID, act); err != nil {
		return nil, err
	}
	var rows []store.MeetingsTranscriptEntry
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		rows, err = store.New(tx).ListTranscriptEntries(ctx, meetingID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("meetings: read transcript: %w", err)
	}
	return entryProtos(rows), nil
}

// RetrySummary implements Service. Only a failed meeting can be retried. A recording
// meeting goes back to pending and its transcription is announced again. A Meet
// meeting that has seen a conference goes back to ingesting with an ingest event that
// finds the transcript in Meet itself, because the failure may have come before any
// transcript was seen. One that never saw a conference (it failed while the poll was
// still waiting for people to join) has nothing to ingest: it goes back to pending and
// the poll keeps looking, as it would have had the failure not happened.
func (s *service) RetrySummary(ctx context.Context, a authz.Actor, meetingID uuid.UUID) error {
	row, err := s.authorize(ctx, a, meetingID, authz.ActMeetingWrite)
	if err != nil {
		return err
	}
	return s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		locked, err := q.LockMeeting(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("meetings: lock meeting: %w", err)
		}
		next := statusIngesting
		if locked.RecordingFileID != nil || locked.ConferenceRecord == nil {
			next = statusPending
		}
		restarted, err := q.RestartMeeting(ctx, store.RestartMeetingParams{ID: row.ID, Status: next})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("meetings: meeting %s has not failed: %w", meetingID, errs.ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("meetings: restart meeting: %w", err)
		}
		switch {
		case restarted.RecordingFileID != nil:
			if err := enqueueTranscribe(ctx, tx, restarted); err != nil {
				return err
			}
		case next == statusIngesting:
			if err := enqueue(ctx, tx, events.TopicMeetingIngest, restarted, time.Time{}, &eventsv1.MeetingIngest{
				WorkspaceId: restarted.WorkspaceID.String(), MeetingId: restarted.ID.String(), Attempt: 1,
			}); err != nil {
				return err
			}
		}
		return enqueueStatus(ctx, tx, restarted)
	})
}

// ---- writes shared by the service, the poll and the jobs ---------------------

// failMeeting moves a meeting to failed with reason and announces it. A meeting that
// already left pending and ingesting is left as it is, and nothing is announced.
func failMeeting(ctx context.Context, tx pgx.Tx, id uuid.UUID, reason string) error {
	q := store.New(tx)
	if _, err := q.LockMeeting(ctx, id); err != nil {
		return fmt.Errorf("meetings: lock meeting: %w", err)
	}
	failed, err := q.MarkMeetingFailed(ctx, store.MarkMeetingFailedParams{ID: id, Reason: &reason})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("meetings: fail meeting: %w", err)
	}
	return enqueueStatus(ctx, tx, failed)
}

// agentPost is a message the assistant posts into the meeting's channel. The
// client_msg_id is global (an agent author is the same id in every workspace), so it
// is derived from the meeting.
func agentPost(row store.MeetingsMeeting, clientMsgID, body string) *chatv1.SendMessageRequest {
	return &chatv1.SendMessageRequest{
		ChannelId:   row.ChannelID.String(),
		ClientMsgId: clientMsgID,
		Body:        body,
		AuthorKind:  chatv1.AuthorKind_AUTHOR_KIND_AGENT.Enum(),
		AuthorId:    proto.String(authz.AgentID(slipAuthor).String()),
	}
}

// ---- events and protobuf ----------------------------------------------------

// enqueue writes ev to the outbox in tx, keyed by the meeting it concerns; a zero
// notBefore means now.
func enqueue(ctx context.Context, tx pgx.Tx, topic string, row store.MeetingsMeeting, notBefore time.Time, ev proto.Message) error {
	payload, err := protoJSON.Marshal(ev)
	if err != nil {
		return fmt.Errorf("meetings: encode %s event: %w", topic, err)
	}
	if err := outbox.Enqueue(ctx, tx, outbox.Event{Topic: topic, Key: row.ID.String(), Payload: payload, NotBefore: notBefore}); err != nil {
		return fmt.Errorf("meetings: %w", err)
	}
	return nil
}

// enqueueTranscribe announces a recording that waits to be transcribed.
func enqueueTranscribe(ctx context.Context, tx pgx.Tx, row store.MeetingsMeeting) error {
	return enqueue(ctx, tx, events.TopicMeetingTranscribe, row, time.Time{}, &eventsv1.MeetingTranscribe{
		WorkspaceId: row.WorkspaceID.String(), MeetingId: row.ID.String(), FileId: row.RecordingFileID.String(),
	})
}

// enqueueStatus announces the status row has just moved to.
func enqueueStatus(ctx context.Context, tx pgx.Tx, row store.MeetingsMeeting) error {
	return enqueue(ctx, tx, events.TopicMeetingSummaryReady, row, time.Time{}, &eventsv1.MeetingSummaryReady{
		WorkspaceId: row.WorkspaceID.String(), MeetingId: row.ID.String(), ChannelId: row.ChannelID.String(),
		Status: statusEnum(row.Status), FailureReason: deref(row.FailureReason),
	})
}

func statusEnum(status string) meetingsv1.MeetingStatus {
	switch status {
	case statusPending:
		return meetingsv1.MeetingStatus_MEETING_STATUS_PENDING
	case statusIngesting:
		return meetingsv1.MeetingStatus_MEETING_STATUS_INGESTING
	case statusReady:
		return meetingsv1.MeetingStatus_MEETING_STATUS_READY
	case statusFailed:
		return meetingsv1.MeetingStatus_MEETING_STATUS_FAILED
	case statusNoTranscript:
		return meetingsv1.MeetingStatus_MEETING_STATUS_NO_TRANSCRIPT
	}
	return meetingsv1.MeetingStatus_MEETING_STATUS_UNSPECIFIED
}

// meetingProto maps a row, and its summary once it has one.
func meetingProto(r store.MeetingsMeeting, sum *store.MeetingsSummary) *meetingsv1.Meeting {
	m := &meetingsv1.Meeting{
		Id: r.ID.String(), ChannelId: r.ChannelID.String(), Title: r.Title, Ref: deref(r.Ref),
		Status: statusEnum(r.Status), FailureReason: deref(r.FailureReason), MeetUri: deref(r.MeetUri),
		StartedAt: timestampProto(r.StartedAt), EndedAt: timestampProto(r.EndedAt),
	}
	if sum != nil {
		m.Summary = sum.Summary
		m.Decisions = itemProtos(sum.Decisions)
		m.OpenQuestions = itemProtos(sum.OpenQuestions)
	}
	return m
}

// itemJSON is a decision or an open question as summaries stores it.
type itemJSON struct {
	Text      string `json:"text"`
	EntryRefs []int  `json:"entry_refs"`
}

// itemsJSON encodes items for a jsonb column; no items is an empty array, never null.
func itemsJSON(items []Item) (json.RawMessage, error) {
	out := make([]itemJSON, 0, len(items))
	for _, it := range items {
		rs := it.EntryRefs
		if rs == nil {
			rs = []int{}
		}
		out = append(out, itemJSON{Text: it.Text, EntryRefs: rs})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("meetings: encode summary items: %w", err)
	}
	return raw, nil
}

func itemProtos(raw json.RawMessage) []*meetingsv1.SummaryItem {
	var items []itemJSON
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]*meetingsv1.SummaryItem, 0, len(items))
	for _, it := range items {
		rs := make([]int32, 0, len(it.EntryRefs))
		for _, r := range it.EntryRefs {
			rs = append(rs, int32(r))
		}
		out = append(out, &meetingsv1.SummaryItem{Text: it.Text, EntryRefs: rs})
	}
	return out
}

func entryProtos(rows []store.MeetingsTranscriptEntry) []*meetingsv1.Entry {
	out := make([]*meetingsv1.Entry, 0, len(rows))
	for _, r := range rows {
		out = append(out, &meetingsv1.Entry{Idx: r.Idx, Speaker: r.Speaker, StartMs: r.StartMs, EndMs: r.EndMs, Text: r.Text})
	}
	return out
}

func timestamp(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func timestampProto(t pgtype.Timestamptz) *timestamppb.Timestamp {
	if !t.Valid {
		return nil
	}
	return timestamppb.New(t.Time)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
