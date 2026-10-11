package outbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/outbox"
	"kiwi/internal/testkit/dbtest"
)

const (
	// patience bounds every wait. A healthy run needs milliseconds; the bound only
	// turns a deadlock into a failure.
	patience = 30 * time.Second
	// poll is the relay's idle poll in tests.
	poll = 10 * time.Millisecond
	// neverRetry parks a failed row for an hour, so "the next pass" is something a
	// test makes due by hand instead of waiting for.
	neverRetry = time.Hour
)

// ---- fixture ----------------------------------------------------------------

type fixture struct {
	env   *dbtest.Env
	db    *db.DB
	owner *pgxpool.Pool // kiwi_owner: seeds and inspects what kiwi_app cannot read
	ws    uuid.UUID
	logs  *syncBuffer
}

// syncBuffer collects the logs of one test, so the output stays pristine and a
// test can assert on what the relay reported.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// start gives the test a migrated database, a seeded workspace, application and
// system pools, and a table the consumers write their effects into.
func start(t *testing.T) *fixture {
	t.Helper()

	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	env := dbtest.Start(t)
	ws, _ := dbtest.SeedTwoWorkspaces(t, env)
	owner, err := pgxpool.New(t.Context(), env.OwnerURL)
	if err != nil {
		t.Fatalf("owner pool: %v", err)
	}
	t.Cleanup(owner.Close)
	f := &fixture{env: env, owner: owner, ws: ws, logs: logs}
	f.db = f.openDB(t)

	// A tenant-agnostic effect table: the consumers under test write here from
	// inside Apply, in the transaction OnceFor opens.
	if _, err := owner.Exec(t.Context(), `
		CREATE TABLE public.outbox_effects (
			id           bigserial PRIMARY KEY,
			consumer     text NOT NULL,
			event_id     text NOT NULL,
			workspace_id uuid
		);
		GRANT SELECT, INSERT ON public.outbox_effects TO kiwi_app;
		GRANT USAGE ON SEQUENCE public.outbox_effects_id_seq TO kiwi_app;`); err != nil {
		t.Fatalf("create effects table: %v", err)
	}
	return f
}

// openDB opens another handle on the test database, as a second process would.
func (f *fixture) openDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.Context(), config.Config{DatabaseURL: f.env.AppURL, DatabaseSystemURL: f.env.SystemURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(d.Close)
	return d
}

// event is an outbox event for the fixture workspace.
func (f *fixture) event(topic, key string) outbox.Event {
	return outbox.Event{Topic: topic, Key: key, Payload: fmt.Appendf(nil, `{"workspace_id":%q,"key":%q}`, f.ws.String(), key)}
}

// enqueue commits the events through the application pool and returns their row
// ids in order. Tests enqueue sequentially, so the newest rows are theirs.
func (f *fixture) enqueue(t *testing.T, evs ...outbox.Event) []int64 {
	t.Helper()
	err := f.db.InWorkspace(t.Context(), f.ws, func(ctx context.Context, tx pgx.Tx) error {
		for _, ev := range evs {
			if err := outbox.Enqueue(ctx, tx, ev); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	rows, err := f.owner.Query(t.Context(),
		`SELECT id FROM (SELECT id FROM platform.outbox ORDER BY id DESC LIMIT $1) newest ORDER BY id`, len(evs))
	if err != nil {
		t.Fatalf("read ids: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(ids) != len(evs) {
		t.Fatalf("read ids: %v (got %d, want %d)", err, len(ids), len(evs))
	}
	return ids
}

type storedRow struct {
	Topic, Key   string
	Payload      []byte
	TraceContext string
	NotBefore    time.Time
	CreatedAt    time.Time
	Published    bool
}

func (f *fixture) row(t *testing.T, id int64) storedRow {
	t.Helper()
	var (
		r         storedRow
		published *time.Time
	)
	err := f.owner.QueryRow(t.Context(),
		`SELECT topic, key, payload, trace_context, not_before, created_at, published_at FROM platform.outbox WHERE id = $1`, id).
		Scan(&r.Topic, &r.Key, &r.Payload, &r.TraceContext, &r.NotBefore, &r.CreatedAt, &published)
	if err != nil {
		t.Fatalf("read outbox row %d: %v", id, err)
	}
	r.Published = published != nil
	return r
}

func (f *fixture) outboxCount(t *testing.T) int {
	t.Helper()
	return f.count(t, `SELECT count(*) FROM platform.outbox`)
}

func (f *fixture) publishedCount(t *testing.T) int {
	t.Helper()
	return f.count(t, `SELECT count(*) FROM platform.outbox WHERE published_at IS NOT NULL`)
}

func (f *fixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.owner.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (f *fixture) effects(t *testing.T, consumer, eventID string) int {
	t.Helper()
	return f.count(t, `SELECT count(*) FROM public.outbox_effects WHERE consumer = $1 AND event_id = $2`, consumer, eventID)
}

func (f *fixture) processed(t *testing.T, consumer, eventID string) int {
	t.Helper()
	return f.count(t, `SELECT count(*) FROM platform.processed_events WHERE consumer = $1 AND event_id = $2`, consumer, eventID)
}

// dbNow is the database clock, the one not_before is compared against.
func (f *fixture) dbNow(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := f.owner.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("database clock: %v", err)
	}
	return now
}

// record is an Apply that writes one effect row and counts its runs.
func (f *fixture) record(consumer, eventID string, runs *atomic.Int32) outbox.Apply {
	return func(ctx context.Context, tx pgx.Tx) error {
		runs.Add(1)
		_, err := tx.Exec(ctx, `
			INSERT INTO public.outbox_effects (consumer, event_id, workspace_id)
			VALUES ($1, $2, NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)`, consumer, eventID)
		return err
	}
}

// dueNow makes a parked row claimable.
func (f *fixture) dueNow(t *testing.T, id int64) {
	t.Helper()
	if _, err := f.owner.Exec(t.Context(), `UPDATE platform.outbox SET not_before = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("make row %d due: %v", id, err)
	}
}

// ---- test doubles and waiting ----------------------------------------------

type pubFunc func(ctx context.Context, m bus.Message) error

func (p pubFunc) Publish(ctx context.Context, m bus.Message) error { return p(ctx, m) }

type fanFunc func(ctx context.Context, topic string, frame []byte) error

func (p fanFunc) SPublish(ctx context.Context, topic string, frame []byte) error {
	return p(ctx, topic, frame)
}

type routerFunc func(outbox.Stored) ([]bus.Message, []outbox.Fanout, error)

func (r routerFunc) Route(ev outbox.Stored) ([]bus.Message, []outbox.Fanout, error) { return r(ev) }

var (
	noFan   = fanFunc(func(context.Context, string, []byte) error { return nil })
	noRoute = routerFunc(func(outbox.Stored) ([]bus.Message, []outbox.Fanout, error) { return nil, nil, nil })
)

func newBus() *bus.Memory {
	return bus.NewMemory(bus.MemoryOptions{Backoff: []time.Duration{0}})
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(patience):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// eventually polls cond for state that lands in the database after a signal the
// test already holds, such as a commit that follows Publish returning.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(patience)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// run starts the relay and returns stop, which cancels it and waits for Run to
// return. The test's cleanup calls stop too.
func run(t *testing.T, r *outbox.Relay) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Relay.Run: %v", err)
				}
			case <-time.After(patience):
				t.Error("Relay.Run did not return after its context was cancelled")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func jsonEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("not JSON: %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("not JSON: %s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

// ---- Enqueue ----------------------------------------------------------------

func TestEnqueue_RollsBackWithTx(t *testing.T) {
	f := start(t)
	errRollback := errors.New("the business write failed")

	err := f.db.InWorkspace(t.Context(), f.ws, func(ctx context.Context, tx pgx.Tx) error {
		if err := outbox.Enqueue(ctx, tx, f.event("job.rollback", "k")); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("InWorkspace = %v, want the callback's error", err)
	}
	if n := f.outboxCount(t); n != 0 {
		t.Fatalf("%d outbox rows after a rolled-back transaction, want 0", n)
	}
}

func TestEnqueue_StoresTheEvent(t *testing.T) {
	f := start(t)
	later := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	ev := f.event("job.store", "k")
	delayed := f.event("job.store", "delayed")
	delayed.NotBefore = later

	ids := f.enqueue(t, ev, delayed)

	now := f.row(t, ids[0])
	if now.Topic != "job.store" || now.Key != "k" {
		t.Errorf("stored topic/key = %q/%q, want job.store/k", now.Topic, now.Key)
	}
	if !jsonEqual(t, now.Payload, ev.Payload) {
		t.Errorf("stored payload = %s, want %s", now.Payload, ev.Payload)
	}
	if now.TraceContext != "" {
		t.Errorf("trace_context = %q, want empty (no trace context locally)", now.TraceContext)
	}
	if now.Published {
		t.Error("a new row is already published")
	}
	if !now.NotBefore.Equal(now.CreatedAt) {
		t.Errorf("zero NotBefore stored as %v, want now (created_at %v)", now.NotBefore, now.CreatedAt)
	}
	if got := f.row(t, ids[1]).NotBefore; !got.Equal(later) {
		t.Errorf("NotBefore stored as %v, want %v", got, later)
	}
}

func TestEnqueue_RejectsEventsTheRelayCouldNotRoute(t *testing.T) {
	f := start(t)
	good := f.event("job.invalid", "k")
	cases := map[string]outbox.Event{
		"empty topic":              {Payload: good.Payload},
		"payload is not JSON":      {Topic: good.Topic, Payload: []byte("not json")},
		"payload is not an object": {Topic: good.Topic, Payload: []byte(`"x"`)},
		"no workspace_id":          {Topic: good.Topic, Payload: []byte(`{"key":"k"}`)},
		"workspace_id not a uuid":  {Topic: good.Topic, Payload: []byte(`{"workspace_id":"w-1"}`)},
		"nil workspace_id":         {Topic: good.Topic, Payload: []byte(`{"workspace_id":"00000000-0000-0000-0000-000000000000"}`)},
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			err := f.db.InWorkspace(t.Context(), f.ws, func(ctx context.Context, tx pgx.Tx) error {
				return outbox.Enqueue(ctx, tx, ev)
			})
			if !errors.Is(err, errs.ErrInvalid) {
				t.Fatalf("Enqueue = %v, want errs.ErrInvalid", err)
			}
		})
	}
	if n := f.outboxCount(t); n != 0 {
		t.Fatalf("%d outbox rows after rejected events, want 0", n)
	}
}

// ---- Relay ------------------------------------------------------------------

func TestRelay_DefaultOptionsPublish(t *testing.T) {
	f := start(t)
	id := f.enqueue(t, f.event("job.defaults", "k"))[0]
	published := make(chan bus.Message, 1)
	pub := pubFunc(func(_ context.Context, m bus.Message) error { published <- m; return nil })

	run(t, outbox.NewRelay(f.db, pub, noFan, noRoute, outbox.RelayOptions{}))

	if m := recv(t, published, "the publish"); m.ID != strconv.FormatInt(id, 10) {
		t.Fatalf("published id %q, want %d", m.ID, id)
	}
	eventually(t, "the row to be marked published", func() bool { return f.row(t, id).Published })
}

func TestRelay_PublishFailureMovesNotBeforeAndRetries(t *testing.T) {
	f := start(t)
	id := f.enqueue(t, f.event("job.retry", "k"))[0]
	before := f.dbNow(t)

	var calls atomic.Int32
	published := make(chan bus.Message, 4)
	pub := pubFunc(func(_ context.Context, m bus.Message) error {
		published <- m
		if calls.Add(1) == 1 {
			return errors.New("bus is down")
		}
		return nil
	})
	run(t, outbox.NewRelay(f.db, pub, noFan, noRoute, outbox.RelayOptions{Poll: poll, RetryAfter: neverRetry}))

	first := recv(t, published, "the first publish")
	eventually(t, "not_before to move past RetryAfter", func() bool {
		return !f.row(t, id).NotBefore.Before(before.Add(neverRetry))
	})
	if f.row(t, id).Published {
		t.Fatal("a row whose publish failed was marked published")
	}
	if !strings.Contains(f.logs.String(), "level=ERROR") {
		t.Errorf("the failed publish was not logged at ERROR: %q", f.logs.String())
	}

	f.dueNow(t, id)
	second := recv(t, published, "the retry")
	if second.ID != first.ID {
		t.Fatalf("retry published %q, want the same message id %q", second.ID, first.ID)
	}
	eventually(t, "the retried row to be marked published", func() bool { return f.row(t, id).Published })
	if n := calls.Load(); n != 2 {
		t.Fatalf("Publish ran %d times, want 2 (one failure, one retry)", n)
	}
}

func TestRelay_RouterAndFanoutFailuresAlsoMoveNotBefore(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]struct {
		router routerFunc
		fan    fanFunc
	}{
		"router error": {
			router: func(outbox.Stored) ([]bus.Message, []outbox.Fanout, error) { return nil, nil, boom },
			fan:    noFan,
		},
		"fan-out error": {
			router: func(outbox.Stored) ([]bus.Message, []outbox.Fanout, error) {
				return nil, []outbox.Fanout{{Topic: "ch:1", Frame: []byte("f")}}, nil
			},
			fan: func(context.Context, string, []byte) error { return boom },
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := start(t)
			id := f.enqueue(t, f.event("job.fail", "k"))[0]
			before := f.dbNow(t)
			var publishes atomic.Int32
			pub := pubFunc(func(context.Context, bus.Message) error { publishes.Add(1); return nil })
			run(t, outbox.NewRelay(f.db, pub, c.fan, c.router, outbox.RelayOptions{Poll: poll, RetryAfter: neverRetry}))

			eventually(t, "not_before to move past RetryAfter", func() bool {
				return !f.row(t, id).NotBefore.Before(before.Add(neverRetry))
			})
			if f.row(t, id).Published {
				t.Error("a row whose routing failed was marked published")
			}
			if n := publishes.Load(); n != 0 {
				t.Errorf("the bus message was published %d times despite the failure, want 0", n)
			}
		})
	}
}

func TestRelay_RouterExtrasAndFanoutPublished(t *testing.T) {
	f := start(t)
	ev := f.event("chat.message.created", "msg-1")
	id := f.enqueue(t, ev)[0]

	var (
		mu     sync.Mutex
		order  []string
		msgs   []bus.Message
		fanned = map[string][]byte{}
		seen   outbox.Stored
	)
	router := routerFunc(func(st outbox.Stored) ([]bus.Message, []outbox.Fanout, error) {
		mu.Lock()
		seen = st
		mu.Unlock()
		extras := []bus.Message{
			{
				ID: "run-1", Topic: "agent.invoke", Data: []byte(`{"run":1}`),
				Attrs: map[string]string{"topic": "agent.invoke", "workspace_id": st.WorkspaceID.String(), "origin": "router"},
			},
			{Topic: "agent.invoke", Data: []byte(`{"run":2}`)},
		}
		fan := []outbox.Fanout{{Topic: "ch:one", Frame: []byte("frame-1")}, {Topic: "u:two", Frame: []byte("frame-2")}}
		return extras, fan, nil
	})
	fp := fanFunc(func(_ context.Context, topic string, frame []byte) error {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "spublish "+topic)
		fanned[topic] = frame
		return nil
	})
	pub := pubFunc(func(_ context.Context, m bus.Message) error {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "publish "+m.Topic+" "+m.ID)
		msgs = append(msgs, m)
		return nil
	})
	run(t, outbox.NewRelay(f.db, pub, fp, router, outbox.RelayOptions{Poll: poll}))
	eventually(t, "the row to be marked published", func() bool { return f.row(t, id).Published })

	mu.Lock()
	defer mu.Unlock()
	ws := f.ws.String()
	wantOrder := []string{
		"spublish ch:one", "spublish u:two",
		"publish chat.message.created " + strconv.FormatInt(id, 10),
		"publish agent.invoke run-1",
		"publish agent.invoke " + strconv.FormatInt(id, 10) + "/1",
	}
	if !slices.Equal(order, wantOrder) {
		t.Errorf("publish order = %q, want %q", order, wantOrder)
	}
	if string(fanned["ch:one"]) != "frame-1" || string(fanned["u:two"]) != "frame-2" {
		t.Errorf("fan-out frames = %q", fanned)
	}

	if seen.ID != id || seen.Topic != ev.Topic || seen.Key != "msg-1" || seen.WorkspaceID != f.ws || seen.NotBefore.IsZero() {
		t.Errorf("Router saw %+v, want id %d, topic %q, key msg-1, workspace %s and a not_before", seen, id, ev.Topic, ws)
	}
	if !jsonEqual(t, seen.Payload, ev.Payload) {
		t.Errorf("Router saw payload %s, want %s", seen.Payload, ev.Payload)
	}

	if len(msgs) != 3 {
		t.Fatalf("published %d bus messages, want 3 (the event and two extras)", len(msgs))
	}
	main := msgs[0]
	if main.ID != strconv.FormatInt(id, 10) || main.Topic != ev.Topic || main.Key != "msg-1" || !jsonEqual(t, main.Data, ev.Payload) {
		t.Errorf("event message = %+v", main)
	}
	if want := map[string]string{"topic": ev.Topic, "workspace_id": ws}; !maps.Equal(main.Attrs, want) {
		t.Errorf("event attrs = %v, want %v", main.Attrs, want)
	}
	if want := map[string]string{"topic": "agent.invoke", "workspace_id": ws, "origin": "router"}; !maps.Equal(msgs[1].Attrs, want) {
		t.Errorf("a complete extra's attrs = %v, want them untouched: %v", msgs[1].Attrs, want)
	}
	if want := map[string]string{"topic": "agent.invoke", "workspace_id": ws}; !maps.Equal(msgs[2].Attrs, want) {
		t.Errorf("a bare extra's attrs = %v, want the relay to add %v", msgs[2].Attrs, want)
	}
}

func TestRelay_RespectsNotBefore(t *testing.T) {
	f := start(t)
	future := f.event("job.later", "future")
	future.NotBefore = time.Now().Add(neverRetry)
	past := f.event("job.later", "past")
	past.NotBefore = time.Now().Add(-time.Minute)
	ids := f.enqueue(t, future, past) // the future row has the lower id: the claim sees it first and must pass it over

	published := make(chan bus.Message, 4)
	pub := pubFunc(func(_ context.Context, m bus.Message) error { published <- m; return nil })
	run(t, outbox.NewRelay(f.db, pub, noFan, noRoute, outbox.RelayOptions{Poll: poll}))

	if m := recv(t, published, "the due row"); m.Key != "past" {
		t.Fatalf("published key %q, want the row whose not_before has passed", m.Key)
	}
	eventually(t, "the due row to be marked published", func() bool { return f.row(t, ids[1]).Published })
	if f.row(t, ids[0]).Published || len(published) != 0 {
		t.Fatal("the relay published a row before its not_before")
	}

	f.dueNow(t, ids[0])
	if m := recv(t, published, "the row once due"); m.Key != "future" {
		t.Fatalf("published key %q, want the row that became due", m.Key)
	}
}

func TestRelay_TwoRelaysPublishEachRowOnce(t *testing.T) {
	f := start(t)
	const rows, relays, perRelay = 200, 2, 2

	evs := make([]outbox.Event, rows)
	for i := range evs {
		evs[i] = f.event("job.many", strconv.Itoa(i))
	}
	ids := f.enqueue(t, evs...)

	// The first relays*perRelay publishes wait for each other: every claimer must hold a
	// different row at the same time, which SKIP LOCKED allows and a plain FOR UPDATE
	// would turn into a wait on the first holder.
	const gate = relays * perRelay
	var (
		arrived atomic.Int32
		open    = make(chan struct{})
		mu      sync.Mutex
		got     = map[string]int{}
	)
	pub := pubFunc(func(_ context.Context, m bus.Message) error {
		if n := arrived.Add(1); n == gate {
			close(open)
		} else if n < gate {
			select {
			case <-open:
			case <-time.After(patience):
				return errors.New("the concurrent claims never overlapped")
			}
		}
		mu.Lock()
		got[m.ID]++
		mu.Unlock()
		return nil
	})
	for range relays {
		run(t, outbox.NewRelay(f.openDB(t), pub, noFan, noRoute, outbox.RelayOptions{Concurrency: perRelay, Poll: poll}))
	}

	eventually(t, "every row to be marked published", func() bool { return f.publishedCount(t) == rows })

	mu.Lock()
	defer mu.Unlock()
	for _, id := range ids {
		if n := got[strconv.FormatInt(id, 10)]; n != 1 {
			t.Errorf("row %d was published %d times, want once", id, n)
		}
	}
	if len(got) != rows {
		t.Errorf("published %d distinct messages, want %d", len(got), rows)
	}
}

func TestRelay_SlowHandlerDoesNotBlockOtherRows(t *testing.T) {
	f := start(t)
	b := newBus()
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	b.Register("slow.consumer", []string{"job.mixed"}, func(ctx context.Context, m bus.Message) error {
		if m.Key != "slow" {
			return nil
		}
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	ids := f.enqueue(t, f.event("job.mixed", "slow"), f.event("job.mixed", "fast"))

	run(t, outbox.NewRelay(f.db, b, noFan, noRoute, outbox.RelayOptions{Concurrency: 2, Poll: poll}))
	t.Cleanup(letGo) // registered after run, so it runs before stop waits for Run

	recv(t, started, "the slow consumer to start")
	eventually(t, "the fast row to be published while the slow one runs", func() bool { return f.row(t, ids[1]).Published })
	if f.row(t, ids[0]).Published {
		t.Fatal("the slow row was marked published while its consumer was still running")
	}

	letGo()
	eventually(t, "the slow row to be marked once its consumer returns", func() bool { return f.row(t, ids[0]).Published })
}

func TestRelay_CrashMidHandler_RowStaysUnpublishedAndIsRedelivered(t *testing.T) {
	f := start(t)
	const consumer = "crash.consumer"
	id := f.enqueue(t, f.event("job.crash", "k"))[0]
	msgID := strconv.FormatInt(id, 10)
	untouched := f.row(t, id)

	var deliveries, applies atomic.Int32
	once := outbox.Once(f.db, consumer, func(_ context.Context, m bus.Message) (outbox.Apply, error) {
		return f.record(consumer, m.ID, &applies), nil
	})
	applied := make(chan struct{})
	handler := func(ctx context.Context, m bus.Message) error {
		first := deliveries.Add(1) == 1
		if err := once(ctx, m); err != nil {
			return err
		}
		if first {
			close(applied)
			<-ctx.Done() // the process dies here: the effect is committed, the job is not acked
			return ctx.Err()
		}
		return nil
	}

	b1 := newBus()
	b1.Register(consumer, []string{"job.crash"}, handler)
	stop := run(t, outbox.NewRelay(f.db, b1, noFan, noRoute, outbox.RelayOptions{Poll: poll}))
	recv(t, applied, "the consumer to apply its effect")
	stop() // the relay is cancelled while the consumer is mid-handler

	after := f.row(t, id)
	if after.Published {
		t.Fatal("the row was marked published although its consumer never finished")
	}
	if !after.NotBefore.Equal(untouched.NotBefore) {
		t.Errorf("not_before moved to %v on shutdown, want %v: a cancelled relay is not a publish failure", after.NotBefore, untouched.NotBefore)
	}
	if strings.Contains(f.logs.String(), "level=ERROR") {
		t.Errorf("a cancelled relay logged an error: %q", f.logs.String())
	}
	if got := f.effects(t, consumer, msgID); got != 1 {
		t.Fatalf("%d effects after the first delivery, want 1", got)
	}

	// The restarted process: new handles, a new bus, the same consumer.
	b2 := newBus()
	b2.Register(consumer, []string{"job.crash"}, handler)
	run(t, outbox.NewRelay(f.openDB(t), b2, noFan, noRoute, outbox.RelayOptions{Poll: poll}))
	eventually(t, "the redelivered row to be marked published", func() bool { return f.row(t, id).Published })

	if got := deliveries.Load(); got != 2 {
		t.Errorf("the consumer ran %d times, want 2 (the crashed delivery and the redelivery)", got)
	}
	if got := applies.Load(); got != 1 {
		t.Errorf("Apply ran %d times across the two deliveries, want exactly once", got)
	}
	if got := f.effects(t, consumer, msgID); got != 1 {
		t.Errorf("%d effect rows after the redelivery, want 1", got)
	}
}

// ---- OnceFor and Once -------------------------------------------------------

func TestOnceFor_DuplicateReturnsNilAndSkipsApply(t *testing.T) {
	f := start(t)
	var runs atomic.Int32
	deliver := func(consumer, eventID string) error {
		return outbox.OnceFor(t.Context(), f.db, consumer, eventID, f.ws, f.record(consumer, eventID, &runs))
	}

	if err := deliver("c1", "evt-1"); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := deliver("c1", "evt-1"); err != nil {
		t.Fatalf("duplicate delivery = %v, want nil", err)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("Apply ran %d times for a duplicate, want once", got)
	}
	if got := f.effects(t, "c1", "evt-1"); got != 1 {
		t.Fatalf("%d effect rows, want 1", got)
	}
	if got := f.processed(t, "c1", "evt-1"); got != 1 {
		t.Fatalf("%d processed_events rows, want 1", got)
	}

	// The key is (consumer, event): another consumer of the same event applies it too.
	if err := deliver("c2", "evt-1"); err != nil {
		t.Fatalf("second consumer: %v", err)
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("Apply ran %d times for two consumers of one event, want 2", got)
	}
}

func TestOnceFor_ApplyErrorRollsBackSoARetryRunsIt(t *testing.T) {
	f := start(t)
	boom := errors.New("effect failed")

	err := outbox.OnceFor(t.Context(), f.db, "c1", "evt-1", f.ws, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO public.outbox_effects (consumer, event_id) VALUES ('c1', 'evt-1')`); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("OnceFor = %v, want the Apply error", err)
	}
	if f.processed(t, "c1", "evt-1") != 0 || f.effects(t, "c1", "evt-1") != 0 {
		t.Fatal("a failed Apply left the processed_events marker or its effect behind")
	}

	var runs atomic.Int32
	if err := outbox.OnceFor(t.Context(), f.db, "c1", "evt-1", f.ws, f.record("c1", "evt-1", &runs)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if runs.Load() != 1 || f.effects(t, "c1", "evt-1") != 1 {
		t.Fatal("the retry after a failed Apply did not apply the effect")
	}
}

func TestOnce_ExactlyOnceEffects(t *testing.T) {
	f := start(t)
	const (
		consumer   = "once.consumer"
		deliveries = 5
	)
	msg := bus.Message{ID: "evt-1", Topic: "job.once", Attrs: map[string]string{"workspace_id": f.ws.String()}}

	// Every delivery waits in Prepare until all five are past the processed_events
	// pre-check, so none can see another's commit there: only the insert's ON CONFLICT
	// separates them.
	var prepared, applied atomic.Int32
	allPrepared := make(chan struct{})
	h := outbox.Once(f.db, consumer, func(_ context.Context, m bus.Message) (outbox.Apply, error) {
		if prepared.Add(1) == deliveries {
			close(allPrepared)
		}
		select {
		case <-allPrepared:
		case <-time.After(patience):
			return nil, errors.New("the deliveries never overlapped")
		}
		return f.record(consumer, m.ID, &applied), nil
	})

	results := make(chan error, deliveries)
	for range deliveries {
		go func() { results <- h(t.Context(), msg) }()
	}
	for range deliveries {
		if err := recv(t, results, "a delivery"); err != nil {
			t.Errorf("delivery: %v", err)
		}
	}

	if got := applied.Load(); got != 1 {
		t.Fatalf("Apply ran %d times for %d concurrent deliveries of one message, want once", got, deliveries)
	}
	if got := f.effects(t, consumer, msg.ID); got != 1 {
		t.Fatalf("%d effect rows, want 1", got)
	}
	if got := f.processed(t, consumer, msg.ID); got != 1 {
		t.Fatalf("%d processed_events rows, want 1", got)
	}
	var bound uuid.UUID
	if err := f.owner.QueryRow(t.Context(), `SELECT workspace_id FROM public.outbox_effects WHERE consumer = $1`, consumer).Scan(&bound); err != nil || bound != f.ws {
		t.Fatalf("Apply ran bound to workspace %v (%v), want the message's %s", bound, err, f.ws)
	}

	// A later delivery is settled by the pre-check: Prepare, which may be expensive, is not run.
	if err := h(t.Context(), msg); err != nil {
		t.Fatalf("late duplicate: %v", err)
	}
	if prepared.Load() != deliveries || applied.Load() != 1 {
		t.Fatalf("a processed message ran Prepare/Apply again (prepared %d, applied %d)", prepared.Load(), applied.Load())
	}
}

func TestOnce_FailuresRecordNothing(t *testing.T) {
	f := start(t)
	boom := errors.New("prepare failed")
	var prepares, applies atomic.Int32
	withWorkspace := func(ws string) bus.Message {
		return bus.Message{ID: "evt-1", Topic: "job.once", Attrs: map[string]string{"workspace_id": ws}}
	}
	cases := map[string]struct {
		prepare outbox.Prepare
		msg     bus.Message
		check   func(error) bool
	}{
		"Prepare fails": {
			prepare: func(context.Context, bus.Message) (outbox.Apply, error) { prepares.Add(1); return nil, boom },
			msg:     withWorkspace(f.ws.String()),
			check:   func(err error) bool { return errors.Is(err, boom) },
		},
		"no workspace_id attribute": {
			prepare: func(context.Context, bus.Message) (outbox.Apply, error) {
				prepares.Add(1)
				return f.record("c", "evt-1", &applies), nil
			},
			msg:   bus.Message{ID: "evt-1", Topic: "job.once"},
			check: func(err error) bool { return errors.Is(err, errs.ErrInvalid) },
		},
		"workspace_id is not a uuid": {
			prepare: func(context.Context, bus.Message) (outbox.Apply, error) {
				prepares.Add(1)
				return f.record("c", "evt-1", &applies), nil
			},
			msg:   withWorkspace("w-1"),
			check: func(err error) bool { return errors.Is(err, errs.ErrInvalid) },
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			prepares.Store(0)
			err := outbox.Once(f.db, "c", c.prepare)(t.Context(), c.msg)
			if err == nil || !c.check(err) {
				t.Fatalf("handler = %v", err)
			}
			if f.processed(t, "c", "evt-1") != 0 || applies.Load() != 0 {
				t.Fatal("a failed delivery recorded the event or ran Apply")
			}
			if name != "Prepare fails" && prepares.Load() != 0 {
				t.Fatal("Prepare ran for a message that cannot be bound to a workspace")
			}
		})
	}
}

func TestOnce_NilApplyRecordsTheEventWithoutEffect(t *testing.T) {
	f := start(t)
	var prepares atomic.Int32
	h := outbox.Once(f.db, "c", func(context.Context, bus.Message) (outbox.Apply, error) { prepares.Add(1); return nil, nil })
	msg := bus.Message{ID: "evt-1", Topic: "job.once", Attrs: map[string]string{"workspace_id": f.ws.String()}}

	for range 2 {
		if err := h(t.Context(), msg); err != nil {
			t.Fatalf("handler: %v", err)
		}
	}
	if f.processed(t, "c", "evt-1") != 1 || prepares.Load() != 1 {
		t.Fatalf("processed %d, prepared %d: a nil Apply must record the event so it is not prepared again", f.processed(t, "c", "evt-1"), prepares.Load())
	}
}
