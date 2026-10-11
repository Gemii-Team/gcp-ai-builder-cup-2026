package tasks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
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
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	tasksv1 "kiwi/gen/kiwi/tasks/v1"
	"kiwi/gen/kiwi/tasks/v1/tasksv1connect"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/testkit/dbtest"
)

// ---- fixture ----------------------------------------------------------------

// sqlMembership answers channel membership straight from chat.channel_members over
// db.InWorkspace. The tasks tests need no Valkey: the service sees membership
// only through authz.Membership and Deps.MemberChannels, and this is the real
// table behind both.
type sqlMembership struct{ db *db.DB }

func (m sqlMembership) IsMember(ctx context.Context, channelID, userID uuid.UUID) (ok bool, err error) {
	err = m.db.InWorkspace(ctx, authz.WorkspaceFrom(ctx), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM chat.channel_members WHERE channel_id = $1 AND user_id = $2)`,
			channelID, userID).Scan(&ok)
	})
	return ok, err
}

func (m sqlMembership) memberChannels(ctx context.Context, ws, userID uuid.UUID) (ids []uuid.UUID, err error) {
	err = m.db.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT channel_id FROM chat.channel_members WHERE user_id = $1 ORDER BY channel_id`, userID)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	return ids, err
}

// fixture is one migrated database and the tasks service over it, with a
// workspace holding Alice, Bob and Carol.
type fixture struct {
	t   *testing.T
	env *dbtest.Env
	db  *db.DB
	svc Service

	ws                uuid.UUID
	alice, bob, carol uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := dbtest.Start(t)
	d, err := db.Open(t.Context(), config.Config{DatabaseURL: env.AppURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(d.Close)

	members := sqlMembership{db: d}
	f := &fixture{t: t, env: env, db: d}
	f.svc = New(Deps{DB: d, Authz: authz.New(members), MemberChannels: members.memberChannels})
	f.ws = f.seedWorkspace("acme.example")
	f.alice = f.seedUser(f.ws, "Alice")
	f.bob = f.seedUser(f.ws, "Bob")
	f.carol = f.seedUser(f.ws, "Carol")
	return f
}

// asOwner runs fn as kiwi_owner in a transaction bound to ws: the tests seed and
// inspect tenant rows this way and never through kiwi_system.
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

func (f *fixture) scalar(dst any, sql string, args ...any) {
	f.t.Helper()
	f.asOwner(f.ws, func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			f.t.Fatalf("%s: %v", sql, err)
		}
	})
}

func (f *fixture) seedWorkspace(domain string) uuid.UUID {
	f.t.Helper()
	ws := uuid.New()
	f.exec(ws, `INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, $2, $2)`, ws, domain)
	return ws
}

func (f *fixture) seedUser(ws uuid.UUID, name string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(ws, `INSERT INTO identity.users (id, workspace_id, google_sub, email, name) VALUES ($1, $2, $3, $4, $5)`,
		id, ws, "sub-"+id.String(), strings.ToLower(name)+"@example.test", name)
	return id
}

// channel creates a channel of kind in the fixture workspace with the given members.
func (f *fixture) channel(kind string, members ...uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(f.ws, `INSERT INTO chat.channels (id, workspace_id, name, kind) VALUES ($1, $2, $3, $4)`,
		id, f.ws, "ch-"+id.String()[:8], kind)
	for _, m := range members {
		f.exec(f.ws, `INSERT INTO chat.channel_members (channel_id, user_id, workspace_id, role) VALUES ($1, $2, $3, 'member')`,
			id, m, f.ws)
	}
	return id
}

func (f *fixture) user(id uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorUser, WorkspaceID: f.ws, UserID: id}
}

func (f *fixture) agentFor(id uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorAgent, WorkspaceID: f.ws, UserID: uuid.New(), OnBehalfOf: id, Agent: "assistant"}
}

func (f *fixture) system(origin uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorSystem, WorkspaceID: f.ws, UserID: origin}
}

// create makes a task as a, failing the test on error. mods adjust the request.
func (f *fixture) create(a authz.Actor, ch uuid.UUID, title string, mods ...func(*tasksv1.CreateTaskRequest)) *tasksv1.Task {
	f.t.Helper()
	req := &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: title}
	for _, m := range mods {
		m(req)
	}
	task, err := f.svc.CreateTask(f.t.Context(), a, req)
	if err != nil {
		f.t.Fatalf("CreateTask(%q): %v", title, err)
	}
	return task
}

// draft makes a draft as the agent acting for alice.
func (f *fixture) draft(ch uuid.UUID, title string, mods ...func(*tasksv1.CreateTaskRequest)) *tasksv1.Task {
	f.t.Helper()
	req := &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: title, Source: meetingSource(ch)}
	for _, m := range mods {
		m(req)
	}
	task, err := f.svc.CreateDraft(f.t.Context(), f.agentFor(f.alice), req)
	if err != nil {
		f.t.Fatalf("CreateDraft(%q): %v", title, err)
	}
	return task
}

func withAssignee(id uuid.UUID) func(*tasksv1.CreateTaskRequest) {
	return func(r *tasksv1.CreateTaskRequest) { r.AssigneeId = proto.String(id.String()) }
}

func withSource(s *chatv1.Source) func(*tasksv1.CreateTaskRequest) {
	return func(r *tasksv1.CreateTaskRequest) { r.Source = s }
}

func meetingSource(ch uuid.UUID) *chatv1.Source {
	return &chatv1.Source{
		Kind: chatv1.SourceKind_SOURCE_KIND_MEETING, Id: uuid.NewString(), Anchor: "12",
		Label: "Sprint review", ChannelId: ch.String(), OffsetMs: proto.Int64(754000),
	}
}

func messageSource(ch uuid.UUID, seq int64) *chatv1.Source {
	return &chatv1.Source{
		Kind: chatv1.SourceKind_SOURCE_KIND_MESSAGE, Id: uuid.NewString(), Anchor: strconv.FormatInt(seq, 10),
		Label: "Alice", ChannelId: ch.String(), At: timestamppb.New(time.Date(2026, 10, 11, 9, 30, 15, 123456000, time.UTC)),
	}
}

// list returns the tasks of one channel as a.
func (f *fixture) list(a authz.Actor, ch uuid.UUID, states ...tasksv1.TaskState) []*tasksv1.Task {
	f.t.Helper()
	got, err := f.svc.ListTasks(f.t.Context(), a, ListTasksQuery{ChannelID: &ch, States: states})
	if err != nil {
		f.t.Fatalf("ListTasks: %v", err)
	}
	return got
}

// taskEvents returns the task.changed events enqueued so far, oldest first.
func (f *fixture) taskEvents() []*eventsv1.TaskChanged {
	f.t.Helper()
	var out []*eventsv1.TaskChanged
	f.asOwner(f.ws, func(ctx context.Context, tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT payload::text FROM platform.outbox WHERE topic = $1 ORDER BY id`, events.TopicTaskChanged)
		if err != nil {
			f.t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				f.t.Fatal(err)
			}
			ev := &eventsv1.TaskChanged{}
			if err := protojson.Unmarshal([]byte(p), ev); err != nil {
				f.t.Fatalf("decode %q: %v", p, err)
			}
			out = append(out, ev)
		}
		if err := rows.Err(); err != nil {
			f.t.Fatal(err)
		}
	})
	return out
}

// confirmedEvents counts the task.changed events that announce a confirmed task.
func (f *fixture) confirmedEvents() int {
	n := 0
	for _, ev := range f.taskEvents() {
		if ev.GetTask().GetState() == tasksv1.TaskState_TASK_STATE_CONFIRMED {
			n++
		}
	}
	return n
}

func (f *fixture) taskCount() int {
	f.t.Helper()
	var n int
	f.scalar(&n, `SELECT count(*) FROM tasks.tasks`)
	return n
}

func wantErr(t *testing.T, err, target error, what string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("%s: err = %v, want %v", what, err, target)
	}
}

func titles(ts []*tasksv1.Task) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.GetTitle()
	}
	return out
}

// ---- position ---------------------------------------------------------------

func TestPositionBetween_StrictlyOrdersUnderRandomInserts(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) // fixed seed: the same 1000 inserts every run

	var keys []string
	for i := range 1000 {
		at := rng.IntN(len(keys) + 1) // 0 = new first, len = new last
		var lo, hi string
		if at > 0 {
			lo = keys[at-1]
		}
		if at < len(keys) {
			hi = keys[at]
		}
		key := PositionBetween(lo, hi)
		if lo != "" && key <= lo || hi != "" && key >= hi {
			t.Fatalf("insert %d: PositionBetween(%q, %q) = %q, not strictly between", i, lo, hi, key)
		}
		keys = slices.Insert(keys, at, key)
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			t.Fatalf("keys[%d] = %q >= keys[%d] = %q: order broke or a key repeated", i-1, keys[i-1], i, keys[i])
		}
	}
}

func TestPositionBetween_AppendsAndPrependsStayShort(t *testing.T) {
	if got := PositionBetween("", ""); got != "a0" {
		t.Fatalf("PositionBetween of an empty board = %q, want the first key a0", got)
	}
	last, first := "a0", "a0"
	for range 1000 {
		next := PositionBetween(last, "")
		if next <= last {
			t.Fatalf("append after %q gave %q", last, next)
		}
		last = next
		prev := PositionBetween("", first)
		if prev >= first {
			t.Fatalf("prepend before %q gave %q", first, prev)
		}
		first = prev
	}
	if len(last) > 4 || len(first) > 4 {
		t.Fatalf("after 1000 appends the key is %q and after 1000 prepends %q: keys must not grow with every insert at an end", last, first)
	}
}

func TestPositionBetween_PanicsOnUnorderedOrMalformedKeys(t *testing.T) {
	for name, args := range map[string][2]string{
		"lo equal to hi":     {"a1", "a1"},
		"lo above hi":        {"a2", "a1"},
		"trailing zero":      {"a10", ""},
		"not a key":          {"", "-"},
		"integer part short": {"b", ""},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("PositionBetween(%q, %q) returned instead of panicking", args[0], args[1])
				}
			}()
			PositionBetween(args[0], args[1])
		})
	}
}

// ---- confirm ----------------------------------------------------------------

func TestConfirmDraft_RequiresAssignee(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	draft := f.draft(ch, "Send the invoice")

	// The API refuses it; no UI is involved.
	_, err := f.svc.ConfirmDraft(t.Context(), f.user(f.alice), uuid.MustParse(draft.GetId()))
	wantErr(t, err, errs.ErrInvalid, "ConfirmDraft without an owner")
	if !strings.Contains(err.Error(), "choose an owner first") {
		t.Fatalf("err = %v, want it to say to choose an owner first", err)
	}
	if got := f.list(f.user(f.alice), ch); len(got) != 1 || got[0].GetState() != tasksv1.TaskState_TASK_STATE_DRAFT {
		t.Fatalf("after the refused confirm: %v, want the one draft untouched", got)
	}
	if n := f.confirmedEvents(); n != 0 {
		t.Fatalf("task.changed(CONFIRMED) rows = %d after a refused confirm, want 0", n)
	}

	// Once the draft has an owner it confirms.
	if _, err := f.svc.UpdateTask(t.Context(), f.user(f.alice), &tasksv1.UpdateTaskRequest{Id: draft.GetId(), AssigneeId: proto.String(f.bob.String())}); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	sealed, err := f.svc.ConfirmDraft(t.Context(), f.user(f.alice), uuid.MustParse(draft.GetId()))
	if err != nil {
		t.Fatalf("ConfirmDraft with an owner: %v", err)
	}
	if sealed.GetState() != tasksv1.TaskState_TASK_STATE_CONFIRMED || sealed.GetAssigneeId() != f.bob.String() || sealed.GetRef() != draft.GetRef() {
		t.Fatalf("confirmed task = %v, want CONFIRMED, owned by Bob, keeping %s", sealed, draft.GetRef())
	}
	if n := f.confirmedEvents(); n != 1 {
		t.Fatalf("task.changed(CONFIRMED) rows = %d, want 1", n)
	}
}

func TestConfirmDraft_Concurrent_OnlyOneWins(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	draft := f.draft(ch, "Book the room", withAssignee(f.bob))
	id := uuid.MustParse(draft.GetId())

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, actor := range []authz.Actor{f.user(f.alice), f.user(f.bob)} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.svc.ConfirmDraft(t.Context(), actor, id)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var won, conflicted int
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, errs.ErrConflict):
			conflicted++
		default:
			t.Fatalf("ConfirmDraft: unexpected error %v", err)
		}
	}
	if won != 1 || conflicted != 1 {
		t.Fatalf("two concurrent confirms: %d succeeded, %d conflicted; want exactly one of each", won, conflicted)
	}
	if n := f.confirmedEvents(); n != 1 {
		t.Fatalf("task.changed(CONFIRMED) rows = %d, want exactly 1", n)
	}
}

func TestConfirmDraft_NonMemberForbidden(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	draft := f.draft(ch, "Review the contract", withAssignee(f.bob))
	id := uuid.MustParse(draft.GetId())

	// Carol is in the workspace but not in the channel; an agent never confirms.
	_, err := f.svc.ConfirmDraft(t.Context(), f.user(f.carol), id)
	wantErr(t, err, errs.ErrForbidden, "non-member confirms")
	_, err = f.svc.ConfirmDraft(t.Context(), f.agentFor(f.alice), id)
	wantErr(t, err, errs.ErrForbidden, "agent confirms")
	_, err = f.svc.ConfirmDraft(t.Context(), f.user(f.alice), uuid.New())
	wantErr(t, err, errs.ErrNotFound, "confirm of an unknown task")

	if got := f.list(f.user(f.alice), ch); len(got) != 1 || got[0].GetState() != tasksv1.TaskState_TASK_STATE_DRAFT {
		t.Fatalf("after the refused confirms: %v, want the draft untouched", got)
	}
	if n := f.confirmedEvents(); n != 0 {
		t.Fatalf("task.changed(CONFIRMED) rows = %d, want 0", n)
	}
}

// ---- update, discard --------------------------------------------------------

func TestUpdateTask_EmptyTitleInvalid(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	task := f.create(f.user(f.alice), ch, "Original")

	for _, title := range []string{"", "   \t\n", "bad\x00title"} {
		_, err := f.svc.UpdateTask(t.Context(), f.user(f.alice), &tasksv1.UpdateTaskRequest{Id: task.GetId(), Title: proto.String(title)})
		wantErr(t, err, errs.ErrInvalid, fmt.Sprintf("UpdateTask title %q", title))
	}
	if got := f.list(f.user(f.alice), ch); got[0].GetTitle() != "Original" {
		t.Fatalf("title = %q after refused updates, want Original", got[0].GetTitle())
	}

	got, err := f.svc.UpdateTask(t.Context(), f.user(f.alice), &tasksv1.UpdateTaskRequest{Id: task.GetId(), Title: proto.String("  Renamed  ")})
	if err != nil || got.GetTitle() != "Renamed" {
		t.Fatalf("UpdateTask = %v, %v, want the trimmed title Renamed", got, err)
	}
}

func TestUpdateTask_AppliesFieldsAndClearsThem(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	task := f.create(f.user(f.alice), ch, "Plan", withAssignee(f.alice))
	id := task.GetId()
	due := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)

	got, err := f.svc.UpdateTask(t.Context(), f.user(f.alice), &tasksv1.UpdateTaskRequest{
		Id: id, Description: proto.String("details"), AssigneeId: proto.String(f.bob.String()), DueAt: timestamppb.New(due),
	})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if got.GetDescription() != "details" || got.GetAssigneeId() != f.bob.String() || !got.GetDueAt().AsTime().Equal(due) || got.GetTitle() != "Plan" {
		t.Fatalf("after set: %v, want description, Bob and the due date set and the title kept", got)
	}

	got, err = f.svc.UpdateTask(t.Context(), f.user(f.alice), &tasksv1.UpdateTaskRequest{Id: id, ClearAssignee: true, ClearDue: true})
	if err != nil {
		t.Fatalf("UpdateTask clear: %v", err)
	}
	if got.AssigneeId != nil || got.GetDueAt() != nil || got.GetDescription() != "details" {
		t.Fatalf("after clear: %v, want no assignee and no due date, description kept", got)
	}

	// Setting and clearing the same field is contradictory.
	for name, req := range map[string]*tasksv1.UpdateTaskRequest{
		"assignee": {Id: id, AssigneeId: proto.String(f.bob.String()), ClearAssignee: true},
		"due":      {Id: id, DueAt: timestamppb.New(due), ClearDue: true},
	} {
		_, err := f.svc.UpdateTask(t.Context(), f.user(f.alice), req)
		wantErr(t, err, errs.ErrInvalid, "set and clear "+name)
	}
	_, err = f.svc.UpdateTask(t.Context(), f.user(f.alice), &tasksv1.UpdateTaskRequest{Id: uuid.NewString(), Title: proto.String("x")})
	wantErr(t, err, errs.ErrNotFound, "update of an unknown task")

	evs := f.taskEvents()
	if last := evs[len(evs)-1]; last.GetChannelId() != ch.String() || last.GetTask().GetId() != id || last.GetTask().AssigneeId != nil {
		t.Fatalf("last task.changed = %v, want the cleared task on its channel", last)
	}
}

func TestDiscardDraft_RemovesOnlyDrafts(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	draft := f.draft(ch, "Maybe")
	kept := f.create(f.user(f.alice), ch, "Real")

	if err := f.svc.DiscardDraft(t.Context(), f.agentFor(f.alice), uuid.MustParse(draft.GetId())); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("agent discards: %v, want forbidden", err)
	}
	if err := f.svc.DiscardDraft(t.Context(), f.user(f.alice), uuid.MustParse(kept.GetId())); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("discard of a confirmed task: %v, want a conflict", err)
	}
	if err := f.svc.DiscardDraft(t.Context(), f.user(f.alice), uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("discard of an unknown task: %v, want not found", err)
	}
	if err := f.svc.DiscardDraft(t.Context(), f.user(f.alice), uuid.MustParse(draft.GetId())); err != nil {
		t.Fatalf("DiscardDraft: %v", err)
	}
	if got := titles(f.list(f.user(f.alice), ch)); !slices.Equal(got, []string{"Real"}) {
		t.Fatalf("tasks after the discard = %v, want only the confirmed one", got)
	}
	if err := f.svc.DiscardDraft(t.Context(), f.user(f.alice), uuid.MustParse(draft.GetId())); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("second discard: %v, want not found", err)
	}
}

func TestWriteMethods_NonMemberForbidden(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	task := f.create(f.user(f.alice), ch, "Mine")
	draft := f.draft(ch, "Proposed")
	carol := f.user(f.carol)

	_, err := f.svc.UpdateTask(t.Context(), carol, &tasksv1.UpdateTaskRequest{Id: task.GetId(), Title: proto.String("hijacked")})
	wantErr(t, err, errs.ErrForbidden, "UpdateTask")
	_, err = f.svc.MoveTask(t.Context(), carol, &tasksv1.MoveTaskRequest{Id: task.GetId(), Status: tasksv1.TaskStatus_TASK_STATUS_DONE})
	wantErr(t, err, errs.ErrForbidden, "MoveTask")
	wantErr(t, f.svc.DiscardDraft(t.Context(), carol, uuid.MustParse(draft.GetId())), errs.ErrForbidden, "DiscardDraft")
	_, err = f.svc.CreateTask(t.Context(), carol, &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: "x"})
	wantErr(t, err, errs.ErrForbidden, "CreateTask")
	_, err = f.svc.CreateDraft(t.Context(), carol, &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: "x", Source: meetingSource(ch)})
	wantErr(t, err, errs.ErrForbidden, "CreateDraft")
	_, err = f.svc.ListTasks(t.Context(), carol, ListTasksQuery{ChannelID: &ch})
	wantErr(t, err, errs.ErrForbidden, "ListTasks of a channel she is not in")

	if got := f.list(f.user(f.alice), ch); len(got) != 2 {
		t.Fatalf("tasks = %v, want the two seeded ones untouched", titles(got))
	}
}

// ---- create -----------------------------------------------------------------

func TestCreateDraft_AgentActorAllowed_CreateTaskForbidden(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	agent := f.agentFor(f.alice)

	draft, err := f.svc.CreateDraft(t.Context(), agent, &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: "Follow up", Source: meetingSource(ch)})
	if err != nil {
		t.Fatalf("CreateDraft as an agent: %v", err)
	}
	if draft.GetState() != tasksv1.TaskState_TASK_STATE_DRAFT || draft.GetRef() != "TSK-1001" ||
		draft.GetCreatedByKind() != tasksv1.CreatedByKind_CREATED_BY_KIND_AGENT || draft.GetCreatedById() != authz.AgentID("assistant").String() {
		t.Fatalf("agent draft = %v, want a DRAFT TSK-1001 created by the assistant agent", draft)
	}

	_, err = f.svc.CreateTask(t.Context(), agent, &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: "Direct"})
	wantErr(t, err, errs.ErrForbidden, "agent CreateTask")

	// A job drafts for the user whose action started it, without a membership check.
	job, err := f.svc.CreateDraft(t.Context(), f.system(f.alice), &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: "From the transcript", Source: meetingSource(ch)})
	if err != nil || job.GetCreatedByKind() != tasksv1.CreatedByKind_CREATED_BY_KIND_AGENT {
		t.Fatalf("CreateDraft as a job = %v, %v, want an agent-made draft", job, err)
	}
	_, err = f.svc.CreateTask(t.Context(), f.system(f.alice), &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: "Direct"})
	wantErr(t, err, errs.ErrForbidden, "job CreateTask")

	// An agent acting for someone outside the channel drafts nothing.
	_, err = f.svc.CreateDraft(t.Context(), f.agentFor(f.carol), &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: "x", Source: meetingSource(ch)})
	wantErr(t, err, errs.ErrForbidden, "agent for a non-member")
	if n := f.taskCount(); n != 2 {
		t.Fatalf("tasks stored = %d, want the two drafts", n)
	}
}

func TestCreateDraft_RequiresSource(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	agent := f.agentFor(f.alice)

	for name, src := range map[string]*chatv1.Source{
		"no source":       nil,
		"no kind":         {Id: uuid.NewString(), Anchor: "1"},
		"no id":           {Kind: chatv1.SourceKind_SOURCE_KIND_MEETING, Anchor: "1"},
		"id not a UUID":   {Kind: chatv1.SourceKind_SOURCE_KIND_MEETING, Id: "m-1", Anchor: "1"},
		"nil UUID":        {Kind: chatv1.SourceKind_SOURCE_KIND_FILE, Id: uuid.Nil.String(), Anchor: "page:1"},
		"unknown kind":    {Kind: chatv1.SourceKind(99), Id: uuid.NewString()},
		"NUL in the text": {Kind: chatv1.SourceKind_SOURCE_KIND_MEETING, Id: uuid.NewString(), Label: "a\x00b"},
	} {
		_, err := f.svc.CreateDraft(t.Context(), agent, &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: "x", Source: src})
		wantErr(t, err, errs.ErrInvalid, "CreateDraft with "+name)
	}
	if n := f.taskCount(); n != 0 {
		t.Fatalf("tasks stored = %d after refused drafts, want 0", n)
	}
	// A refused draft consumes no REF.
	if got := f.draft(ch, "First"); got.GetRef() != "TSK-1001" {
		t.Fatalf("first draft REF = %s, want TSK-1001", got.GetRef())
	}
}

func TestAssignee_MustBeChannelMember(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	other := f.channel("public", f.alice, f.carol)
	alice := f.user(f.alice)

	_, err := f.svc.CreateTask(t.Context(), alice, &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: "x", AssigneeId: proto.String(f.carol.String())})
	wantErr(t, err, errs.ErrInvalid, "CreateTask for a user outside the channel")
	_, err = f.svc.CreateTask(t.Context(), alice, &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: "x", AssigneeId: proto.String(uuid.NewString())})
	wantErr(t, err, errs.ErrInvalid, "CreateTask for a user who does not exist")
	_, err = f.svc.CreateTask(t.Context(), alice, &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: "x", AssigneeId: proto.String("bob")})
	wantErr(t, err, errs.ErrInvalid, "CreateTask with an assignee that is not a UUID")
	_, err = f.svc.CreateDraft(t.Context(), f.agentFor(f.alice), &tasksv1.CreateTaskRequest{
		ChannelId: ch.String(), Title: "x", Source: meetingSource(ch), AssigneeId: proto.String(f.carol.String())})
	wantErr(t, err, errs.ErrInvalid, "CreateDraft for a user outside the channel")

	task := f.create(alice, ch, "Mine", withAssignee(f.bob))
	if task.GetAssigneeId() != f.bob.String() {
		t.Fatalf("assignee = %q, want Bob", task.GetAssigneeId())
	}
	_, err = f.svc.UpdateTask(t.Context(), alice, &tasksv1.UpdateTaskRequest{Id: task.GetId(), AssigneeId: proto.String(f.carol.String())})
	wantErr(t, err, errs.ErrInvalid, "UpdateTask to a user outside the channel")
	// Carol belongs to the other channel, which is not this task's.
	if got := f.create(alice, other, "Hers", withAssignee(f.carol)); got.GetAssigneeId() != f.carol.String() {
		t.Fatalf("assignee in her own channel = %q, want Carol", got.GetAssigneeId())
	}
	if n := f.taskCount(); n != 2 {
		t.Fatalf("tasks stored = %d, want the two valid ones", n)
	}
}

func TestCreateDraftTx_RollsBackWithCaller(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	agent := f.agentFor(f.alice)
	req := func() *tasksv1.CreateTaskRequest {
		return &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: "From a pipeline", Source: meetingSource(ch)}
	}
	errAbort := errors.New("pipeline failed after drafting")

	err := f.db.InWorkspace(t.Context(), f.ws, func(ctx context.Context, tx pgx.Tx) error {
		draft, err := f.svc.CreateDraftTx(ctx, tx, agent, req())
		if err != nil {
			return err
		}
		if draft.GetRef() != "TSK-1001" {
			t.Errorf("draft REF = %s, want TSK-1001", draft.GetRef())
		}
		return errAbort
	})
	wantErr(t, err, errAbort, "the caller's transaction")
	if n := f.taskCount(); n != 0 {
		t.Fatalf("tasks stored = %d after the caller rolled back, want 0", n)
	}
	if evs := f.taskEvents(); len(evs) != 0 {
		t.Fatalf("task.changed rows = %d after the rollback, want 0", len(evs))
	}

	// The rolled-back transaction consumed no REF; a committed one stores draft and event together.
	err = f.db.InWorkspace(t.Context(), f.ws, func(ctx context.Context, tx pgx.Tx) error {
		draft, err := f.svc.CreateDraftTx(ctx, tx, agent, req())
		if err == nil && draft.GetRef() != "TSK-1001" {
			err = fmt.Errorf("draft REF = %s, want TSK-1001: the rollback leaked a number", draft.GetRef())
		}
		return err
	})
	if err != nil {
		t.Fatalf("committed CreateDraftTx: %v", err)
	}
	if got := f.list(f.user(f.alice), ch); len(got) != 1 || got[0].GetState() != tasksv1.TaskState_TASK_STATE_DRAFT {
		t.Fatalf("tasks = %v, want the committed draft", got)
	}
	if evs := f.taskEvents(); len(evs) != 1 || evs[0].GetTask().GetState() != tasksv1.TaskState_TASK_STATE_DRAFT {
		t.Fatalf("task.changed rows = %v, want one for the draft", evs)
	}
	// CreateDraftTx authorizes like CreateDraft.
	err = f.db.InWorkspace(t.Context(), f.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.CreateDraftTx(ctx, tx, f.agentFor(f.carol), req())
		return err
	})
	wantErr(t, err, errs.ErrForbidden, "CreateDraftTx for a non-member")
}

func TestCreateTask_FromMessageSource_LinksBackToSeq(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	src := messageSource(ch, 7)

	task := f.create(f.user(f.alice), ch, "Answer the customer", withSource(src))
	if task.GetState() != tasksv1.TaskState_TASK_STATE_CONFIRMED || !proto.Equal(task.GetSource(), src) {
		t.Fatalf("task = %v, want CONFIRMED with source %v", task, src)
	}

	var kind, anchor string
	var id uuid.UUID
	f.scalar(&kind, `SELECT source_kind FROM tasks.tasks WHERE id = $1`, task.GetId())
	f.scalar(&anchor, `SELECT source_anchor FROM tasks.tasks WHERE id = $1`, task.GetId())
	f.scalar(&id, `SELECT source_id FROM tasks.tasks WHERE id = $1`, task.GetId())
	if kind != "message" || anchor != "7" || id.String() != src.GetId() {
		t.Fatalf("stored source = (%s, %s, %s), want (message, 7, %s)", kind, id, anchor, src.GetId())
	}

	got := f.list(f.user(f.alice), ch)
	if len(got) != 1 || !proto.Equal(got[0], task) {
		t.Fatalf("ListTasks = %v, want the created task round-tripped: %v", got, task)
	}
}

func TestCreateTask_DefaultStateIsConfirmedAndSourceIsOptional(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)

	unset := f.create(f.user(f.alice), ch, "No state given")
	explicit := f.create(f.user(f.alice), ch, "Explicit", func(r *tasksv1.CreateTaskRequest) {
		r.State = tasksv1.TaskState_TASK_STATE_CONFIRMED.Enum()
	})
	for _, task := range []*tasksv1.Task{unset, explicit} {
		if task.GetState() != tasksv1.TaskState_TASK_STATE_CONFIRMED || task.GetStatus() != tasksv1.TaskStatus_TASK_STATUS_TODO || task.GetSource() != nil {
			t.Fatalf("task = %v, want CONFIRMED, TODO and no source", task)
		}
	}
	if unset.GetRef() != "TSK-1001" || explicit.GetRef() != "TSK-1002" {
		t.Fatalf("REFs = %s, %s, want TSK-1001, TSK-1002", unset.GetRef(), explicit.GetRef())
	}
	// New cards land at the end of the To do lane.
	if got := titles(f.list(f.user(f.alice), ch)); !slices.Equal(got, []string{"No state given", "Explicit"}) {
		t.Fatalf("order = %v, want creation order", got)
	}
}

func TestCreateTask_HandMade_NoSource_IsConfirmedUserTaskWithCreatedAt(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	before := time.Now().Add(-time.Minute)

	task := f.create(f.user(f.alice), ch, "  Buy milk  ", func(r *tasksv1.CreateTaskRequest) { r.Description = proto.String("2 litres") })
	if task.GetRef() != "TSK-1001" || task.GetTitle() != "Buy milk" || task.GetDescription() != "2 litres" ||
		task.GetState() != tasksv1.TaskState_TASK_STATE_CONFIRMED || task.GetSource() != nil || task.FromRef != nil ||
		task.GetCreatedByKind() != tasksv1.CreatedByKind_CREATED_BY_KIND_USER || task.GetCreatedById() != f.alice.String() {
		t.Fatalf("hand-made task = %v", task)
	}
	if created := task.GetCreatedAt().AsTime(); created.Before(before) || created.After(time.Now().Add(time.Minute)) {
		t.Fatalf("created_at = %v, want about now", created)
	}

	_, err := f.svc.CreateTask(t.Context(), f.user(f.alice), &tasksv1.CreateTaskRequest{
		ChannelId: ch.String(), Title: "Needs a source", State: tasksv1.TaskState_TASK_STATE_DRAFT.Enum(),
	})
	wantErr(t, err, errs.ErrInvalid, "a hand-made draft")
	_, err = f.svc.CreateTask(t.Context(), f.user(f.alice), &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: " "})
	wantErr(t, err, errs.ErrInvalid, "a task with a blank title")
}

func TestCreateTask_Concurrent_GetDistinctPositionsAndRefs(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)

	const n = 16
	var wg sync.WaitGroup
	created := make([]*tasksv1.Task, n)
	failed := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			created[i], failed[i] = f.svc.CreateTask(t.Context(), f.user(f.alice), &tasksv1.CreateTaskRequest{
				ChannelId: ch.String(), Title: fmt.Sprintf("task %d", i),
			})
		}()
	}
	close(start)
	wg.Wait()

	positions, refs := map[string]bool{}, map[string]bool{}
	for i, task := range created {
		if failed[i] != nil {
			t.Fatalf("CreateTask %d: %v", i, failed[i])
		}
		positions[task.GetPosition()], refs[task.GetRef()] = true, true
	}
	if len(positions) != n || len(refs) != n {
		t.Fatalf("%d concurrent creations got %d distinct positions and %d distinct REFs, want %d of each: cards would tie and a move between them could not be placed",
			n, len(positions), len(refs), n)
	}
}

func TestCreateTask_DraftStateRequiresSource_AndCreatesADraftByTheUser(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	src := messageSource(ch, 3)

	draft := f.create(f.user(f.alice), ch, "Maybe later", withSource(src), func(r *tasksv1.CreateTaskRequest) {
		r.State = tasksv1.TaskState_TASK_STATE_DRAFT.Enum()
	})
	if draft.GetState() != tasksv1.TaskState_TASK_STATE_DRAFT || draft.GetCreatedByKind() != tasksv1.CreatedByKind_CREATED_BY_KIND_USER ||
		draft.GetCreatedById() != f.alice.String() || !proto.Equal(draft.GetSource(), src) {
		t.Fatalf("draft = %v, want a DRAFT created by Alice from %v", draft, src)
	}
	if got := f.list(f.user(f.alice), ch, tasksv1.TaskState_TASK_STATE_DRAFT); len(got) != 1 || got[0].GetId() != draft.GetId() {
		t.Fatalf("drafts = %v, want the one just made", got)
	}
}

func TestCreateTask_FromAnswer_StoresFromRefAndSourceSnapshot_RoundTripsThroughListTasks(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	src := &chatv1.Source{
		Kind: chatv1.SourceKind_SOURCE_KIND_MEETING, Id: uuid.NewString(), Anchor: "12", Label: "Sprint review",
		ChannelId: ch.String(), OffsetMs: proto.Int64(754000), Quote: proto.String("We ship the pricing page on Friday."),
	}
	file := &chatv1.Source{
		Kind: chatv1.SourceKind_SOURCE_KIND_FILE, Id: uuid.NewString(), Anchor: "page:4", Label: "สัญญา.pdf", Page: proto.Int32(4),
	}

	task := f.create(f.user(f.alice), ch, "Ship pricing", withSource(src), func(r *tasksv1.CreateTaskRequest) { r.FromRef = proto.String("ANS-2209") })
	fromFile := f.create(f.user(f.alice), ch, "Read the contract", withSource(file))

	listed := f.list(f.user(f.alice), ch)
	if len(listed) != 2 || !proto.Equal(listed[0], task) || !proto.Equal(listed[1], fromFile) {
		t.Fatalf("ListTasks = %v, want %v then %v", listed, task, fromFile)
	}
	got := listed[0]
	if got.GetFromRef() != "ANS-2209" || got.GetSource().GetLabel() != "Sprint review" || got.GetSource().GetOffsetMs() != 754000 ||
		got.GetSource().GetQuote() != "We ship the pricing page on Friday." || got.GetSource().GetChannelId() != ch.String() {
		t.Fatalf("task = %v, want from_ref ANS-2209 and the source snapshot", got)
	}
	// The channel of a source that names none is the task's own.
	if p := listed[1].GetSource(); p.GetChannelId() != ch.String() || p.GetPage() != 4 || p.GetLabel() != "สัญญา.pdf" || p.GetAnchor() != "page:4" {
		t.Fatalf("file source = %v, want page 4 of สัญญา.pdf on the task's channel", p)
	}
}

func TestCreateTask_RejectsMalformedFromRefOversizeQuoteAndForeignSourceChannel(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	otherCh := f.channel("public", f.alice)
	thai500 := strings.Repeat("ก", 500) // 1500 bytes, 500 runes: the limit counts runes

	srcWith := func(mod func(*chatv1.Source)) func(*tasksv1.CreateTaskRequest) {
		return func(r *tasksv1.CreateTaskRequest) {
			s := meetingSource(ch)
			mod(s)
			r.Source = s
		}
	}
	fromRef := func(ref string) func(*tasksv1.CreateTaskRequest) {
		return func(r *tasksv1.CreateTaskRequest) { r.FromRef = proto.String(ref) }
	}

	for name, mod := range map[string]func(*tasksv1.CreateTaskRequest){
		"from_ref without a number":   fromRef("ANS"),
		"from_ref lower case":         fromRef("ans-2209"),
		"from_ref too few digits":     fromRef("ANS-220"),
		"from_ref too many digits":    fromRef("ANS-22090"),
		"from_ref with trailing text": fromRef("ANS-2209 "),
		"quote of 501 runes":          srcWith(func(s *chatv1.Source) { s.Quote = proto.String(thai500 + "ก") }),
		"source of another channel":   srcWith(func(s *chatv1.Source) { s.ChannelId = otherCh.String() }),
		"source channel not a UUID":   srcWith(func(s *chatv1.Source) { s.ChannelId = "general" }),
		"at on a meeting source":      srcWith(func(s *chatv1.Source) { s.At = timestamppb.Now() }),
	} {
		req := &tasksv1.CreateTaskRequest{ChannelId: ch.String(), Title: "x"}
		mod(req)
		_, err := f.svc.CreateTask(t.Context(), f.user(f.alice), req)
		wantErr(t, err, errs.ErrInvalid, "CreateTask with "+name)
	}
	if n := f.taskCount(); n != 0 {
		t.Fatalf("tasks stored = %d after refused requests, want 0", n)
	}

	ok := f.create(f.user(f.alice), ch, "Boundary", fromRef("ANS-2209"), srcWith(func(s *chatv1.Source) { s.Quote = proto.String(thai500) }))
	if got := ok.GetSource().GetQuote(); got != thai500 || ok.GetFromRef() != "ANS-2209" {
		t.Fatalf("500-rune Thai quote did not round-trip: %d bytes, from_ref %q", len(got), ok.GetFromRef())
	}
	// An empty from_ref is no from_ref.
	if none := f.create(f.user(f.alice), ch, "No answer", fromRef("")); none.FromRef != nil {
		t.Fatalf("from_ref = %q for an empty one, want unset", none.GetFromRef())
	}
}

// ---- list -------------------------------------------------------------------

func TestListTasks_WorkspaceWide_ReturnsOnlyMemberChannels(t *testing.T) {
	f := newFixture(t)
	ch1 := f.channel("public", f.alice, f.bob)
	ch2 := f.channel("public", f.bob)  // public, but Alice has not joined
	ch3 := f.channel("private", f.bob) // private
	f.create(f.user(f.bob), ch1, "in channel 1")
	f.create(f.user(f.bob), ch2, "in channel 2")
	f.create(f.user(f.bob), ch3, "in the private channel")

	got, err := f.svc.ListTasks(t.Context(), f.user(f.alice), ListTasksQuery{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if want := []string{"in channel 1"}; !slices.Equal(titles(got), want) {
		t.Fatalf("Alice's workspace-wide list = %v, want %v", titles(got), want)
	}

	// Bob is in all three; the list is ordered by channel.
	got, err = f.svc.ListTasks(t.Context(), f.user(f.bob), ListTasksQuery{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	channels := make([]string, len(got))
	for i, task := range got {
		channels[i] = task.GetChannelId()
	}
	if len(got) != 3 || !slices.IsSorted(channels) {
		t.Fatalf("Bob's list = %v on channels %v, want all three ordered by channel", titles(got), channels)
	}

	// The agent acts for Alice: the same bound.
	got, err = f.svc.ListTasks(t.Context(), f.agentFor(f.alice), ListTasksQuery{})
	if err != nil || len(got) != 1 {
		t.Fatalf("agent's workspace-wide list = %v, %v, want Alice's one task", titles(got), err)
	}

	// A user in no channel sees nothing.
	if got, err = f.svc.ListTasks(t.Context(), f.user(f.carol), ListTasksQuery{}); err != nil || len(got) != 0 {
		t.Fatalf("Carol's list = %v, %v, want none", titles(got), err)
	}
	// One channel's list needs membership of that channel.
	_, err = f.svc.ListTasks(t.Context(), f.user(f.alice), ListTasksQuery{ChannelID: &ch2})
	wantErr(t, err, errs.ErrForbidden, "ListTasks of a channel Alice is not in")
}

// ---- move -------------------------------------------------------------------

func TestMoveTask_SetsAndClearsDoneAt(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	task := f.create(f.user(f.alice), ch, "Ship it")
	move := func(status tasksv1.TaskStatus) *tasksv1.Task {
		t.Helper()
		got, err := f.svc.MoveTask(t.Context(), f.user(f.alice), &tasksv1.MoveTaskRequest{Id: task.GetId(), Status: status})
		if err != nil {
			t.Fatalf("MoveTask(%s): %v", status, err)
		}
		if got.GetStatus() != status {
			t.Fatalf("status = %s, want %s", got.GetStatus(), status)
		}
		return got
	}

	if got := move(tasksv1.TaskStatus_TASK_STATUS_DOING); got.GetDoneAt() != nil {
		t.Fatalf("done_at after doing = %v, want none", got.GetDoneAt())
	}
	done := move(tasksv1.TaskStatus_TASK_STATUS_DONE)
	if d := done.GetDoneAt().AsTime(); time.Since(d) > time.Minute || time.Until(d) > time.Minute {
		t.Fatalf("done_at = %v, want about now", d)
	}
	// Moving within Done reorders; it does not change when the task was finished.
	if again := move(tasksv1.TaskStatus_TASK_STATUS_DONE); !proto.Equal(again.GetDoneAt(), done.GetDoneAt()) {
		t.Fatalf("done_at changed from %v to %v on a move within Done", done.GetDoneAt(), again.GetDoneAt())
	}
	if got := move(tasksv1.TaskStatus_TASK_STATUS_TODO); got.GetDoneAt() != nil {
		t.Fatalf("done_at after reopening = %v, want it cleared", got.GetDoneAt())
	}
	if got := f.list(f.user(f.alice), ch); got[0].GetDoneAt() != nil || got[0].GetStatus() != tasksv1.TaskStatus_TASK_STATUS_TODO {
		t.Fatalf("stored task = %v, want TODO with no done_at", got[0])
	}
	if evs := f.taskEvents(); evs[len(evs)-1].GetTask().GetStatus() != tasksv1.TaskStatus_TASK_STATUS_TODO {
		t.Fatalf("last task.changed = %v, want the reopened task", evs[len(evs)-1])
	}
}

func TestMoveTask_PlacesTheCardBetweenItsNeighbours(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	alice := f.user(f.alice)
	a, b, c, d := f.create(alice, ch, "A"), f.create(alice, ch, "B"), f.create(alice, ch, "C"), f.create(alice, ch, "D")
	move := func(task *tasksv1.Task, status tasksv1.TaskStatus, before, after string) (*tasksv1.Task, error) {
		return f.svc.MoveTask(t.Context(), alice, &tasksv1.MoveTaskRequest{Id: task.GetId(), Status: status, BeforeId: before, AfterId: after})
	}
	todo := tasksv1.TaskStatus_TASK_STATUS_TODO

	// D between A and B: it comes after A (after_id) and before B (before_id).
	if _, err := move(d, todo, b.GetId(), a.GetId()); err != nil {
		t.Fatalf("move D between A and B: %v", err)
	}
	if got := titles(f.list(alice, ch)); !slices.Equal(got, []string{"A", "D", "B", "C"}) {
		t.Fatalf("order = %v, want A D B C", got)
	}
	// An open after_id puts it first; an open before_id puts it last.
	if _, err := move(c, todo, a.GetId(), ""); err != nil {
		t.Fatalf("move C to the top: %v", err)
	}
	if got := titles(f.list(alice, ch)); !slices.Equal(got, []string{"C", "A", "D", "B"}) {
		t.Fatalf("order = %v, want C A D B", got)
	}
	if _, err := move(c, todo, "", b.GetId()); err != nil {
		t.Fatalf("move C to the bottom: %v", err)
	}
	if got := titles(f.list(alice, ch)); !slices.Equal(got, []string{"A", "D", "B", "C"}) {
		t.Fatalf("order = %v, want A D B C", got)
	}

	// Into another status: its own ordering, the cards of To do are untouched.
	doing := tasksv1.TaskStatus_TASK_STATUS_DOING
	if got, err := move(a, doing, "", ""); err != nil || got.GetPosition() != "a0" {
		t.Fatalf("move A into the empty Doing lane = %v, %v, want position a0", got, err)
	}
	if got, err := move(b, doing, a.GetId(), ""); err != nil || got.GetPosition() >= "a0" {
		t.Fatalf("move B above A in Doing = %v, %v, want a position below a0", got, err)
	}
	// Both ends open on a lane that has cards appends.
	if got, err := move(d, doing, "", ""); err != nil || got.GetPosition() <= "a0" {
		t.Fatalf("move D to the end of Doing = %v, %v, want a position above a0", got, err)
	}
	var doingTitles []string
	for _, task := range f.list(alice, ch) {
		if task.GetStatus() == doing {
			doingTitles = append(doingTitles, task.GetTitle())
		}
	}
	if !slices.Equal(doingTitles, []string{"B", "A", "D"}) {
		t.Fatalf("Doing = %v, want B A D", doingTitles)
	}
}

func TestMoveTask_RejectsNeighboursThatAreNotPlacesInTheDestinationLane(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	other := f.channel("public", f.alice)
	alice := f.user(f.alice)
	a, b, c := f.create(alice, ch, "A"), f.create(alice, ch, "B"), f.create(alice, ch, "C")
	doing := f.create(alice, ch, "Doing")
	if _, err := f.svc.MoveTask(t.Context(), alice, &tasksv1.MoveTaskRequest{Id: doing.GetId(), Status: tasksv1.TaskStatus_TASK_STATUS_DOING}); err != nil {
		t.Fatal(err)
	}
	foreign := f.create(alice, other, "Elsewhere")
	todo := tasksv1.TaskStatus_TASK_STATUS_TODO

	for name, req := range map[string]*tasksv1.MoveTaskRequest{
		"unspecified status":        {Id: a.GetId()},
		"neighbours reversed":       {Id: c.GetId(), Status: todo, BeforeId: a.GetId(), AfterId: b.GetId()},
		"itself as a neighbour":     {Id: a.GetId(), Status: todo, BeforeId: a.GetId()},
		"unknown neighbour":         {Id: a.GetId(), Status: todo, BeforeId: uuid.NewString()},
		"neighbour not a UUID":      {Id: a.GetId(), Status: todo, AfterId: "b"},
		"neighbour of another lane": {Id: a.GetId(), Status: todo, BeforeId: doing.GetId()},
		"neighbour of a channel":    {Id: a.GetId(), Status: todo, AfterId: foreign.GetId()},
	} {
		_, err := f.svc.MoveTask(t.Context(), alice, req)
		wantErr(t, err, errs.ErrInvalid, "MoveTask with "+name)
	}
	_, err := f.svc.MoveTask(t.Context(), alice, &tasksv1.MoveTaskRequest{Id: uuid.NewString(), Status: todo})
	wantErr(t, err, errs.ErrNotFound, "MoveTask of an unknown task")
	if got := titles(f.list(alice, ch)); !slices.Equal(got, []string{"A", "B", "C", "Doing"}) {
		t.Fatalf("order = %v after refused moves, want A B C Doing", got)
	}
}

// ---- Connect ----------------------------------------------------------------

func TestConnect_RequiresTheProtocolHeader(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	actor := f.user(f.alice)

	// No option is passed: the handler adds the requirement itself.
	path, h := Handler(f.svc, func(context.Context) authz.Actor { return actor })
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// A browser can send this cross-site without a preflight: POST with a JSON body.
	call := func(version string) (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path+"CreateTask",
			strings.NewReader(fmt.Sprintf(`{"channelId":%q,"title":"over Connect %s"}`, ch, version)))
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
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	status, body := call("")
	if status != http.StatusBadRequest || !strings.Contains(body, "Connect-Protocol-Version") {
		t.Fatalf("POST without the header: %d %s; want 400 naming Connect-Protocol-Version", status, body)
	}
	if n := f.taskCount(); n != 0 {
		t.Fatalf("the refused call reached the service: %d tasks stored", n)
	}
	status, body = call("1")
	if status != http.StatusOK || !strings.Contains(body, `"over Connect 1"`) {
		t.Fatalf("POST with the header: %d %s; want 200 and the stored task", status, body)
	}

	// The generated client sends the header itself and reaches every method.
	client := tasksv1connect.NewTaskServiceClient(srv.Client(), srv.URL)
	created, err := client.CreateTask(t.Context(), connect.NewRequest(&tasksv1.CreateTaskRequest{
		ChannelId: ch.String(), Title: "via client", AssigneeId: proto.String(f.bob.String()),
	}))
	if err != nil || created.Msg.GetTask().GetTitle() != "via client" || created.Msg.GetTask().GetAssigneeId() != f.bob.String() {
		t.Fatalf("CreateTask = %v, %v", created, err)
	}
	draft := f.draft(ch, "proposed", withAssignee(f.bob))

	listed, err := client.ListTasks(t.Context(), connect.NewRequest(&tasksv1.ListTasksRequest{
		ChannelId: proto.String(ch.String()), States: []tasksv1.TaskState{tasksv1.TaskState_TASK_STATE_CONFIRMED},
	}))
	if err != nil || len(listed.Msg.GetTasks()) != 2 {
		t.Fatalf("ListTasks(confirmed) = %v, %v, want the two confirmed tasks", listed, err)
	}
	everything, err := client.ListTasks(t.Context(), connect.NewRequest(&tasksv1.ListTasksRequest{}))
	if err != nil || len(everything.Msg.GetTasks()) != 3 {
		t.Fatalf("ListTasks() = %v, %v, want all three tasks of Alice's channels", everything, err)
	}
	updated, err := client.UpdateTask(t.Context(), connect.NewRequest(&tasksv1.UpdateTaskRequest{Id: draft.GetId(), Title: proto.String("proposed, edited")}))
	if err != nil || updated.Msg.GetTask().GetTitle() != "proposed, edited" {
		t.Fatalf("UpdateTask = %v, %v", updated, err)
	}
	sealed, err := client.ConfirmDraft(t.Context(), connect.NewRequest(&tasksv1.ConfirmDraftRequest{Id: draft.GetId()}))
	if err != nil || sealed.Msg.GetTask().GetState() != tasksv1.TaskState_TASK_STATE_CONFIRMED {
		t.Fatalf("ConfirmDraft = %v, %v", sealed, err)
	}
	moved, err := client.MoveTask(t.Context(), connect.NewRequest(&tasksv1.MoveTaskRequest{
		Id: draft.GetId(), Status: tasksv1.TaskStatus_TASK_STATUS_DOING,
	}))
	if err != nil || moved.Msg.GetTask().GetStatus() != tasksv1.TaskStatus_TASK_STATUS_DOING {
		t.Fatalf("MoveTask = %v, %v", moved, err)
	}
	second := f.draft(ch, "to discard")
	if _, err := client.DiscardDraft(t.Context(), connect.NewRequest(&tasksv1.DiscardDraftRequest{Id: second.GetId()})); err != nil {
		t.Fatalf("DiscardDraft: %v", err)
	}
	if _, err := client.ConfirmDraft(t.Context(), connect.NewRequest(&tasksv1.ConfirmDraftRequest{Id: "not-a-uuid"})); err == nil {
		t.Fatal("ConfirmDraft with a malformed id succeeded")
	}
}
