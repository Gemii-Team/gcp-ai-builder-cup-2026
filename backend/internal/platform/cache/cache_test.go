package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"kiwi/internal/platform/errs"
)

const valkeyImage = "valkey/valkey:8"

// One plain (not cluster-mode) Valkey serves the whole test package. Every test
// works on keys made from fresh UUIDs, so they do not see each other.
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

	// Persistence off: the data is thrown away with the container.
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

// open returns a Cache on the shared Valkey, closed when the test ends.
func open(t *testing.T, opt Options) *Cache {
	t.Helper()
	valkeyOnce.Do(func() { valkeyURL, valkeyErr = bootValkey() })
	if valkeyErr != nil {
		t.Fatalf("valkey: %v", valkeyErr)
	}
	c, err := Open(context.Background(), valkeyURL, opt)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return c
}

func pttl(t *testing.T, c *Cache, key string) time.Duration {
	t.Helper()
	d, err := c.rdb.PTTL(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("PTTL %s: %v", key, err)
	}
	return d
}

func exists(t *testing.T, c *Cache, key string) bool {
	t.Helper()
	n, err := c.rdb.Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("EXISTS %s: %v", key, err)
	}
	return n == 1
}

// eventually polls cond until it holds; it fails the test after 10 s. It
// waits for server state, it never asserts on elapsed time.
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

func TestOpen_DefaultsAndFailures(t *testing.T) {
	c := open(t, Options{})
	want := Options{SessionSliding: 7 * 24 * time.Hour, SessionAbsolute: 30 * 24 * time.Hour, TicketTTL: 30 * time.Second}
	if c.opt != want {
		t.Errorf("default options = %+v, want %+v", c.opt, want)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}

	custom := open(t, Options{SessionSliding: time.Minute, SessionAbsolute: time.Hour, TicketTTL: time.Second})
	if custom.opt != (Options{SessionSliding: time.Minute, SessionAbsolute: time.Hour, TicketTTL: time.Second}) {
		t.Errorf("explicit options were overwritten: %+v", custom.opt)
	}

	// A URL that does not parse never echoes its password.
	_, err := Open(context.Background(), "redis://:hunter2@host:notaport/0", Options{})
	if err == nil {
		t.Fatal("Open accepted a malformed URL")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the URL password: %v", err)
	}

	// A failed ping surfaces as an Open error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, valkeyURL, Options{}); err == nil {
		t.Error("Open succeeded with a cancelled context")
	}
}

func TestRedeemTicket_SingleUse(t *testing.T) {
	c := open(t, Options{})
	ctx := context.Background()
	rec := TicketRecord{UserID: uuid.New(), WorkspaceID: uuid.New()}

	ticket, err := c.IssueTicket(ctx, rec)
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	other, err := c.IssueTicket(ctx, rec)
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	if ticket == "" || ticket == other {
		t.Fatalf("tickets must be non-empty and unique, got %q and %q", ticket, other)
	}

	// §3.3: key rtt:{ticket}, TTL 30 s.
	key := "rtt:" + ticket
	if ttl := pttl(t, c, key); ttl <= 25*time.Second || ttl > 30*time.Second {
		t.Errorf("ticket TTL = %v, want within (25s, 30s]", ttl)
	}

	// Sixteen racing redeems: GETDEL hands the record to exactly one.
	const racers = 16
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		got      []TicketRecord
		notFound int
		failed   []error
	)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.RedeemTicket(ctx, ticket)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				got = append(got, r)
			case errors.Is(err, errs.ErrNotFound):
				notFound++
			default:
				failed = append(failed, err)
			}
		}()
	}
	wg.Wait()
	if len(failed) > 0 {
		t.Fatalf("unexpected redeem errors: %v", failed)
	}
	if len(got) != 1 || got[0] != rec || notFound != racers-1 {
		t.Fatalf("redeemed %d times (%v), not-found %d; want exactly one redeem of %v and %d not-found", len(got), got, notFound, rec, racers-1)
	}

	if _, err := c.RedeemTicket(ctx, ticket); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("reuse: err = %v, want ErrNotFound", err)
	}
	if exists(t, c, key) {
		t.Error("redeemed ticket is still in Valkey")
	}
	if _, err := c.RedeemTicket(ctx, "never-issued"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown ticket: err = %v, want ErrNotFound", err)
	}
	// The sibling ticket is untouched.
	if r, err := c.RedeemTicket(ctx, other); err != nil || r != rec {
		t.Errorf("redeem of the other ticket = %v, %v", r, err)
	}

	short := open(t, Options{TicketTTL: 5 * time.Second})
	st, err := short.IssueTicket(ctx, rec)
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	if ttl := pttl(t, short, "rtt:"+st); ttl <= 0 || ttl > 5*time.Second {
		t.Errorf("TicketTTL option ignored: TTL = %v, want within (0, 5s]", ttl)
	}
}

func TestSession_SlidingExtendsAndAbsoluteCaps(t *testing.T) {
	c := open(t, Options{SessionSliding: time.Second, SessionAbsolute: 2 * time.Second})
	ctx := context.Background()

	// The absolute cap is judged against an injected clock so the test does
	// not sit through the real 2 s; the server's TTL still runs in real time.
	start := time.Now()
	offset := time.Duration(0)
	c.now = func() time.Time { return start.Add(offset) }

	id := uuid.NewString()
	rec := SessionRecord{UserID: uuid.New(), WorkspaceID: uuid.New()}
	if err := c.PutSession(ctx, id, rec); err != nil {
		t.Fatalf("PutSession: %v", err)
	}
	key := "sess:" + id // §3.3
	if ttl := pttl(t, c, key); ttl <= 0 || ttl > time.Second {
		t.Fatalf("fresh session TTL = %v, want within (0, 1s]", ttl)
	}

	got, err := c.GetSession(ctx, id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.UserID != rec.UserID || got.WorkspaceID != rec.WorkspaceID || !got.CreatedAt.Equal(start) {
		t.Fatalf("GetSession = %+v, want %+v created at %v", got, rec, start)
	}

	// Sliding: after 250 ms the TTL has run down; a read puts it back to 1 s.
	time.Sleep(250 * time.Millisecond)
	offset = 250 * time.Millisecond
	before := pttl(t, c, key)
	if _, err := c.GetSession(ctx, id); err != nil {
		t.Fatalf("GetSession after 250ms: %v", err)
	}
	after := pttl(t, c, key)
	if after-before < 200*time.Millisecond || after > time.Second {
		t.Errorf("TTL before/after a read = %v / %v, want the read to restore it to at most 1s", before, after)
	}

	// Absolute: 1.5 s in, 0.5 s of the 2 s lifetime is left, so the slide is
	// capped to that instead of a full second.
	offset = 1500 * time.Millisecond
	if _, err := c.GetSession(ctx, id); err != nil {
		t.Fatalf("GetSession at 1.5s: %v", err)
	}
	if ttl := pttl(t, c, key); ttl <= 0 || ttl > 500*time.Millisecond {
		t.Errorf("TTL near the cap = %v, want within (0, 500ms]", ttl)
	}

	// Past the cap the session is gone even though the key still lives.
	offset = 2*time.Second + time.Millisecond
	if _, err := c.GetSession(ctx, id); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("GetSession past the absolute cap: err = %v, want ErrNotFound", err)
	}
	if exists(t, c, key) {
		t.Error("the capped session was left in Valkey")
	}
}

func TestSession_PutValidationAndDelete(t *testing.T) {
	c := open(t, Options{SessionSliding: time.Second, SessionAbsolute: 2 * time.Second})
	ctx := context.Background()

	if err := c.PutSession(ctx, "", SessionRecord{}); !errors.Is(err, errs.ErrInvalid) {
		t.Errorf("empty id: err = %v, want ErrInvalid", err)
	}
	old := SessionRecord{UserID: uuid.New(), WorkspaceID: uuid.New(), CreatedAt: time.Now().Add(-time.Hour)}
	if err := c.PutSession(ctx, uuid.NewString(), old); !errors.Is(err, errs.ErrInvalid) {
		t.Errorf("record older than the absolute lifetime: err = %v, want ErrInvalid", err)
	}

	id := uuid.NewString()
	if err := c.PutSession(ctx, id, SessionRecord{UserID: uuid.New(), WorkspaceID: uuid.New()}); err != nil {
		t.Fatalf("PutSession: %v", err)
	}
	if err := c.DeleteSession(ctx, id); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := c.GetSession(ctx, id); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("GetSession after delete: err = %v, want ErrNotFound", err)
	}
	if err := c.DeleteSession(ctx, id); err != nil {
		t.Errorf("deleting an absent session: %v", err)
	}
}

func TestOAuthState_TakeOnce(t *testing.T) {
	c := open(t, Options{})
	ctx := context.Background()
	state := uuid.NewString()
	want := OAuthState{Verifier: "pkce-verifier", ReturnTo: "/channels/general", Nonce: "nonce-of-the-flow"}

	if err := c.PutOAuthState(ctx, state, want); err != nil {
		t.Fatalf("PutOAuthState: %v", err)
	}
	if ttl := pttl(t, c, "oauth:"+state); ttl <= 9*time.Minute || ttl > 10*time.Minute {
		t.Errorf("state TTL = %v, want within (9m, 10m]", ttl)
	}

	got, err := c.TakeOAuthState(ctx, state)
	if err != nil || got != want {
		t.Fatalf("TakeOAuthState = %+v, %v; want %+v", got, err, want)
	}
	if _, err := c.TakeOAuthState(ctx, state); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("second take: err = %v, want ErrNotFound", err)
	}
	if _, err := c.TakeOAuthState(ctx, uuid.NewString()); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown state: err = %v, want ErrNotFound", err)
	}
	if err := c.PutOAuthState(ctx, "", want); !errors.Is(err, errs.ErrInvalid) {
		t.Errorf("empty state: err = %v, want ErrInvalid", err)
	}
}

func sorted(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	slices.Sort(out)
	return out
}

func requireMembers(t *testing.T, c *Cache, channel uuid.UUID, wantHit bool, want ...uuid.UUID) {
	t.Helper()
	ids, hit, err := c.Members(context.Background(), channel)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if hit != wantHit {
		t.Fatalf("Members hit = %v, want %v (ids %v)", hit, wantHit, ids)
	}
	if g, w := fmt.Sprint(sorted(ids)), fmt.Sprint(sorted(want)); g != w {
		t.Fatalf("Members = %s, want %s", g, w)
	}
}

func TestMembersCache_InvalidateRemoves(t *testing.T) {
	c := open(t, Options{})
	ctx := context.Background()
	ch1, ch2 := uuid.New(), uuid.New()
	a, b, d := uuid.New(), uuid.New(), uuid.New()

	requireMembers(t, c, ch1, false)

	if err := c.SetMembers(ctx, ch1, []uuid.UUID{a, b, a}); err != nil {
		t.Fatalf("SetMembers: %v", err)
	}
	if err := c.SetMembers(ctx, ch2, []uuid.UUID{d}); err != nil {
		t.Fatalf("SetMembers: %v", err)
	}
	if !exists(t, c, "members:"+ch1.String()) { // §3.3
		t.Error("no members:{channel} key after SetMembers")
	}
	requireMembers(t, c, ch1, true, a, b)
	requireMembers(t, c, ch2, true, d)

	// SetMembers replaces the set, it does not merge into it.
	if err := c.SetMembers(ctx, ch1, []uuid.UUID{d}); err != nil {
		t.Fatalf("SetMembers: %v", err)
	}
	requireMembers(t, c, ch1, true, d)

	// A removed member must not survive in the cache: after Invalidate the
	// next read is a miss, and only that channel is affected.
	if err := c.InvalidateMembers(ctx, ch1); err != nil {
		t.Fatalf("InvalidateMembers: %v", err)
	}
	requireMembers(t, c, ch1, false)
	requireMembers(t, c, ch2, true, d)
	if err := c.InvalidateMembers(ctx, ch1); err != nil {
		t.Errorf("invalidating an absent channel: %v", err)
	}

	// An empty set is stored as no key, so it reads as a miss.
	if err := c.SetMembers(ctx, ch2, nil); err != nil {
		t.Fatalf("SetMembers(nil): %v", err)
	}
	requireMembers(t, c, ch2, false)
}

func recv(t *testing.T, sub *Subscription) Frame {
	t.Helper()
	select {
	case f, ok := <-sub.C():
		if !ok {
			t.Fatal("subscription channel closed while waiting for a frame")
		}
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a frame")
		return Frame{}
	}
}

func shardSubscribers(t *testing.T, c *Cache, topic string) int64 {
	t.Helper()
	m, err := c.rdb.PubSubShardNumSub(context.Background(), topic).Result()
	if err != nil {
		t.Fatalf("PUBSUB SHARDNUMSUB: %v", err)
	}
	return m[topic]
}

func TestSharded_SPublishReachesSubscriber_AndRemoveStops(t *testing.T) {
	c := open(t, Options{})
	ctx := context.Background()
	topicA, topicB := "ch:"+uuid.NewString(), "u:"+uuid.NewString()

	sub, err := c.SSubscribe(ctx)
	if err != nil {
		t.Fatalf("SSubscribe: %v", err)
	}
	defer sub.Close()
	sub.Add(topicA, topicB)
	sub.Add() // no topics: a no-op, never "subscribe to everything"
	for _, topic := range []string{topicA, topicB} {
		eventually(t, "SSUBSCRIBE "+topic, func() bool { return shardSubscribers(t, c, topic) == 1 })
	}

	// Frames are binary protobuf: bytes that are not valid UTF-8 survive.
	payload := []byte{0x00, 0xff, 0xfe, '\n', 0x08, 0x96, 0x01}
	if err := c.SPublish(ctx, topicA, payload); err != nil {
		t.Fatalf("SPublish: %v", err)
	}
	if f := recv(t, sub); f.Topic != topicA || !bytes.Equal(f.Data, payload) {
		t.Fatalf("frame = %q %x, want %q %x", f.Topic, f.Data, topicA, payload)
	}

	// After Remove a later publish is not received. A is published before B on
	// one connection, so had A been delivered it would arrive ahead of B.
	sub.Remove(topicA)
	sub.Remove() // no topics: a no-op, never "unsubscribe from everything"
	eventually(t, "SUNSUBSCRIBE "+topicA, func() bool { return shardSubscribers(t, c, topicA) == 0 })
	if err := c.SPublish(ctx, topicA, []byte("late")); err != nil {
		t.Fatalf("SPublish: %v", err)
	}
	if err := c.SPublish(ctx, topicB, []byte("marker")); err != nil {
		t.Fatalf("SPublish: %v", err)
	}
	if f := recv(t, sub); f.Topic != topicB || string(f.Data) != "marker" {
		t.Fatalf("after Remove got frame %q %q, want only %q %q", f.Topic, f.Data, topicB, "marker")
	}
	select {
	case f := <-sub.C():
		t.Fatalf("unexpected extra frame %q %q", f.Topic, f.Data)
	default:
	}

	// Close releases the server-side subscription, ends C, and may be repeated.
	sub.Close()
	sub.Close()
	eventually(t, "SUNSUBSCRIBE on Close", func() bool { return shardSubscribers(t, c, topicB) == 0 })
	deadline := time.After(10 * time.Second)
	for more := true; more; {
		select {
		case _, more = <-sub.C():
		case <-deadline:
			t.Fatal("C was not closed by Close")
		}
	}
}

func TestSharded_SubscriptionsAreIndependent(t *testing.T) {
	c := open(t, Options{})
	ctx := context.Background()
	shared, onlyOne := "ch:"+uuid.NewString(), "ch:"+uuid.NewString()

	s1, err := c.SSubscribe(ctx)
	if err != nil {
		t.Fatalf("SSubscribe: %v", err)
	}
	defer s1.Close()
	s2, err := c.SSubscribe(ctx)
	if err != nil {
		t.Fatalf("SSubscribe: %v", err)
	}
	defer s2.Close()
	s1.Add(shared, onlyOne)
	s2.Add(shared)
	eventually(t, "both subscribed", func() bool {
		return shardSubscribers(t, c, shared) == 2 && shardSubscribers(t, c, onlyOne) == 1
	})

	if err := c.SPublish(ctx, onlyOne, []byte("one")); err != nil {
		t.Fatalf("SPublish: %v", err)
	}
	if err := c.SPublish(ctx, shared, []byte("all")); err != nil {
		t.Fatalf("SPublish: %v", err)
	}
	if f := recv(t, s1); f.Topic != onlyOne {
		t.Errorf("s1 first frame on %q, want %q", f.Topic, onlyOne)
	}
	if f := recv(t, s1); f.Topic != shared {
		t.Errorf("s1 second frame on %q, want %q", f.Topic, shared)
	}
	// s2 never subscribed to onlyOne: its first frame is the shared one.
	if f := recv(t, s2); f.Topic != shared || string(f.Data) != "all" {
		t.Errorf("s2 frame = %q %q, want %q %q", f.Topic, f.Data, shared, "all")
	}
}
