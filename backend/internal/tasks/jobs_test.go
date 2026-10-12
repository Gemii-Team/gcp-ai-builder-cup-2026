package tasks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	tasksv1 "kiwi/gen/kiwi/tasks/v1"
	"kiwi/internal/chat"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
)

// ---- task.draft ---------------------------------------------------------------

// meetingEnded is the end of the meeting the draft events are about: 01:30 on Saturday
// in Bangkok, still Friday in UTC.
var meetingEnded = time.Date(2026, 10, 9, 18, 30, 0, 0, time.UTC)

// draftJob is the task.draft handler over the real tasks and chat services. members,
// when set, replaces the lookup of a channel's members.
func (f *fixture) draftJob(members func(ctx context.Context, a authz.Actor, channelID uuid.UUID) ([]*chatv1.Member, error)) bus.Handler {
	f.t.Helper()
	m := sqlMembership{db: f.db}
	az := authz.New(m)
	if members == nil {
		members = chat.New(chat.Deps{DB: f.db, Authz: az}).ListMembers
	}
	jobs := Jobs(Deps{DB: f.db, Authz: az, MemberChannels: m.memberChannels, ChannelMembers: members})
	if len(jobs) != 1 || jobs[0].Consumer != "task.draft" || !slices.Equal(jobs[0].Topics, []string{events.TopicTaskDraft}) {
		f.t.Fatalf("Jobs = %+v, want the one consumer task.draft on the task.draft topic", jobs)
	}
	return jobs[0].Handler
}

// draftEvent is a task.draft message as the meetings pipeline publishes it.
func (f *fixture) draftEvent(ch, meeting, owner uuid.UUID, date *time.Time, items ...*eventsv1.TaskDraftRequested_Item) bus.Message {
	f.t.Helper()
	ev := &eventsv1.TaskDraftRequested{
		WorkspaceId: f.ws.String(), MeetingId: meeting.String(), MeetingTitle: "Sprint review",
		ChannelId: ch.String(), OwnerUserId: owner.String(), Items: items,
	}
	if date != nil {
		ev.MeetingDate = timestamppb.New(*date)
	}
	data, err := protoJSON.Marshal(ev)
	if err != nil {
		f.t.Fatal(err)
	}
	return bus.Message{
		ID: uuid.NewString(), Topic: events.TopicTaskDraft, Data: data,
		Attrs: map[string]string{"workspace_id": f.ws.String()}, Attempt: 1,
	}
}

func item(title, owner, due string, entry int32, offsetMs *int64) *eventsv1.TaskDraftRequested_Item {
	return &eventsv1.TaskDraftRequested_Item{Title: title, OwnerHint: owner, DueHint: due, EntryIdx: entry, OffsetMs: offsetMs}
}

// drafts returns the draft tasks of a channel by title.
func (f *fixture) drafts(ch uuid.UUID) map[string]*tasksv1.Task {
	f.t.Helper()
	out := map[string]*tasksv1.Task{}
	for _, task := range f.list(f.user(f.alice), ch, tasksv1.TaskState_TASK_STATE_DRAFT) {
		out[task.GetTitle()] = task
	}
	return out
}

// bangkokDay is midnight of the given date in Asia/Bangkok, how a due date is stored.
func bangkokDay(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.FixedZone("Asia/Bangkok", 7*60*60))
}

func TestTaskDraftJob_CreatesDraftsWithSourceSnapshotAndNullOwner(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	meeting := uuid.New()
	job := f.draftJob(nil)

	msg := f.draftEvent(ch, meeting, f.alice, &meetingEnded,
		item("Write the release notes", "Alice", "+1d", 4, proto.Int64(754000)),
		item("Book the room", "Somchai", "soon", 7, nil),
		item("Call the vendor", "", "next:mon", -1, nil),
		item("Open the sprint", "bob", "2026-10-16", 0, proto.Int64(0)),
		item("   ", "Alice", "+1d", 2, nil),
	)
	if err := job(t.Context(), msg); err != nil {
		t.Fatalf("task.draft: %v", err)
	}

	got := f.drafts(ch)
	if len(got) != 4 {
		t.Fatalf("drafts = %v, want the four items with a title (the blank one skipped)", got)
	}
	for title, task := range got {
		if task.GetState() != tasksv1.TaskState_TASK_STATE_DRAFT || task.GetStatus() != tasksv1.TaskStatus_TASK_STATUS_TODO ||
			task.GetCreatedByKind() != tasksv1.CreatedByKind_CREATED_BY_KIND_AGENT || task.GetCreatedById() != authz.AgentID("system").String() ||
			task.GetChannelId() != ch.String() || task.GetRef() == "" {
			t.Errorf("%q = %v, want a draft in the channel made by the job as an agent, with a REF", title, task)
		}
		src := task.GetSource()
		if src.GetKind() != chatv1.SourceKind_SOURCE_KIND_MEETING || src.GetId() != meeting.String() || src.GetLabel() != "Sprint review" || src.GetChannelId() != ch.String() {
			t.Errorf("%q source = %v, want the meeting with its title as the label", title, src)
		}
	}

	// Owner and due date are resolved in code, in Bangkok time: the meeting ended on
	// Saturday 10 October there, so +1d is Sunday 11 October, not Saturday.
	notes := got["Write the release notes"]
	if notes.GetAssigneeId() != f.alice.String() || !notes.GetDueAt().AsTime().Equal(bangkokDay(2026, 10, 11)) {
		t.Errorf("notes = assignee %q due %v, want Alice and 2026-10-11 Bangkok", notes.GetAssigneeId(), notes.GetDueAt().AsTime())
	}
	if src := notes.GetSource(); src.GetAnchor() != "4" || src.OffsetMs == nil || src.GetOffsetMs() != 754000 {
		t.Errorf("notes source = %v, want anchor 4 at 754000 ms", src)
	}

	// A hint that names no member leaves the owner null, an unusable due hint leaves the
	// date null, and an entry without timing has no offset.
	room := got["Book the room"]
	if room.AssigneeId != nil || room.DueAt != nil {
		t.Errorf("room = assignee %v due %v, want both empty", room.AssigneeId, room.DueAt)
	}
	if src := room.GetSource(); src.GetAnchor() != "7" || src.OffsetMs != nil {
		t.Errorf("room source = %v, want anchor 7 and no offset", src)
	}

	// An item that names no transcript entry (entry_idx < 0) cites the meeting alone.
	call := got["Call the vendor"]
	if call.AssigneeId != nil || !call.GetDueAt().AsTime().Equal(bangkokDay(2026, 10, 12)) {
		t.Errorf("call = assignee %v due %v, want no owner and Monday 2026-10-12 Bangkok", call.AssigneeId, call.GetDueAt().AsTime())
	}
	if src := call.GetSource(); src.GetAnchor() != "" || src.OffsetMs != nil {
		t.Errorf("call source = %v, want no anchor and no offset", src)
	}

	// Entry 0 at offset 0 is a real moment of a timed transcript and keeps both, and the
	// owner hint is folded: "bob" is Bob.
	open := got["Open the sprint"]
	if src := open.GetSource(); src.GetAnchor() != "0" || src.OffsetMs == nil || src.GetOffsetMs() != 0 {
		t.Errorf("open source = %v, want anchor 0 at offset 0", src)
	}
	if open.GetAssigneeId() != f.bob.String() || !open.GetDueAt().AsTime().Equal(bangkokDay(2026, 10, 16)) {
		t.Errorf("open = assignee %q due %v, want Bob and 2026-10-16 Bangkok", open.GetAssigneeId(), open.GetDueAt().AsTime())
	}

	// A draft is a proposal: nothing here is a confirmed task.
	if n := len(f.list(f.user(f.alice), ch, tasksv1.TaskState_TASK_STATE_CONFIRMED)); n != 0 {
		t.Errorf("%d confirmed tasks, want none", n)
	}
}

func TestTaskDraftJob_RedeliveryNoDuplicateDrafts(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	job := f.draftJob(nil)
	msg := f.draftEvent(ch, uuid.New(), f.alice, &meetingEnded,
		item("Write the release notes", "Alice", "+1d", 1, nil),
		item("Book the room", "Bob", "none", 2, nil),
	)

	for attempt := 1; attempt <= 3; attempt++ {
		msg.Attempt = attempt
		if err := job(t.Context(), msg); err != nil {
			t.Fatalf("delivery %d: %v", attempt, err)
		}
	}

	if got := f.drafts(ch); len(got) != 2 {
		t.Errorf("drafts = %v, want the two items once however often the message is delivered", got)
	}
	var n int
	f.scalar(&n, `SELECT count(*) FROM tasks.tasks`)
	if n != 2 {
		t.Errorf("%d task rows, want 2", n)
	}
}

func TestTaskDraftJob_AmbiguousOwnerStaysEmpty(t *testing.T) {
	f := newFixture(t)
	a, b := f.seedUser(f.ws, "Somchai Jaidee"), f.seedUser(f.ws, "Somchai Wong")
	ch := f.channel("public", f.alice, a, b)
	job := f.draftJob(nil)

	msg := f.draftEvent(ch, uuid.New(), f.alice, &meetingEnded,
		item("Send the invoice", "Somchai", "+2d", 1, nil),
		item("Fix the login", "Somchai Wong", "+2d", 2, nil),
	)
	if err := job(t.Context(), msg); err != nil {
		t.Fatal(err)
	}

	got := f.drafts(ch)
	if invoice := got["Send the invoice"]; invoice.AssigneeId != nil {
		t.Errorf("a hint that names two members was resolved to %q, want no owner", invoice.GetAssigneeId())
	}
	if login := got["Fix the login"]; login.GetAssigneeId() != b.String() {
		t.Errorf("a full name was resolved to %q, want %s", login.GetAssigneeId(), b)
	}
}

func TestTaskDraftJob_OwnersAreTheChannelsMembersOnly(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob) // Carol is in the workspace but not the channel.
	job := f.draftJob(nil)

	msg := f.draftEvent(ch, uuid.New(), f.alice, &meetingEnded, item("Review the contract", "Carol", "+2d", 1, nil))
	if err := job(t.Context(), msg); err != nil {
		t.Fatal(err)
	}

	if task := f.drafts(ch)["Review the contract"]; task == nil || task.AssigneeId != nil {
		t.Errorf("draft = %v, want it without an owner: Carol is not in the channel", task)
	}
}

func TestTaskDraftJob_NoMeetingDate_LeavesDueEmpty(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	job := f.draftJob(nil)

	msg := f.draftEvent(ch, uuid.New(), f.alice, nil,
		item("Send the invoice", "Alice", "+2d", 1, nil),
		item("Fix the login", "Alice", "2026-10-16", 2, nil),
	)
	if err := job(t.Context(), msg); err != nil {
		t.Fatal(err)
	}

	for title, task := range f.drafts(ch) {
		if task.DueAt != nil {
			t.Errorf("%q has due %v, want none: an event without a meeting date resolves no date", title, task.GetDueAt().AsTime())
		}
		if task.GetAssigneeId() != f.alice.String() {
			t.Errorf("%q has owner %q, want Alice", title, task.GetAssigneeId())
		}
	}
}

func TestTaskDraftJob_NothingToDraft_IsAcknowledged(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	job := f.draftJob(func(context.Context, authz.Actor, uuid.UUID) ([]*chatv1.Member, error) {
		return nil, errors.New("the members must not be asked for when there is nothing to draft")
	})

	if err := job(t.Context(), f.draftEvent(ch, uuid.New(), f.alice, &meetingEnded, item(" ", "Alice", "+1d", 0, nil))); err != nil {
		t.Errorf("an event whose items have no title: %v, want it acknowledged", err)
	}
	if err := job(t.Context(), f.draftEvent(ch, uuid.New(), f.alice, &meetingEnded)); err != nil {
		t.Errorf("an event without items: %v, want it acknowledged", err)
	}
	if got := f.drafts(ch); len(got) != 0 {
		t.Errorf("drafts = %v, want none", got)
	}
}

func TestTaskDraftJob_MalformedEvent_IsInvalid(t *testing.T) {
	f := newFixture(t)
	job := f.draftJob(nil)

	good := f.draftEvent(f.channel("public", f.alice), uuid.New(), f.alice, &meetingEnded, item("x", "", "none", 0, nil))
	for name, mutate := range map[string]func(*bus.Message){
		"not json": func(m *bus.Message) { m.Data = []byte("{") },
		"no channel": func(m *bus.Message) {
			m.Data = []byte(fmt.Sprintf(`{"workspace_id":%q,"meeting_id":%q,"owner_user_id":%q}`, f.ws, uuid.New(), f.alice))
		},
		"bad meeting": func(m *bus.Message) {
			m.Data = []byte(fmt.Sprintf(`{"workspace_id":%q,"meeting_id":"x","channel_id":%q,"owner_user_id":%q}`, f.ws, uuid.New(), f.alice))
		},
	} {
		msg := good
		msg.ID = uuid.NewString()
		mutate(&msg)
		if err := job(t.Context(), msg); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestTaskDraftJob_FailureLeavesNoPartialDrafts_AndRedeliveryStartsOver(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	real := chat.New(chat.Deps{DB: f.db, Authz: authz.New(sqlMembership{db: f.db})}).ListMembers

	// The first lookup still lists Bob, who has left the channel since: the second draft
	// names him as its owner and the service refuses it. The member list of the second
	// delivery is the real one.
	var calls int
	stale := []*chatv1.Member{{UserId: f.alice.String(), Name: "Alice"}, {UserId: f.bob.String(), Name: "Bob"}}
	job := f.draftJob(func(ctx context.Context, a authz.Actor, channelID uuid.UUID) ([]*chatv1.Member, error) {
		calls++
		if calls == 1 {
			return stale, nil
		}
		return real(ctx, a, channelID)
	})
	f.exec(f.ws, `DELETE FROM chat.channel_members WHERE channel_id = $1 AND user_id = $2`, ch, f.bob)

	msg := f.draftEvent(ch, uuid.New(), f.alice, &meetingEnded,
		item("Write the release notes", "Alice", "+1d", 1, nil),
		item("Book the room", "Bob", "+1d", 2, nil),
	)
	if err := job(t.Context(), msg); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("first delivery: err = %v, want the service's refusal of an assignee outside the channel", err)
	}
	if got := f.drafts(ch); len(got) != 0 {
		t.Fatalf("drafts after a failed delivery = %v, want none: all drafts or none", got)
	}

	msg.Attempt = 2
	if err := job(t.Context(), msg); err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	got := f.drafts(ch)
	if len(got) != 2 || got["Write the release notes"].GetAssigneeId() != f.alice.String() || got["Book the room"].AssigneeId != nil {
		t.Errorf("drafts = %v, want both, Bob's without an owner now that he is not a member", got)
	}
}

func TestTaskDraftJob_MemberLookupFailure_NacksAndLeavesNothing(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	boom := errors.New("members are down")
	job := f.draftJob(func(context.Context, authz.Actor, uuid.UUID) ([]*chatv1.Member, error) { return nil, boom })

	msg := f.draftEvent(ch, uuid.New(), f.alice, &meetingEnded, item("Send the invoice", "Alice", "+1d", 1, nil))
	if err := job(t.Context(), msg); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the lookup's error so the message is delivered again", err)
	}
	if got := f.drafts(ch); len(got) != 0 {
		t.Errorf("drafts = %v, want none", got)
	}
	var n int
	f.scalar(&n, `SELECT count(*) FROM platform.processed_events WHERE consumer = 'task.draft'`)
	if n != 0 {
		t.Errorf("%d processed events recorded, want none: the redelivery must run", n)
	}
}

func TestJobs_RequireBothMemberLookups(t *testing.T) {
	channelMembers := func(context.Context, authz.Actor, uuid.UUID) ([]*chatv1.Member, error) { return nil, nil }
	memberChannels := func(context.Context, uuid.UUID, uuid.UUID) ([]uuid.UUID, error) { return nil, nil }
	panics := func(d Deps) (panicked bool) {
		defer func() { panicked = recover() != nil }()
		Jobs(d)
		return false
	}

	// Each lookup is used in the middle of a job (the members to resolve owners from, the
	// member channels to check an owner before a draft is written): a missing one must stop
	// the wiring, not a message.
	for name, d := range map[string]Deps{
		"no lookups":         {},
		"no member channels": {ChannelMembers: channelMembers},
		"no channel members": {MemberChannels: memberChannels},
	} {
		if !panics(d) {
			t.Errorf("%s: Jobs did not fail at wiring time, which would be a nil dereference in the middle of a job", name)
		}
	}
	if panics(Deps{ChannelMembers: channelMembers, MemberChannels: memberChannels}) {
		t.Error("Jobs with both lookups panicked")
	}
}

// A due date a timestamp cannot hold is left off its draft. It is never the reason a
// meeting's whole batch is refused: the drafts are written in one transaction, and an
// error there would be delivered again until every draft of the meeting was lost.
func TestTaskDraftJob_AnUnstorableDueDateLeavesTheDraftWithoutIt(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	job := f.draftJob(nil)

	// A meeting on the last days of year 9999: +3d is in the year 10000. A meeting on the
	// first days of year 1: 0001-01-01 in Bangkok is before the first instant a timestamp
	// has. Both are inside the window ResolveDue allows around a meeting.
	edge9999 := time.Date(9999, 12, 30, 0, 0, 0, 0, time.UTC)
	edge0001 := time.Date(1, 1, 2, 10, 0, 0, 0, time.UTC)
	if err := job(t.Context(), f.draftEvent(ch, uuid.New(), f.alice, &edge9999,
		item("Plan the next millennium", "Alice", "+3d", 1, nil),
		item("Close the books", "Alice", "9999-12-31", 2, nil),
	)); err != nil {
		t.Fatalf("a meeting at the end of year 9999: %v, want its drafts written", err)
	}
	if err := job(t.Context(), f.draftEvent(ch, uuid.New(), f.alice, &edge0001,
		item("Date the founding charter", "Alice", "0001-01-01", 1, nil),
		item("Plan the next day", "Alice", "+1d", 2, nil),
	)); err != nil {
		t.Fatalf("a meeting in year 1: %v, want its drafts written", err)
	}

	got := f.drafts(ch)
	if len(got) != 4 {
		t.Fatalf("drafts = %v, want all four: a due date that cannot be stored must not cost a meeting its drafts", got)
	}
	for _, title := range []string{"Plan the next millennium", "Date the founding charter"} {
		if got[title].DueAt != nil || got[title].GetAssigneeId() != f.alice.String() {
			t.Errorf("%q = due %v, assignee %q, want no due date and its owner", title, got[title].GetDueAt(), got[title].GetAssigneeId())
		}
	}
	// What can be stored is: the last day of 9999, and the day after the first of year 1.
	if due := got["Close the books"].GetDueAt(); due == nil || due.CheckValid() != nil {
		t.Errorf("Close the books = due %v, want the last day of year 9999", due)
	}
	if due := got["Plan the next day"].GetDueAt(); due == nil || due.CheckValid() != nil {
		t.Errorf("Plan the next day = due %v, want a valid date", due)
	}
}
