// Package outbox is how a domain write and its event commit together. Enqueue
// inserts the event into platform.outbox in the writer's transaction; the Relay
// claims each row and keeps it until every consumer has settled the message, so a
// process that dies mid-job leaves the row to be redelivered; Once makes a
// consumer's effects exactly-once on top of that at-least-once delivery.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kiwi/internal/platform/errs"
)

// Event is one row of the outbox. Payload is the protojson of the event message and
// carries its workspace_id, which is how the relay and the consumers find the tenant.
type Event struct {
	Topic string
	// Key orders or partitions events that belong together; it is passed to the bus.
	Key     string
	Payload []byte
	// NotBefore delays the event: the relay publishes it once this time has passed.
	// The zero value means now. Handlers never sleep; they enqueue with a later
	// NotBefore instead.
	NotBefore time.Time
}

// Stored is an Event as the relay read it: its row id, which is also the id of the
// bus message, and the workspace taken from Payload. Payload comes back from jsonb,
// so it is equal to what was enqueued as JSON but not byte for byte.
type Stored struct {
	ID int64
	Event
	WorkspaceID uuid.UUID
}

// Enqueue inserts ev into platform.outbox inside tx, so the event exists exactly
// when the writer's changes do. An event the relay could not route, one without a
// topic or whose payload has no workspace_id, is errs.ErrInvalid.
func Enqueue(ctx context.Context, tx pgx.Tx, ev Event) error {
	if ev.Topic == "" {
		return fmt.Errorf("outbox: enqueue: empty topic: %w", errs.ErrInvalid)
	}
	if _, err := workspaceOf(ev.Payload); err != nil {
		return fmt.Errorf("outbox: enqueue %s: %w", ev.Topic, err)
	}
	var notBefore *time.Time
	if !ev.NotBefore.IsZero() {
		notBefore = &ev.NotBefore
	}
	// payload is jsonb and pgx's exec mode refuses a []byte for it: RawMessage is sent as JSON text.
	_, err := tx.Exec(ctx, `
		INSERT INTO platform.outbox (topic, key, payload, not_before)
		VALUES ($1, $2, $3, COALESCE($4::timestamptz, now()))`,
		ev.Topic, ev.Key, json.RawMessage(ev.Payload), notBefore)
	if err != nil {
		return fmt.Errorf("outbox: enqueue %s: %w", ev.Topic, err)
	}
	return nil
}

// workspaceOf reads the workspace_id every event payload carries.
func workspaceOf(payload []byte) (uuid.UUID, error) {
	var p struct {
		WorkspaceID uuid.UUID `json:"workspace_id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return uuid.Nil, fmt.Errorf("payload has no valid workspace_id: %v: %w", err, errs.ErrInvalid)
	}
	if p.WorkspaceID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("payload has no workspace_id: %w", errs.ErrInvalid)
	}
	return p.WorkspaceID, nil
}
