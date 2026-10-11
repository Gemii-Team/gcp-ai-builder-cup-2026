package realtime

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	realtimev1 "kiwi/gen/kiwi/realtime/v1"
	"kiwi/internal/platform/cache"
)

// writeTimeout bounds one frame write. A client that stopped reading blocks the
// write once the kernel's buffers are full; the timeout ends the connection even
// when its queue is not full yet.
const writeTimeout = 10 * time.Second

// conn is one client's WebSocket. Frames for it go through send, a bounded queue
// drained by writeLoop, so a slow client can delay only itself.
type conn struct {
	ws        *websocket.Conn
	user      uuid.UUID
	workspace uuid.UUID

	send       chan []byte
	overflowed atomic.Bool

	// ctx ends the connection: the read and write loops and any membership check
	// in flight stop with it.
	ctx    context.Context
	cancel context.CancelFunc

	// topics are the ch:{channelId} topics the connection follows; the hub's lock
	// guards it.
	topics map[string]struct{}
}

func newConn(ctx context.Context, ws *websocket.Conn, who cache.TicketRecord, sendBuffer int) *conn {
	ctx, cancel := context.WithCancel(ctx)
	return &conn{
		ws:        ws,
		user:      who.UserID,
		workspace: who.WorkspaceID,
		send:      make(chan []byte, sendBuffer),
		ctx:       ctx,
		cancel:    cancel,
		topics:    make(map[string]struct{}),
	}
}

// enqueue queues a frame without blocking. A full queue means the client cannot
// keep up: the connection is ended and the client resyncs by seq on reconnect.
func (c *conn) enqueue(frame []byte) {
	select {
	case c.send <- frame:
	default:
		if c.overflowed.CompareAndSwap(false, true) {
			slog.Info("realtime: send buffer full, closing the connection",
				"user_id", c.user, "workspace_id", c.workspace, "buffer", cap(c.send))
			c.cancel()
		}
	}
}

// closeWith runs the close handshake with code and then ends the connection. It
// waits for the peer's reply, a few seconds at most.
func (c *conn) closeWith(code websocket.StatusCode, reason string) {
	_ = c.ws.Close(code, reason)
	c.cancel()
}

// serve runs c until it ends: client frames are read on the calling goroutine and
// queued frames written on a second one. It detaches c from the hub on the way out.
func (g *Gateway) serve(c *conn) {
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.writeLoop()
	}()

	g.readLoop(c)

	c.cancel()
	_ = c.ws.CloseNow()
	g.hub.detach(c)
	<-writerDone
}

func (c *conn) writeLoop() {
	defer c.cancel() // a connection that cannot be written to is over
	for {
		select {
		case <-c.ctx.Done():
			return
		case frame := <-c.send:
			ctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
			err := c.ws.Write(ctx, websocket.MessageBinary, frame)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// readLoop handles client frames until the connection ends. Every frame, Ping
// included, restarts the idle timer. Only Subscribe is acted on; Typing and the
// rest are ignored, because the gateway relays no typing or presence.
func (g *Gateway) readLoop(c *conn) {
	idle := time.AfterFunc(g.opt.IdleTimeout, func() {
		c.closeWith(websocket.StatusPolicyViolation, "idle timeout")
	})
	defer idle.Stop()

	for {
		typ, data, err := c.ws.Read(c.ctx)
		if err != nil {
			return
		}
		idle.Reset(g.opt.IdleTimeout)

		if typ != websocket.MessageBinary {
			c.closeWith(websocket.StatusUnsupportedData, "frames are binary protobuf")
			return
		}
		var frame realtimev1.ClientFrame
		if err := proto.Unmarshal(data, &frame); err != nil {
			c.closeWith(websocket.StatusInvalidFramePayloadData, "malformed frame")
			return
		}
		if sub := frame.GetSubscribe(); sub != nil {
			g.subscribe(c, sub.GetChannelIds())
		}
	}
}
