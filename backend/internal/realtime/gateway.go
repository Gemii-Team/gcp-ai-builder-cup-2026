// Package realtime is the WebSocket gateway: it authenticates a client with a
// one-time ticket, lets it follow the channels it belongs to and forwards the
// binary protobuf frames that arrive on Valkey's sharded pub/sub topics
// ch:{channelId} and u:{userId}. Frames are hints; Postgres is the source of
// truth and clients resync by seq. The gateway holds no business logic and
// publishes nothing.
//
// Revocation is best effort, because the API enforces access and a frame only
// hints. When a MembershipChanged{REMOVED} frame arrives on u:{userId} the user's
// connections stop following the channel and the cached member set is dropped, but
// two windows remain. Frames already queued for such a connection (up to the send
// buffer, 256 for a client that reads slowly) are still written after the
// revocation. And a REMOVED hint lost while the Valkey subscription reconnects is
// not recovered: nothing re-validates a connection's channels periodically, so
// the user keeps following the channel until the connection ends.
package realtime

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	realtimev1 "kiwi/gen/kiwi/realtime/v1"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/errs"
)

// Defaults for the zero fields of Options.
const (
	DefaultSendBuffer  = 256
	DefaultIdleTimeout = 60 * time.Second
)

// invalidateTimeout bounds the cache write of a revocation, which holds up the
// frames behind it.
const invalidateTimeout = 5 * time.Second

// closeGrace is how long Shutdown lets close handshakes finish after its context
// has ended, before it cuts the connections still open.
const closeGrace = 500 * time.Millisecond

// Deps are what the gateway needs; neither may be nil.
type Deps struct {
	// Cache redeems tickets, carries the pub/sub subscription and holds the
	// members:{channel} sets.
	Cache *cache.Cache
	// Membership decides which channels a user may follow. chat.NewMembership is
	// the production implementation.
	Membership authz.Membership
}

// Options tunes the gateway; a zero SendBuffer or IdleTimeout takes its default.
type Options struct {
	// SendBuffer is the frames queued per connection. A connection whose queue is
	// full is closed.
	SendBuffer int
	// IdleTimeout closes a connection that sent no frame for this long. Clients
	// send Ping every 25 s.
	IdleTimeout time.Duration
	// DrainTimeout is how long Shutdown keeps serving after readiness fails, for the
	// load balancer to stop routing here. Zero skips the wait.
	DrainTimeout time.Duration
	// OriginPatterns are the extra origins (host or host:port patterns, matched
	// case-insensitively with path.Match against the Origin header; a pattern with
	// "://" is matched against scheme://host) that may open a connection. The
	// request's own host is always allowed, so empty means same-origin only. The app
	// passes the host of its public URL and, outside prod, the Vite dev origin.
	OriginPatterns []string
}

// Gateway serves GET /ws?ticket=… as an http.Handler.
type Gateway struct {
	cache   *cache.Cache
	members authz.Membership
	opt     Options
	hub     *hub

	stopping atomic.Bool
	stopped  chan struct{} // closed when Shutdown has finished
}

// New returns a Gateway. It opens its Valkey subscription when the first client
// connects, so it can be built while Valkey is still unreachable.
func New(deps Deps, opt Options) *Gateway {
	if opt.SendBuffer <= 0 {
		opt.SendBuffer = DefaultSendBuffer
	}
	if opt.IdleTimeout <= 0 {
		opt.IdleTimeout = DefaultIdleTimeout
	}
	g := &Gateway{cache: deps.Cache, members: deps.Membership, opt: opt, stopped: make(chan struct{})}
	g.hub = newHub(deps.Cache.SSubscribe, g.onFrame)
	return g
}

// ServeHTTP redeems the ticket, upgrades the request and serves the connection
// until it ends. An unknown, used or missing ticket is 401, and a gateway that is
// shut down or cannot reach Valkey is 503.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.hub.isClosed() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	ticket := r.URL.Query().Get("ticket")
	if ticket == "" {
		http.Error(w, "ticket required", http.StatusUnauthorized)
		return
	}
	who, err := g.cache.RedeemTicket(r.Context(), ticket)
	if errors.Is(err, errs.ErrNotFound) {
		http.Error(w, "invalid ticket", http.StatusUnauthorized)
		return
	}
	if err != nil {
		slog.Error("realtime: redeem ticket", "error", err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}

	// The ticket is consumed above, before Accept's origin check, so a request
	// refused with 403 has used its ticket up: the client fetches a new ticket for
	// every connect, so nothing is lost.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
		OriginPatterns:  g.opt.OriginPatterns, // the request's own host is always allowed
	})
	if err != nil {
		return // Accept has written the HTTP error
	}
	// The library's default of 32 KB is undocumented API behaviour; the largest
	// legitimate frame is a Subscribe, about 38 bytes per channel id.
	ws.SetReadLimit(maxClientFrame)

	c := newConn(context.WithoutCancel(r.Context()), ws, who, g.opt.SendBuffer)
	if err := g.hub.attach(c.ctx, c); err != nil {
		c.cancel()
		if !errors.Is(err, errHubClosed) {
			slog.Error("realtime: open subscription", "error", err)
		}
		_ = ws.Close(websocket.StatusTryAgainLater, "unavailable")
		return
	}
	g.serve(c)
}

// Ready reports whether the gateway takes new connections; it turns false when
// Shutdown starts, so the readiness probe fails and the load balancer drains it.
func (g *Gateway) Ready() bool { return !g.stopping.Load() }

// Shutdown fails readiness, keeps serving for Options.DrainTimeout (or until ctx
// ends), then closes every connection with StatusGoingAway, so clients reconnect
// elsewhere and resync, and releases the Valkey subscription. A close handshake
// needs the client's answer, which one that stopped reading never gives (the
// library waits 5 s for it), so once ctx has ended the handshakes get closeGrace
// to finish and the connections still open are then cut. It returns when the
// connections are gone; a second call waits for the first. The Cache is the
// caller's to close.
func (g *Gateway) Shutdown(ctx context.Context) {
	if !g.stopping.CompareAndSwap(false, true) {
		select {
		case <-g.stopped:
		case <-ctx.Done():
		}
		return
	}
	defer close(g.stopped)

	if g.opt.DrainTimeout > 0 {
		drain := time.NewTimer(g.opt.DrainTimeout)
		select {
		case <-drain.C:
		case <-ctx.Done():
		}
		drain.Stop()
	}

	conns := g.hub.close()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		var wg sync.WaitGroup
		for _, c := range conns {
			wg.Go(func() { c.closeWith(websocket.StatusGoingAway, "server shutting down") })
		}
		wg.Wait()
	}()
	select {
	case <-closed:
	case <-ctx.Done():
		grace := time.NewTimer(closeGrace)
		select {
		case <-closed:
		case <-grace.C:
			// Cancelling a connection's context closes its socket even in the middle
			// of a close handshake, which CloseNow does not.
			for _, c := range conns {
				c.cancel()
			}
			<-closed
		}
		grace.Stop()
	}
	g.hub.stop()
}

// subscribe follows the channels of ids that c's user belongs to. Ids that are not
// uuids, channels the user is not a member of and channels whose membership cannot
// be checked are skipped: the answer is "no" unless it is a confirmed "yes".
func (g *Gateway) subscribe(c *conn, ids []string) {
	ctx := authz.WithWorkspace(c.ctx, c.workspace)
	for _, raw := range ids {
		channel, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		for {
			epoch := g.hub.epochNow()
			ok, err := g.members.IsMember(ctx, channel, c.user)
			if err != nil {
				if c.ctx.Err() == nil {
					slog.Warn("realtime: membership check failed, channel not followed",
						"channel_id", channel, "user_id", c.user, "error", err)
				}
				break
			}
			if !ok || g.hub.subscribe(c, channel, epoch) {
				break
			}
			// A revocation landed while the check ran, so the answer may predate it:
			// ask again against the invalidated cache.
		}
	}
}

// onFrame routes one frame from Valkey. A frame on u:{userId} may carry a
// revocation, which is acted on before the frame reaches the user.
func (g *Gateway) onFrame(f cache.Frame) {
	if id, ok := strings.CutPrefix(f.Topic, userPrefix); ok {
		if user, err := uuid.Parse(id); err == nil {
			g.onUserFrame(user, f.Data)
		}
	}
	g.hub.deliver(f.Topic, f.Data)
}

// onUserFrame handles a MembershipChanged{REMOVED} for user: it invalidates
// members:{channel} so the next check reads Postgres, then stops the user's
// connections from following the channel. Other frames are only forwarded.
func (g *Gateway) onUserFrame(user uuid.UUID, data []byte) {
	var ev realtimev1.ServerEvent
	if proto.Unmarshal(data, &ev) != nil {
		return
	}
	change := ev.GetMembershipChanged()
	if change.GetChange() != chatv1.MembershipChange_MEMBERSHIP_CHANGE_REMOVED {
		return
	}
	channel, err := uuid.Parse(change.GetChannelId())
	if err != nil {
		slog.Warn("realtime: removal frame without a channel id", "user_id", user)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), invalidateTimeout)
	defer cancel()
	if err := g.cache.InvalidateMembers(ctx, channel); err != nil {
		// The connections are still dropped below; only a re-subscribe could be
		// answered from the stale set, until the chat service next rewrites it.
		slog.Error("realtime: invalidate members after removal", "channel_id", channel, "error", err)
	}
	g.hub.revoke(user, channel)
}
