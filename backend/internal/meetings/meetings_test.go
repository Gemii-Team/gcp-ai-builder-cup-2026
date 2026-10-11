package meetings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
	"kiwi/gen/kiwi/meetings/v1/meetingsv1connect"
	"kiwi/internal/chat"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/google"
	"kiwi/internal/platform/outbox"
	"kiwi/internal/testkit/dbtest"
	"kiwi/internal/testkit/fakegoogle"
)

// ---- fixture ----------------------------------------------------------------

const (
	meetURI = "https://meet.google.com/abc-defg-hij"
	space   = "spaces/abc123"
)

// sqlMembership answers channel membership straight from chat.channel_members over
// db.InWorkspace: the meetings tests need no Valkey, and the table is the real one
// behind chat's own lookup.
type sqlMembership struct{ db *db.DB }

func (m sqlMembership) IsMember(ctx context.Context, channelID, userID uuid.UUID) (ok bool, err error) {
	err = m.db.InWorkspace(ctx, authz.WorkspaceFrom(ctx), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM chat.channel_members WHERE channel_id = $1 AND user_id = $2)`,
			channelID, userID).Scan(&ok)
	})
	return ok, err
}

// fakeSummarizer is a scripted Summarizer that records what it was asked.
type fakeSummarizer struct {
	mu    sync.Mutex
	reply Summary
	err   error
	calls []SummarizeInput
}

func (s *fakeSummarizer) Summarize(_ context.Context, in SummarizeInput) (Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, in)
	return s.reply, s.err
}

func (s *fakeSummarizer) asked() []SummarizeInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// hookedChat is the real chat service whose transactional post can be made to fail,
// which is how the tests break the write a job makes in its one transaction.
type hookedChat struct {
	chat.Service
	hook *func(ctx context.Context) error
}

func (c hookedChat) PostMessageTx(ctx context.Context, tx pgx.Tx, a authz.Actor, req *chatv1.SendMessageRequest) (*chatv1.Message, error) {
	if h := *c.hook; h != nil {
		if err := h(ctx); err != nil {
			return nil, err
		}
	}
	return c.Service.PostMessageTx(ctx, tx, a, req)
}

// fixture is one migrated database, the real chat service, a scripted Meet and a
// scripted summarizer, with a workspace holding Alice, Bob and Carol.
type fixture struct {
	t    *testing.T
	env  *dbtest.Env
	db   *db.DB
	chat chat.Service
	g    *fakegoogle.Client
	summ *fakeSummarizer
	deps Deps
	svc  Service
	// ingest and check are the meeting.ingest and meeting.transcript_check handlers.
	ingest, check bus.Handler
	// logs holds what the meetings code logged during the test.
	logs *bytes.Buffer
	// chatHook, when set, runs before every post the meetings code makes inside a
	// transaction; an error from it fails the post.
	chatHook func(ctx context.Context) error
	// now is the clock the code under test reads.
	now time.Time

	ws                uuid.UUID
	alice, bob, carol uuid.UUID
	seq               int
}

var base = time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := dbtest.Start(t)
	// The code under test logs the failures it records; keep them out of the test output.
	logs := new(bytes.Buffer)
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	d, err := db.Open(t.Context(), config.Config{DatabaseURL: env.AppURL, DatabaseSystemURL: env.SystemURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(d.Close)

	az := authz.New(sqlMembership{db: d})
	f := &fixture{t: t, env: env, db: d, g: fakegoogle.New(), summ: &fakeSummarizer{reply: defaultSummary()}, logs: logs, now: base}
	f.chat = chat.New(chat.Deps{DB: d, Authz: az})
	f.deps = Deps{
		DB: d, Authz: az, Google: f.g, Chat: hookedChat{Service: f.chat, hook: &f.chatHook},
		Summarizer: f.summ, Clock: func() time.Time { return f.now },
	}
	f.svc = New(f.deps)
	jobs := Jobs(f.deps)
	if len(jobs) != 2 || jobs[0].Consumer != "meeting.ingest" || jobs[1].Consumer != "meeting.transcript_check" {
		t.Fatalf("Jobs = %+v, want meeting.ingest then meeting.transcript_check", jobs)
	}
	f.ingest, f.check = jobs[0].Handler, jobs[1].Handler
	f.ws = f.seedWorkspace("acme.example")
	f.alice = f.seedUser("Alice")
	f.bob = f.seedUser("Bob")
	f.carol = f.seedUser("Carol")
	return f
}

func defaultSummary() Summary {
	return Summary{
		Summary:       "We agreed to ship on Friday.",
		Decisions:     []Item{{Text: "Ship on Friday", EntryRefs: []int{1}}},
		OpenQuestions: []Item{{Text: "Who owns QA?", EntryRefs: []int{0}}},
		ActionItems:   []ActionItem{{Title: "Write the release notes", OwnerHint: "Alice", DueHint: "+3d", EntryRefs: []int{1}}},
	}
}

// asOwner runs fn as kiwi_owner in a transaction bound to the fixture workspace: the
// tests seed and inspect rows this way and never through kiwi_system.
func (f *fixture) asOwner(fn func(ctx context.Context, tx pgx.Tx)) {
	f.t.Helper()
	f.asOwnerIn(f.ws, fn)
}

func (f *fixture) asOwnerIn(ws uuid.UUID, fn func(ctx context.Context, tx pgx.Tx)) {
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

func (f *fixture) exec(sql string, args ...any) { f.execIn(f.ws, sql, args...) }

func (f *fixture) execIn(ws uuid.UUID, sql string, args ...any) {
	f.t.Helper()
	f.asOwnerIn(ws, func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			f.t.Fatalf("%s: %v", sql, err)
		}
	})
}

func (f *fixture) scalar(dst any, sql string, args ...any) {
	f.t.Helper()
	f.asOwner(func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			f.t.Fatalf("%s: %v", sql, err)
		}
	})
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	f.scalar(&n, sql, args...)
	return n
}

func (f *fixture) seedWorkspace(domain string) uuid.UUID {
	f.t.Helper()
	ws := uuid.New()
	f.execIn(ws, `INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, $2, $2)`, ws, domain)
	return ws
}

func (f *fixture) seedUser(name string) uuid.UUID { return f.seedUserIn(f.ws, name) }

func (f *fixture) seedUserIn(ws uuid.UUID, name string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.execIn(ws, `INSERT INTO identity.users (id, workspace_id, google_sub, email, name) VALUES ($1, $2, $3, $4, $5)`,
		id, ws, "sub-"+id.String(), strings.ToLower(name)+"@example.test", name)
	return id
}

// channel creates a channel of kind in the fixture workspace with the given members.
func (f *fixture) channel(kind string, members ...uuid.UUID) uuid.UUID {
	f.t.Helper()
	return f.channelIn(f.ws, kind, members...)
}

func (f *fixture) channelIn(ws uuid.UUID, kind string, members ...uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.execIn(ws, `INSERT INTO chat.channels (id, workspace_id, name, kind) VALUES ($1, $2, $3, $4)`,
		id, ws, "ch-"+id.String()[:8], kind)
	for _, m := range members {
		f.execIn(ws, `INSERT INTO chat.channel_members (channel_id, user_id, workspace_id, role) VALUES ($1, $2, $3, 'member')`,
			id, m, ws)
	}
	return id
}

func (f *fixture) user(id uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorUser, WorkspaceID: f.ws, UserID: id}
}

func (f *fixture) system(origin uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorSystem, WorkspaceID: f.ws, UserID: origin}
}

// seed describes a meeting row to insert as the owner role; the zero value is a
// pending Meet meeting of Alice's made now.
type seed struct {
	ws        uuid.UUID
	channel   uuid.UUID
	owner     uuid.UUID
	status    string
	space     string
	recording uuid.UUID
	last      *int32
	title     string
	age       time.Duration
}

func (f *fixture) seedMeeting(s seed) uuid.UUID {
	f.t.Helper()
	if s.ws == uuid.Nil {
		s.ws = f.ws
	}
	if s.owner == uuid.Nil {
		s.owner = f.alice
	}
	if s.status == "" {
		s.status = statusPending
	}
	if s.title == "" {
		s.title = "Weekly sync"
	}
	var meetSpace *string
	if s.space != "" {
		meetSpace = &s.space
	}
	var recording *uuid.UUID
	if s.recording != uuid.Nil {
		recording = &s.recording
	}
	f.seq++
	id := uuid.New()
	f.execIn(s.ws, `
		INSERT INTO meetings.meetings
			(id, workspace_id, channel_id, owner_user_id, ref, title, meet_space, recording_file_id, status, last_entry_count, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		id, s.ws, s.channel, s.owner, fmt.Sprintf("MTG-%d", 9000+f.seq), s.title, meetSpace, recording, s.status, s.last, f.now.Add(-s.age))
	return id
}

// seedEntries stores n transcript entries 0..n-1 of a meeting, all timed.
func (f *fixture) seedEntries(id uuid.UUID, n int) {
	f.t.Helper()
	for i := range n {
		var speaker *string
		if i%2 == 0 {
			s := "Alice"
			speaker = &s
		}
		f.exec(`INSERT INTO meetings.transcript_entries (meeting_id, workspace_id, idx, speaker, start_ms, end_ms, text) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			id, f.ws, i, speaker, int64(i)*1000, int64(i)*1000+900, fmt.Sprintf("line %d", i))
	}
}

// meetingRow is what the tests read back of a meeting.
type meetingRow struct {
	Status      string
	Reason      *string
	LastCount   *int32
	Conference  *string
	Started     *time.Time
	Ended       *time.Time
	MeetSpace   *string
	RecordingID *uuid.UUID
	Owner       uuid.UUID
	Ref         *string
}

func (f *fixture) meeting(id uuid.UUID) meetingRow {
	f.t.Helper()
	var r meetingRow
	f.asOwner(func(ctx context.Context, tx pgx.Tx) {
		err := tx.QueryRow(ctx, `
			SELECT status, failure_reason, last_entry_count, conference_record, started_at, ended_at, meet_space, recording_file_id, owner_user_id, ref
			FROM meetings.meetings WHERE id = $1`, id).
			Scan(&r.Status, &r.Reason, &r.LastCount, &r.Conference, &r.Started, &r.Ended, &r.MeetSpace, &r.RecordingID, &r.Owner, &r.Ref)
		if err != nil {
			f.t.Fatalf("read meeting %s: %v", id, err)
		}
	})
	return r
}

func (f *fixture) status(id uuid.UUID) string { return f.meeting(id).Status }

// outboxRow is one row of platform.outbox as the tests see it.
type outboxRow struct {
	ID        int64
	Topic     string
	Payload   string
	NotBefore time.Time
}

// outbox returns the rows of topic, in order.
func (f *fixture) outbox(topic string) []outboxRow {
	f.t.Helper()
	var out []outboxRow
	f.asOwner(func(ctx context.Context, tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT id, topic, payload::text, not_before FROM platform.outbox WHERE topic = $1 ORDER BY id`, topic)
		if err != nil {
			f.t.Fatal(err)
		}
		if out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[outboxRow]); err != nil {
			f.t.Fatal(err)
		}
	})
	return out
}

func (f *fixture) topics() []string {
	f.t.Helper()
	var topics []string
	f.asOwner(func(ctx context.Context, tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT topic FROM platform.outbox ORDER BY id`)
		if err != nil {
			f.t.Fatal(err)
		}
		if topics, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			f.t.Fatal(err)
		}
	})
	return topics
}

func (f *fixture) topicCount(topic string) int { return len(f.outbox(topic)) }

// message is outbox row r as the relay would hand it to a consumer.
func (f *fixture) message(r outboxRow) bus.Message {
	return bus.Message{
		ID: strconv.FormatInt(r.ID, 10), Topic: r.Topic, Data: []byte(r.Payload),
		Attrs: map[string]string{"workspace_id": f.ws.String()}, Attempt: 1,
	}
}

// ingestEvent is a meeting.ingest message that did not come from the outbox.
func (f *fixture) ingestEvent(meeting uuid.UUID, transcript string, attempt int32) bus.Message {
	data, err := protoJSON.Marshal(&eventsv1.MeetingIngest{
		WorkspaceId: f.ws.String(), MeetingId: meeting.String(), TranscriptName: transcript, Attempt: attempt,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return bus.Message{
		ID: uuid.NewString(), Topic: events.TopicMeetingIngest, Data: data,
		Attrs: map[string]string{"workspace_id": f.ws.String()}, Attempt: 1,
	}
}

func (f *fixture) checkEvent(meeting uuid.UUID) bus.Message {
	data, err := protoJSON.Marshal(&eventsv1.MeetingTranscriptCheck{WorkspaceId: f.ws.String(), MeetingId: meeting.String()})
	if err != nil {
		f.t.Fatal(err)
	}
	return bus.Message{
		ID: uuid.NewString(), Topic: events.TopicMeetingTranscriptCheck, Data: data,
		Attrs: map[string]string{"workspace_id": f.ws.String()}, Attempt: 1,
	}
}

func (f *fixture) deliver(h bus.Handler, m bus.Message, attempt int) error {
	m.Attempt = attempt
	return h(f.t.Context(), m)
}

func (f *fixture) poll() error { return PollMeetings(f.t.Context(), f.db, f.g, f.now) }

// messages returns the messages of a channel, oldest first, as Alice sees them.
func (f *fixture) messages(ch uuid.UUID) []*chatv1.Message {
	f.t.Helper()
	msgs, err := f.chat.ListMessages(f.t.Context(), f.user(f.alice), ch, 0, 100)
	if err != nil {
		f.t.Fatalf("ListMessages: %v", err)
	}
	slices.Reverse(msgs)
	return msgs
}

// payload decodes the JSON payload of an outbox row into ev.
func (f *fixture) payload(r outboxRow, ev proto.Message) {
	f.t.Helper()
	if err := (protojson.UnmarshalOptions{}).Unmarshal([]byte(r.Payload), ev); err != nil {
		f.t.Fatalf("decode %s payload %s: %v", r.Topic, r.Payload, err)
	}
}

// ---- Meet fixtures ----------------------------------------------------------

func at(h, m int) time.Time { return time.Date(2026, 10, 11, h, m, 0, 0, time.UTC) }

// conference is a conference record of the space that started at start.
func conference(name string, start time.Time, end *time.Time, transcripts ...google.TranscriptRef) google.Conference {
	return google.Conference{Name: name, StartTime: start, EndTime: end, Transcripts: transcripts}
}

func transcript(name string, state google.TranscriptState, end *time.Time) google.TranscriptRef {
	return google.TranscriptRef{Name: name, State: state, EndTime: end}
}

func conferences(cs ...google.Conference) fakegoogle.ConferencesReply {
	return fakegoogle.ConferencesReply{Conferences: cs}
}

const t1 = "conferenceRecords/c1/transcripts/t1"

// entriesOf is n transcript entries; timed gives them start and end offsets, named
// gives them a speaker.
func entriesOf(n int, timed, named bool) []google.Entry {
	out := make([]google.Entry, n)
	for i := range out {
		out[i] = google.Entry{Idx: i, Text: fmt.Sprintf("line %d", i)}
		if named {
			out[i].Speaker = "Alice"
		}
		if timed {
			start, end := int64(i)*1000, int64(i)*1000+900
			out[i].StartMS, out[i].EndMS = &start, &end
		}
	}
	return out
}

func entriesReply(n int) fakegoogle.EntriesReply {
	return fakegoogle.EntriesReply{Entries: entriesOf(n, true, true)}
}

// within reports whether got is no more than a second from want.
func within(got, want time.Time) bool { return got.Sub(want).Abs() <= time.Second }

// ---- start ------------------------------------------------------------------

func TestStartMeeting_CreatesSpaceAndPostsLink(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	f.g.QueueCreateMeetSpace(fakegoogle.SpaceReply{Space: google.Space{Name: space, MeetingURI: meetURI, Code: "abc-defg-hij"}})

	got, err := f.svc.StartMeeting(t.Context(), f.user(f.alice), ch)
	if err != nil {
		t.Fatalf("StartMeeting: %v", err)
	}

	if got.GetStatus() != meetingsv1.MeetingStatus_MEETING_STATUS_PENDING || got.GetRef() != "MTG-1001" ||
		got.GetMeetUri() != meetURI || got.GetChannelId() != ch.String() || got.GetTitle() != "Meeting 2026-10-11" {
		t.Fatalf("meeting = %v, want a pending MTG-1001 for the channel with the Meet link", got)
	}
	id := uuid.MustParse(got.GetId())
	row := f.meeting(id)
	if row.Status != statusPending || row.MeetSpace == nil || *row.MeetSpace != space || row.Owner != f.alice || row.RecordingID != nil {
		t.Errorf("stored meeting = %+v, want pending, space %q, owned by Alice, no recording", row, space)
	}
	if want := (fakegoogle.Call{Method: "CreateMeetSpace", UserID: f.alice, EnableTranscription: true}); !slices.Equal(f.g.Calls(), []fakegoogle.Call{want}) {
		t.Errorf("Meet calls = %+v, want exactly [%+v]: the space is created as Alice with transcription on, and nothing else is asked", f.g.Calls(), want)
	}

	msgs := f.messages(ch)
	if len(msgs) != 1 {
		t.Fatalf("channel has %d messages, want the one link message", len(msgs))
	}
	link := msgs[0]
	if link.GetAuthorKind() != chatv1.AuthorKind_AUTHOR_KIND_AGENT || link.GetAuthorId() != authz.AgentID(slipAuthor).String() {
		t.Errorf("link author = %v %s, want the assistant agent", link.GetAuthorKind(), link.GetAuthorId())
	}
	if !strings.Contains(link.GetBody(), meetURI) || link.GetClientMsgId() != "meeting-link-"+id.String() {
		t.Errorf("link message = %q (client id %q), want the Meet URI and an id derived from the meeting", link.GetBody(), link.GetClientMsgId())
	}

	// Nothing is subscribed to and nothing is scheduled: the poll finds the meeting by its space.
	if topics := f.topics(); !slices.Equal(topics, []string{events.TopicChatMessageCreated}) {
		t.Errorf("outbox topics = %v, want only the chat message event", topics)
	}
	if n := f.count(`SELECT count(*) FROM meetings.space_subscriptions`); n != 0 {
		t.Errorf("%d space subscriptions, want none", n)
	}
}

func TestStartMeeting_RequiresMembership(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)

	for name, actor := range map[string]authz.Actor{
		"non-member":   f.user(f.carol),
		"system actor": f.system(f.alice),
		"nobody":       {},
	} {
		if _, err := f.svc.StartMeeting(t.Context(), actor, ch); !errors.Is(err, errs.ErrForbidden) {
			t.Errorf("%s: StartMeeting = %v, want ErrForbidden", name, err)
		}
	}
	if calls := f.g.Calls(); len(calls) != 0 {
		t.Errorf("Meet was called %d times for refused requests", len(calls))
	}
	if n := f.count(`SELECT count(*) FROM meetings.meetings`); n != 0 {
		t.Errorf("%d meetings were created for refused requests", n)
	}
}

func TestStartMeeting_ConsentRequiredPropagates(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	want := &google.ConsentRequiredError{URL: "/api/auth/meet-consent"}
	f.g.QueueCreateMeetSpace(fakegoogle.SpaceReply{Err: want})

	_, err := f.svc.StartMeeting(t.Context(), f.user(f.alice), ch)

	var got *google.ConsentRequiredError
	if !errors.As(err, &got) || got != want {
		t.Fatalf("StartMeeting = %v (%T), want the *google.ConsentRequiredError itself", err, err)
	}
	if n := f.count(`SELECT count(*) FROM meetings.meetings`); n != 0 {
		t.Errorf("%d meetings were created without a Meet space", n)
	}
	if msgs := f.messages(ch); len(msgs) != 0 {
		t.Errorf("%d messages were posted without a Meet space", len(msgs))
	}
	if topics := f.topics(); len(topics) != 0 {
		t.Errorf("outbox topics = %v, want none", topics)
	}
}

func TestCreateFromRecording_CreatesPendingMeetingAndAnnouncesTranscription(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	file := uuid.New()

	got, err := f.svc.CreateFromRecording(t.Context(), f.user(f.alice), ch, file, "  Sprint review  ")
	if err != nil {
		t.Fatalf("CreateFromRecording: %v", err)
	}

	if got.GetStatus() != meetingsv1.MeetingStatus_MEETING_STATUS_PENDING || got.GetTitle() != "Sprint review" || got.GetRef() != "MTG-1001" {
		t.Fatalf("meeting = %v, want a pending MTG-1001 titled Sprint review", got)
	}
	row := f.meeting(uuid.MustParse(got.GetId()))
	if row.MeetSpace != nil || row.RecordingID == nil || *row.RecordingID != file || row.Owner != f.alice {
		t.Errorf("stored meeting = %+v, want no space, the recording file and Alice as owner", row)
	}
	rows := f.outbox(events.TopicMeetingTranscribe)
	if len(rows) != 1 {
		t.Fatalf("%d meeting.transcribe events, want 1", len(rows))
	}
	var ev eventsv1.MeetingTranscribe
	f.payload(rows[0], &ev)
	if ev.GetMeetingId() != got.GetId() || ev.GetFileId() != file.String() || ev.GetWorkspaceId() != f.ws.String() {
		t.Errorf("transcribe event = %v, want the meeting and its file", &ev)
	}
	if calls := f.g.Calls(); len(calls) != 0 {
		t.Errorf("Meet was called %d times for a recording", len(calls))
	}

	for name, call := range map[string]func() error{
		"non-member": func() error {
			_, err := f.svc.CreateFromRecording(t.Context(), f.user(f.carol), ch, file, "x")
			return err
		},
		"empty title": func() error {
			_, err := f.svc.CreateFromRecording(t.Context(), f.user(f.alice), ch, file, "   ")
			return err
		},
		"nil file": func() error {
			_, err := f.svc.CreateFromRecording(t.Context(), f.user(f.alice), ch, uuid.Nil, "x")
			return err
		},
	} {
		err := call()
		want := errs.ErrInvalid
		if name == "non-member" {
			want = errs.ErrForbidden
		}
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", name, err, want)
		}
	}
	if n := f.count(`SELECT count(*) FROM meetings.meetings`); n != 1 {
		t.Errorf("%d meetings, want only the accepted one", n)
	}
}

// ---- poll -------------------------------------------------------------------

func TestPoll_FileGenerated_EnqueuesIngestOnceAndSetsIngesting(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, age: time.Hour})
	f.g.HandleListConferences(func(context.Context, uuid.UUID, string) ([]google.Conference, error) {
		return []google.Conference{conference("conferenceRecords/c1", at(9, 0), new(at(9, 40)),
			transcript("conferenceRecords/c1/transcripts/late", google.StateFileGenerated, new(at(9, 50))),
			transcript("conferenceRecords/c1/transcripts/early", google.StateFileGenerated, new(at(9, 41))),
			transcript("conferenceRecords/c1/transcripts/live", google.StateStarted, nil),
		)}, nil
	})

	if err := f.poll(); err != nil {
		t.Fatalf("PollMeetings: %v", err)
	}

	row := f.meeting(id)
	if row.Status != statusIngesting {
		t.Errorf("status = %q, want ingesting", row.Status)
	}
	if row.Conference == nil || *row.Conference != "conferenceRecords/c1" || row.Started == nil || !row.Started.Equal(at(9, 0)) ||
		row.Ended == nil || !row.Ended.Equal(at(9, 40)) {
		t.Errorf("conference = %v started %v ended %v, want c1 from 09:00 to 09:40", row.Conference, row.Started, row.Ended)
	}
	rows := f.outbox(events.TopicMeetingIngest)
	if len(rows) != 1 {
		t.Fatalf("%d meeting.ingest events, want 1", len(rows))
	}
	var ev eventsv1.MeetingIngest
	f.payload(rows[0], &ev)
	if ev.GetMeetingId() != id.String() || ev.GetWorkspaceId() != f.ws.String() || ev.GetAttempt() != 1 ||
		ev.GetTranscriptName() != "conferenceRecords/c1/transcripts/early" {
		t.Errorf("ingest event = %v, want attempt 1 for the earliest generated transcript only", &ev)
	}

	// Everything the first poll found is already recorded: a second poll adds nothing.
	checks := f.topicCount(events.TopicMeetingTranscriptCheck)
	if err := f.poll(); err != nil {
		t.Fatalf("second PollMeetings: %v", err)
	}
	if n := f.topicCount(events.TopicMeetingIngest); n != 1 {
		t.Errorf("%d meeting.ingest events after the second poll, want still 1", n)
	}
	if n := f.topicCount(events.TopicMeetingTranscriptCheck); n != checks {
		t.Errorf("%d transcript checks after the second poll, want still %d", n, checks)
	}
	for _, c := range f.g.Calls() {
		if c.Method != "ListConferences" || c.UserID != f.alice || c.Name != space {
			t.Errorf("Meet call %+v, want ListConferences of the space as the owner", c)
		}
	}
}

func TestPoll_StartedOrEndedTranscriptState_WaitsForFileGenerated(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space})
	const name = "conferenceRecords/c1/transcripts/t1"
	f.g.QueueListConferences(
		conferences(conference("conferenceRecords/c1", at(9, 0), nil, transcript(name, google.StateStarted, nil))),
		conferences(conference("conferenceRecords/c1", at(9, 0), new(at(9, 40)), transcript(name, google.StateEnded, new(at(9, 40))))),
		conferences(conference("conferenceRecords/c1", at(9, 0), new(at(9, 40)), transcript(name, google.StateFileGenerated, new(at(9, 41))))),
	)

	for i, state := range []string{"STARTED", "ENDED"} {
		if err := f.poll(); err != nil {
			t.Fatalf("poll %d (%s): %v", i+1, state, err)
		}
		if got := f.status(id); got != statusPending {
			t.Fatalf("status after a %s transcript = %q, want pending: its entries are not complete until the file is generated", state, got)
		}
		if n := f.topicCount(events.TopicMeetingIngest); n != 0 {
			t.Fatalf("%d meeting.ingest events after a %s transcript, want none", n, state)
		}
	}

	if err := f.poll(); err != nil {
		t.Fatalf("poll 3 (FILE_GENERATED): %v", err)
	}
	if got := f.status(id); got != statusIngesting {
		t.Errorf("status after FILE_GENERATED = %q, want ingesting", got)
	}
	rows := f.outbox(events.TopicMeetingIngest)
	if len(rows) != 1 {
		t.Fatalf("%d meeting.ingest events after FILE_GENERATED, want 1", len(rows))
	}
	var ev eventsv1.MeetingIngest
	f.payload(rows[0], &ev)
	if ev.GetTranscriptName() != name {
		t.Errorf("ingest names %q, want %q", ev.GetTranscriptName(), name)
	}
}

func TestPoll_Ended_SchedulesTranscriptCheckAt30Minutes(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space})
	running := conference("conferenceRecords/c1", at(9, 0), nil)
	f.g.QueueListConferences(
		conferences(running),
		conferences(conference("conferenceRecords/c1", at(9, 0), new(at(9, 40)))),
		conferences(conference("conferenceRecords/c1", at(9, 0), new(at(9, 40)))),
	)

	if err := f.poll(); err != nil {
		t.Fatal(err)
	}
	if row := f.meeting(id); row.Ended != nil || row.Started == nil {
		t.Fatalf("a running conference: ended %v started %v, want only the start recorded", row.Ended, row.Started)
	}
	if n := f.topicCount(events.TopicMeetingTranscriptCheck); n != 0 {
		t.Fatalf("%d transcript checks for a conference that is still running, want none", n)
	}

	for range 2 {
		if err := f.poll(); err != nil {
			t.Fatal(err)
		}
	}
	row := f.meeting(id)
	if row.Ended == nil || !row.Ended.Equal(at(9, 40)) {
		t.Fatalf("ended_at = %v, want 09:40", row.Ended)
	}
	rows := f.outbox(events.TopicMeetingTranscriptCheck)
	if len(rows) != 1 {
		t.Fatalf("%d transcript checks after the conference ended, want 1 (scheduled once)", len(rows))
	}
	if !within(rows[0].NotBefore, row.Ended.Add(30*time.Minute)) {
		t.Errorf("transcript check due %s, want %s (ended_at + 30 minutes)", rows[0].NotBefore, row.Ended.Add(30*time.Minute))
	}
	var ev eventsv1.MeetingTranscriptCheck
	f.payload(rows[0], &ev)
	if ev.GetMeetingId() != id.String() || ev.GetWorkspaceId() != f.ws.String() {
		t.Errorf("check event = %v, want the meeting", &ev)
	}
	if got := f.status(id); got != statusPending {
		t.Errorf("status = %q, want pending: the end of a conference says nothing about the transcript", got)
	}
}

func TestPoll_TracksTheLatestConference(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space})
	f.g.HandleListConferences(func(context.Context, uuid.UUID, string) ([]google.Conference, error) {
		return []google.Conference{
			conference("conferenceRecords/old", at(8, 0), new(at(8, 30)), transcript("conferenceRecords/old/transcripts/t", google.StateFileGenerated, new(at(8, 31)))),
			conference("conferenceRecords/new", at(9, 30), nil),
		}, nil
	})

	if err := f.poll(); err != nil {
		t.Fatal(err)
	}

	row := f.meeting(id)
	if row.Conference == nil || *row.Conference != "conferenceRecords/new" || row.Started == nil || !row.Started.Equal(at(9, 30)) {
		t.Errorf("conference = %v started %v, want the latest conference", row.Conference, row.Started)
	}
	if row.Status != statusPending || f.topicCount(events.TopicMeetingIngest) != 0 {
		t.Errorf("status %q with %d ingest events: only the latest conference's transcripts count", row.Status, f.topicCount(events.TopicMeetingIngest))
	}
}

func TestPoll_NobodyJoinedYet_ChangesNothing(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space})
	f.g.HandleListConferences(func(context.Context, uuid.UUID, string) ([]google.Conference, error) { return nil, nil })

	if err := f.poll(); err != nil {
		t.Fatal(err)
	}

	if row := f.meeting(id); row.Status != statusPending || row.Conference != nil || row.Started != nil {
		t.Errorf("meeting = %+v, want it untouched", row)
	}
	if topics := f.topics(); len(topics) != 0 {
		t.Errorf("outbox topics = %v, want none", topics)
	}
}

func TestPoll_ConsentRevoked_FailsMeetingWithReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"consent required", &google.ConsentRequiredError{URL: "/api/auth/meet-consent"}},
		{"HTTP 403", fmt.Errorf("google: conferenceRecords.list: %w", &googleapi.Error{Code: http.StatusForbidden})},
		{"HTTP 401", fmt.Errorf("google: conferenceRecords.list: %w", &googleapi.Error{Code: http.StatusUnauthorized})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			ch := f.channel("public", f.alice, f.bob)
			revoked := f.seedMeeting(seed{channel: ch, space: space, owner: f.alice})
			healthy := f.seedMeeting(seed{channel: ch, space: "spaces/other", owner: f.bob})
			f.g.HandleListConferences(func(_ context.Context, user uuid.UUID, _ string) ([]google.Conference, error) {
				if user == f.alice {
					return nil, tc.err
				}
				return []google.Conference{conference("conferenceRecords/c2", at(9, 0), nil, transcript("conferenceRecords/c2/transcripts/t", google.StateFileGenerated, nil))}, nil
			})

			if err := f.poll(); err != nil {
				t.Fatalf("PollMeetings = %v, want nil: one meeting's lost access is not the poll's failure", err)
			}

			row := f.meeting(revoked)
			if row.Status != statusFailed || row.Reason == nil || *row.Reason != "meet_access" {
				t.Errorf("revoked meeting = %q (%v), want failed with meet_access", row.Status, row.Reason)
			}
			rows := f.outbox(events.TopicMeetingSummaryReady)
			if len(rows) != 1 {
				t.Fatalf("%d meeting.summary_ready events, want 1", len(rows))
			}
			var ev eventsv1.MeetingSummaryReady
			f.payload(rows[0], &ev)
			if ev.GetMeetingId() != revoked.String() || ev.GetChannelId() != ch.String() ||
				ev.GetStatus() != meetingsv1.MeetingStatus_MEETING_STATUS_FAILED || ev.GetFailureReason() != "meet_access" {
				t.Errorf("summary_ready = %v, want FAILED meet_access for the revoked meeting", &ev)
			}
			if got := f.status(healthy); got != statusIngesting {
				t.Errorf("the other owner's meeting = %q, want ingesting: it must not be held up", got)
			}
			if n := f.topicCount(events.TopicMeetingIngest); n != 1 {
				t.Errorf("%d meeting.ingest events, want only the healthy meeting's", n)
			}
		})
	}
}

func TestPoll_OtherMeetErrors_AreRetriedNextTick(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"HTTP 500", fmt.Errorf("google: %w", &googleapi.Error{Code: http.StatusInternalServerError})},
		{"HTTP 404", fmt.Errorf("google: %w", &googleapi.Error{Code: http.StatusNotFound})},
		{"network", errors.New("connection reset")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			ch := f.channel("public", f.alice)
			id := f.seedMeeting(seed{channel: ch, space: space})
			f.g.QueueListConferences(
				fakegoogle.ConferencesReply{Err: tc.err},
				conferences(conference("conferenceRecords/c1", at(9, 0), nil, transcript(t1, google.StateFileGenerated, nil))),
			)

			if err := f.poll(); err != nil {
				t.Fatalf("PollMeetings = %v, want nil", err)
			}
			if got := f.status(id); got != statusPending {
				t.Fatalf("status after a Meet error = %q, want pending", got)
			}
			if topics := f.topics(); len(topics) != 0 {
				t.Fatalf("outbox topics = %v after a Meet error, want none", topics)
			}
			if !strings.Contains(f.logs.String(), "poll failed") {
				t.Errorf("the failure was not logged: %q", f.logs.String())
			}

			if err := f.poll(); err != nil {
				t.Fatal(err)
			}
			if got := f.status(id); got != statusIngesting {
				t.Errorf("status on the next tick = %q, want ingesting", got)
			}
		})
	}
}

func TestPoll_IgnoresMeetingsOlderThan24h(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	old := f.seedMeeting(seed{channel: ch, space: "spaces/old", age: 25 * time.Hour})
	fresh := f.seedMeeting(seed{channel: ch, space: "spaces/fresh", age: 23 * time.Hour})
	recording := f.seedMeeting(seed{channel: ch, recording: uuid.New()})
	var settled []uuid.UUID
	for _, status := range []string{statusReady, statusFailed, statusNoTranscript} {
		settled = append(settled, f.seedMeeting(seed{channel: ch, space: "spaces/" + status, status: status}))
	}
	f.g.HandleListConferences(func(context.Context, uuid.UUID, string) ([]google.Conference, error) {
		return []google.Conference{conference("conferenceRecords/c1", at(9, 0), nil, transcript(t1, google.StateFileGenerated, nil))}, nil
	})

	if err := f.poll(); err != nil {
		t.Fatal(err)
	}

	if want := []fakegoogle.Call{{Method: "ListConferences", UserID: f.alice, Name: "spaces/fresh"}}; !slices.Equal(f.g.Calls(), want) {
		t.Errorf("Meet calls = %+v, want only the 23-hour-old meeting asked about", f.g.Calls())
	}
	if got := f.status(fresh); got != statusIngesting {
		t.Errorf("23 hours old: status %q, want ingesting", got)
	}
	for _, tc := range []struct {
		name   string
		id     uuid.UUID
		status string
	}{
		{"25 hours old", old, statusPending},
		{"recording", recording, statusPending},
		{"ready", settled[0], statusReady},
		{"failed", settled[1], statusFailed},
		{"no transcript", settled[2], statusNoTranscript},
	} {
		if row := f.meeting(tc.id); row.Conference != nil || row.Status != tc.status {
			t.Errorf("%s: meeting = %+v, want it untouched (%s)", tc.name, row, tc.status)
		}
	}
	if n := f.topicCount(events.TopicMeetingIngest); n != 1 {
		t.Errorf("%d meeting.ingest events, want 1", n)
	}
}

func TestPoll_ServesEveryWorkspace_AndWritesEachInItsOwn(t *testing.T) {
	f := newFixture(t)
	other := f.seedWorkspace("globex.example")
	zed := f.seedUserIn(other, "Zed")
	mine := f.seedMeeting(seed{channel: f.channel("public", f.alice), space: "spaces/mine"})
	theirs := f.seedMeeting(seed{ws: other, channel: f.channelIn(other, "public", zed), owner: zed, space: "spaces/theirs"})
	f.g.HandleListConferences(func(context.Context, uuid.UUID, string) ([]google.Conference, error) {
		return []google.Conference{conference("conferenceRecords/c", at(9, 0), new(at(9, 30)), transcript(t1, google.StateFileGenerated, new(at(9, 31))))}, nil
	})

	if err := f.poll(); err != nil {
		t.Fatal(err)
	}

	if got := f.status(mine); got != statusIngesting {
		t.Errorf("first workspace's meeting = %q, want ingesting", got)
	}
	var theirStatus string
	f.asOwnerIn(other, func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT status FROM meetings.meetings WHERE id = $1`, theirs).Scan(&theirStatus); err != nil {
			t.Fatal(err)
		}
	})
	if theirStatus != statusIngesting {
		t.Errorf("second workspace's meeting = %q, want ingesting", theirStatus)
	}
	workspaces := map[string]int{}
	for _, topic := range []string{events.TopicMeetingIngest, events.TopicMeetingTranscriptCheck} {
		for _, r := range f.outbox(topic) {
			var p struct {
				WorkspaceID string `json:"workspace_id"`
			}
			if err := json.Unmarshal([]byte(r.Payload), &p); err != nil {
				t.Fatal(err)
			}
			workspaces[p.WorkspaceID]++
		}
	}
	if workspaces[f.ws.String()] != 2 || workspaces[other.String()] != 2 {
		t.Errorf("events per workspace = %v, want an ingest and a check in each", workspaces)
	}
}

// ---- ingest -----------------------------------------------------------------

func TestIngest_RereadsUntilEntryCountStable(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting})
	f.g.QueueListTranscriptEntries(entriesReply(3), entriesReply(5), entriesReply(5))

	// Read 1: 3 entries and nothing remembered yet.
	if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 1), 1); err != nil {
		t.Fatalf("read 1: %v", err)
	}
	assertReread(t, f, id, 3, 1, 2)

	// Read 2: 5 entries, which differs from the 3 remembered.
	if err := f.deliver(f.ingest, f.message(f.outbox(events.TopicMeetingIngest)[0]), 1); err != nil {
		t.Fatalf("read 2: %v", err)
	}
	assertReread(t, f, id, 5, 2, 3)

	// Read 3: 5 entries again: the transcript is stable.
	if err := f.deliver(f.ingest, f.message(f.outbox(events.TopicMeetingIngest)[1]), 1); err != nil {
		t.Fatalf("read 3: %v", err)
	}

	if n := f.count(`SELECT count(*) FROM meetings.transcript_entries WHERE meeting_id = $1`, id); n != 5 {
		t.Errorf("%d entries written after read 3, want 5 (once)", n)
	}
	if n := f.topicCount(events.TopicMeetingIngest); n != 2 {
		t.Errorf("%d meeting.ingest events, want the two re-reads and no third", n)
	}
	if asked := f.summ.asked(); len(asked) != 1 || len(asked[0].Entries) != 5 || asked[0].MeetingTitle != "Weekly sync" {
		t.Errorf("summarizer was asked %+v, want one call with the 5 entries", asked)
	}
	if got := f.status(id); got != statusReady {
		t.Errorf("status = %q, want ready", got)
	}
	for i, c := range f.g.Calls() {
		if c.Method != "ListTranscriptEntries" || c.UserID != f.alice || c.Name != t1 {
			t.Errorf("Meet call %d = %+v, want ListTranscriptEntries of %s as the meeting's owner", i, c, t1)
		}
	}
	if n := len(f.g.Calls()); n != 3 {
		t.Errorf("%d Meet calls, want 3 reads", n)
	}
}

// assertReread checks the state after a read that disagreed with the last one: the
// count is remembered, nothing else is written, and the transcript is due to be read
// again two minutes from now (the wantRows-th meeting.ingest event, attempt wantAttempt).
func assertReread(t *testing.T, f *fixture, id uuid.UUID, count int32, wantRows, wantAttempt int) {
	t.Helper()
	row := f.meeting(id)
	if row.Status != statusIngesting || row.LastCount == nil || *row.LastCount != count {
		t.Errorf("after the read of %d: status %q last_entry_count %v, want ingesting and %d", count, row.Status, row.LastCount, count)
	}
	if n := f.count(`SELECT count(*) FROM meetings.transcript_entries WHERE meeting_id = $1`, id); n != 0 {
		t.Errorf("%d entries written after the read of %d, want none until two reads agree", n, count)
	}
	if n := f.count(`SELECT count(*) FROM meetings.summaries WHERE meeting_id = $1`, id); n != 0 {
		t.Errorf("a summary was written after the read of %d", count)
	}
	if asked := f.summ.asked(); len(asked) != 0 {
		t.Errorf("summarizer was asked %d times after the read of %d, want none", len(asked), count)
	}
	rows := f.outbox(events.TopicMeetingIngest)
	if len(rows) != wantRows {
		t.Fatalf("%d meeting.ingest events after the read of %d, want %d", len(rows), count, wantRows)
	}
	last := rows[len(rows)-1]
	if !within(last.NotBefore, f.now.Add(2*time.Minute)) {
		t.Errorf("re-read due %s, want %s (two minutes out)", last.NotBefore, f.now.Add(2*time.Minute))
	}
	var ev eventsv1.MeetingIngest
	f.payload(last, &ev)
	if ev.GetAttempt() != int32(wantAttempt) || ev.GetTranscriptName() != t1 || ev.GetMeetingId() != id.String() {
		t.Errorf("re-read event = %v, want attempt %d of %s", &ev, wantAttempt, t1)
	}
}

func TestIngest_ReadyWritesEverythingAndAnnouncesIt(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(3))})
	f.exec(`UPDATE meetings.meetings SET ended_at = $2 WHERE id = $1`, id, at(9, 40))
	f.g.QueueListTranscriptEntries(entriesReply(3))

	if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 2), 1); err != nil {
		t.Fatal(err)
	}

	if got := f.meeting(id); got.Status != statusReady || got.Reason != nil {
		t.Errorf("meeting = %+v, want ready", got)
	}
	asked := f.summ.asked()
	if len(asked) != 1 || !asked[0].MeetingDate.Equal(at(9, 40)) || asked[0].Entries[2].GetText() != "line 2" || asked[0].Entries[1].GetStartMs() != 1000 {
		t.Fatalf("summarizer input = %+v, want the title, the meeting date (its end) and the entries with timing", asked)
	}

	// The transcript and the summary.
	entries, err := f.svc.TranscriptEntries(t.Context(), f.user(f.alice), id)
	if err != nil || len(entries) != 3 || entries[0].GetSpeaker() != "Alice" || entries[2].GetStartMs() != 2000 || entries[2].GetEndMs() != 2900 {
		t.Fatalf("TranscriptEntries = %v, %v, want the 3 entries as read", entries, err)
	}
	got, err := f.svc.GetMeeting(t.Context(), f.user(f.bob), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetStatus() != meetingsv1.MeetingStatus_MEETING_STATUS_READY || got.GetSummary() != "We agreed to ship on Friday." ||
		len(got.GetDecisions()) != 1 || got.GetDecisions()[0].GetText() != "Ship on Friday" || !slices.Equal(got.GetDecisions()[0].GetEntryRefs(), []int32{1}) ||
		len(got.GetOpenQuestions()) != 1 || got.GetOpenQuestions()[0].GetText() != "Who owns QA?" || !slices.Equal(got.GetOpenQuestions()[0].GetEntryRefs(), []int32{0}) {
		t.Errorf("GetMeeting = %v, want the summary with its decisions and open questions", got)
	}

	// The slip.
	msgs := f.messages(ch)
	if len(msgs) != 1 {
		t.Fatalf("channel has %d messages, want the one summary slip", len(msgs))
	}
	slip := msgs[0]
	ref := *f.meeting(id).Ref
	if slip.GetAuthorKind() != chatv1.AuthorKind_AUTHOR_KIND_AGENT || slip.GetAuthorId() != authz.AgentID(slipAuthor).String() ||
		slip.GetClientMsgId() != "meeting-summary-"+id.String() || slip.GetBody() != ref+" · Weekly sync" {
		t.Errorf("slip message = %v, want an assistant post id'd by the meeting", slip)
	}
	a := slip.GetArtifact()
	if a.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_MEETING_SUMMARY || a.GetRef() != ref || a.GetTitle() != "Weekly sync" ||
		a.GetBodyMarkdown() != "We agreed to ship on Friday." || a.GetMeetingId() != id.String() || a.GetMeta() != "2026-10-11" {
		t.Errorf("artifact = %v, want the MEETING_SUMMARY of %s", a, ref)
	}
	if srcs := a.GetSources(); len(srcs) != 1 || srcs[0].GetKind() != chatv1.SourceKind_SOURCE_KIND_MEETING || srcs[0].GetId() != id.String() ||
		srcs[0].GetLabel() != "Weekly sync" || srcs[0].GetChannelId() != ch.String() {
		t.Errorf("sources = %v, want the meeting as the only source", srcs)
	}
	tracks := a.GetTracks()
	if len(tracks) != 2 {
		t.Fatalf("tracks = %v, want one decision and one open question", tracks)
	}
	if d := tracks[0]; d.GetKind() != chatv1.TrackKind_TRACK_KIND_DECISION || d.GetText() != "Ship on Friday" || d.EntryIdx == nil || d.GetEntryIdx() != 1 || d.OffsetMs == nil || d.GetOffsetMs() != 1000 {
		t.Errorf("decision track = %v, want entry 1 at 1000 ms", d)
	}
	if q := tracks[1]; q.GetKind() != chatv1.TrackKind_TRACK_KIND_OPEN_QUESTION || q.GetText() != "Who owns QA?" || q.EntryIdx == nil || q.GetEntryIdx() != 0 || q.OffsetMs == nil || q.GetOffsetMs() != 0 {
		t.Errorf("open question track = %v, want entry 0 at 0 ms (a timed entry at the start still links)", q)
	}

	// The events: drafts, indexing, and the channel's status.
	drafts := f.outbox(events.TopicTaskDraft)
	if len(drafts) != 1 {
		t.Fatalf("%d task.draft events, want 1", len(drafts))
	}
	var td eventsv1.TaskDraftRequested
	f.payload(drafts[0], &td)
	if td.GetMeetingId() != id.String() || td.GetMeetingTitle() != "Weekly sync" || td.GetChannelId() != ch.String() ||
		td.GetOwnerUserId() != f.alice.String() || td.GetWorkspaceId() != f.ws.String() || len(td.GetItems()) != 1 {
		t.Fatalf("task.draft = %v, want the meeting, its title, channel and owner with one item", &td)
	}
	if it := td.GetItems()[0]; it.GetTitle() != "Write the release notes" || it.GetOwnerHint() != "Alice" || it.GetDueHint() != "+3d" ||
		it.GetEntryIdx() != 1 || it.OffsetMs == nil || it.GetOffsetMs() != 1000 {
		t.Errorf("task.draft item = %v, want the action item at entry 1, 1000 ms", it)
	}
	embeds := f.outbox(events.TopicEmbedIndex)
	if len(embeds) != 1 {
		t.Fatalf("%d embed.index events, want 1", len(embeds))
	}
	var ei eventsv1.EmbedIndex
	f.payload(embeds[0], &ei)
	if ei.GetSourceKind() != "meeting" || ei.GetSourceId() != id.String() || ei.GetChannelId() != ch.String() {
		t.Errorf("embed.index = %v, want the meeting as a source", &ei)
	}
	ready := f.outbox(events.TopicMeetingSummaryReady)
	if len(ready) != 1 {
		t.Fatalf("%d meeting.summary_ready events, want 1", len(ready))
	}
	var sr eventsv1.MeetingSummaryReady
	f.payload(ready[0], &sr)
	if sr.GetStatus() != meetingsv1.MeetingStatus_MEETING_STATUS_READY || sr.GetMeetingId() != id.String() || sr.GetChannelId() != ch.String() || sr.GetFailureReason() != "" {
		t.Errorf("summary_ready = %v, want READY", &sr)
	}
}

func TestIngest_NullSpeakerAndTimingStoredAsNull_NoOffsetOnSlip(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(3))})
	f.g.QueueListTranscriptEntries(fakegoogle.EntriesReply{Entries: entriesOf(3, false, false)})

	if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 2), 1); err != nil {
		t.Fatal(err)
	}

	if got := f.status(id); got != statusReady {
		t.Fatalf("status = %q, want ready", got)
	}
	if n := f.count(`SELECT count(*) FROM meetings.transcript_entries WHERE meeting_id = $1 AND speaker IS NULL AND start_ms IS NULL AND end_ms IS NULL`, id); n != 3 {
		t.Errorf("%d entries with a null speaker, start and end, want all 3", n)
	}
	entries, err := f.svc.TranscriptEntries(t.Context(), f.user(f.alice), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Speaker != nil || e.StartMs != nil || e.EndMs != nil {
			t.Errorf("entry %d = %v, want no speaker and no timing (not an empty string or 0)", e.GetIdx(), e)
		}
	}

	// No offset link may render for a transcript without timing: not 0:00:00, not anything.
	a := f.messages(ch)[0].GetArtifact()
	if len(a.GetTracks()) != 2 {
		t.Fatalf("tracks = %v, want 2", a.GetTracks())
	}
	for _, tr := range a.GetTracks() {
		if tr.EntryIdx != nil || tr.OffsetMs != nil {
			t.Errorf("track %q carries entry_idx %v offset_ms %v, want neither without timing", tr.GetText(), tr.EntryIdx, tr.OffsetMs)
		}
	}
	var td eventsv1.TaskDraftRequested
	f.payload(f.outbox(events.TopicTaskDraft)[0], &td)
	it := td.GetItems()[0]
	if it.OffsetMs != nil || it.GetEntryIdx() != 1 {
		t.Errorf("draft item = %v, want entry 1 and no offset_ms", it)
	}
	if strings.Contains(f.outbox(events.TopicTaskDraft)[0].Payload, "offset_ms") {
		t.Errorf("task.draft payload %s mentions offset_ms, want it absent", f.outbox(events.TopicTaskDraft)[0].Payload)
	}
}

func TestIngest_ReferencesToMissingEntriesAreIgnored(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(3))})
	f.summ.reply = Summary{
		Summary:       "s",
		Decisions:     []Item{{Text: "invented ref", EntryRefs: []int{99, -1}}, {Text: "second ref counts", EntryRefs: []int{99, 2}}, {Text: "no refs"}},
		OpenQuestions: []Item{},
		ActionItems:   []ActionItem{{Title: "nothing cited"}, {Title: "bad ref", EntryRefs: []int{7}}},
	}
	f.g.QueueListTranscriptEntries(entriesReply(3))

	if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 2), 1); err != nil {
		t.Fatal(err)
	}

	tracks := f.messages(ch)[0].GetArtifact().GetTracks()
	if len(tracks) != 3 {
		t.Fatalf("tracks = %v, want the 3 decisions", tracks)
	}
	for i, wantIdx := range []int32{-1, 2, -1} {
		tr := tracks[i]
		switch {
		case wantIdx < 0 && (tr.EntryIdx != nil || tr.OffsetMs != nil):
			t.Errorf("track %q has an anchor, want none for a reference that names no entry", tr.GetText())
		case wantIdx >= 0 && (tr.GetEntryIdx() != wantIdx || tr.GetOffsetMs() != 2000):
			t.Errorf("track %q = %v, want the first reference that exists (entry %d)", tr.GetText(), tr, wantIdx)
		}
	}
	var td eventsv1.TaskDraftRequested
	f.payload(f.outbox(events.TopicTaskDraft)[0], &td)
	for _, it := range td.GetItems() {
		// -1 says "no source": 0 would be indistinguishable from a real entry 0.
		if it.GetEntryIdx() != -1 || it.OffsetMs != nil {
			t.Errorf("draft item %q = %v, want entry_idx -1 and no offset when no cited entry exists", it.GetTitle(), it)
		}
	}
}

func TestIngest_NoActionItems_EnqueuesNoDraftEvent(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(3))})
	f.summ.reply = Summary{Summary: "Nothing to do."}
	f.g.QueueListTranscriptEntries(entriesReply(3))

	if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 2), 1); err != nil {
		t.Fatal(err)
	}

	if got := f.status(id); got != statusReady {
		t.Fatalf("status = %q, want ready", got)
	}
	if n := f.topicCount(events.TopicTaskDraft); n != 0 {
		t.Errorf("%d task.draft events for a meeting with no action items", n)
	}
	got, err := f.svc.GetMeeting(t.Context(), f.user(f.alice), id)
	if err != nil || len(got.GetDecisions()) != 0 || len(got.GetOpenQuestions()) != 0 {
		t.Errorf("GetMeeting = %v, %v, want a summary with empty decision and question lists", got, err)
	}
}

func TestIngest_NULInTranscriptText_IsDropped(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(1))})
	f.g.QueueListTranscriptEntries(fakegoogle.EntriesReply{Entries: []google.Entry{{Idx: 0, Speaker: "Al\x00ice", Text: "he\x00llo \xff there"}}})

	if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 2), 1); err != nil {
		t.Fatal(err)
	}

	if got := f.status(id); got != statusReady {
		t.Fatalf("status = %q, want ready: Postgres cannot store NUL, so it must not fail the meeting", got)
	}
	entries, err := f.svc.TranscriptEntries(t.Context(), f.user(f.alice), id)
	if err != nil || len(entries) != 1 || entries[0].GetText() != "hello \uFFFD there" || entries[0].GetSpeaker() != "Alice" {
		t.Errorf("TranscriptEntries = %v, %v, want the text without the NUL and with the bad byte replaced", entries, err)
	}
}

func TestIngest_EmptyTranscript_FailsWithReason(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting})
	f.g.QueueListTranscriptEntries(entriesReply(0), entriesReply(0))

	// A first empty read may be Meet being slow right after the file is generated: it
	// is remembered like any other count, not judged.
	if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 1), 1); err != nil {
		t.Fatal(err)
	}
	if got := f.status(id); got != statusIngesting {
		t.Fatalf("status after one empty read = %q, want ingesting", got)
	}
	if n := f.topicCount(events.TopicMeetingIngest); n != 1 {
		t.Fatalf("%d meeting.ingest events after one empty read, want the re-read", n)
	}

	if err := f.deliver(f.ingest, f.message(f.outbox(events.TopicMeetingIngest)[0]), 1); err != nil {
		t.Fatal(err)
	}

	row := f.meeting(id)
	if row.Status != statusFailed || row.Reason == nil || *row.Reason != "no_speech" {
		t.Fatalf("meeting = %q (%v), want failed with no_speech after two empty reads", row.Status, row.Reason)
	}
	if msgs := f.messages(ch); len(msgs) != 0 {
		t.Errorf("%d messages posted for an empty transcript, want no slip", len(msgs))
	}
	if n := f.count(`SELECT count(*) FROM meetings.summaries`); n != 0 {
		t.Errorf("%d summaries written for an empty transcript", n)
	}
	if asked := f.summ.asked(); len(asked) != 0 {
		t.Errorf("summarizer was asked %d times about an empty transcript", len(asked))
	}
	for _, topic := range []string{events.TopicTaskDraft, events.TopicEmbedIndex} {
		if n := f.topicCount(topic); n != 0 {
			t.Errorf("%d %s events for an empty transcript, want none", n, topic)
		}
	}
	ready := f.outbox(events.TopicMeetingSummaryReady)
	if len(ready) != 1 {
		t.Fatalf("%d meeting.summary_ready events, want 1", len(ready))
	}
	var sr eventsv1.MeetingSummaryReady
	f.payload(ready[0], &sr)
	if sr.GetStatus() != meetingsv1.MeetingStatus_MEETING_STATUS_FAILED || sr.GetFailureReason() != "no_speech" || sr.GetMeetingId() != id.String() {
		t.Errorf("summary_ready = %v, want FAILED no_speech", &sr)
	}
}

func TestIngest_RedeliveryAppliesOnce(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(3))})
	f.g.HandleListTranscriptEntries(func(context.Context, uuid.UUID, string) ([]google.Entry, error) {
		return entriesOf(3, true, true), nil
	})
	msg := f.ingestEvent(id, t1, 2)

	if err := f.deliver(f.ingest, msg, 1); err != nil {
		t.Fatal(err)
	}
	reads, asked := len(f.g.Calls()), len(f.summ.asked())
	if err := f.deliver(f.ingest, msg, 2); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if got := len(f.g.Calls()); got != reads {
		t.Errorf("a redelivered message read the transcript again (%d calls, was %d)", got, reads)
	}
	if got := len(f.summ.asked()); got != asked {
		t.Errorf("a redelivered message asked the summarizer again (%d calls, was %d)", got, asked)
	}

	// A stale event for the meeting that is already ready (another id: the event a
	// retry leaves behind) is acknowledged and changes nothing.
	if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 2), 1); err != nil {
		t.Fatalf("stale event: %v", err)
	}
	if got := len(f.g.Calls()); got != reads {
		t.Errorf("a stale event read the transcript (%d calls, was %d)", got, reads)
	}

	assertAppliedOnce(t, f, ch, id)
}

func TestIngest_ConcurrentDelivery_AppliesOnce(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(3))})
	f.g.HandleListTranscriptEntries(func(context.Context, uuid.UUID, string) ([]google.Entry, error) {
		return entriesOf(3, true, true), nil
	})
	msg := f.ingestEvent(id, t1, 2)

	var wg sync.WaitGroup
	errc := make(chan error, 4)
	for range 4 {
		wg.Go(func() { errc <- f.ingest(t.Context(), msg) })
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Errorf("concurrent delivery: %v", err)
		}
	}

	assertAppliedOnce(t, f, ch, id)
}

func assertAppliedOnce(t *testing.T, f *fixture, ch, id uuid.UUID) {
	t.Helper()
	if got := f.status(id); got != statusReady {
		t.Errorf("status = %q, want ready", got)
	}
	if n := f.count(`SELECT count(*) FROM meetings.transcript_entries WHERE meeting_id = $1`, id); n != 3 {
		t.Errorf("%d entries, want 3 (written once)", n)
	}
	if n := f.count(`SELECT count(*) FROM meetings.summaries WHERE meeting_id = $1`, id); n != 1 {
		t.Errorf("%d summaries, want 1", n)
	}
	if msgs := f.messages(ch); len(msgs) != 1 {
		t.Errorf("%d messages, want 1 slip", len(msgs))
	}
	for topic, want := range map[string]int{events.TopicTaskDraft: 1, events.TopicEmbedIndex: 1, events.TopicMeetingSummaryReady: 1} {
		if n := f.topicCount(topic); n != want {
			t.Errorf("%d %s events, want %d", n, topic, want)
		}
	}
}

func TestIngest_MeetAccessLost_FailsAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"consent required", &google.ConsentRequiredError{URL: "/api/auth/meet-consent"}},
		{"HTTP 403", fmt.Errorf("google: transcriptEntries.list: %w", &googleapi.Error{Code: http.StatusForbidden})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			ch := f.channel("public", f.alice)
			id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting})
			f.g.QueueListTranscriptEntries(fakegoogle.EntriesReply{Err: tc.err})

			if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 1), 1); err != nil {
				t.Fatalf("ingest = %v, want nil (the failure is recorded, not retried)", err)
			}

			row := f.meeting(id)
			if row.Status != statusFailed || row.Reason == nil || *row.Reason != "meet_access" {
				t.Errorf("meeting = %q (%v), want failed with meet_access on the first attempt", row.Status, row.Reason)
			}
			var sr eventsv1.MeetingSummaryReady
			f.payload(f.outbox(events.TopicMeetingSummaryReady)[0], &sr)
			if sr.GetStatus() != meetingsv1.MeetingStatus_MEETING_STATUS_FAILED || sr.GetFailureReason() != "meet_access" {
				t.Errorf("summary_ready = %v, want FAILED meet_access", &sr)
			}
		})
	}
}

func TestIngest_TransientErrorBeforeFinalAttempt_Nacks(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(3))})
	boom := errors.New("meet is down")
	f.g.HandleListTranscriptEntries(func(context.Context, uuid.UUID, string) ([]google.Entry, error) { return nil, boom })

	for attempt := 1; attempt < finalAttempt; attempt++ {
		if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 2), attempt); !errors.Is(err, boom) {
			t.Fatalf("attempt %d: ingest = %v, want the Meet error so the bus redelivers", attempt, err)
		}
		if got := f.status(id); got != statusIngesting {
			t.Fatalf("attempt %d: status = %q, want ingesting", attempt, got)
		}
	}
	if topics := f.topics(); len(topics) != 0 {
		t.Errorf("outbox topics = %v, want none", topics)
	}

	// The same for the summarizer: an error is retried until the final attempt.
	f.g.HandleListTranscriptEntries(func(context.Context, uuid.UUID, string) ([]google.Entry, error) {
		return entriesOf(3, true, true), nil
	})
	f.summ.err = boom
	for attempt := 1; attempt < finalAttempt; attempt++ {
		if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 2), attempt); !errors.Is(err, boom) {
			t.Fatalf("summarizer attempt %d: ingest = %v, want the error", attempt, err)
		}
	}
	if got := f.status(id); got != statusIngesting {
		t.Errorf("status = %q, want ingesting", got)
	}
}

func TestIngest_FinalAttemptError_MarksFailed(t *testing.T) {
	boom := errors.New("backend is down")
	for _, tc := range []struct {
		name   string
		setup  func(f *fixture)
		reason string
	}{
		{"meet", func(f *fixture) {
			f.g.HandleListTranscriptEntries(func(context.Context, uuid.UUID, string) ([]google.Entry, error) { return nil, boom })
		}, "meet_unavailable"},
		{"summarizer", func(f *fixture) {
			f.g.HandleListTranscriptEntries(func(context.Context, uuid.UUID, string) ([]google.Entry, error) {
				return entriesOf(3, true, true), nil
			})
			f.summ.err = boom
		}, "model_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			ch := f.channel("public", f.alice)
			id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(3))})
			tc.setup(f)

			if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 2), finalAttempt); err != nil {
				t.Fatalf("ingest on the final attempt = %v, want nil: the failure is recorded", err)
			}

			row := f.meeting(id)
			if row.Status != statusFailed || row.Reason == nil || *row.Reason != tc.reason {
				t.Errorf("meeting = %q (%v), want failed with %s", row.Status, row.Reason, tc.reason)
			}
			if msgs := f.messages(ch); len(msgs) != 0 {
				t.Errorf("%d messages posted for a failed meeting, want no slip", len(msgs))
			}
			if n := f.count(`SELECT count(*) FROM meetings.transcript_entries`); n != 0 {
				t.Errorf("%d entries written for a failed meeting", n)
			}
			var sr eventsv1.MeetingSummaryReady
			f.payload(f.outbox(events.TopicMeetingSummaryReady)[0], &sr)
			if sr.GetStatus() != meetingsv1.MeetingStatus_MEETING_STATUS_FAILED || sr.GetFailureReason() != tc.reason {
				t.Errorf("summary_ready = %v, want FAILED %s", &sr, tc.reason)
			}
		})
	}
}

// A process that is shutting down cancels the handler's context while Meet is being
// read: that is no verdict on the meeting, so even the final attempt leaves it
// ingesting for the redelivery to finish.
func TestIngest_ShutdownIsNotTheMeetingsFault(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(3))})
	ctx, cancel := context.WithCancel(t.Context())
	f.g.HandleListTranscriptEntries(func(ctx context.Context, _ uuid.UUID, _ string) ([]google.Entry, error) {
		cancel()
		return nil, ctx.Err()
	})
	msg := f.ingestEvent(id, t1, 2)
	msg.Attempt = finalAttempt

	err := f.ingest(ctx, msg)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ingest = %v, want context.Canceled so the message is redelivered", err)
	}
	if got := f.status(id); got != statusIngesting {
		t.Errorf("status = %q, want ingesting: a process that is stopping must not fail the meeting", got)
	}
	if strings.Contains(f.logs.String(), "ingest failed") {
		t.Errorf("a failure was recorded in the logs: %s", f.logs.String())
	}
}

func TestIngest_SettledOrForeignMeetings_AreAcked(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	cases := map[string]uuid.UUID{
		"gone":      uuid.New(),
		"pending":   f.seedMeeting(seed{channel: ch, space: space}),
		"ready":     f.seedMeeting(seed{channel: ch, space: space, status: statusReady}),
		"failed":    f.seedMeeting(seed{channel: ch, space: space, status: statusFailed}),
		"recording": f.seedMeeting(seed{channel: ch, recording: uuid.New(), status: statusIngesting}),
	}
	for name, id := range cases {
		if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 1), 1); err != nil {
			t.Errorf("%s: ingest = %v, want nil", name, err)
		}
	}
	if calls := f.g.Calls(); len(calls) != 0 {
		t.Errorf("Meet was called %d times for meetings that were not waiting for a read", len(calls))
	}
	if topics := f.topics(); len(topics) != 0 {
		t.Errorf("outbox topics = %v, want none", topics)
	}
}

func TestIngest_WithoutTranscriptName_FindsTheTranscriptInMeet(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting})
	f.g.QueueListConferences(conferences(
		conference("conferenceRecords/c1", at(9, 0), new(at(9, 40)), transcript("conferenceRecords/c1/transcripts/live", google.StateEnded, nil), transcript(t1, google.StateFileGenerated, new(at(9, 41))))))
	f.g.QueueListTranscriptEntries(entriesReply(3))

	if err := f.deliver(f.ingest, f.ingestEvent(id, "", 1), 1); err != nil {
		t.Fatal(err)
	}

	want := []fakegoogle.Call{
		{Method: "ListConferences", UserID: f.alice, Name: space},
		{Method: "ListTranscriptEntries", UserID: f.alice, Name: t1},
	}
	if !slices.Equal(f.g.Calls(), want) {
		t.Errorf("Meet calls = %+v, want the transcript looked up then read", f.g.Calls())
	}
	var ev eventsv1.MeetingIngest
	f.payload(f.outbox(events.TopicMeetingIngest)[0], &ev)
	if ev.GetTranscriptName() != t1 || ev.GetAttempt() != 2 {
		t.Errorf("re-read event = %v, want attempt 2 carrying the transcript that was found, so the next read need not look it up", &ev)
	}
}

func TestIngest_WithoutTranscriptName_NoneInMeet_EndsWithoutTranscript(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting})
	f.g.QueueListConferences(conferences(conference("conferenceRecords/c1", at(9, 0), new(at(9, 40)), transcript(t1, google.StateEnded, nil))))

	if err := f.deliver(f.ingest, f.ingestEvent(id, "", 1), 1); err != nil {
		t.Fatal(err)
	}

	if got := f.status(id); got != statusNoTranscript {
		t.Errorf("status = %q, want no_transcript", got)
	}
	msgs := f.messages(ch)
	if len(msgs) != 1 || !strings.Contains(msgs[0].GetBody(), "recording") {
		t.Errorf("messages = %v, want the upload-a-recording nudge", msgs)
	}
}

// ---- transcript check -------------------------------------------------------

func TestTranscriptCheck_NudgesOnlyWhenNoTranscript(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	none := f.seedMeeting(seed{channel: ch, space: space, title: "Retro"})
	reading := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting})
	done := f.seedMeeting(seed{channel: ch, space: space, status: statusReady})

	for _, id := range []uuid.UUID{reading, done} {
		if err := f.deliver(f.check, f.checkEvent(id), 1); err != nil {
			t.Fatal(err)
		}
		if msgs := f.messages(ch); len(msgs) != 0 {
			t.Fatalf("a meeting that has its transcript was nudged: %v", msgs)
		}
	}
	if got := f.status(reading); got != statusIngesting {
		t.Errorf("ingesting meeting = %q, want it left alone", got)
	}
	if got := f.status(done); got != statusReady {
		t.Errorf("ready meeting = %q, want it left alone", got)
	}
	if topics := f.topics(); len(topics) != 0 {
		t.Errorf("outbox topics = %v, want none for meetings that have a transcript", topics)
	}

	msg := f.checkEvent(none)
	if err := f.deliver(f.check, msg, 1); err != nil {
		t.Fatal(err)
	}
	if got := f.status(none); got != statusNoTranscript {
		t.Fatalf("status = %q, want no_transcript", got)
	}
	msgs := f.messages(ch)
	if len(msgs) != 1 {
		t.Fatalf("%d messages, want the one nudge", len(msgs))
	}
	nudge := msgs[0]
	if nudge.GetAuthorKind() != chatv1.AuthorKind_AUTHOR_KIND_AGENT || nudge.GetAuthorId() != authz.AgentID(slipAuthor).String() ||
		nudge.GetClientMsgId() != "meeting-nudge-"+none.String() || !strings.Contains(nudge.GetBody(), "Retro") ||
		!strings.Contains(nudge.GetBody(), "Upload a recording") || nudge.GetArtifact() != nil {
		t.Errorf("nudge = %v, want a plain assistant message naming the meeting and asking for a recording", nudge)
	}
	ready := f.outbox(events.TopicMeetingSummaryReady)
	if len(ready) != 1 {
		t.Fatalf("%d meeting.summary_ready events, want 1", len(ready))
	}
	var sr eventsv1.MeetingSummaryReady
	f.payload(ready[0], &sr)
	if sr.GetStatus() != meetingsv1.MeetingStatus_MEETING_STATUS_NO_TRANSCRIPT || sr.GetMeetingId() != none.String() || sr.GetChannelId() != ch.String() {
		t.Errorf("summary_ready = %v, want NO_TRANSCRIPT", &sr)
	}

	// Redelivery, and a second check for the same meeting, nudge nobody again.
	if err := f.deliver(f.check, msg, 2); err != nil {
		t.Fatal(err)
	}
	if err := f.deliver(f.check, f.checkEvent(none), 1); err != nil {
		t.Fatal(err)
	}
	if got := len(f.messages(ch)); got != 1 {
		t.Errorf("%d messages after redelivery, want still 1", got)
	}
	if n := f.topicCount(events.TopicMeetingSummaryReady); n != 1 {
		t.Errorf("%d meeting.summary_ready events after redelivery, want still 1", n)
	}
}

// ---- retry ------------------------------------------------------------------

func TestRetrySummary_OnlyFailedMeetings(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	file := uuid.New()
	meet := f.seedMeeting(seed{channel: ch, space: space, status: statusFailed, last: new(int32(4))})
	f.exec(`UPDATE meetings.meetings SET failure_reason = 'model_unavailable', conference_record = 'conferenceRecords/c1' WHERE id = $1`, meet)
	recording := f.seedMeeting(seed{channel: ch, recording: file, status: statusFailed})
	f.exec(`UPDATE meetings.meetings SET failure_reason = 'no_speech' WHERE id = $1`, recording)

	// A Meet meeting is read again, and its transcript looked up because the failure
	// may have come before any was seen.
	if err := f.svc.RetrySummary(t.Context(), f.user(f.bob), meet); err != nil {
		t.Fatalf("RetrySummary of a failed Meet meeting: %v", err)
	}
	row := f.meeting(meet)
	if row.Status != statusIngesting || row.Reason != nil || row.LastCount != nil {
		t.Errorf("Meet meeting = %+v, want ingesting with no reason and its read count forgotten", row)
	}
	ingests := f.outbox(events.TopicMeetingIngest)
	if len(ingests) != 1 {
		t.Fatalf("%d meeting.ingest events, want 1", len(ingests))
	}
	var ing eventsv1.MeetingIngest
	f.payload(ingests[0], &ing)
	if ing.GetMeetingId() != meet.String() || ing.GetTranscriptName() != "" || ing.GetAttempt() != 1 {
		t.Errorf("ingest event = %v, want attempt 1 without a transcript name", &ing)
	}

	// A recording meeting is transcribed again.
	if err := f.svc.RetrySummary(t.Context(), f.user(f.alice), recording); err != nil {
		t.Fatalf("RetrySummary of a failed recording meeting: %v", err)
	}
	if row := f.meeting(recording); row.Status != statusPending || row.Reason != nil {
		t.Errorf("recording meeting = %+v, want pending with no reason", row)
	}
	transcribes := f.outbox(events.TopicMeetingTranscribe)
	if len(transcribes) != 1 {
		t.Fatalf("%d meeting.transcribe events, want 1", len(transcribes))
	}
	var tr eventsv1.MeetingTranscribe
	f.payload(transcribes[0], &tr)
	if tr.GetMeetingId() != recording.String() || tr.GetFileId() != file.String() {
		t.Errorf("transcribe event = %v, want the meeting and its file", &tr)
	}

	// The UI is told that the failure is over.
	var statuses []meetingsv1.MeetingStatus
	for _, r := range f.outbox(events.TopicMeetingSummaryReady) {
		var sr eventsv1.MeetingSummaryReady
		f.payload(r, &sr)
		statuses = append(statuses, sr.GetStatus())
	}
	want := []meetingsv1.MeetingStatus{meetingsv1.MeetingStatus_MEETING_STATUS_INGESTING, meetingsv1.MeetingStatus_MEETING_STATUS_PENDING}
	if !slices.Equal(statuses, want) {
		t.Errorf("summary_ready statuses = %v, want %v", statuses, want)
	}

	// Anything that has not failed has nothing to retry, and a second retry is refused.
	for name, id := range map[string]uuid.UUID{
		"already retried": meet,
		"pending":         f.seedMeeting(seed{channel: ch, space: space}),
		"ready":           f.seedMeeting(seed{channel: ch, space: space, status: statusReady}),
		"no transcript":   f.seedMeeting(seed{channel: ch, space: space, status: statusNoTranscript}),
	} {
		if err := f.svc.RetrySummary(t.Context(), f.user(f.alice), id); !errors.Is(err, errs.ErrConflict) {
			t.Errorf("%s: RetrySummary = %v, want ErrConflict", name, err)
		}
	}
	failed := f.seedMeeting(seed{channel: ch, space: space, status: statusFailed})
	if err := f.svc.RetrySummary(t.Context(), f.user(f.carol), failed); !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("RetrySummary by a non-member = %v, want ErrForbidden", err)
	}
	if err := f.svc.RetrySummary(t.Context(), f.user(f.alice), uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("RetrySummary of a missing meeting = %v, want ErrNotFound", err)
	}
	if got := f.status(failed); got != statusFailed {
		t.Errorf("refused retry changed the status to %q", got)
	}
}

// ---- read -------------------------------------------------------------------

func TestGetMeeting_ChecksMembershipAndWorkspace(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, title: "Standup", status: statusFailed})
	f.exec(`UPDATE meetings.meetings SET failure_reason = 'no_speech', meet_uri = $2, started_at = $3, ended_at = $4 WHERE id = $1`, id, meetURI, at(9, 0), at(9, 40))

	got, err := f.svc.GetMeeting(t.Context(), f.user(f.alice), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetId() != id.String() || got.GetTitle() != "Standup" || got.GetStatus() != meetingsv1.MeetingStatus_MEETING_STATUS_FAILED ||
		got.GetFailureReason() != "no_speech" || got.GetMeetUri() != meetURI || got.GetChannelId() != ch.String() ||
		!got.GetStartedAt().AsTime().Equal(at(9, 0)) || !got.GetEndedAt().AsTime().Equal(at(9, 40)) || got.GetSummary() != "" {
		t.Errorf("GetMeeting = %v, want the stored meeting without a summary", got)
	}
	if _, err := f.svc.GetMeeting(t.Context(), f.user(f.bob), id); !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("GetMeeting by a non-member = %v, want ErrForbidden", err)
	}
	if _, err := f.svc.GetMeeting(t.Context(), f.user(f.alice), uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("GetMeeting of a missing meeting = %v, want ErrNotFound", err)
	}
	foreign := authz.Actor{Kind: authz.ActorUser, WorkspaceID: f.seedWorkspace("globex.example"), UserID: f.alice}
	if _, err := f.svc.GetMeeting(t.Context(), foreign, id); !errors.Is(err, errs.ErrNotFound) && !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("GetMeeting from another workspace = %v, want it refused", err)
	}
}

func TestTranscriptExcerptAndEntries(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, title: "Standup", status: statusReady})
	f.seedEntries(id, 10)
	empty := f.seedMeeting(seed{channel: ch, space: space})
	alice := f.user(f.alice)

	idxs := func(ex *meetingsv1.Excerpt) []int32 {
		var out []int32
		for _, e := range ex.GetEntries() {
			out = append(out, e.GetIdx())
		}
		return out
	}
	for _, tc := range []struct {
		name          string
		entry, radius int32
		want          []int32
	}{
		{"window", 5, 2, []int32{3, 4, 5, 6, 7}},
		{"clamped at the start", 0, 3, []int32{0, 1, 2, 3}},
		{"clamped at the end", 9, 3, []int32{6, 7, 8, 9}},
		{"radius zero", 4, 0, []int32{4}},
		{"huge radius is capped, not an error", 5, 1 << 30, []int32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}},
	} {
		ex, err := f.svc.GetTranscriptExcerpt(t.Context(), alice, id, tc.entry, tc.radius)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !slices.Equal(idxs(ex), tc.want) || ex.GetFocusIdx() != tc.entry || ex.GetTitle() != "Standup" || ex.GetMeetingId() != id.String() {
			t.Errorf("%s: excerpt = %v, want entries %v around %d", tc.name, ex, tc.want, tc.entry)
		}
	}
	ex, _ := f.svc.GetTranscriptExcerpt(t.Context(), alice, id, 1, 0)
	if e := ex.GetEntries()[0]; e.Speaker != nil || e.GetStartMs() != 1000 || e.GetEndMs() != 1900 || e.GetText() != "line 1" {
		t.Errorf("entry 1 = %v, want no speaker, 1000-1900 ms and its text", e)
	}

	for name, call := range map[string]func() error{
		"entry that does not exist": func() error { _, err := f.svc.GetTranscriptExcerpt(t.Context(), alice, id, 10, 2); return err },
		"meeting without a transcript": func() error {
			_, err := f.svc.GetTranscriptExcerpt(t.Context(), alice, empty, 0, 2)
			return err
		},
	} {
		if err := call(); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
	for name, call := range map[string]func() error{
		"negative entry":  func() error { _, err := f.svc.GetTranscriptExcerpt(t.Context(), alice, id, -1, 1); return err },
		"negative radius": func() error { _, err := f.svc.GetTranscriptExcerpt(t.Context(), alice, id, 1, -1); return err },
	} {
		if err := call(); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := f.svc.GetTranscriptExcerpt(t.Context(), f.user(f.bob), id, 1, 1); !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("excerpt for a non-member = %v, want ErrForbidden", err)
	}

	// The whole transcript: a member reads it as a member, the indexer as a system actor.
	for name, actor := range map[string]authz.Actor{"member": alice, "indexer": f.system(f.alice)} {
		all, err := f.svc.TranscriptEntries(t.Context(), actor, id)
		if err != nil || len(all) != 10 || all[0].GetIdx() != 0 || all[9].GetIdx() != 9 {
			t.Errorf("%s: TranscriptEntries = %d entries, %v, want all 10 in order", name, len(all), err)
		}
	}
	if _, err := f.svc.TranscriptEntries(t.Context(), f.user(f.bob), id); !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("TranscriptEntries for a non-member = %v, want ErrForbidden", err)
	}
	if none, err := f.svc.TranscriptEntries(t.Context(), alice, empty); err != nil || len(none) != 0 {
		t.Errorf("TranscriptEntries of a meeting without one = %v, %v, want none", none, err)
	}
}

// ---- Connect ----------------------------------------------------------------

func TestConnect_RequiresTheProtocolHeader(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	actor := f.user(f.alice)
	f.g.QueueCreateMeetSpace(fakegoogle.SpaceReply{Space: google.Space{Name: space, MeetingURI: meetURI}})

	// No option is passed: the handler adds the requirement itself.
	path, h := Handler(f.svc, func(context.Context) authz.Actor { return actor })
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// A browser can send this cross-site without a preflight: POST with a JSON body.
	call := func(version string) (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path+"StartMeeting",
			strings.NewReader(fmt.Sprintf(`{"channelId":%q}`, ch)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if version != "" {
			req.Header.Set("Connect-Protocol-Version", version)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode, mustRead(t, resp)
	}

	status, body := call("")
	if status != http.StatusBadRequest || !strings.Contains(body, "Connect-Protocol-Version") {
		t.Fatalf("POST without the header: %d %s; want 400 naming Connect-Protocol-Version", status, body)
	}
	if n := f.count(`SELECT count(*) FROM meetings.meetings`); n != 0 || len(f.g.Calls()) != 0 {
		t.Fatalf("the refused call reached the service: %d meetings, %d Meet calls", n, len(f.g.Calls()))
	}
	status, body = call("1")
	if status != http.StatusOK || !strings.Contains(body, meetURI) {
		t.Fatalf("POST with the header: %d %s; want 200 and the meeting", status, body)
	}

	// The generated client sends the header itself and reaches every method.
	client := meetingsv1connect.NewMeetingServiceClient(srv.Client(), srv.URL)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusFailed})
	got, err := client.GetMeeting(t.Context(), connect.NewRequest(&meetingsv1.GetMeetingRequest{MeetingId: id.String()}))
	if err != nil || got.Msg.GetMeeting().GetId() != id.String() {
		t.Fatalf("GetMeeting = %v, %v", got, err)
	}
	if _, err := client.RetrySummary(t.Context(), connect.NewRequest(&meetingsv1.RetrySummaryRequest{MeetingId: id.String()})); err != nil {
		t.Fatalf("RetrySummary: %v", err)
	}
	created, err := client.CreateFromRecording(t.Context(), connect.NewRequest(&meetingsv1.CreateFromRecordingRequest{
		ChannelId: ch.String(), FileId: uuid.NewString(), Title: "Over Connect",
	}))
	if err != nil || created.Msg.GetMeeting().GetTitle() != "Over Connect" {
		t.Fatalf("CreateFromRecording = %v, %v", created, err)
	}
	f.seedEntries(id, 3)
	ex, err := client.GetTranscriptExcerpt(t.Context(), connect.NewRequest(&meetingsv1.GetTranscriptExcerptRequest{MeetingId: id.String(), EntryIdx: 1, Radius: 1}))
	if err != nil || len(ex.Msg.GetExcerpt().GetEntries()) != 3 {
		t.Fatalf("GetTranscriptExcerpt = %v, %v", ex, err)
	}
	for name, err := range map[string]error{
		"malformed meeting id": func() error {
			_, err := client.GetMeeting(t.Context(), connect.NewRequest(&meetingsv1.GetMeetingRequest{MeetingId: "not-a-uuid"}))
			return err
		}(),
		"malformed channel id": func() error {
			_, err := client.StartMeeting(t.Context(), connect.NewRequest(&meetingsv1.StartMeetingRequest{ChannelId: "x"}))
			return err
		}(),
		"malformed file id": func() error {
			_, err := client.CreateFromRecording(t.Context(), connect.NewRequest(&meetingsv1.CreateFromRecordingRequest{ChannelId: ch.String(), FileId: "x", Title: "t"}))
			return err
		}(),
	} {
		if err == nil {
			t.Errorf("%s: the call succeeded", name)
		}
	}
}

func mustRead(t *testing.T, resp *http.Response) string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// ---- the whole path ---------------------------------------------------------

// TestPipeline_StartToReady_ThroughRelayAndMemoryBus runs the Meet path as the app
// does: StartMeeting, the poll, the real outbox relay and the in-process bus with the
// registered jobs, up to a summary slip in the channel. The clock is an hour behind
// the database's, so every delayed event (the re-read and the transcript check) is
// already due and the relay delivers it at once.
func TestPipeline_StartToReady_ThroughRelayAndMemoryBus(t *testing.T) {
	f := newFixture(t)
	past := time.Now().Add(-time.Hour)
	f.now = past
	ch := f.channel("public", f.alice)
	f.g.QueueCreateMeetSpace(fakegoogle.SpaceReply{Space: google.Space{Name: space, MeetingURI: meetURI}})
	started, err := f.svc.StartMeeting(t.Context(), f.user(f.alice), ch)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(started.GetId())

	f.g.HandleListConferences(func(context.Context, uuid.UUID, string) ([]google.Conference, error) {
		return []google.Conference{conference("conferenceRecords/c1", past.Add(-90*time.Minute), new(past.Add(-50*time.Minute)),
			transcript(t1, google.StateFileGenerated, new(past.Add(-49*time.Minute))))}, nil
	})
	f.g.QueueListTranscriptEntries(entriesReply(2), entriesReply(2))

	mem := bus.NewMemory(bus.MemoryOptions{Backoff: []time.Duration{0}})
	for _, j := range Jobs(f.deps) {
		mem.Register(j.Consumer, j.Topics, j.Handler)
	}
	fan := fanFunc(func(context.Context, string, []byte) error { return nil })
	route := routerFunc(func(outbox.Stored) ([]bus.Message, []outbox.Fanout, error) { return nil, nil, nil })
	relay := outbox.NewRelay(f.db, mem, fan, route, outbox.RelayOptions{Poll: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- relay.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	if err := f.poll(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for f.status(id) != statusReady {
		if time.Now().After(deadline) {
			t.Fatalf("meeting is %q after 30s, want ready; logs: %s", f.status(id), f.logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The relay marks a row published only after its consumers settled; wait for the
	// last one so the assertions below see the finished state.
	for f.count(`SELECT count(*) FROM platform.outbox WHERE published_at IS NULL`) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("outbox rows were still unpublished after 30s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	msgs := f.messages(ch)
	if len(msgs) != 2 || msgs[0].GetBody() != "Meeting started: "+meetURI || msgs[1].GetArtifact().GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_MEETING_SUMMARY {
		t.Errorf("messages = %v, want the Meet link then the summary slip", msgs)
	}
	if n := f.topicCount(events.TopicMeetingIngest); n != 2 {
		t.Errorf("%d meeting.ingest events, want the poll's and one re-read", n)
	}
	if got := f.count(`SELECT count(*) FROM meetings.transcript_entries WHERE meeting_id = $1`, id); got != 2 {
		t.Errorf("%d entries, want 2", got)
	}
	// The transcript check fired too, found the meeting already reading, and left it alone.
	if n := f.topicCount(events.TopicMeetingTranscriptCheck); n != 1 {
		t.Errorf("%d transcript checks, want 1", n)
	}
}

// ---- review fixes -----------------------------------------------------------

// rowVersion identifies the physical version of a meeting row and who locked it: an
// UPDATE moves xmin and SELECT ... FOR UPDATE sets xmax, so it changes whenever the
// code wrote to, or even only locked, the row.
func (f *fixture) rowVersion(id uuid.UUID) string {
	f.t.Helper()
	var v string
	f.scalar(&v, `SELECT xmin::text || '/' || xmax::text FROM meetings.meetings WHERE id = $1`, id)
	return v
}

func TestIngest_ModelOutputIsSanitisedBeforeItIsStored(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(3))})
	// A NUL (Postgres text and jsonb refuse it), an invalid byte (protojson refuses it),
	// a bidi override and a bidi isolate (they reorder what a reader sees).
	const dirty = "a\x00b\xffc\u202ed\u2066e"
	const clean = "ab\uFFFDcde"
	f.summ.reply = Summary{
		Summary:       "S" + dirty,
		Decisions:     []Item{{Text: "D" + dirty, EntryRefs: []int{1}}},
		OpenQuestions: []Item{{Text: "Q" + dirty, EntryRefs: []int{0}}},
		ActionItems:   []ActionItem{{Title: "T" + dirty, OwnerHint: "O" + dirty, DueHint: "H" + dirty, EntryRefs: []int{1}}},
	}
	f.g.QueueListTranscriptEntries(entriesReply(3))

	if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 2), 1); err != nil {
		t.Fatalf("ingest = %v, want nil: the model's text must not make the one transaction fail for ever", err)
	}

	if got := f.status(id); got != statusReady {
		t.Fatalf("status = %q, want ready", got)
	}
	got, err := f.svc.GetMeeting(t.Context(), f.user(f.alice), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSummary() != "S"+clean || got.GetDecisions()[0].GetText() != "D"+clean || got.GetOpenQuestions()[0].GetText() != "Q"+clean {
		t.Errorf("stored summary = %q %q %q, want the model's text without NUL, bad bytes and bidi controls",
			got.GetSummary(), got.GetDecisions()[0].GetText(), got.GetOpenQuestions()[0].GetText())
	}
	a := f.messages(ch)[0].GetArtifact()
	if a.GetBodyMarkdown() != "S"+clean || a.GetTracks()[0].GetText() != "D"+clean || a.GetTracks()[1].GetText() != "Q"+clean {
		t.Errorf("slip = %q %q %q, want the same clean text", a.GetBodyMarkdown(), a.GetTracks()[0].GetText(), a.GetTracks()[1].GetText())
	}
	var td eventsv1.TaskDraftRequested
	f.payload(f.outbox(events.TopicTaskDraft)[0], &td)
	if it := td.GetItems()[0]; it.GetTitle() != "T"+clean || it.GetOwnerHint() != "O"+clean || it.GetDueHint() != "H"+clean {
		t.Errorf("task.draft item = %v, want the same clean text", it)
	}
}

func TestIngest_FailingApply_NacksThenEndsFailedOnTheFinalDelivery(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(3))})
	f.g.HandleListTranscriptEntries(func(context.Context, uuid.UUID, string) ([]google.Entry, error) {
		return entriesOf(3, true, true), nil
	})
	boom := errors.New("chat is down")
	f.chatHook = func(context.Context) error { return boom }
	msg := f.ingestEvent(id, t1, 2)

	for attempt := 1; attempt < finalAttempt; attempt++ {
		if err := f.deliver(f.ingest, msg, attempt); !errors.Is(err, boom) {
			t.Fatalf("delivery %d: ingest = %v, want the write's error so the bus redelivers", attempt, err)
		}
		if got := f.status(id); got != statusIngesting {
			t.Fatalf("delivery %d: status = %q, want ingesting", attempt, got)
		}
		if n := f.count(`SELECT count(*) FROM meetings.transcript_entries`) + f.count(`SELECT count(*) FROM meetings.summaries`) +
			f.count(`SELECT count(*) FROM platform.processed_events`) + len(f.topics()); n != 0 {
			t.Fatalf("delivery %d left %d rows behind, want the failed transaction rolled back whole", attempt, n)
		}
	}

	if err := f.deliver(f.ingest, msg, finalAttempt); err != nil {
		t.Fatalf("final delivery: ingest = %v, want nil: nothing delivers it again, so the meeting must be settled", err)
	}
	row := f.meeting(id)
	if row.Status != statusFailed || row.Reason == nil || *row.Reason != "internal" {
		t.Fatalf("meeting = %q (%v), want failed with internal", row.Status, row.Reason)
	}
	if msgs := f.messages(ch); len(msgs) != 0 {
		t.Errorf("%d messages posted for a failed meeting", len(msgs))
	}
	ready := f.outbox(events.TopicMeetingSummaryReady)
	if len(ready) != 1 {
		t.Fatalf("%d meeting.summary_ready events, want 1", len(ready))
	}
	var sr eventsv1.MeetingSummaryReady
	f.payload(ready[0], &sr)
	if sr.GetStatus() != meetingsv1.MeetingStatus_MEETING_STATUS_FAILED || sr.GetFailureReason() != "internal" || sr.GetMeetingId() != id.String() {
		t.Errorf("summary_ready = %v, want FAILED internal", &sr)
	}

	// The message is settled: a late redelivery (the cause gone) changes nothing.
	f.chatHook = nil
	if err := f.deliver(f.ingest, msg, 1); err != nil {
		t.Fatal(err)
	}
	if got := f.status(id); got != statusFailed {
		t.Errorf("status after a late redelivery = %q, want it still failed", got)
	}
}

func TestIngest_FailingApplyOnTheFinalDeliveryWhileStopping_LeavesTheMeeting(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(3))})
	f.g.HandleListTranscriptEntries(func(context.Context, uuid.UUID, string) ([]google.Entry, error) {
		return entriesOf(3, true, true), nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	f.chatHook = func(context.Context) error {
		cancel() // the process is stopping while the transaction runs
		return errors.New("chat is down")
	}
	msg := f.ingestEvent(id, t1, 2)
	msg.Attempt = finalAttempt

	if err := f.ingest(ctx, msg); err == nil {
		t.Fatal("ingest returned nil, want the error so the message is redelivered")
	}

	if got := f.status(id); got != statusIngesting {
		t.Errorf("status = %q, want ingesting: a process that is stopping must not fail the meeting", got)
	}
	if n := f.topicCount(events.TopicMeetingSummaryReady); n != 0 {
		t.Errorf("%d meeting.summary_ready events, want none", n)
	}
}

func TestTranscriptCheck_FailingApply_NacksThenEndsFailedOnTheFinalDelivery(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space})
	boom := errors.New("chat is down")
	f.chatHook = func(context.Context) error { return boom }
	msg := f.checkEvent(id)

	for attempt := 1; attempt < finalAttempt; attempt++ {
		if err := f.deliver(f.check, msg, attempt); !errors.Is(err, boom) {
			t.Fatalf("delivery %d: check = %v, want the write's error", attempt, err)
		}
		if got := f.status(id); got != statusPending {
			t.Fatalf("delivery %d: status = %q, want pending", attempt, got)
		}
	}
	if err := f.deliver(f.check, msg, finalAttempt); err != nil {
		t.Fatalf("final delivery: check = %v, want nil", err)
	}
	row := f.meeting(id)
	if row.Status != statusFailed || row.Reason == nil || *row.Reason != "internal" {
		t.Errorf("meeting = %q (%v), want failed with internal", row.Status, row.Reason)
	}
}

func TestIngest_RereadLoopIsBounded(t *testing.T) {
	// The cap is 10 reads: one per link of the chain of re-reads, counted by the
	// event's attempt.
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space, status: statusIngesting, last: new(int32(1))})
	n := 1
	f.g.HandleListTranscriptEntries(func(context.Context, uuid.UUID, string) ([]google.Entry, error) {
		n++ // a transcript that never settles: every read differs from the last
		return entriesOf(n, true, true), nil
	})

	if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 9), 1); err != nil {
		t.Fatal(err)
	}
	if got := f.status(id); got != statusIngesting {
		t.Fatalf("status after read 9 = %q, want ingesting", got)
	}
	rows := f.outbox(events.TopicMeetingIngest)
	if len(rows) != 1 {
		t.Fatalf("%d re-reads after read 9, want 1", len(rows))
	}
	var ev eventsv1.MeetingIngest
	f.payload(rows[0], &ev)
	if ev.GetAttempt() != 10 {
		t.Fatalf("re-read attempt = %d, want 10", ev.GetAttempt())
	}

	if err := f.deliver(f.ingest, f.message(rows[0]), 1); err != nil {
		t.Fatalf("read 10: %v", err)
	}
	row := f.meeting(id)
	if row.Status != statusFailed || row.Reason == nil || *row.Reason != "meet_unavailable" {
		t.Fatalf("meeting = %q (%v), want failed with meet_unavailable once the 10th read still disagrees", row.Status, row.Reason)
	}
	if n := f.topicCount(events.TopicMeetingIngest); n != 1 {
		t.Errorf("%d meeting.ingest events, want no re-read after the cap", n)
	}
	var sr eventsv1.MeetingSummaryReady
	f.payload(f.outbox(events.TopicMeetingSummaryReady)[0], &sr)
	if sr.GetStatus() != meetingsv1.MeetingStatus_MEETING_STATUS_FAILED || sr.GetFailureReason() != "meet_unavailable" {
		t.Errorf("summary_ready = %v, want FAILED meet_unavailable", &sr)
	}
	if n := f.count(`SELECT count(*) FROM meetings.transcript_entries`); n != 0 {
		t.Errorf("%d entries written for a transcript that never settled", n)
	}
}

func TestPoll_UnchangedTickWritesNothing(t *testing.T) {
	// Nanoseconds that Postgres cannot keep: the stored value must still compare equal.
	start := time.Date(2026, 10, 11, 9, 0, 0, 123456789, time.UTC)
	end := time.Date(2026, 10, 11, 9, 40, 0, 987654321, time.UTC)
	for _, tc := range []struct {
		name   string
		status string
		conf   google.Conference
	}{
		{"running conference", statusPending, conference("conferenceRecords/c1", start, nil)},
		{"ended conference without a transcript", statusPending, conference("conferenceRecords/c1", start, &end)},
		{"transcript still being generated", statusPending, conference("conferenceRecords/c1", start, &end, transcript(t1, google.StateEnded, &end))},
		{"transcript read has started", statusPending, conference("conferenceRecords/c1", start, &end, transcript(t1, google.StateFileGenerated, &end))},
		{"ingesting meeting", statusIngesting, conference("conferenceRecords/c1", start, &end, transcript(t1, google.StateFileGenerated, &end))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			ch := f.channel("public", f.alice)
			id := f.seedMeeting(seed{channel: ch, space: space, status: tc.status})
			f.g.HandleListConferences(func(context.Context, uuid.UUID, string) ([]google.Conference, error) {
				return []google.Conference{tc.conf}, nil
			})
			if err := f.poll(); err != nil {
				t.Fatal(err)
			}
			version, outbox := f.rowVersion(id), len(f.topics())

			for i := range 3 {
				if err := f.poll(); err != nil {
					t.Fatal(err)
				}
				if got := f.rowVersion(id); got != version {
					t.Fatalf("tick %d wrote to (or locked) a meeting nothing changed for: row version %s, was %s", i+2, got, version)
				}
			}
			if got := len(f.topics()); got != outbox {
				t.Errorf("%d outbox rows after the unchanged ticks, want still %d", got, outbox)
			}
			if calls := len(f.g.Calls()); calls != 4 {
				t.Errorf("%d Meet calls, want one per tick", calls)
			}
		})
	}
}

func TestPoll_ChangeAfterAnUnchangedTick_IsStillRecorded(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space})
	running := conference("conferenceRecords/c1", at(9, 0), nil)
	ended := conference("conferenceRecords/c1", at(9, 0), new(at(9, 40)))
	generated := conference("conferenceRecords/c1", at(9, 0), new(at(9, 40)), transcript(t1, google.StateFileGenerated, new(at(9, 41))))
	f.g.QueueListConferences(conferences(running), conferences(running), conferences(ended), conferences(ended), conferences(generated))

	for range 2 {
		if err := f.poll(); err != nil {
			t.Fatal(err)
		}
	}
	if row := f.meeting(id); row.Ended != nil {
		t.Fatalf("ended_at = %v while the conference runs", row.Ended)
	}
	for range 2 {
		if err := f.poll(); err != nil {
			t.Fatal(err)
		}
	}
	if row := f.meeting(id); row.Ended == nil || !row.Ended.Equal(at(9, 40)) || f.topicCount(events.TopicMeetingTranscriptCheck) != 1 {
		t.Fatalf("after the end: ended_at %v with %d checks, want 09:40 and one check", row.Ended, f.topicCount(events.TopicMeetingTranscriptCheck))
	}
	if err := f.poll(); err != nil {
		t.Fatal(err)
	}
	if got := f.status(id); got != statusIngesting || f.topicCount(events.TopicMeetingIngest) != 1 {
		t.Errorf("status %q with %d ingest events after the transcript was generated, want ingesting and one", got, f.topicCount(events.TopicMeetingIngest))
	}
}

func TestRetrySummary_MeetMeetingThatNeverHadAConference_ReturnsToPending(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	id := f.seedMeeting(seed{channel: ch, space: space})
	f.g.QueueListConferences(fakegoogle.ConferencesReply{Err: &google.ConsentRequiredError{URL: "/api/auth/meet-consent"}})
	if err := f.poll(); err != nil {
		t.Fatal(err)
	}
	if row := f.meeting(id); row.Status != statusFailed || row.Reason == nil || *row.Reason != "meet_access" || row.Conference != nil {
		t.Fatalf("meeting = %+v, want failed with meet_access before any conference was seen", row)
	}

	if err := f.svc.RetrySummary(t.Context(), f.user(f.alice), id); err != nil {
		t.Fatal(err)
	}

	row := f.meeting(id)
	if row.Status != statusPending || row.Reason != nil {
		t.Fatalf("meeting = %+v, want pending with no reason: there is no transcript to look for yet, the poll keeps looking", row)
	}
	if n := f.topicCount(events.TopicMeetingIngest); n != 0 {
		t.Errorf("%d meeting.ingest events, want none: an ingest would end it as no_transcript", n)
	}
	var sr eventsv1.MeetingSummaryReady
	f.payload(f.outbox(events.TopicMeetingSummaryReady)[1], &sr)
	if sr.GetStatus() != meetingsv1.MeetingStatus_MEETING_STATUS_PENDING {
		t.Errorf("summary_ready after the retry = %v, want PENDING", &sr)
	}

	// The grant is back: the poll finds the transcript as it would have the first time.
	f.g.QueueListConferences(conferences(conference("conferenceRecords/c1", at(9, 0), new(at(9, 40)), transcript(t1, google.StateFileGenerated, new(at(9, 41))))))
	if err := f.poll(); err != nil {
		t.Fatal(err)
	}
	if got := f.status(id); got != statusIngesting || f.topicCount(events.TopicMeetingIngest) != 1 {
		t.Errorf("status %q with %d ingest events, want ingesting and one", got, f.topicCount(events.TopicMeetingIngest))
	}
}

func TestMeetingDates_AreBangkokTime(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	// 01:30 on 11 October in Bangkok, still the 10th in UTC.
	f.now = time.Date(2026, 10, 10, 18, 30, 0, 0, time.UTC)
	f.g.QueueCreateMeetSpace(fakegoogle.SpaceReply{Space: google.Space{Name: space, MeetingURI: meetURI}})

	started, err := f.svc.StartMeeting(t.Context(), f.user(f.alice), ch)
	if err != nil {
		t.Fatal(err)
	}
	if started.GetTitle() != "Meeting 2026-10-11" {
		t.Errorf("default title = %q, want the Bangkok date", started.GetTitle())
	}

	id := uuid.MustParse(started.GetId())
	f.exec(`UPDATE meetings.meetings SET status = 'ingesting', last_entry_count = 3, ended_at = $2 WHERE id = $1`, id, f.now)
	f.g.QueueListTranscriptEntries(entriesReply(3))
	if err := f.deliver(f.ingest, f.ingestEvent(id, t1, 2), 1); err != nil {
		t.Fatal(err)
	}
	slip := f.messages(ch)[1].GetArtifact()
	if slip.GetMeta() != "2026-10-11" {
		t.Errorf("slip meta = %q, want the Bangkok date of the meeting", slip.GetMeta())
	}
	if asked := f.summ.asked(); len(asked) != 1 || !asked[0].MeetingDate.Equal(f.now) {
		t.Errorf("summarizer input = %+v, want the meeting date as the instant it ended", asked)
	}
}

func TestPoll_ExactlyTwentyFourHoursOld_IsIgnored(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	f.seedMeeting(seed{channel: ch, space: "spaces/exactly", age: 24 * time.Hour})
	f.seedMeeting(seed{channel: ch, space: "spaces/inside", age: 24*time.Hour - time.Second})
	f.g.HandleListConferences(func(context.Context, uuid.UUID, string) ([]google.Conference, error) { return nil, nil })

	if err := f.poll(); err != nil {
		t.Fatal(err)
	}

	if want := []fakegoogle.Call{{Method: "ListConferences", UserID: f.alice, Name: "spaces/inside"}}; !slices.Equal(f.g.Calls(), want) {
		t.Errorf("Meet calls = %+v, want only the meeting a second inside the window", f.g.Calls())
	}
}

func TestStartMeeting_LinkPostFailure_RollsBackTheMeeting(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	boom := errors.New("chat is down")
	f.chatHook = func(context.Context) error { return boom }
	f.g.QueueCreateMeetSpace(fakegoogle.SpaceReply{Space: google.Space{Name: space, MeetingURI: meetURI}})

	if _, err := f.svc.StartMeeting(t.Context(), f.user(f.alice), ch); !errors.Is(err, boom) {
		t.Fatalf("StartMeeting = %v, want the post's error", err)
	}

	if n := f.count(`SELECT count(*) FROM meetings.meetings`); n != 0 {
		t.Errorf("%d meetings after a failed link post, want the row rolled back with it", n)
	}
	if msgs := f.messages(ch); len(msgs) != 0 {
		t.Errorf("%d messages, want none", len(msgs))
	}
	if topics := f.topics(); len(topics) != 0 {
		t.Errorf("outbox topics = %v, want none", topics)
	}

	// The REF number rolled back too: the next meeting is the first one.
	f.chatHook = nil
	f.g.QueueCreateMeetSpace(fakegoogle.SpaceReply{Space: google.Space{Name: space, MeetingURI: meetURI}})
	got, err := f.svc.StartMeeting(t.Context(), f.user(f.alice), ch)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetRef() != "MTG-1001" {
		t.Errorf("ref = %q, want MTG-1001: a rolled-back meeting consumes no number", got.GetRef())
	}
}

func TestPoll_CancelledContext_StopsAndWritesNothing(t *testing.T) {
	t.Run("already cancelled", func(t *testing.T) {
		f := newFixture(t)
		ch := f.channel("public", f.alice)
		id := f.seedMeeting(seed{channel: ch, space: space})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if err := PollMeetings(ctx, f.db, f.g, f.now); !errors.Is(err, context.Canceled) {
			t.Fatalf("PollMeetings = %v, want context.Canceled", err)
		}
		if calls := f.g.Calls(); len(calls) != 0 {
			t.Errorf("%d Meet calls from a cancelled poll", len(calls))
		}
		if got := f.status(id); got != statusPending {
			t.Errorf("status = %q, want pending", got)
		}
	})

	t.Run("cancelled between meetings", func(t *testing.T) {
		f := newFixture(t)
		ch := f.channel("public", f.alice)
		first := f.seedMeeting(seed{channel: ch, space: "spaces/first", age: 2 * time.Hour})
		second := f.seedMeeting(seed{channel: ch, space: "spaces/second", age: time.Hour})
		ctx, cancel := context.WithCancel(t.Context())
		f.g.HandleListConferences(func(ctx context.Context, _ uuid.UUID, _ string) ([]google.Conference, error) {
			cancel() // the process stops while Meet is being asked about the first meeting
			return nil, ctx.Err()
		})

		if err := PollMeetings(ctx, f.db, f.g, f.now); !errors.Is(err, context.Canceled) {
			t.Fatalf("PollMeetings = %v, want context.Canceled", err)
		}
		if calls := f.g.Calls(); len(calls) != 1 || calls[0].Name != "spaces/first" {
			t.Errorf("Meet calls = %+v, want only the first meeting asked about", calls)
		}
		for name, id := range map[string]uuid.UUID{"first": first, "second": second} {
			if got := f.status(id); got != statusPending {
				t.Errorf("%s meeting = %q, want pending", name, got)
			}
		}
		if strings.Contains(f.logs.String(), "poll failed") {
			t.Errorf("a shutdown was logged as a failure: %s", f.logs.String())
		}
	})
}

type fanFunc func(ctx context.Context, topic string, frame []byte) error

func (p fanFunc) SPublish(ctx context.Context, topic string, frame []byte) error {
	return p(ctx, topic, frame)
}

type routerFunc func(outbox.Stored) ([]bus.Message, []outbox.Fanout, error)

func (r routerFunc) Route(ev outbox.Stored) ([]bus.Message, []outbox.Fanout, error) { return r(ev) }
