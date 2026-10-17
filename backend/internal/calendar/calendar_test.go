package calendar

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"google.golang.org/protobuf/types/known/timestamppb"

	calendarv1 "kiwi/gen/kiwi/calendar/v1"
	"kiwi/internal/chat"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/google"
	"kiwi/internal/testkit/dbtest"
	"kiwi/internal/testkit/fakegoogle"
)

// sqlMembership answers channel membership from chat.channel_members, so the calendar
// tests need no Valkey (the same shortcut the meetings tests take).
type sqlMembership struct{ db *db.DB }

func (m sqlMembership) IsMember(ctx context.Context, channelID, userID uuid.UUID) (ok bool, err error) {
	err = m.db.InWorkspace(ctx, authz.WorkspaceFrom(ctx), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM chat.channel_members WHERE channel_id = $1 AND user_id = $2)`,
			channelID, userID).Scan(&ok)
	})
	return ok, err
}

var base = time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)

type fixture struct {
	t    *testing.T
	env  *dbtest.Env
	db   *db.DB
	chat chat.Service
	g    *fakegoogle.Client
	svc  Service
	now  time.Time

	ws         uuid.UUID
	alice, bob uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := dbtest.Start(t)
	d, err := db.Open(t.Context(), config.Config{DatabaseURL: env.AppURL, DatabaseSystemURL: env.SystemURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(d.Close)

	az := authz.New(sqlMembership{db: d})
	f := &fixture{t: t, env: env, db: d, g: fakegoogle.New(), now: base}
	f.chat = chat.New(chat.Deps{DB: d, Authz: az})
	f.svc = New(Deps{DB: d, Authz: az, Google: f.g, Chat: f.chat, MemberChannels: f.chat.MemberChannelIDs, Clock: func() time.Time { return f.now }})
	f.ws = f.seedWorkspace("acme.example")
	f.alice = f.seedUser("Alice")
	f.bob = f.seedUser("Bob")
	return f
}

func (f *fixture) exec(sql string, args ...any) {
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
	if _, err := tx.Exec(ctx, `SELECT set_config('kiwi.workspace_id', $1, true)`, f.ws.String()); err != nil {
		f.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	ctx := f.t.Context()
	conn, err := pgx.Connect(ctx, f.env.OwnerURL)
	if err != nil {
		f.t.Fatalf("connect as owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if _, err := conn.Exec(ctx, `SELECT set_config('kiwi.workspace_id', $1, false)`, f.ws.String()); err != nil {
		f.t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (f *fixture) seedWorkspace(domain string) uuid.UUID {
	ws := uuid.New()
	f.ws = ws
	f.exec(`INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, $2, $2)`, ws, domain)
	return ws
}

func (f *fixture) seedUser(name string) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO identity.users (id, workspace_id, google_sub, email, name) VALUES ($1, $2, $3, $4, $5)`,
		id, f.ws, "sub-"+id.String(), strings.ToLower(name)+"@acme.example", name)
	return id
}

func (f *fixture) channel(members ...uuid.UUID) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO chat.channels (id, workspace_id, name, kind) VALUES ($1, $2, $3, 'public')`, id, f.ws, "ch-"+id.String()[:8])
	for _, m := range members {
		f.exec(`INSERT INTO chat.channel_members (channel_id, user_id, workspace_id, role) VALUES ($1, $2, $3, 'member')`, id, m, f.ws)
	}
	return id
}

func (f *fixture) user(id uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorUser, WorkspaceID: f.ws, UserID: id}
}

func (f *fixture) queueEvent(id, meetURI string) {
	f.g.QueueScheduleCalendarEvent(fakegoogle.EventReply{
		Event: google.CalendarEvent{ID: id, HTMLLink: "https://calendar.google.com/event?eid=" + id, MeetURI: meetURI},
	})
}

func tspb(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

func req(channel uuid.UUID, title string, start time.Time, minutes int32) *calendarv1.ScheduleMeetingRequest {
	r := &calendarv1.ScheduleMeetingRequest{ChannelId: channel.String(), Title: title, DurationMinutes: minutes}
	if !start.IsZero() {
		r.StartAt = tspb(start)
	}
	return r
}

func orgReq(channel uuid.UUID, title string, start time.Time, minutes int32) *calendarv1.ScheduleMeetingRequest {
	r := req(channel, title, start, minutes)
	r.OrgWide = true
	return r
}

func TestScheduleMeeting_CreatesEventWithMeetLinkAndPostsSlip(t *testing.T) {
	f := newFixture(t)
	ch := f.channel(f.alice)
	f.queueEvent("gcal-1", "https://meet.google.com/abc-defg-hij")

	start := f.now.Add(2 * time.Hour)
	ev, err := f.svc.ScheduleMeeting(t.Context(), f.user(f.alice), req(ch, "Weekly sync", start, 30))
	if err != nil {
		t.Fatalf("ScheduleMeeting: %v", err)
	}

	if ev.GetRef() != "EVT-1001" {
		t.Errorf("ref = %q, want EVT-1001", ev.GetRef())
	}
	if ev.GetMeetUri() != "https://meet.google.com/abc-defg-hij" || ev.GetHtmlLink() == "" {
		t.Errorf("event links = %q / %q, want the Meet and calendar links", ev.GetMeetUri(), ev.GetHtmlLink())
	}
	if ev.GetStatus() != calendarv1.EventStatus_EVENT_STATUS_SCHEDULED {
		t.Errorf("status = %v, want SCHEDULED", ev.GetStatus())
	}
	if !ev.GetStartAt().AsTime().Equal(start) || !ev.GetEndAt().AsTime().Equal(start.Add(30*time.Minute)) {
		t.Errorf("span = [%s, %s], want [%s, +30m]", ev.GetStartAt().AsTime(), ev.GetEndAt().AsTime(), start)
	}

	// Google was asked as Alice, with the span the request named.
	calls := f.g.Calls()
	if len(calls) != 1 || calls[0].Method != "ScheduleCalendarEvent" || calls[0].UserID != f.alice {
		t.Fatalf("Google calls = %+v, want one ScheduleCalendarEvent as Alice", calls)
	}
	if calls[0].Event.Title != "Weekly sync" || !calls[0].Event.EndAt.Equal(start.Add(30*time.Minute)) {
		t.Errorf("Google event input = %+v, want the title and the 30-minute span", calls[0].Event)
	}

	if n := f.count(`SELECT count(*) FROM calendar.events WHERE id = $1 AND status = 'scheduled'`, ev.GetId()); n != 1 {
		t.Errorf("stored scheduled events = %d, want 1", n)
	}
	if n := f.count(`SELECT count(*) FROM chat.messages WHERE channel_id = $1 AND body LIKE 'Meeting scheduled:%'`, ch); n != 1 {
		t.Errorf("scheduling slips in the channel = %d, want 1", n)
	}
}

func TestScheduleMeeting_RejectsBadInputAndNonMembersBeforeCallingGoogle(t *testing.T) {
	f := newFixture(t)
	ch := f.channel(f.alice)
	future := f.now.Add(time.Hour)

	cases := []struct {
		name string
		as   uuid.UUID
		req  *calendarv1.ScheduleMeetingRequest
	}{
		{"empty title", f.alice, req(ch, "  ", future, 30)},
		{"past start", f.alice, req(ch, "Retro", f.now.Add(-time.Minute), 30)},
		{"no start", f.alice, req(ch, "Retro", time.Time{}, 30)},
		{"zero duration", f.alice, req(ch, "Retro", future, 0)},
		{"duration too long", f.alice, req(ch, "Retro", future, 481)},
		{"not a member", f.bob, req(ch, "Retro", future, 30)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.ScheduleMeeting(t.Context(), f.user(tc.as), tc.req)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
	// Nothing reached Google, and no row or slip was written.
	if n := len(f.g.Calls()); n != 0 {
		t.Errorf("Google calls = %d, want 0: validation and authz come first", n)
	}
	if n := f.count(`SELECT count(*) FROM calendar.events`); n != 0 {
		t.Errorf("stored events = %d, want 0", n)
	}
}

func TestListUpcoming_ByChannelAndWorkspaceWideExcludesEndedAndCanceled(t *testing.T) {
	f := newFixture(t)
	chA := f.channel(f.alice)
	chB := f.channel(f.alice)
	other := f.channel(f.bob) // Alice is not a member

	f.queueEvent("g1", "https://meet.google.com/a")
	f.queueEvent("g2", "https://meet.google.com/b")
	f.queueEvent("g3", "https://meet.google.com/c")
	soon, _ := f.svc.ScheduleMeeting(t.Context(), f.user(f.alice), req(chA, "Soon", f.now.Add(time.Hour), 30))
	later, _ := f.svc.ScheduleMeeting(t.Context(), f.user(f.alice), req(chA, "Later", f.now.Add(3*time.Hour), 30))
	inB, _ := f.svc.ScheduleMeeting(t.Context(), f.user(f.alice), req(chB, "In B", f.now.Add(2*time.Hour), 30))

	// One channel, soonest first.
	got, err := f.svc.ListUpcoming(t.Context(), f.user(f.alice), &chA)
	if err != nil {
		t.Fatalf("ListUpcoming(chA): %v", err)
	}
	if len(got) != 2 || got[0].GetId() != soon.GetId() || got[1].GetId() != later.GetId() {
		t.Fatalf("chA upcoming = %v, want [Soon, Later] soonest first", ids(got))
	}

	// Workspace-wide: Alice's two channels, soonest first across both; not chOther.
	all, err := f.svc.ListUpcoming(t.Context(), f.user(f.alice), nil)
	if err != nil {
		t.Fatalf("ListUpcoming(all): %v", err)
	}
	if len(all) != 3 || all[0].GetId() != soon.GetId() || all[1].GetId() != inB.GetId() || all[2].GetId() != later.GetId() {
		t.Fatalf("workspace-wide upcoming = %v, want [Soon, In B, Later]", ids(all))
	}
	_ = other

	// An event that has already ended drops out, and a canceled one too.
	f.exec(`UPDATE calendar.events SET start_at = $2, end_at = $3 WHERE id = $1`,
		later.GetId(), f.now.Add(-2*time.Hour), f.now.Add(-time.Hour))
	f.exec(`UPDATE calendar.events SET status = 'canceled' WHERE id = $1`, inB.GetId())
	all, _ = f.svc.ListUpcoming(t.Context(), f.user(f.alice), nil)
	if len(all) != 1 || all[0].GetId() != soon.GetId() {
		t.Fatalf("after ending Later and canceling In B, upcoming = %v, want [Soon]", ids(all))
	}
}

func TestListUpcoming_OrgWideEventIsVisibleAcrossTheWholeWorkspace(t *testing.T) {
	f := newFixture(t)
	chAlice := f.channel(f.alice)
	chBob := f.channel(f.bob) // Alice is not a member

	f.queueEvent("g-team", "https://meet.google.com/team")
	f.queueEvent("g-org", "https://meet.google.com/org")
	// A team event in Bob's channel, and an org-wide event Bob schedules in his channel.
	teamOnly, err := f.svc.ScheduleMeeting(t.Context(), f.user(f.bob), req(chBob, "Bob's standup", f.now.Add(2*time.Hour), 30))
	if err != nil {
		t.Fatalf("team event: %v", err)
	}
	orgWide, err := f.svc.ScheduleMeeting(t.Context(), f.user(f.bob), orgReq(chBob, "All hands", f.now.Add(time.Hour), 60))
	if err != nil {
		t.Fatalf("org-wide event: %v", err)
	}
	if !orgWide.GetOrgWide() {
		t.Errorf("scheduled org event org_wide = false, want true")
	}

	// Alice, who is not in Bob's channel, sees the org-wide event but not the team one.
	got, err := f.svc.ListUpcoming(t.Context(), f.user(f.alice), nil)
	if err != nil {
		t.Fatalf("ListUpcoming(all) for Alice: %v", err)
	}
	if len(got) != 1 || got[0].GetId() != orgWide.GetId() {
		t.Fatalf("Alice's workspace-wide list = %v, want only the org-wide All hands", ids(got))
	}
	_ = teamOnly
	_ = chAlice
}

func TestCancelEvent_OnlyOrganizerCancelsAndGoogleIsTold(t *testing.T) {
	f := newFixture(t)
	ch := f.channel(f.alice, f.bob)
	f.queueEvent("gcal-9", "https://meet.google.com/x")
	ev, err := f.svc.ScheduleMeeting(t.Context(), f.user(f.alice), req(ch, "Sync", f.now.Add(time.Hour), 30))
	if err != nil {
		t.Fatalf("ScheduleMeeting: %v", err)
	}
	eventID := uuid.MustParse(ev.GetId())

	// Bob is a member (he could schedule) but did not organize this one.
	if err := f.svc.CancelEvent(t.Context(), f.user(f.bob), eventID); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("Bob cancelling Alice's event = %v, want forbidden", err)
	}

	var canceledID string
	f.g.HandleCancelCalendarEvent(func(_ context.Context, userID uuid.UUID, id string) error {
		if userID != f.alice {
			t.Errorf("cancel asked Google as %s, want Alice", userID)
		}
		canceledID = id
		return nil
	})
	if err := f.svc.CancelEvent(t.Context(), f.user(f.alice), eventID); err != nil {
		t.Fatalf("Alice cancelling her event: %v", err)
	}
	if canceledID != "gcal-9" {
		t.Errorf("Google asked to cancel %q, want gcal-9", canceledID)
	}
	if n := f.count(`SELECT count(*) FROM calendar.events WHERE id = $1 AND status = 'canceled'`, ev.GetId()); n != 1 {
		t.Errorf("canceled rows = %d, want 1", n)
	}

	// Cancelling an already-canceled event is a conflict, not a second Google call.
	if err := f.svc.CancelEvent(t.Context(), f.user(f.alice), eventID); !errors.Is(err, errs.ErrConflict) {
		t.Errorf("second cancel = %v, want conflict", err)
	}
}

func ids(events []*calendarv1.CalendarEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.GetTitle()
	}
	return out
}
