// Package bus is the message-bus port. Delivery is at-least-once with pull
// semantics: a Handler that returns nil acks the message and one that returns an
// error nacks it, so handlers must be idempotent (see outbox.Once). Memory is the
// in-process adapter.
package bus

import "context"

// Message is one event on a topic. Attempt is 1 on the first delivery and grows by
// one per redelivery; the publisher's value is ignored. Handlers must treat a
// delivered Message, including Data and Attrs, as read-only.
type Message struct {
	ID    string
	Topic string
	// Key orders or partitions messages that belong together; adapters that do not
	// need it ignore it.
	Key   string
	Data  []byte
	Attrs map[string]string

	Attempt int
}

// Publisher sends a message to every consumer registered for its topic.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

// Handler processes one delivery: nil acks the message, an error nacks it and the
// adapter redelivers it with Attempt+1 until its attempts run out.
type Handler func(ctx context.Context, m Message) error

// Subscriber attaches a consumer, a durable named reader of the given topics, to the
// bus.
type Subscriber interface {
	// Subscribe delivers messages of topics to h until ctx is done, then returns.
	Subscribe(ctx context.Context, consumer string, topics []string, h Handler) error
}

// Job is one consumer a domain contributes to the bus: its name, the topics it reads
// and its handler. The composition root registers every Job.
type Job struct {
	Consumer string
	Topics   []string
	Handler  Handler
}

type keepAliveKey struct{}

// WithKeepAlive returns a context for a handler whose KeepAlive calls extend; adapters
// whose broker enforces an acknowledgement deadline inject the function that extends it.
func WithKeepAlive(ctx context.Context, extend func()) context.Context {
	return context.WithValue(ctx, keepAliveKey{}, extend)
}

// KeepAlive tells the broker that the handler is still working, so a long handler is
// not redelivered. It does nothing where the adapter has no deadline to extend, as in
// Memory.
func KeepAlive(ctx context.Context) {
	if extend, ok := ctx.Value(keepAliveKey{}).(func()); ok {
		extend()
	}
}
