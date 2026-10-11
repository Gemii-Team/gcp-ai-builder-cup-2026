package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/db"
)

const (
	defaultConcurrency = 8
	defaultPoll        = 100 * time.Millisecond
	defaultRetryAfter  = 5 * time.Second
)

// Fanout is one realtime frame the relay sends to a Valkey topic.
type Fanout struct {
	Topic string
	Frame []byte
}

// FanoutPublisher sends a frame to every subscriber of a topic; cache.SPublish is
// the production implementation.
type FanoutPublisher interface {
	SPublish(ctx context.Context, topic string, frame []byte) error
}

// Router decides what else an outbox row produces. It must be deterministic, since a
// redelivered row is routed again: ids it puts in extra must not be random.
type Router interface {
	// Route returns bus messages to publish after the row's own, and the frames to
	// fan out before it. An extra without an ID gets "<row id>/<index>"; one without a
	// topic or workspace_id attribute gets them from the row, as the row's own message
	// has. Everything the router sets is kept.
	Route(ev Stored) (extra []bus.Message, fan []Fanout, err error)
}

// RelayOptions configures a Relay. Zero fields take the value in the comment.
type RelayOptions struct {
	// Concurrency is the number of rows in flight: one goroutine each, one system
	// connection each while its handlers run. 0 means 8. The system pool holds 16
	// connections, so more than that only queues.
	Concurrency int
	// Poll is how long a goroutine that found no row waits before looking again. 0
	// means 100ms.
	Poll time.Duration
	// RetryAfter is how far a row whose delivery failed is pushed back. 0 means 5s.
	RetryAfter time.Duration
}

// Relay moves outbox rows to the bus. Each of its goroutines claims one unpublished,
// due row in a transaction (FOR UPDATE SKIP LOCKED), delivers it and marks it
// published in that same transaction. The row stays locked while the bus's consumers
// work, so a process that stops mid-job, as every rebuild does, rolls the transaction
// back and the row is delivered again; delivery is at-least-once and consumers are
// idempotent through Once. Frames and messages of different rows can therefore reach
// consumers out of order.
type Relay struct {
	d      *db.DB
	pub    bus.Publisher
	fp     FanoutPublisher
	router Router
	opt    RelayOptions
}

// NewRelay returns a Relay that publishes to pub, fans frames out through fp and asks
// r what else each row produces. None of them may be nil.
func NewRelay(d *db.DB, pub bus.Publisher, fp FanoutPublisher, r Router, opt RelayOptions) *Relay {
	if opt.Concurrency <= 0 {
		opt.Concurrency = defaultConcurrency
	}
	if opt.Poll <= 0 {
		opt.Poll = defaultPoll
	}
	if opt.RetryAfter <= 0 {
		opt.RetryAfter = defaultRetryAfter
	}
	return &Relay{d: d, pub: pub, fp: fp, router: r, opt: opt}
}

// Run relays rows until ctx is cancelled, then returns nil once every goroutine has
// stopped. Cancelling aborts the handlers (the bus returns ctx.Err()) and rolls the
// claims back: the rows in flight stay unpublished and keep their not_before.
func (r *Relay) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for range r.opt.Concurrency {
		wg.Go(func() { r.loop(ctx) })
	}
	wg.Wait()
	return nil
}

func (r *Relay) loop(ctx context.Context) {
	for ctx.Err() == nil {
		claimed, err := r.relayOne(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("outbox: relay pass failed", "error", err)
		}
		if err != nil || !claimed {
			pause(ctx, r.opt.Poll)
		}
	}
}

// relayOne claims one row and settles it, reporting whether there was one. A row whose
// delivery fails is pushed back by RetryAfter and the transaction commits that, so the
// row is not claimed again at once; a cancelled ctx is no failure of the row, so its
// transaction rolls back and the row stays as it was.
func (r *Relay) relayOne(ctx context.Context) (claimed bool, err error) {
	err = r.d.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		st, err := claim(ctx, tx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim: %w", err)
		}
		claimed = true

		if err := r.deliver(ctx, st); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("outbox: delivery failed, retrying later",
				"outbox_id", st.ID, "topic", st.Topic, "retry_after", r.opt.RetryAfter, "error", err)
			return postpone(ctx, tx, st.ID, r.opt.RetryAfter)
		}
		return mark(ctx, tx, st.ID)
	})
	return claimed, err
}

// claim locks the oldest unpublished row that is due; pgx.ErrNoRows means none is.
func claim(ctx context.Context, tx pgx.Tx) (Stored, error) {
	var st Stored
	err := tx.QueryRow(ctx, `
		SELECT id, topic, key, payload, not_before
		FROM platform.outbox
		WHERE published_at IS NULL AND not_before <= now()
		ORDER BY id
		LIMIT 1
		FOR UPDATE SKIP LOCKED`).Scan(&st.ID, &st.Topic, &st.Key, &st.Payload, &st.NotBefore)
	return st, err
}

func mark(ctx context.Context, tx pgx.Tx, id int64) error {
	if _, err := tx.Exec(ctx, `UPDATE platform.outbox SET published_at = clock_timestamp() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("mark %d published: %w", id, err)
	}
	return nil
}

func postpone(ctx context.Context, tx pgx.Tx, id int64, d time.Duration) error {
	_, err := tx.Exec(ctx,
		`UPDATE platform.outbox SET not_before = clock_timestamp() + $1::float8 * interval '1 second' WHERE id = $2`,
		d.Seconds(), id)
	if err != nil {
		return fmt.Errorf("postpone %d: %w", id, err)
	}
	return nil
}

// deliver sends the frames, then the row's bus message, then the router's extras, and
// returns on the first failure. The bus message waits for its consumers (see
// bus.Memory.Publish), which is what keeps the row locked meanwhile.
func (r *Relay) deliver(ctx context.Context, st Stored) error {
	ws, err := workspaceOf(st.Payload)
	if err != nil {
		return err
	}
	st.WorkspaceID = ws

	extras, fan, err := r.router.Route(st)
	if err != nil {
		return fmt.Errorf("route %s: %w", st.Topic, err)
	}
	for _, f := range fan {
		if err := r.fp.SPublish(ctx, f.Topic, f.Frame); err != nil {
			return fmt.Errorf("fan out to %s: %w", f.Topic, err)
		}
	}

	msg := bus.Message{
		ID:    strconv.FormatInt(st.ID, 10),
		Topic: st.Topic,
		Key:   st.Key,
		Data:  st.Payload,
		Attrs: attrs(nil, st.Topic, ws),
	}
	if err := r.pub.Publish(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", st.Topic, err)
	}
	for i, x := range extras {
		if x.ID == "" {
			x.ID = fmt.Sprintf("%d/%d", st.ID, i)
		}
		x.Attrs = attrs(x.Attrs, x.Topic, ws)
		if err := r.pub.Publish(ctx, x); err != nil {
			return fmt.Errorf("publish %s: %w", x.Topic, err)
		}
	}
	return nil
}

// attrs returns a copy of have with the topic and workspace_id attributes added where
// they are missing.
func attrs(have map[string]string, topic string, ws uuid.UUID) map[string]string {
	out := make(map[string]string, len(have)+2)
	maps.Copy(out, have)
	if _, ok := out["topic"]; !ok {
		out["topic"] = topic
	}
	if _, ok := out["workspace_id"]; !ok {
		out["workspace_id"] = ws.String()
	}
	return out
}

// pause waits d or until ctx ends.
func pause(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
