package cache

import (
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
	"kiwi/internal/testkit/containers"
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
	ctr, err := containers.Start(ctx, valkeyImage,
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
func open(t *testing.T, opt Options) *Valkey {
	t.Helper()
	valkeyOnce.Do(func() { valkeyURL, valkeyErr = bootValkey() })
	if valkeyErr != nil {
		t.Fatalf("valkey: %v", valkeyErr)
	}
	c, err := OpenValkey(context.Background(), valkeyURL, opt)
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

func pttl(t *testing.T, c *Valkey, key string) time.Duration {
	t.Helper()
	d, err := c.rdb.PTTL(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("PTTL %s: %v", key, err)
	}
	return d
}

func exists(t *testing.T, c *Valkey, key string) bool {
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
	_, err := OpenValkey(context.Background(), "redis://:hunter2@host:notaport/0", Options{})
	if err == nil {
		t.Fatal("Open accepted a malformed URL")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the URL password: %v", err)
	}

	// A failed ping surfaces as an Open error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenValkey(ctx, valkeyURL, Options{}); err == nil {
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
	want := OAuthState{Verifier: "pkce-verifier", ReturnTo: "/channels/general", Nonce: "nonce-of-the-flow", Consent: true}

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

// A state written before the Consent field existed (an old binary's, still in Valkey during a deploy) has no `consent` key:
// it decodes as a plain sign-in, and a plain sign-in's state does not carry the key at all.
func TestOAuthState_WithoutAConsentKey_DecodesAsNoConsent(t *testing.T) {
	c := open(t, Options{})
	ctx := context.Background()
	state := uuid.NewString()
	old := `{"verifier":"pkce-verifier","return_to":"/channels/general","nonce":"nonce-of-the-flow"}`
	if err := c.rdb.Set(ctx, "oauth:"+state, old, 10*time.Minute).Err(); err != nil {
		t.Fatal(err)
	}

	got, err := c.TakeOAuthState(ctx, state)

	want := OAuthState{Verifier: "pkce-verifier", ReturnTo: "/channels/general", Nonce: "nonce-of-the-flow"}
	if err != nil || got != want || got.Consent {
		t.Fatalf("TakeOAuthState = %+v, %v; want %+v with Consent false", got, err, want)
	}

	plain := uuid.NewString()
	if err := c.PutOAuthState(ctx, plain, want); err != nil {
		t.Fatal(err)
	}
	raw, err := c.rdb.Get(ctx, "oauth:"+plain).Result()
	if err != nil || strings.Contains(raw, "consent") {
		t.Errorf("stored state of a plain sign-in = %s (%v), want no consent key: an old binary reads what a new one writes", raw, err)
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

func requireMembers(t *testing.T, c *Valkey, channel uuid.UUID, wantHit bool, want ...uuid.UUID) {
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

func TestMembersCache_StaleFillIsRefusedAfterInvalidate(t *testing.T) {
	c := open(t, Options{})
	ctx := context.Background()
	ch, other := uuid.New(), uuid.New()
	a, b := uuid.New(), uuid.New()

	epoch, err := c.MembersEpoch(ctx, ch)
	if err != nil {
		t.Fatalf("MembersEpoch: %v", err)
	}
	if epoch != 0 {
		t.Fatalf("epoch of a never-invalidated channel = %d, want 0", epoch)
	}
	// A reader that sampled the epoch fills the set when nothing was invalidated since.
	stored, err := c.SetMembersAtEpoch(ctx, ch, epoch, []uuid.UUID{a, b, a})
	if err != nil || !stored {
		t.Fatalf("SetMembersAtEpoch = %v, %v, want stored", stored, err)
	}
	requireMembers(t, c, ch, true, a, b)
	if ttl := c.rdb.PTTL(ctx, "members:"+ch.String()).Val(); ttl <= 0 || ttl > membersBackstopTTL {
		t.Errorf("TTL of the filled set = %v, want a positive backstop up to %v", ttl, membersBackstopTTL)
	}

	// A slow reader sampled the epoch, then Postgres changed and the set was invalidated.
	slow, err := c.MembersEpoch(ctx, ch)
	if err != nil {
		t.Fatalf("MembersEpoch: %v", err)
	}
	if err := c.InvalidateMembers(ctx, ch); err != nil {
		t.Fatalf("InvalidateMembers: %v", err)
	}
	requireMembers(t, c, ch, false)
	if now, _ := c.MembersEpoch(ctx, ch); now != slow+1 {
		t.Fatalf("epoch after one invalidation = %d, want %d", now, slow+1)
	}
	stored, err = c.SetMembersAtEpoch(ctx, ch, slow, []uuid.UUID{a, b})
	if err != nil || stored {
		t.Fatalf("stale SetMembersAtEpoch = %v, %v, want refused without error", stored, err)
	}
	requireMembers(t, c, ch, false) // the stale fill did not stick

	// A reader that sampled after the invalidation fills it; other channels are independent.
	fresh, _ := c.MembersEpoch(ctx, ch)
	if stored, err = c.SetMembersAtEpoch(ctx, ch, fresh, []uuid.UUID{a}); err != nil || !stored {
		t.Fatalf("fresh SetMembersAtEpoch = %v, %v, want stored", stored, err)
	}
	requireMembers(t, c, ch, true, a)
	if e, _ := c.MembersEpoch(ctx, other); e != 0 {
		t.Fatalf("another channel's epoch = %d, want 0", e)
	}

	// An empty set stores nothing but is not an error when the epoch still matches.
	if stored, err = c.SetMembersAtEpoch(ctx, other, 0, nil); err != nil || !stored {
		t.Fatalf("SetMembersAtEpoch(nil) = %v, %v, want accepted", stored, err)
	}
	requireMembers(t, c, other, false)
}

func shardSubscribers(t *testing.T, c *Valkey, topic string) int64 {
	t.Helper()
	m, err := c.rdb.PubSubShardNumSub(context.Background(), topic).Result()
	if err != nil {
		t.Fatalf("PUBSUB SHARDNUMSUB: %v", err)
	}
	return m[topic]
}
