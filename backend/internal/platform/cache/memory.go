package cache

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"kiwi/internal/platform/errs"
)

const (
	// maxPending bounds the one-time entries (tickets, login states) a Memory holds.
	// Both are issued to callers that may not be signed in yet (a login state is made
	// by an anonymous request), so a flood must not grow the process without limit. At
	// the bound the oldest entry is dropped: new logins keep working and the one that
	// loses its state is asked to sign in again.
	maxPending = 10_000
	// sweepEvery is how often a put looks for expired entries. Takes drop what they
	// find expired on their own; this only collects what nobody came back for.
	sweepEvery = time.Minute
	// subscriptionBuffer is how many frames a Subscription queues for a consumer that is
	// slower than the publishers; the next frames are dropped (frames are hints).
	subscriptionBuffer = 1024
)

// Memory is the in-process adapter of Cache, for one process that runs every serving
// role (config refuses it otherwise). Tickets, login states and the fan-out are held
// by the process, so a restart loses what is in flight: a sign-in that was in
// progress, a ticket that was not yet redeemed (the client asks for another), and the
// frames of a few seconds (the clients resync by seq).
//
// It keeps no sessions and does not implement Sessions: those must survive a
// restart, so a memory deployment keeps them in Postgres (identity.NewSessions).
//
// It caches no members either. Members always misses, so chat.NewMembership reads
// Postgres on every check and a revocation is effective at once, in this process and
// in a second one that overlaps it during a rollout. That costs one short transaction
// per check, which is cheap for the ten people this profile serves; a cache would add
// a staleness window to bound and nothing else.
type Memory struct {
	opt Options
	now func() time.Time // replaced by tests to move time

	tickets pending[TicketRecord]
	oauth   pending[OAuthState]
	broker  *broker

	closed atomic.Bool
}

var _ Cache = (*Memory)(nil)

// NewMemory returns an empty in-process cache. A zero field of opt takes its default;
// only TicketTTL applies, since a Memory keeps no sessions.
func NewMemory(opt Options) *Memory {
	return &Memory{
		opt:     opt.WithDefaults(),
		now:     time.Now,
		tickets: pending[TicketRecord]{items: map[string]expiring[TicketRecord]{}},
		oauth:   pending[OAuthState]{items: map[string]expiring[OAuthState]{}},
		broker:  newBroker(),
	}
}

// Ping always answers while the cache is open.
func (m *Memory) Ping(context.Context) error {
	if m.closed.Load() {
		return errors.New("cache: memory cache is closed")
	}
	return nil
}

// Close closes every open Subscription and refuses further use. It is safe to call
// twice.
func (m *Memory) Close() error {
	if m.closed.CompareAndSwap(false, true) {
		m.broker.closeAll()
	}
	return nil
}

// expiring is a value that can be taken until expires.
type expiring[T any] struct {
	v       T
	expires time.Time
}

// pending holds single-use values by key, each for a limited time. The zero value is
// not usable; build it with a map.
type pending[T any] struct {
	mu        sync.Mutex
	items     map[string]expiring[T]
	nextSweep time.Time
}

// put stores v under key until expires. At maxPending entries it first collects the
// expired ones, then drops the entry that expires first.
func (p *pending[T]) put(key string, v T, now, expires time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.After(p.nextSweep) || len(p.items) >= maxPending {
		p.sweepLocked(now)
		p.nextSweep = now.Add(sweepEvery)
	}
	if _, replacing := p.items[key]; !replacing && len(p.items) >= maxPending {
		var oldest string
		var oldestExpires time.Time
		for k, e := range p.items {
			if oldest == "" || e.expires.Before(oldestExpires) {
				oldest, oldestExpires = k, e.expires
			}
		}
		delete(p.items, oldest)
	}
	p.items[key] = expiring[T]{v: v, expires: expires}
}

// take removes the value under key and returns it if it has not expired. Under the
// lock the lookup and the delete are one step, so of any number of concurrent takes
// exactly one gets the value.
func (p *pending[T]) take(key string, now time.Time) (v T, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, found := p.items[key]
	if !found {
		return v, false
	}
	delete(p.items, key)
	if !now.Before(e.expires) {
		return v, false
	}
	return e.v, true
}

func (p *pending[T]) sweepLocked(now time.Time) {
	for k, e := range p.items {
		if !now.Before(e.expires) {
			delete(p.items, k)
		}
	}
}

func (p *pending[T]) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.items)
}

// IssueTicket implements Tickets: 128 random bits, valid for Options.TicketTTL.
func (m *Memory) IssueTicket(_ context.Context, t TicketRecord) (string, error) {
	if m.closed.Load() {
		return "", errors.New("cache: issue ticket: memory cache is closed")
	}
	ticket := rand.Text()
	now := m.now()
	m.tickets.put(ticket, t, now, now.Add(m.opt.TicketTTL))
	return ticket, nil
}

// RedeemTicket implements Tickets: single use, so of concurrent redeems one wins.
func (m *Memory) RedeemTicket(_ context.Context, ticket string) (TicketRecord, error) {
	t, ok := m.tickets.take(ticket, m.now())
	if !ok {
		return TicketRecord{}, fmt.Errorf("cache: ticket: %w", errs.ErrNotFound)
	}
	return t, nil
}

// PutOAuthState implements OAuthStates: valid for ten minutes.
func (m *Memory) PutOAuthState(_ context.Context, state string, v OAuthState) error {
	if state == "" {
		return fmt.Errorf("cache: empty oauth state: %w", errs.ErrInvalid)
	}
	if m.closed.Load() {
		return errors.New("cache: put oauth state: memory cache is closed")
	}
	now := m.now()
	m.oauth.put(state, v, now, now.Add(oauthStateTTL))
	return nil
}

// TakeOAuthState implements OAuthStates: single use, so a replayed callback finds
// nothing.
func (m *Memory) TakeOAuthState(_ context.Context, state string) (OAuthState, error) {
	v, ok := m.oauth.take(state, m.now())
	if !ok {
		return OAuthState{}, fmt.Errorf("cache: oauth state: %w", errs.ErrNotFound)
	}
	return v, nil
}

// Members implements Members: nothing is cached, so it always misses. See Memory.
func (*Memory) Members(context.Context, uuid.UUID) ([]uuid.UUID, bool, error) {
	return nil, false, nil
}

// SetMembers implements Members: it stores nothing.
func (*Memory) SetMembers(context.Context, uuid.UUID, []uuid.UUID) error { return nil }

// MembersEpoch implements Members: with nothing cached there is nothing to invalidate.
func (*Memory) MembersEpoch(context.Context, uuid.UUID) (int64, error) { return 0, nil }

// SetMembersAtEpoch implements Members: it stores nothing and reports so.
func (*Memory) SetMembersAtEpoch(context.Context, uuid.UUID, int64, []uuid.UUID) (bool, error) {
	return false, nil
}

// InvalidateMembers implements Members: there is nothing to drop.
func (*Memory) InvalidateMembers(context.Context, uuid.UUID) error { return nil }

// SPublish implements Fanout: the frame reaches every Subscription of this process
// that follows topic, in publish order per publisher. It never blocks on a slow
// subscriber: a subscription whose queue is full drops the frame.
func (m *Memory) SPublish(_ context.Context, topic string, frame []byte) error {
	if m.closed.Load() {
		return fmt.Errorf("cache: spublish %s: memory cache is closed", topic)
	}
	m.broker.publish(topic, frame)
	return nil
}

// SSubscribe implements Fanout. Unlike a remote store it cannot be unreachable, and
// Add takes effect before it returns: a frame published after Add returns is
// delivered. That is the confirmation the gateway's tests wait for on Valkey.
func (m *Memory) SSubscribe(context.Context) (Subscription, error) {
	if m.closed.Load() {
		return nil, errors.New("cache: open subscription: memory cache is closed")
	}
	return m.broker.subscribe()
}

// Subscribers is how many open Subscriptions follow topic. It is what Valkey's
// PUBSUB SHARDNUMSUB is to the Valkey adapter, for tests that wait on a
// subscription; nothing in the product reads it.
func (m *Memory) Subscribers(topic string) int { return m.broker.subscribers(topic) }

// broker is the in-process sharded pub/sub: a map from topic to the subscriptions
// that follow it.
type broker struct {
	mu     sync.RWMutex
	topics map[string]map[*memorySubscription]struct{}
	subs   map[*memorySubscription]struct{}
	closed bool
}

func newBroker() *broker {
	return &broker{
		topics: map[string]map[*memorySubscription]struct{}{},
		subs:   map[*memorySubscription]struct{}{},
	}
}

func (b *broker) subscribe() (*memorySubscription, error) {
	s := &memorySubscription{
		b:    b,
		in:   make(chan Frame, subscriptionBuffer),
		out:  make(chan Frame),
		done: make(chan struct{}),
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("cache: open subscription: memory cache is closed")
	}
	b.subs[s] = struct{}{}
	go s.pump()
	return s, nil
}

func (b *broker) publish(topic string, frame []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	followers := b.topics[topic]
	if len(followers) == 0 {
		return // a topic nobody follows drops the frame
	}
	// The caller may reuse its buffer once SPublish returns, as with a network client.
	data := slices.Clone(frame)
	for s := range followers {
		s.offer(Frame{Topic: topic, Data: data})
	}
}

func (b *broker) subscribers(topic string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.topics[topic])
}

func (b *broker) closeAll() {
	b.mu.Lock()
	b.closed = true
	subs := make([]*memorySubscription, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.mu.Unlock()
	for _, s := range subs {
		s.Close()
	}
}

// memorySubscription follows topics of one broker. Frames are queued by the
// publishers and handed to the consumer by a goroutine of their own, so a consumer
// that is busy never blocks a publisher.
type memorySubscription struct {
	b      *broker
	in     chan Frame
	out    chan Frame
	done   chan struct{}
	once   sync.Once
	warned atomic.Bool
}

// offer queues f without blocking; a full queue drops it.
func (s *memorySubscription) offer(f Frame) {
	select {
	case s.in <- f:
	default:
		if s.warned.CompareAndSwap(false, true) {
			slog.Warn("cache: a memory subscriber is not keeping up; frames are dropped and clients resync",
				"queue", subscriptionBuffer)
		}
	}
}

// pump hands queued frames to C until Close. A consumer that stopped reading cannot
// block Close: done releases the send.
func (s *memorySubscription) pump() {
	defer close(s.out)
	for {
		select {
		case f := <-s.in:
			select {
			case s.out <- f:
			case <-s.done:
				return
			}
		case <-s.done:
			return
		}
	}
}

// Add implements Subscription. It takes effect before it returns.
func (s *memorySubscription) Add(topics ...string) {
	if len(topics) == 0 {
		return
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if _, open := s.b.subs[s]; !open {
		return
	}
	for _, topic := range topics {
		set := s.b.topics[topic]
		if set == nil {
			set = map[*memorySubscription]struct{}{}
			s.b.topics[topic] = set
		}
		set[s] = struct{}{}
	}
}

// Remove implements Subscription. With no topics it does nothing.
func (s *memorySubscription) Remove(topics ...string) {
	if len(topics) == 0 {
		return
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	for _, topic := range topics {
		s.b.unfollowLocked(s, topic)
	}
}

// C implements Subscription.
func (s *memorySubscription) C() <-chan Frame { return s.out }

// Close implements Subscription: it leaves every topic and ends C.
func (s *memorySubscription) Close() {
	s.once.Do(func() {
		s.b.mu.Lock()
		delete(s.b.subs, s)
		for topic := range s.b.topics {
			s.b.unfollowLocked(s, topic)
		}
		s.b.mu.Unlock()
		close(s.done)
	})
}

// unfollowLocked removes s from topic and forgets a topic nobody follows.
func (b *broker) unfollowLocked(s *memorySubscription, topic string) {
	set := b.topics[topic]
	delete(set, s)
	if len(set) == 0 {
		delete(b.topics, topic)
	}
}
