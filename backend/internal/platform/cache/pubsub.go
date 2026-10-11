package cache

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/redis/go-redis/v9"
)

// Frame is one message received on a topic.
type Frame struct {
	Topic string
	Data  []byte
}

// SPublish sends frame to every subscriber of topic (SPUBLISH, sharded pub/sub),
// for example ch:{channelId} or u:{userId}. A topic nobody follows drops it:
// frames are hints, Postgres is the source of truth.
func (c *Cache) SPublish(ctx context.Context, topic string, frame []byte) error {
	if err := c.rdb.SPublish(ctx, topic, frame).Err(); err != nil {
		return fmt.Errorf("cache: spublish %s: %w", topic, err)
	}
	return nil
}

// Subscription follows sharded topics over one dedicated connection. go-redis
// reconnects it and resubscribes to the current topics when the connection
// drops; frames published meanwhile are lost, which the clients' seq resync
// absorbs.
type Subscription struct {
	ps   *redis.PubSub
	out  chan Frame
	done chan struct{}
	once sync.Once
}

// SSubscribe opens a Subscription with no topics yet; Add them. It fails if
// Valkey cannot be reached.
func (c *Cache) SSubscribe(ctx context.Context) (*Subscription, error) {
	ps := c.rdb.SSubscribe(ctx)
	// Dial now so an unreachable Valkey is an error here, not a silent
	// subscription that never receives.
	if err := ps.Ping(ctx); err != nil {
		_ = ps.Close()
		return nil, fmt.Errorf("cache: open subscription: %w", err)
	}
	s := &Subscription{ps: ps, out: make(chan Frame), done: make(chan struct{})}
	go s.pump(ps.Channel())
	return s, nil
}

// pump turns go-redis messages into Frames until Close ends the PubSub. A
// consumer that stopped reading cannot block Close: done releases the send.
func (s *Subscription) pump(in <-chan *redis.Message) {
	defer close(s.out)
	for m := range in {
		select {
		case s.out <- Frame{Topic: m.Channel, Data: []byte(m.Payload)}:
		case <-s.done:
		}
	}
}

// Add starts following topics. It returns once the command is sent, not once
// Valkey confirms it. A send that fails is logged and retried by go-redis when
// it reconnects.
func (s *Subscription) Add(topics ...string) {
	if len(topics) == 0 || s.closed() {
		return
	}
	if err := s.ps.SSubscribe(context.Background(), topics...); err != nil {
		slog.Warn("cache: ssubscribe not sent; topics are resubscribed on reconnect", "topics", len(topics), "err", err)
	}
}

// Remove stops following topics once Valkey has processed the command; frames
// already on their way may still arrive. With no topics it does nothing (it
// never unsubscribes from all).
func (s *Subscription) Remove(topics ...string) {
	if len(topics) == 0 || s.closed() {
		return
	}
	if err := s.ps.SUnsubscribe(context.Background(), topics...); err != nil {
		slog.Warn("cache: sunsubscribe not sent", "topics", len(topics), "err", err)
	}
}

// C delivers the frames of every followed topic, in publish order per topic.
// It is closed after Close. A consumer that falls behind for a minute loses
// frames (go-redis drops them), so read it promptly.
func (s *Subscription) C() <-chan Frame { return s.out }

// Close drops the connection and every topic and then closes C. It may be
// called more than once.
func (s *Subscription) Close() {
	s.once.Do(func() {
		close(s.done)
		_ = s.ps.Close()
	})
}

func (s *Subscription) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}
