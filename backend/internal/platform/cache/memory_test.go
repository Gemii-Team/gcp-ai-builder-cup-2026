package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"kiwi/internal/platform/errs"
)

// clock is a time that moves when a test says so.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newMemory(t *testing.T, opt Options) (*Memory, *clock) {
	t.Helper()
	m := NewMemory(opt)
	clk := newClock()
	m.now = clk.now
	t.Cleanup(func() { _ = m.Close() })
	return m, clk
}

func TestMemory_TicketsExpireAfterTheTTL(t *testing.T) {
	m, clk := newMemory(t, Options{})
	ctx := context.Background()
	rec := TicketRecord{UserID: uuid.New(), WorkspaceID: uuid.New()}

	// 30 s by default, the Valkey adapter's.
	ticket, err := m.IssueTicket(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(30*time.Second - time.Millisecond)
	if got, err := m.RedeemTicket(ctx, ticket); err != nil || got != rec {
		t.Fatalf("redeem just inside the TTL = %v, %v; want the record", got, err)
	}

	late, _ := m.IssueTicket(ctx, rec)
	clk.advance(30 * time.Second)
	if _, err := m.RedeemTicket(ctx, late); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("redeem at the TTL: err = %v, want ErrNotFound", err)
	}
	// An expired ticket is spent too: it is gone, not merely refused.
	if n := m.tickets.len(); n != 0 {
		t.Errorf("%d tickets held after the expired one was redeemed, want 0", n)
	}

	short, clk := newMemory(t, Options{TicketTTL: 5 * time.Second})
	st, _ := short.IssueTicket(ctx, rec)
	clk.advance(5 * time.Second)
	if _, err := short.RedeemTicket(ctx, st); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("TicketTTL option ignored: err = %v, want ErrNotFound after 5 s", err)
	}
}

func TestMemory_OAuthStatesExpireAfterTenMinutes(t *testing.T) {
	m, clk := newMemory(t, Options{})
	ctx := context.Background()
	want := OAuthState{Verifier: "v", ReturnTo: "/board", Nonce: "n"}

	if err := m.PutOAuthState(ctx, "inside", want); err != nil {
		t.Fatal(err)
	}
	if err := m.PutOAuthState(ctx, "outside", want); err != nil {
		t.Fatal(err)
	}
	clk.advance(10*time.Minute - time.Millisecond)
	if got, err := m.TakeOAuthState(ctx, "inside"); err != nil || got != want {
		t.Fatalf("take just inside ten minutes = %+v, %v", got, err)
	}
	clk.advance(time.Millisecond)
	if _, err := m.TakeOAuthState(ctx, "outside"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("take at ten minutes: err = %v, want ErrNotFound", err)
	}
}

// What is never taken is collected, so a flood of logins that never come back cannot
// grow the process for ever.
func TestMemory_PendingEntriesAreSweptAndBounded(t *testing.T) {
	m, clk := newMemory(t, Options{})
	ctx := context.Background()

	for i := range 50 {
		if err := m.PutOAuthState(ctx, fmt.Sprintf("old-%d", i), OAuthState{}); err != nil {
			t.Fatal(err)
		}
	}
	clk.advance(11 * time.Minute) // all expired; the next put sweeps them
	if err := m.PutOAuthState(ctx, "fresh", OAuthState{}); err != nil {
		t.Fatal(err)
	}
	if n := m.oauth.len(); n != 1 {
		t.Errorf("%d states held after the sweep, want only the fresh one", n)
	}

	// At the bound the oldest entry goes and the new one stays.
	b, clk := newMemory(t, Options{})
	for i := range maxPending {
		if err := b.PutOAuthState(ctx, fmt.Sprintf("s-%d", i), OAuthState{Nonce: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
		clk.advance(time.Millisecond) // distinct expiry times: the first put expires first
	}
	if n := b.oauth.len(); n != maxPending {
		t.Fatalf("%d states held, want %d", n, maxPending)
	}
	if err := b.PutOAuthState(ctx, "one-more", OAuthState{Nonce: "new"}); err != nil {
		t.Fatalf("a put at the bound failed: %v: new logins must keep working", err)
	}
	if n := b.oauth.len(); n != maxPending {
		t.Errorf("%d states held after one more, want the bound %d", n, maxPending)
	}
	if _, err := b.TakeOAuthState(ctx, "s-0"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the oldest state survived the bound: err = %v", err)
	}
	if got, err := b.TakeOAuthState(ctx, "one-more"); err != nil || got.Nonce != "new" {
		t.Errorf("the newest state = %+v, %v; want it kept", got, err)
	}
	if _, err := b.TakeOAuthState(ctx, "s-1"); err != nil {
		t.Errorf("the second oldest state was dropped too: %v", err)
	}
}

// Sessions must outlive a restart, so the in-process cache must never hold them: a
// future change that gives Memory session methods would silently sign everyone out on
// a redeploy.
func TestMemory_KeepsNoSessions(t *testing.T) {
	var c Cache = NewMemory(Options{})
	if _, ok := c.(Sessions); ok {
		t.Fatal("Memory implements Sessions: sessions would be lost on every restart; they belong in Postgres (identity.NewSessions)")
	}
	var v Cache = (*Valkey)(nil)
	if _, ok := v.(Sessions); !ok {
		t.Fatal("Valkey no longer implements Sessions: the valkey profile would have nowhere to keep them")
	}
}

// Nothing is cached, so nothing can be stale: a revocation takes effect at once, in this
// process and in a second one that overlaps it during a rollout.
func TestMemory_CachesNoMembers(t *testing.T) {
	m, _ := newMemory(t, Options{})
	ctx := context.Background()
	channel, user := uuid.New(), uuid.New()

	epoch, err := m.MembersEpoch(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	if stored, err := m.SetMembersAtEpoch(ctx, channel, epoch, []uuid.UUID{user}); err != nil || stored {
		t.Errorf("SetMembersAtEpoch = %v, %v; want nothing stored", stored, err)
	}
	if err := m.SetMembers(ctx, channel, []uuid.UUID{user}); err != nil {
		t.Fatal(err)
	}
	if ids, hit, err := m.Members(ctx, channel); err != nil || hit || len(ids) != 0 {
		t.Errorf("Members = %v, hit %v, %v; want a miss: the caller must read Postgres", ids, hit, err)
	}
	if err := m.InvalidateMembers(ctx, channel); err != nil {
		t.Errorf("InvalidateMembers: %v", err)
	}
}

func TestMemory_PublishWithNoSubscriberDropsTheFrame(t *testing.T) {
	m, _ := newMemory(t, Options{})
	ctx := context.Background()
	topic := "ch:" + uuid.NewString()

	if err := m.SPublish(ctx, topic, []byte("nobody listens")); err != nil {
		t.Fatalf("SPublish: %v", err)
	}
	sub, err := m.SSubscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	sub.Add(topic)
	if err := m.SPublish(ctx, topic, []byte("heard")); err != nil {
		t.Fatal(err)
	}
	if f := recv(t, sub); string(f.Data) != "heard" {
		t.Errorf("first frame = %q, want only the one published after Add (no replay)", f.Data)
	}
}

// Add is the confirmation: a frame published once it has returned is delivered, with no
// wait for the store to acknowledge, because there is no store.
func TestMemory_AddTakesEffectBeforeItReturns(t *testing.T) {
	m, _ := newMemory(t, Options{})
	ctx := context.Background()
	for i := range 200 {
		topic := fmt.Sprintf("ch:%d:%s", i, uuid.NewString())
		sub, err := m.SSubscribe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sub.Add(topic)
		if n := m.Subscribers(topic); n != 1 {
			t.Fatalf("Subscribers right after Add = %d, want 1", n)
		}
		if err := m.SPublish(ctx, topic, []byte("x")); err != nil {
			t.Fatal(err)
		}
		if f := recv(t, sub); f.Topic != topic {
			t.Fatalf("frame on %q, want %q", f.Topic, topic)
		}
		sub.Close()
		if n := m.Subscribers(topic); n != 0 {
			t.Fatalf("Subscribers after Close = %d, want 0", n)
		}
	}
}

// The publisher may reuse its buffer, as with a network client.
func TestMemory_FramesAreCopiedOnPublish(t *testing.T) {
	m, _ := newMemory(t, Options{})
	ctx := context.Background()
	topic := "u:" + uuid.NewString()
	sub, _ := m.SSubscribe(ctx)
	defer sub.Close()
	sub.Add(topic)

	buf := []byte("original")
	if err := m.SPublish(ctx, topic, buf); err != nil {
		t.Fatal(err)
	}
	copy(buf, "CLOBBER!")
	if f := recv(t, sub); string(f.Data) != "original" {
		t.Errorf("frame = %q, want the bytes as they were when published", f.Data)
	}
}

// A consumer that stopped reading must not stall the publisher (the relay publishes
// from the outbox transaction) nor Close.
func TestMemory_ASlowSubscriberNeverBlocksThePublisherOrClose(t *testing.T) {
	m, _ := newMemory(t, Options{})
	ctx := context.Background()
	topic := "ch:" + uuid.NewString()
	slow, _ := m.SSubscribe(ctx)
	fast, _ := m.SSubscribe(ctx)
	defer fast.Close()
	slow.Add(topic)
	fast.Add(topic)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 3 * subscriptionBuffer {
			if err := m.SPublish(ctx, topic, []byte{byte(i)}); err != nil {
				t.Errorf("SPublish: %v", err)
				return
			}
			recv(t, fast) // the fast one keeps up and sees every frame
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a subscriber that does not read blocked the publisher")
	}

	closed := make(chan struct{})
	go func() { slow.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close of a subscriber that does not read blocked")
	}
}

func TestMemory_CloseEndsEverySubscriptionAndRefusesNewWork(t *testing.T) {
	m := NewMemory(Options{})
	ctx := context.Background()
	sub, _ := m.SSubscribe(ctx)
	sub.Add("ch:x")

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Error("a frame arrived after Close")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("C stayed open after the cache was closed")
	}
	sub.Add("ch:y") // after Close: a no-op, as for Valkey
	if n := m.Subscribers("ch:y"); n != 0 {
		t.Errorf("Add after Close followed a topic (%d subscribers)", n)
	}
	for name, err := range map[string]error{
		"Ping":        m.Ping(ctx),
		"SPublish":    m.SPublish(ctx, "ch:x", nil),
		"PutOAuth":    m.PutOAuthState(ctx, "s", OAuthState{}),
		"IssueTicket": func() error { _, err := m.IssueTicket(ctx, TicketRecord{}); return err }(),
		"SSubscribe":  func() error { _, err := m.SSubscribe(ctx); return err }(),
	} {
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Errorf("%s after Close: err = %v, want a closed error", name, err)
		}
	}
}
