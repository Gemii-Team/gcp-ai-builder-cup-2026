package bus

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const defaultMaxAttempts = 5

// defaultBackoff is the wait before the second, third, fourth and later attempts.
var defaultBackoff = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute}

// MemoryOptions configures a Memory. The zero value is the production setting.
type MemoryOptions struct {
	// MaxAttempts is how many deliveries a consumer gets before the message is dead
	// lettered; 0 means 5.
	MaxAttempts int
	// Backoff is the wait before each redelivery: Backoff[0] before the second
	// attempt, Backoff[1] before the third and so on, the last entry repeating. Empty
	// means 1s, 5s, 30s, 2m. Tests pass []time.Duration{0} to retry at once.
	Backoff []time.Duration
	// OnDead is called when a consumer has failed MaxAttempts times, with the last
	// message and error. Consumers of one message settle in parallel, so OnDead can be
	// called concurrently, one goroutine per consumer, and must be safe for that.
	// Empty means the failure is logged at ERROR.
	OnDead func(consumer string, m Message, err error)
}

// Memory is the in-process bus. Publish delivers synchronously: it runs every
// consumer registered for the topic, retries failures in process, and returns once
// each of them has acked or been dead lettered. Nothing is stored, so a restart
// loses whatever was in flight; the outbox relay, which holds its row until Publish
// returns, is what makes that safe.
type Memory struct {
	maxAttempts int
	backoff     []time.Duration
	onDead      func(consumer string, m Message, err error)

	mu   sync.RWMutex
	subs []*subscription
}

var (
	_ Publisher  = (*Memory)(nil)
	_ Subscriber = (*Memory)(nil)
)

type subscription struct {
	name    string
	topics  []string
	handler Handler
}

// NewMemory returns an empty Memory.
func NewMemory(opt MemoryOptions) *Memory {
	m := &Memory{
		maxAttempts: opt.MaxAttempts,
		backoff:     slices.Clone(opt.Backoff),
		onDead:      opt.OnDead,
	}
	if m.maxAttempts <= 0 {
		m.maxAttempts = defaultMaxAttempts
	}
	if len(m.backoff) == 0 {
		m.backoff = defaultBackoff
	}
	if m.onDead == nil {
		m.onDead = logDead
	}
	return m
}

func logDead(consumer string, m Message, err error) {
	slog.Error("bus: message dead-lettered",
		"consumer", consumer, "topic", m.Topic, "message_id", m.ID, "attempts", m.Attempt, "error", err)
}

// Register attaches consumer to topics without blocking, so the app can register
// every job before the outbox relay starts publishing. Consumer names must be unique.
func (m *Memory) Register(consumer string, topics []string, h Handler) {
	m.add(consumer, topics, h)
}

// Subscribe registers the consumer, as Register does, and blocks until ctx is done;
// it then unregisters the consumer and returns nil. Deliveries already running are
// not interrupted.
func (m *Memory) Subscribe(ctx context.Context, consumer string, topics []string, h Handler) error {
	s := m.add(consumer, topics, h)
	<-ctx.Done()
	m.mu.Lock()
	m.subs = slices.DeleteFunc(m.subs, func(x *subscription) bool { return x == s })
	m.mu.Unlock()
	return nil
}

func (m *Memory) add(consumer string, topics []string, h Handler) *subscription {
	s := &subscription{name: consumer, topics: slices.Clone(topics), handler: h}
	m.mu.Lock()
	m.subs = append(m.subs, s)
	m.mu.Unlock()
	return s
}

// Publish blocks until every consumer registered for msg.Topic has acked it or used
// up its attempts (see MemoryOptions); an exhausted consumer is reported to OnDead
// and counts as settled, so a dead letter does not fail Publish. A topic without
// consumers returns nil. When ctx ends before every consumer has settled, Publish
// returns ctx.Err() once the running handlers have returned; the abandoned messages
// are not dead lettered, the caller still owns them.
func (m *Memory) Publish(ctx context.Context, msg Message) error {
	targets := m.subscribers(msg.Topic)
	if len(targets) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	var interrupted atomic.Bool
	run := func(s *subscription) {
		if !m.deliver(ctx, s, msg) {
			interrupted.Store(true)
		}
	}
	for _, s := range targets[1:] {
		wg.Go(func() { run(s) })
	}
	run(targets[0])
	wg.Wait()

	if interrupted.Load() {
		return ctx.Err()
	}
	return nil
}

func (m *Memory) subscribers(topic string) []*subscription {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*subscription
	for _, s := range m.subs {
		if slices.Contains(s.topics, topic) {
			out = append(out, s)
		}
	}
	return out
}

// deliver runs s's attempts for msg and reports whether s settled (acked or dead
// lettered) rather than being cut short by ctx.
func (m *Memory) deliver(ctx context.Context, s *subscription, msg Message) bool {
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		msg.Attempt = attempt
		err := s.handler(ctx, msg)
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		if attempt >= m.maxAttempts {
			m.onDead(s.name, msg, err)
			return true
		}
		if !sleep(ctx, m.backoff[min(attempt, len(m.backoff))-1]) {
			return false
		}
	}
}

// sleep waits d and reports false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
