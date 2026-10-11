package outbox

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
)

// Apply writes a consumer's effects inside the transaction OnceFor opens, so they
// commit together with the processed_events row and roll back with it.
type Apply func(ctx context.Context, tx pgx.Tx) error

// Prepare does a consumer's slow, non-transactional work for one message, such as a
// model call or a download, and returns the Apply that records its result. It may
// run more than once for a message that is delivered concurrently. A nil Apply means
// there is nothing to write: the message is still recorded as processed.
type Prepare func(ctx context.Context, m bus.Message) (Apply, error)

// Once makes consumer exactly-once on at-least-once delivery. A message already in
// platform.processed_events for consumer is acked without running p. Otherwise p runs
// outside any transaction, then OnceFor records the message and applies the result in
// one transaction bound to the message's workspace_id attribute. A Prepare or Apply
// error nacks the message and leaves nothing recorded, so the redelivery starts over.
func Once(d *db.DB, consumer string, p Prepare) bus.Handler {
	return func(ctx context.Context, m bus.Message) error {
		ws, err := uuid.Parse(m.Attrs["workspace_id"])
		if err != nil || ws == uuid.Nil {
			return fmt.Errorf("outbox: %s: message %s has no valid workspace_id attribute: %w", consumer, m.ID, errs.ErrInvalid)
		}

		// A cheap read that spares a redelivery the work in p; the insert in OnceFor is
		// what makes the effect exactly-once.
		var done bool
		err = d.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM platform.processed_events WHERE consumer = $1 AND event_id = $2)`,
				consumer, m.ID).Scan(&done)
		})
		if err != nil {
			return fmt.Errorf("outbox: %s: check processed_events: %w", consumer, err)
		}
		if done {
			return nil
		}

		apply, err := p(ctx, m)
		if err != nil {
			return err
		}
		return OnceFor(ctx, d, consumer, m.ID, ws, apply)
	}
}

// OnceFor is the transactional core of Once, for consumers that learn the workspace
// themselves. In one transaction bound to ws it records (consumer, eventID) in
// platform.processed_events and runs apply only if that insert took effect. A
// duplicate returns nil without running apply; two concurrent calls for one event
// serialise on the insert, so exactly one of them applies. An apply error rolls the
// record back with it.
func OnceFor(ctx context.Context, d *db.DB, consumer, eventID string, ws uuid.UUID, apply Apply) error {
	return d.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO platform.processed_events (consumer, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			consumer, eventID)
		if err != nil {
			return fmt.Errorf("outbox: %s: record event %s: %w", consumer, eventID, err)
		}
		if tag.RowsAffected() == 0 || apply == nil {
			return nil
		}
		return apply(ctx, tx)
	})
}
