package realtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	realtimev1 "kiwi/gen/kiwi/realtime/v1"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/events"
	"kiwi/internal/realtime/frames"
	"kiwi/internal/testkit/dbtest"
)

const valkeyImage = "valkey/valkey:8"

// One plain (not cluster-mode) Valkey serves the whole test package. Every test
// works on topics made from fresh UUIDs, so they do not see each other; nothing
// here runs in parallel, which is what lets one test kill pub/sub connections.
var (
	valkeyOnce sync.Once
	valkeyURL  string
	valkeyErr  error
	valkeyCtr  testcontainers.Container
)

func TestMain(m *testing.M) {
	code := m.Run()
	if valkeyCtr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = valkeyCtr.Terminate(ctx)
		cancel()
	}
	os.Exit(code)
}

func bootValkey() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ctr, err := testcontainers.Run(ctx, valkeyImage,
		testcontainers.WithLogger(log.New(io.Discard, "", 0)),
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithCmd("valkey-server", "--save", "", "--appendonly", "no"),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")),
	)
	if err != nil {
		return "", fmt.Errorf("start %s (is the Docker daemon reachable?): %w", valkeyImage, err)
	}
	valkeyCtr = ctr
	host, err := ctr.Host(ctx)
	if err != nil {
		return "", err
	}
	port, err := ctr.MappedPort(ctx, "6379/tcp")
	if err != nil {
		return "", err
	}
	return "redis://" + host + ":" + port.Port(), nil
}

// subscribers is how many local connections follow topic. Tests wait on it
// instead of sleeping, because a Subscribe frame has no acknowledgement.
func (g *Gateway) subscribers(topic string) int {
	g.hub.mu.RLock()
	defer g.hub.mu.RUnlock()
	return len(g.hub.topics[topic])
}

// captureLogs sends slog output to a buffer for the rest of the test, so the log
// lines a test provokes are asserted on, not printed. It returns what was logged.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	prev := slog.Default()
	buf := &syncBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

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

// eventually polls cond until it holds; it fails the test after 10 s. It waits
// for server state, it never asserts on elapsed time.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fixture is one gateway behind an httptest server, on the shared Valkey and a
// private migrated Postgres with one workspace.
type fixture struct {
	t     *testing.T
	ws    uuid.UUID
	env   *dbtest.Env
	db    *db.DB
	cache *cache.Cache
	rdb   *redis.Client // observes Valkey directly: PUBSUB SHARDNUMSUB, EXISTS, CLIENT KILL
	gw    *Gateway
	srv   *httptest.Server
}

// newFixture starts a gateway. membership nil means the cache-aside lookup over
// Postgres that chat.NewMembership will be: members:{channel} first, then
// channel_members inside the workspace.
func newFixture(t *testing.T, opt Options, membership func(*fixture) authz.Membership) *fixture {
	t.Helper()
	valkeyOnce.Do(func() { valkeyURL, valkeyErr = bootValkey() })
	if valkeyErr != nil {
		t.Fatalf("valkey: %v", valkeyErr)
	}

	f := &fixture{t: t, ws: uuid.Must(uuid.NewV7()), env: dbtest.Start(t)}
	var err error
	if f.db, err = db.Open(t.Context(), config.Config{DatabaseURL: f.env.AppURL}); err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(f.db.Close)
	if f.cache, err = cache.Open(t.Context(), valkeyURL, cache.Options{}); err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = f.cache.Close() })
	ro, err := redis.ParseURL(valkeyURL)
	if err != nil {
		t.Fatal(err)
	}
	f.rdb = redis.NewClient(ro)
	t.Cleanup(func() { _ = f.rdb.Close() })

	f.seed(`INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, 'ws', $2)`, f.ws, f.ws.String()+".example")

	var m authz.Membership = cacheAside{d: f.db, c: f.cache}
	if membership != nil {
		m = membership(f)
	}
	f.gw = New(Deps{Cache: f.cache, Membership: m}, opt)
	mux := http.NewServeMux()
	mux.Handle("/ws", f.gw)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	t.Cleanup(func() { // runs first
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		f.gw.Shutdown(ctx)
	})
	return f
}

// cacheAside is the membership lookup the gateway is wired with in production,
// written against the real tables so the tests exercise the real invalidation.
type cacheAside struct {
	d *db.DB
	c *cache.Cache
}

func (m cacheAside) IsMember(ctx context.Context, channelID, userID uuid.UUID) (bool, error) {
	ids, hit, err := m.c.Members(ctx, channelID)
	if err != nil {
		return false, err
	}
	if !hit {
		err = m.d.InWorkspace(ctx, authz.WorkspaceFrom(ctx), func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT user_id FROM chat.channel_members WHERE channel_id = $1`, channelID)
			if err != nil {
				return err
			}
			ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			return err
		})
		if err != nil {
			return false, err
		}
		if err := m.c.SetMembers(ctx, channelID, ids); err != nil {
			return false, err
		}
	}
	return slices.Contains(ids, userID), nil
}

// seed runs one statement as kiwi_owner inside the fixture's workspace; RLS binds
// the owner too, so the workspace is set first.
func (f *fixture) seed(sql string, args ...any) {
	f.t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, f.env.OwnerURL)
	if err != nil {
		f.t.Fatalf("connect as kiwi_owner: %v", err)
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
		f.t.Fatalf("seed %q: %v", sql, err)
	}
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) newUser() uuid.UUID {
	id := uuid.Must(uuid.NewV7())
	f.seed(`INSERT INTO identity.users (id, workspace_id, google_sub, email, name) VALUES ($1, $2, $3, $4, 'u')`,
		id, f.ws, "sub-"+id.String(), id.String()+"@example.com")
	return id
}

// newChannel creates a channel and adds members to it.
func (f *fixture) newChannel(members ...uuid.UUID) uuid.UUID {
	id := uuid.Must(uuid.NewV7())
	f.seed(`INSERT INTO chat.channels (id, workspace_id, name, kind) VALUES ($1, $2, $3, 'private')`, id, f.ws, id.String())
	for _, u := range members {
		f.seed(`INSERT INTO chat.channel_members (channel_id, user_id, workspace_id) VALUES ($1, $2, $3)`, id, u, f.ws)
	}
	return id
}

func (f *fixture) removeMember(channel, user uuid.UUID) {
	f.seed(`DELETE FROM chat.channel_members WHERE channel_id = $1 AND user_id = $2`, channel, user)
}

func (f *fixture) ticket(user uuid.UUID) string {
	f.t.Helper()
	tk, err := f.cache.IssueTicket(f.t.Context(), cache.TicketRecord{UserID: user, WorkspaceID: f.ws})
	if err != nil {
		f.t.Fatalf("IssueTicket: %v", err)
	}
	return tk
}

func (f *fixture) wsURL(ticket string) string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws?ticket=" + url.QueryEscape(ticket)
}

// dial opens a WebSocket with ticket and returns the HTTP handshake response too.
func (f *fixture) dial(ticket string) (*websocket.Conn, *http.Response, error) {
	f.t.Helper()
	return f.dialWith(ticket, "")
}

// dialWith is dial from a page of the given origin ("" sends no Origin header).
func (f *fixture) dialWith(ticket, origin string) (*websocket.Conn, *http.Response, error) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), 10*time.Second)
	defer cancel()
	var opts *websocket.DialOptions
	if origin != "" {
		opts = &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}}
	}
	c, resp, err := websocket.Dial(ctx, f.wsURL(ticket), opts)
	if err == nil {
		f.t.Cleanup(func() { _ = c.CloseNow() })
	}
	return c, resp, err
}

// client is one signed-in WebSocket connection of the test.
type client struct {
	t  *testing.T
	ws *websocket.Conn
}

// dialClient signs user in without waiting for the gateway to register the
// connection, for tests whose clock starts at the dial.
func (f *fixture) dialClient(user uuid.UUID) *client {
	f.t.Helper()
	c, _, err := f.dial(f.ticket(user))
	if err != nil {
		f.t.Fatalf("dial: %v", err)
	}
	return &client{t: f.t, ws: c}
}

// connect signs user in and waits until the gateway follows u:{user}.
func (f *fixture) connect(user uuid.UUID) *client {
	f.t.Helper()
	c := f.dialClient(user)
	eventually(f.t, "the connection to be registered", func() bool { return f.gw.subscribers(usr(user)) > 0 })
	return c
}

func (c *client) write(frame *realtimev1.ClientFrame) {
	c.t.Helper()
	b, err := proto.Marshal(frame)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(c.t.Context(), 10*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, b); err != nil {
		c.t.Fatalf("write frame: %v", err)
	}
}

func (c *client) subscribe(channels ...string) {
	c.t.Helper()
	c.write(&realtimev1.ClientFrame{Frame: &realtimev1.ClientFrame_Subscribe{
		Subscribe: &realtimev1.Subscribe{ChannelIds: channels}}})
}

func (c *client) ping() {
	c.t.Helper()
	c.write(&realtimev1.ClientFrame{Frame: &realtimev1.ClientFrame_Ping{Ping: &realtimev1.Ping{}}})
}

// next reads the next server frame. A frame that never comes ends the test after
// 10 s; absence is proven by a sentinel frame arriving first, never by waiting.
func (c *client) next() *realtimev1.ServerEvent {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), 10*time.Second)
	defer cancel()
	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		c.t.Fatalf("read frame: %v", err)
	}
	if typ != websocket.MessageBinary {
		c.t.Fatalf("frame type = %v, want binary", typ)
	}
	var ev realtimev1.ServerEvent
	if err := proto.Unmarshal(data, &ev); err != nil {
		c.t.Fatalf("frame is not a ServerEvent: %v", err)
	}
	return &ev
}

// nextSeq reads the next frame and requires it to be the message with seq.
func (c *client) nextSeq(seq int64) {
	c.t.Helper()
	ev := c.next()
	if got := ev.GetMessageCreated(); got == nil || got.GetSeq() != seq {
		c.t.Fatalf("next frame = %v, want message_created seq %d", ev, seq)
	}
}

// readErr reads until the connection ends and returns why, failing if it stays
// open for 10 s.
func (c *client) readErr() error {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), 10*time.Second)
	defer cancel()
	for {
		_, _, err := c.ws.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				c.t.Fatal("the connection is still open")
			}
			return err
		}
	}
}

// messageFrame is a message_created frame as the relay would publish it.
func messageFrame(channel uuid.UUID, seq int64, body string) []byte {
	b, err := proto.Marshal(&realtimev1.ServerEvent{Event: &realtimev1.ServerEvent_MessageCreated{
		MessageCreated: &chatv1.Message{Id: uuid.NewString(), ChannelId: channel.String(), Seq: seq, Body: body}}})
	if err != nil {
		panic(err)
	}
	return b
}

func (f *fixture) publish(topic string, frame []byte) {
	f.t.Helper()
	if err := f.cache.SPublish(f.t.Context(), topic, frame); err != nil {
		f.t.Fatalf("SPublish %s: %v", topic, err)
	}
}

// publishMembership fans a membership change out the way the relay does, through
// the real frame mapper.
func (f *fixture) publishMembership(channel, user uuid.UUID, change chatv1.MembershipChange) {
	f.t.Helper()
	payload, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&eventsv1.MembershipChanged{
		WorkspaceId: f.ws.String(), ChannelId: channel.String(), UserId: user.String(), Change: change})
	if err != nil {
		f.t.Fatal(err)
	}
	fan, err := frames.FrameFor(events.TopicMembershipChanged, payload)
	if err != nil {
		f.t.Fatalf("FrameFor: %v", err)
	}
	for _, x := range fan {
		f.publish(x.Topic, x.Frame)
	}
}

func (f *fixture) shardNumSub(topic string) int64 {
	f.t.Helper()
	m, err := f.rdb.PubSubShardNumSub(f.t.Context(), topic).Result()
	if err != nil {
		f.t.Fatalf("PUBSUB SHARDNUMSUB %s: %v", topic, err)
	}
	return m[topic]
}

// live waits until n local connections follow topic and Valkey has confirmed the
// gateway's SSUBSCRIBE: cache.Subscription.Add returns when the command is sent,
// so a frame published earlier could be lost.
func (f *fixture) live(topic string, n int) {
	f.t.Helper()
	eventually(f.t, fmt.Sprintf("%d local subscribers of %s", n, topic), func() bool { return f.gw.subscribers(topic) == n })
	eventually(f.t, "Valkey to confirm the SSUBSCRIBE of "+topic, func() bool { return f.shardNumSub(topic) == 1 })
}

func ch(id uuid.UUID) string  { return "ch:" + id.String() }
func usr(id uuid.UUID) string { return "u:" + id.String() }

func TestTicket_SingleUse(t *testing.T) {
	f := newFixture(t, Options{}, nil)
	ana := f.newUser()
	ticket := f.ticket(ana)

	_, resp, err := f.dial(ticket)
	if err != nil {
		t.Fatalf("first dial: %v (response %v)", err, resp)
	}
	eventually(t, "the connection to be registered", func() bool { return f.gw.subscribers(usr(ana)) == 1 })

	for name, tk := range map[string]string{"redeemed ticket": ticket, "unknown ticket": "nope", "no ticket": ""} {
		_, resp, err := f.dial(tk)
		if err == nil {
			t.Fatalf("dial with %s succeeded, want 401", name)
		}
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("dial with %s: response %v, want status 401 (err %v)", name, resp, err)
		}
	}
	if n := f.gw.subscribers(usr(ana)); n != 1 {
		t.Errorf("rejected dials left %d connections for the user, want the one", n)
	}
}

func TestSubscribe_OnlyMemberChannels(t *testing.T) {
	f := newFixture(t, Options{}, nil)
	ana := f.newUser()
	member := f.newChannel(ana)
	other := f.newChannel(f.newUser()) // ana is not in it

	c := f.connect(ana)
	// The non-member channel and a junk id come first: Subscribe is handled in
	// order, so once the member channel is live the others were already decided.
	c.subscribe(other.String(), "not-a-uuid", member.String())
	f.live(ch(member), 1)

	if n := f.gw.subscribers(ch(other)); n != 0 {
		t.Fatalf("the gateway subscribed a non-member to %s (%d local subscribers)", ch(other), n)
	}
	if n := f.shardNumSub(ch(other)); n != 0 {
		t.Fatalf("the gateway SSUBSCRIBEd %s for a non-member (%d subscribers)", ch(other), n)
	}
	f.publish(ch(other), messageFrame(other, 1, "secret"))
	f.publish(ch(member), messageFrame(member, 2, "visible"))
	c.nextSeq(2) // the frame for the channel she is not in never arrived
}

func TestFanout_OnlyToSubscribedConnections(t *testing.T) {
	f := newFixture(t, Options{}, nil)
	ana, ben, cho := f.newUser(), f.newUser(), f.newUser()
	x := f.newChannel(ana, ben, cho)
	y := f.newChannel(ana, ben, cho)

	a, b, c := f.connect(ana), f.connect(ben), f.connect(cho)
	a.subscribe(x.String())
	b.subscribe(y.String())
	c.subscribe(x.String(), y.String())
	eventually(t, "every subscription to be registered", func() bool {
		return f.gw.subscribers(ch(x)) == 2 && f.gw.subscribers(ch(y)) == 2
	})
	f.live(ch(x), 2)
	f.live(ch(y), 2)

	f.publish(ch(x), messageFrame(x, 1, "to x"))
	f.publish(ch(y), messageFrame(y, 2, "to y"))
	f.publish(ch(x), messageFrame(x, 3, "to x again"))

	a.nextSeq(1) // x, then not y, then x again
	a.nextSeq(3)
	b.nextSeq(2) // only y
	c.nextSeq(1) // everything, in publish order
	c.nextSeq(2)
	c.nextSeq(3)
}

func TestBackpressure_SlowConsumerClosed(t *testing.T) {
	const sendBuffer = 256
	logs := captureLogs(t)
	f := newFixture(t, Options{SendBuffer: sendBuffer}, nil)
	slowUser, fastUser := f.newUser(), f.newUser()
	x := f.newChannel(slowUser, fastUser)

	slow, fast := f.connect(slowUser), f.connect(fastUser)
	slow.subscribe(x.String())
	fast.subscribe(x.String())
	f.live(ch(x), 2)

	// The slow client never reads. Frames are big enough that the kernel's socket
	// buffers cannot hide the overflow, and the fast client reads each frame
	// before the next is published, so it can never fall behind itself.
	body := strings.Repeat("x", 16<<10)
	published := 0
	for f.shardNumSub(usr(slowUser)) != 0 {
		if published >= 5000 {
			t.Fatalf("%d frames published and the slow consumer is still connected", published)
		}
		published++
		f.publish(ch(x), messageFrame(x, int64(published), body))
		fast.nextSeq(int64(published))
	}
	if published <= sendBuffer {
		t.Errorf("the slow consumer was closed after %d frames, before its %d-frame buffer could be full", published, sendBuffer)
	}
	if got := logs(); !strings.Contains(got, "send buffer full") {
		t.Errorf("closing the slow consumer was not logged; log: %q", got)
	}

	if err := slow.readErr(); err == nil {
		t.Fatal("the slow consumer's connection ended without an error")
	}
	if n := f.gw.subscribers(ch(x)); n != 1 {
		t.Errorf("%d connections still follow the channel, want only the fast one", n)
	}
	f.publish(ch(x), messageFrame(x, 9999, "after"))
	fast.nextSeq(9999) // the other connection was unaffected
}

func TestIdleClose_AfterNoFrame(t *testing.T) {
	const idle = 200 * time.Millisecond
	f := newFixture(t, Options{IdleTimeout: idle}, nil)
	ana := f.newUser()

	start := time.Now() // before the dial: the gateway's clock starts after this
	c := f.dialClient(ana)
	err := c.readErr()
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Fatalf("closed with %v (%v), want status policy violation", got, err)
	}
	if elapsed := time.Since(start); elapsed < idle {
		t.Errorf("closed after %v, before the %v idle timeout", elapsed, idle)
	}
	eventually(t, "the idle connection to be unregistered", func() bool { return f.gw.subscribers(usr(ana)) == 0 })
	eventually(t, "Valkey to drop u:{user}", func() bool { return f.shardNumSub(usr(ana)) == 0 })
}

func TestPing_KeepsConnectionAlive(t *testing.T) {
	const idle = 800 * time.Millisecond
	f := newFixture(t, Options{IdleTimeout: idle}, nil)
	ana := f.newUser()
	x := f.newChannel(ana)

	c := f.dialClient(ana)
	c.subscribe(x.String())
	// Sixteen pings 100 ms apart outlast the idle timeout twice over; without them
	// the connection is closed (TestIdleClose_AfterNoFrame). The pings are the
	// test's input, not a wait: nothing here sleeps for the server.
	start := time.Now()
	for range 16 {
		c.ping()
		time.Sleep(idle / 8)
	}
	if elapsed := time.Since(start); elapsed < 2*idle {
		t.Fatalf("pinged for %v, want at least two idle timeouts", elapsed)
	}
	f.live(ch(x), 1)
	f.publish(ch(x), messageFrame(x, 1, "still here"))
	c.nextSeq(1)

	if got := websocket.CloseStatus(c.readErr()); got != websocket.StatusPolicyViolation {
		t.Errorf("after the pings stopped the connection closed with %v, want policy violation", got)
	}
}

func TestMembershipRevoked_StopsFrames(t *testing.T) {
	f := newFixture(t, Options{}, nil)
	ana, ben := f.newUser(), f.newUser()
	x := f.newChannel(ana, ben)
	z := f.newChannel(ana) // for the sentinel frames on ana's own topic

	a, b := f.connect(ana), f.connect(ben)
	a.subscribe(x.String())
	b.subscribe(x.String())
	f.live(ch(x), 2)
	f.publish(ch(x), messageFrame(x, 1, "before"))
	a.nextSeq(1)
	b.nextSeq(1)
	if _, hit, err := f.cache.Members(t.Context(), x); err != nil || !hit {
		t.Fatalf("members:{channel} = hit %v, err %v; the subscribe check should have filled it", hit, err)
	}

	// chat.RemoveMember commits the delete and publishes membership.changed. The
	// stale members:{channel} set still lists ana, which is the case to survive.
	f.removeMember(x, ana)
	f.publishMembership(x, ana, chatv1.MembershipChange_MEMBERSHIP_CHANGE_REMOVED)

	got := a.next().GetMembershipChanged()
	if got.GetChannelId() != x.String() || got.GetUserId() != ana.String() ||
		got.GetChange() != chatv1.MembershipChange_MEMBERSHIP_CHANGE_REMOVED {
		t.Fatalf("ana's u: frame = %v, want membership_changed REMOVED for the channel", got)
	}
	// The frame reaches ana only after the gateway has acted on it.
	if _, hit, err := f.cache.Members(t.Context(), x); err != nil || hit {
		t.Errorf("members:{channel} = hit %v, err %v after the revocation, want it invalidated", hit, err)
	}
	if n := f.gw.subscribers(ch(x)); n != 1 {
		t.Fatalf("%d connections still follow the channel, want only ben's", n)
	}

	f.publish(ch(x), messageFrame(x, 2, "after"))
	f.publishMembership(z, ana, chatv1.MembershipChange_MEMBERSHIP_CHANGE_ADDED) // sentinel on u:{ana}
	b.nextSeq(2)
	if ev := a.next(); ev.GetMessageCreated() != nil || ev.GetMembershipChanged().GetChannelId() != z.String() {
		t.Fatalf("ana's next frame = %v, want the sentinel: the channel frame must not reach her", ev)
	}

	// Subscribing again asks Postgres, since the stale set is gone, and is refused.
	a.subscribe(x.String())
	f.publish(ch(x), messageFrame(x, 3, "still after"))
	f.publishMembership(z, ana, chatv1.MembershipChange_MEMBERSHIP_CHANGE_ADDED)
	b.nextSeq(3)
	eventually(t, "ana's second subscribe to be decided", func() bool {
		_, hit, _ := f.cache.Members(t.Context(), x) // the refusal re-reads Postgres and refills the set
		return hit
	})
	if n := f.gw.subscribers(ch(x)); n != 1 {
		t.Fatalf("ana re-subscribed to a channel she was removed from (%d local subscribers)", n)
	}
	if ev := a.next(); ev.GetMessageCreated() != nil {
		t.Fatalf("ana received %v after the revocation", ev)
	}
}

// A membership check that started before the revocation can return the stale
// answer after it. The subscription must not survive that.
func TestSubscribe_RevocationDuringMembershipCheck_DoesNotSubscribe(t *testing.T) {
	m := &racingMembership{entered: make(chan struct{}), release: make(chan struct{}), rechecked: make(chan struct{})}
	f := newFixture(t, Options{}, func(*fixture) authz.Membership { return m })
	ana := uuid.Must(uuid.NewV7())
	x := uuid.Must(uuid.NewV7())

	a := f.connect(ana)
	a.subscribe(x.String())
	<-m.entered // the check is running and will answer "member"

	f.publishMembership(x, ana, chatv1.MembershipChange_MEMBERSHIP_CHANGE_REMOVED)
	if got := a.next().GetMembershipChanged(); got.GetChange() != chatv1.MembershipChange_MEMBERSHIP_CHANGE_REMOVED {
		t.Fatalf("ana's u: frame = %v, want the REMOVED frame", got)
	}
	close(m.release) // now the stale answer is delivered

	select {
	case <-m.rechecked:
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway acted on a membership answer that predates the revocation instead of asking again")
	}
	if n := f.gw.subscribers(ch(x)); n != 0 {
		t.Fatalf("%d connections follow a channel the user was removed from", n)
	}
}

// racingMembership answers its first call, once released, with a stale "yes" and
// every later call with "no".
type racingMembership struct {
	calls     atomic.Int32
	entered   chan struct{}
	release   chan struct{}
	rechecked chan struct{}
}

func (m *racingMembership) IsMember(ctx context.Context, _, _ uuid.UUID) (bool, error) {
	if m.calls.Add(1) == 1 {
		close(m.entered)
		select {
		case <-m.release:
		case <-ctx.Done():
			return false, ctx.Err()
		}
		return true, nil
	}
	close(m.rechecked)
	return false, nil
}

func TestSharded_SubscribeOnFirstAndUnsubscribeOnLast(t *testing.T) {
	f := newFixture(t, Options{}, nil)
	ana, ben := f.newUser(), f.newUser()
	x := f.newChannel(ana, ben)

	a := f.connect(ana)
	a.subscribe(x.String())
	f.live(ch(x), 1)
	if n := f.shardNumSub(ch(x)); n != 1 {
		t.Fatalf("SHARDNUMSUB after the first subscriber = %d, want 1", n)
	}

	b := f.connect(ben)
	b.subscribe(x.String())
	f.live(ch(x), 2)
	if n := f.shardNumSub(ch(x)); n != 1 {
		t.Fatalf("SHARDNUMSUB with two local subscribers = %d, want the gateway's single subscription", n)
	}

	if err := a.ws.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close ana: %v", err)
	}
	eventually(t, "ana to be unregistered", func() bool { return f.gw.subscribers(ch(x)) == 1 && f.gw.subscribers(usr(ana)) == 0 })
	f.publish(ch(x), messageFrame(x, 1, "for ben"))
	b.nextSeq(1) // the channel stays subscribed while ben remains

	if err := b.ws.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close ben: %v", err)
	}
	eventually(t, "the last unsubscribe of the channel", func() bool { return f.shardNumSub(ch(x)) == 0 })
	eventually(t, "the last unsubscribe of u:{ana}", func() bool { return f.shardNumSub(usr(ana)) == 0 })
	eventually(t, "the last unsubscribe of u:{ben}", func() bool { return f.shardNumSub(usr(ben)) == 0 })
	if n := f.gw.subscribers(ch(x)); n != 0 {
		t.Errorf("%d local subscribers remain after every connection closed", n)
	}
}

func TestShutdown_ReadinessFlipsThenGoingAway(t *testing.T) {
	const drain = 1500 * time.Millisecond
	f := newFixture(t, Options{DrainTimeout: drain}, nil)
	ana := f.newUser()
	x := f.newChannel(ana)
	c := f.connect(ana)
	c.subscribe(x.String())
	f.live(ch(x), 1)
	if !f.gw.Ready() {
		t.Fatal("Ready() = false before Shutdown")
	}

	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.gw.Shutdown(context.Background())
	}()
	eventually(t, "readiness to fail", func() bool { return !f.gw.Ready() })

	// During the drain the connection still works.
	f.publish(ch(x), messageFrame(x, 1, "during the drain"))
	c.nextSeq(1)

	err := c.readErr()
	if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
		t.Fatalf("closed with %v (%v), want going away", got, err)
	}
	if elapsed := time.Since(start); elapsed < drain {
		t.Errorf("sockets closed after %v, before the %v drain elapsed", elapsed, drain)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	if f.gw.Ready() {
		t.Error("Ready() = true after Shutdown")
	}
	eventually(t, "Valkey to drop every subscription", func() bool {
		return f.shardNumSub(ch(x)) == 0 && f.shardNumSub(usr(ana)) == 0
	})

	_, resp, err := f.dial(f.ticket(ana))
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("dial after Shutdown: response %v, err %v; want status 503", resp, err)
	}
}

func TestShutdown_ContextEndsTheDrainEarly(t *testing.T) {
	f := newFixture(t, Options{DrainTimeout: time.Hour}, nil)
	c := f.connect(f.newUser())

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.gw.Shutdown(ctx)
	}()
	eventually(t, "readiness to fail", func() bool { return !f.gw.Ready() })
	cancel()

	if got := websocket.CloseStatus(c.readErr()); got != websocket.StatusGoingAway {
		t.Errorf("closed with %v, want going away", got)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown ignored its context")
	}
}

func TestSubscription_SurvivesValkeyDroppingPubSubConnections(t *testing.T) {
	f := newFixture(t, Options{}, nil)
	ana := f.newUser()
	x := f.newChannel(ana)
	c := f.connect(ana)
	c.subscribe(x.String())
	f.live(ch(x), 1)
	f.live(usr(ana), 1)

	killed, err := f.rdb.Do(t.Context(), "CLIENT", "KILL", "TYPE", "pubsub").Int()
	if err != nil || killed < 1 {
		t.Fatalf("CLIENT KILL TYPE pubsub = %d, %v; want the gateway's connection killed", killed, err)
	}
	// The kill removed every subscription at once; seeing them again means the
	// subscription reconnected and resubscribed its current topics by itself.
	f.live(ch(x), 1)
	f.live(usr(ana), 1)

	f.publish(ch(x), messageFrame(x, 1, "after the reconnect"))
	c.nextSeq(1)
	f.publishMembership(uuid.Must(uuid.NewV7()), ana, chatv1.MembershipChange_MEMBERSHIP_CHANGE_ADDED)
	if got := c.next().GetMembershipChanged(); got == nil {
		t.Error("no user frame after the reconnect")
	}
}

func TestClientFrames_TypingAndPingAreNotRelayed(t *testing.T) {
	f := newFixture(t, Options{}, nil)
	ana, ben := f.newUser(), f.newUser()
	x, z := f.newChannel(ana, ben), f.newChannel(ana)
	a, b := f.connect(ana), f.connect(ben)
	a.subscribe(x.String())
	b.subscribe(x.String())
	f.live(ch(x), 2)

	a.write(&realtimev1.ClientFrame{Frame: &realtimev1.ClientFrame_Typing{Typing: &realtimev1.Typing{ChannelId: x.String()}}})
	a.ping()
	// Frames of one connection are handled in order: once this subscription is
	// registered, the Typing before it has been handled, and whatever the gateway
	// would have published for it is already in Valkey ahead of the sentinel.
	a.subscribe(z.String())
	f.live(ch(z), 1)
	f.publish(ch(x), messageFrame(x, 1, "sentinel"))
	b.nextSeq(1) // no typing frame came first
	a.nextSeq(1)
}

func TestClientFrames_MalformedOrTextFramesCloseTheConnection(t *testing.T) {
	f := newFixture(t, Options{}, nil)
	ctx := t.Context()

	bad := f.connect(f.newUser())
	if err := bad.ws.Write(ctx, websocket.MessageBinary, []byte{0xff, 0xff, 0xff}); err != nil {
		t.Fatal(err)
	}
	if got := websocket.CloseStatus(bad.readErr()); got != websocket.StatusInvalidFramePayloadData {
		t.Errorf("malformed protobuf closed with %v, want invalid frame payload data", got)
	}

	text := f.connect(f.newUser())
	if err := text.ws.Write(ctx, websocket.MessageText, []byte(`{"subscribe":[]}`)); err != nil {
		t.Fatal(err)
	}
	if got := websocket.CloseStatus(text.readErr()); got != websocket.StatusUnsupportedData {
		t.Errorf("text frame closed with %v, want unsupported data", got)
	}
}

// onlyChannel is a membership of exactly one channel.
type onlyChannel struct{ channel uuid.UUID }

func (m onlyChannel) IsMember(_ context.Context, channelID, _ uuid.UUID) (bool, error) {
	return channelID == m.channel, nil
}

func TestClientFrames_FrameSizeIsBounded(t *testing.T) {
	member := uuid.Must(uuid.NewV7())
	f := newFixture(t, Options{}, func(*fixture) authz.Membership { return onlyChannel{member} })

	// A Subscribe of 400 channel ids is about 15 KB: inside the bound, served in full.
	big := f.connect(uuid.Must(uuid.NewV7()))
	ids := make([]string, 0, 400)
	for range 399 {
		ids = append(ids, uuid.NewString())
	}
	big.subscribe(append(ids, member.String())...)
	f.live(ch(member), 1)

	// 20 KB is above the bound and below the library's own default (32 KB), so only
	// the gateway's explicit limit can refuse it: with none the frame is read and
	// fails as a protobuf instead (1007).
	oversized := f.connect(uuid.Must(uuid.NewV7()))
	if err := oversized.ws.Write(t.Context(), websocket.MessageBinary, make([]byte, 20<<10)); err != nil {
		t.Fatal(err)
	}
	if got := websocket.CloseStatus(oversized.readErr()); got != websocket.StatusMessageTooBig {
		t.Errorf("a 20 KB frame closed with %v, want message too big", got)
	}
}

func TestOrigin_SameOriginByDefault_ConfiguredPatternsAdded(t *testing.T) {
	const dev = "localhost:5173"
	for _, tc := range []struct {
		name     string
		patterns []string
		origin   func(f *fixture) string
		want     int
	}{
		{"no origin header", nil, func(*fixture) string { return "" }, http.StatusSwitchingProtocols},
		{"same origin", nil, func(f *fixture) string { return f.srv.URL }, http.StatusSwitchingProtocols},
		{"another origin, none configured", nil, func(*fixture) string { return "http://" + dev }, http.StatusForbidden},
		{"configured origin", []string{dev}, func(*fixture) string { return "http://" + dev }, http.StatusSwitchingProtocols},
		{"same origin, others configured", []string{dev}, func(f *fixture) string { return f.srv.URL }, http.StatusSwitchingProtocols},
		{"origin that is not configured", []string{dev}, func(*fixture) string { return "https://evil.example" }, http.StatusForbidden},
		{"look-alike of the configured origin", []string{dev}, func(*fixture) string { return "http://" + dev + "0" }, http.StatusForbidden},
		{"scheme-qualified pattern for another scheme", []string{"https://" + dev}, func(*fixture) string { return "http://" + dev }, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, Options{OriginPatterns: tc.patterns}, nil)
			_, resp, err := f.dialWith(f.ticket(f.newUser()), tc.origin(f))
			if resp == nil || resp.StatusCode != tc.want {
				t.Fatalf("dial with Origin %q: response %v, err %v; want status %d", tc.origin(f), resp, err, tc.want)
			}
		})
	}
}

func TestShutdown_ExpiredContext_CutsConnectionsWithoutWaitingForHandshakes(t *testing.T) {
	f := newFixture(t, Options{DrainTimeout: time.Hour}, nil)
	ana := f.newUser()
	c := f.connect(ana) // never reads, so it never answers the close frame

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	f.gw.Shutdown(ctx)
	// Without the cut, Shutdown waits for the close handshake, which the library
	// gives up on after 5 s; the bound sits well under that and far above a cut.
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Errorf("Shutdown with an expired context took %v, want it not to wait for close handshakes", elapsed)
	}

	if err := c.readErr(); err == nil {
		t.Error("the connection is still readable after Shutdown returned")
	}
	if n := f.gw.subscribers(usr(ana)); n != 0 {
		t.Errorf("%d connections still registered after Shutdown", n)
	}
	eventually(t, "Valkey to drop u:{user}", func() bool { return f.shardNumSub(usr(ana)) == 0 })
}

func TestSubscribe_MembershipLookupFailureFailsClosed(t *testing.T) {
	logs := captureLogs(t)
	broken, fine := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	f := newFixture(t, Options{}, func(*fixture) authz.Membership { return failingFor{channel: broken} })
	ana := uuid.Must(uuid.NewV7())
	c := f.connect(ana)

	// Subscribe is handled in order: once the second channel is live, the lookup
	// failure on the first has been handled.
	c.subscribe(broken.String(), fine.String())
	f.live(ch(fine), 1)
	if n := f.gw.subscribers(ch(broken)); n != 0 {
		t.Errorf("subscribed to a channel whose membership could not be checked (%d subscribers)", n)
	}
	f.publish(ch(fine), messageFrame(fine, 1, "still serving"))
	c.nextSeq(1) // the failed lookup did not take the connection down
	if got := logs(); !strings.Contains(got, "membership check failed") {
		t.Errorf("the failed lookup was not logged; log: %q", got)
	}
}

// failingFor answers "member" for every channel but one, whose lookup fails.
type failingFor struct{ channel uuid.UUID }

func (m failingFor) IsMember(_ context.Context, channelID, _ uuid.UUID) (bool, error) {
	if channelID == m.channel {
		return false, errors.New("lookup failed")
	}
	return true, nil
}
