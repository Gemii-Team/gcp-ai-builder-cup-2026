package cache

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"kiwi/internal/platform/errs"
)

// The behaviour the gateway, the sign-in flow and the relay rely on, run against every
// adapter: the Valkey one on a real Valkey, and the in-process one. A test here that
// passes for one and fails for the other is a difference the app would meet in
// production, so it is a bug of the adapter, not of the test.

type adapter struct {
	name string
	open func(t *testing.T, opt Options) Cache
	// subscribers is how many open subscriptions follow topic, as the store confirms it:
	// PUBSUB SHARDNUMSUB for Valkey, whose SSUBSCRIBE is asynchronous, and the broker's
	// own count for the memory cache.
	subscribers func(t *testing.T, c Cache, topic string) int
}

func adapters() []adapter {
	return []adapter{
		{
			name: "valkey",
			open: func(t *testing.T, opt Options) Cache { return open(t, opt) },
			subscribers: func(t *testing.T, c Cache, topic string) int {
				return int(shardSubscribers(t, c.(*Valkey), topic))
			},
		},
		{
			name: "memory",
			open: func(t *testing.T, opt Options) Cache {
				m := NewMemory(opt)
				t.Cleanup(func() { _ = m.Close() })
				return m
			},
			subscribers: func(_ *testing.T, c Cache, topic string) int { return c.(*Memory).Subscribers(topic) },
		},
	}
}

func eachAdapter(t *testing.T, fn func(t *testing.T, a adapter)) {
	t.Helper()
	for _, a := range adapters() {
		t.Run(a.name, func(t *testing.T) { fn(t, a) })
	}
}

func recv(t *testing.T, sub Subscription) Frame {
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

func TestContract_RedeemTicketIsSingleUse(t *testing.T) {
	eachAdapter(t, func(t *testing.T, a adapter) {
		c := a.open(t, Options{})
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

		// Sixteen racing redeems hand the record to exactly one.
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
		if _, err := c.RedeemTicket(ctx, "never-issued"); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("unknown ticket: err = %v, want ErrNotFound", err)
		}
		if r, err := c.RedeemTicket(ctx, other); err != nil || r != rec {
			t.Errorf("redeem of the other ticket = %v, %v", r, err)
		}
	})
}

func TestContract_OAuthStateIsTakenOnce(t *testing.T) {
	eachAdapter(t, func(t *testing.T, a adapter) {
		c := a.open(t, Options{})
		ctx := context.Background()
		state := uuid.NewString()
		want := OAuthState{Verifier: "pkce-verifier", ReturnTo: "/channels/general", Nonce: "nonce-of-the-flow", Consent: true}

		if err := c.PutOAuthState(ctx, state, want); err != nil {
			t.Fatalf("PutOAuthState: %v", err)
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
	})
}

func TestContract_SPublishReachesSubscriber_AndRemoveStops(t *testing.T) {
	eachAdapter(t, func(t *testing.T, a adapter) {
		c := a.open(t, Options{})
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
			eventually(t, "the subscription of "+topic+" to be confirmed", func() bool { return a.subscribers(t, c, topic) == 1 })
		}

		// Frames are binary protobuf: bytes that are not valid UTF-8 survive.
		payload := []byte{0x00, 0xff, 0xfe, '\n', 0x08, 0x96, 0x01}
		if err := c.SPublish(ctx, topicA, payload); err != nil {
			t.Fatalf("SPublish: %v", err)
		}
		if f := recv(t, sub); f.Topic != topicA || !bytes.Equal(f.Data, payload) {
			t.Fatalf("frame = %q %x, want %q %x", f.Topic, f.Data, topicA, payload)
		}

		// After Remove a later publish is not received. A is published before B, so had
		// A been delivered it would arrive ahead of B.
		sub.Remove(topicA)
		sub.Remove() // no topics: a no-op, never "unsubscribe from everything"
		eventually(t, "the unsubscription of "+topicA+" to be confirmed", func() bool { return a.subscribers(t, c, topicA) == 0 })
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

		// Close releases the subscription, ends C, and may be repeated.
		sub.Close()
		sub.Close()
		eventually(t, "the unsubscription on Close", func() bool { return a.subscribers(t, c, topicB) == 0 })
		deadline := time.After(10 * time.Second)
		for more := true; more; {
			select {
			case _, more = <-sub.C():
			case <-deadline:
				t.Fatal("C was not closed by Close")
			}
		}
	})
}

func TestContract_SubscriptionsAreIndependent(t *testing.T) {
	eachAdapter(t, func(t *testing.T, a adapter) {
		c := a.open(t, Options{})
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
			return a.subscribers(t, c, shared) == 2 && a.subscribers(t, c, onlyOne) == 1
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
	})
}

// Frames of one topic reach a subscriber in publish order, which the gateway's seq
// resync assumes for the frames it does not drop.
func TestContract_FramesOfATopicKeepTheirOrder(t *testing.T) {
	eachAdapter(t, func(t *testing.T, a adapter) {
		c := a.open(t, Options{})
		ctx := context.Background()
		topic := "ch:" + uuid.NewString()
		sub, err := c.SSubscribe(ctx)
		if err != nil {
			t.Fatalf("SSubscribe: %v", err)
		}
		defer sub.Close()
		sub.Add(topic)
		eventually(t, "the subscription to be confirmed", func() bool { return a.subscribers(t, c, topic) == 1 })

		const n = 200
		for i := range n {
			if err := c.SPublish(ctx, topic, []byte{byte(i)}); err != nil {
				t.Fatalf("SPublish: %v", err)
			}
		}
		for i := range n {
			if f := recv(t, sub); len(f.Data) != 1 || f.Data[0] != byte(i) {
				t.Fatalf("frame %d = %v, want %d: frames arrived out of order", i, f.Data, byte(i))
			}
		}
	})
}
