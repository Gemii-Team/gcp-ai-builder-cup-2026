package realtime

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"

	"kiwi/internal/platform/cache"
)

const (
	channelPrefix = "ch:"
	userPrefix    = "u:"
)

func channelTopic(id uuid.UUID) string { return channelPrefix + id.String() }
func userTopic(id uuid.UUID) string    { return userPrefix + id.String() }

var errHubClosed = errors.New("realtime: gateway is shut down")

// hub is the local registry of connections and the one Valkey subscription that
// feeds them. It maps each topic (ch:{channelId} for subscribed channels,
// u:{userId} for every connected user) to the local connections that follow it,
// and subscribes Valkey on the first of them and unsubscribes on the last.
type hub struct {
	open    func(context.Context) (*cache.Subscription, error)
	onFrame func(cache.Frame)

	mu     sync.RWMutex
	sub    *cache.Subscription // opened with the first connection
	topics map[string]map[*conn]struct{}
	conns  map[*conn]struct{}
	// epoch counts revocations. A Subscribe whose membership check began before one
	// cannot trust its answer, because the check may have read the set the
	// revocation invalidated.
	epoch    uint64
	closed   bool
	active   sync.WaitGroup // attached connections, until detach
	pumpDone chan struct{}
}

func newHub(open func(context.Context) (*cache.Subscription, error), onFrame func(cache.Frame)) *hub {
	return &hub{
		open:     open,
		onFrame:  onFrame,
		topics:   make(map[string]map[*conn]struct{}),
		conns:    make(map[*conn]struct{}),
		pumpDone: make(chan struct{}),
	}
}

// attach registers c and follows u:{user} for it, opening the Valkey subscription
// if c is the first connection. It fails once the hub is closed.
func (h *hub) attach(ctx context.Context, c *conn) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errHubClosed
	}
	if h.sub == nil {
		sub, err := h.open(ctx)
		if err != nil {
			return err
		}
		h.sub = sub
		go h.pump(sub)
	}
	h.conns[c] = struct{}{}
	h.active.Add(1)
	h.join(c, userTopic(c.user))
	return nil
}

// pump hands every frame of the subscription to onFrame until it is closed.
func (h *hub) pump(sub *cache.Subscription) {
	defer close(h.pumpDone)
	for f := range sub.C() {
		h.onFrame(f)
	}
}

// detach drops c from every topic and unsubscribes the ones it was last on. It
// does nothing for a connection that is not attached.
func (h *hub) detach(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.conns[c]; !ok {
		return
	}
	delete(h.conns, c)
	for topic := range c.topics {
		h.leave(c, topic)
	}
	clear(c.topics)
	h.leave(c, userTopic(c.user))
	h.active.Done()
}

// subscribe makes c follow channel. epoch is the value of epochNow before the
// membership check that allowed it; if a revocation has happened since, nothing is
// changed and false is returned so the caller checks again.
func (h *hub) subscribe(c *conn, channel uuid.UUID, epoch uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.epoch != epoch {
		return false
	}
	topic := channelTopic(channel)
	if _, attached := h.conns[c]; !attached {
		return true // closed meanwhile: nothing to follow
	}
	if _, already := c.topics[topic]; already {
		return true
	}
	c.topics[topic] = struct{}{}
	h.join(c, topic)
	return true
}

// revoke stops every connection of user from following channel and starts a new
// epoch. Call it after the channel's cached member set is invalidated.
func (h *hub) revoke(user, channel uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.epoch++
	topic := channelTopic(channel)
	for c := range h.topics[userTopic(user)] {
		if _, ok := c.topics[topic]; ok {
			delete(c.topics, topic)
			h.leave(c, topic)
		}
	}
}

func (h *hub) epochNow() uint64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.epoch
}

// deliver queues data on every connection that follows topic. It never blocks: a
// connection with a full queue is closed instead.
func (h *hub) deliver(topic string, data []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.topics[topic] {
		c.enqueue(data)
	}
}

// close refuses further connections and returns the ones attached. The Valkey
// subscription stays open until stop, so frames keep flowing while they close.
func (h *hub) close() []*conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	out := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		out = append(out, c)
	}
	return out
}

func (h *hub) isClosed() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.closed
}

// stop waits for every attached connection to detach, then closes the Valkey
// subscription and waits for its last frame to be handled.
func (h *hub) stop() {
	h.active.Wait()
	h.mu.Lock()
	sub := h.sub
	h.mu.Unlock()
	if sub != nil {
		sub.Close()
		<-h.pumpDone
	}
}

// join adds c to topic, subscribing Valkey if c is its first local follower. The
// caller holds mu, which keeps SSUBSCRIBE and SUNSUBSCRIBE in the order of the map.
func (h *hub) join(c *conn, topic string) {
	set := h.topics[topic]
	if set == nil {
		set = make(map[*conn]struct{})
		h.topics[topic] = set
		h.sub.Add(topic)
	}
	set[c] = struct{}{}
}

// leave removes c from topic, unsubscribing Valkey if c was its last local
// follower. The caller holds mu.
func (h *hub) leave(c *conn, topic string) {
	set, ok := h.topics[topic]
	if !ok {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(h.topics, topic)
		h.sub.Remove(topic)
	}
}
