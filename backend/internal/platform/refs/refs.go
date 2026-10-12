// Package refs allocates the REF codes users see: MTG-1001, MTG-1002, ... per
// workspace and kind.
package refs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"kiwi/internal/platform/errs"
)

// first is the number the first REF of a workspace and kind gets.
const first = 1001

// kinds are the three-letter REF prefixes: meeting, answer, document, task.
var kinds = map[string]bool{"MTG": true, "ANS": true, "DOC": true, "TSK": true}

// Next returns the next REF of kind for the workspace tx is bound to
// (db.InWorkspace). The counter row is locked until tx ends, so concurrent
// allocations queue and a rolled-back transaction consumes no number: the
// sequence has no gaps. An unknown kind is errs.ErrInvalid; outside a
// workspace the allocation fails.
func Next(ctx context.Context, tx pgx.Tx, kind string) (string, error) {
	if !kinds[kind] {
		return "", fmt.Errorf("refs: unknown kind %q: %w", kind, errs.ErrInvalid)
	}
	var n int
	err := tx.QueryRow(ctx, `
		INSERT INTO platform.ref_counters AS c (workspace_id, kind, last)
		VALUES (NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid, $1, $2)
		ON CONFLICT (workspace_id, kind) DO UPDATE SET last = c.last + 1
		RETURNING last`, kind, first).Scan(&n)
	if err != nil {
		return "", fmt.Errorf("refs: allocate %s: %w", kind, err)
	}
	return fmt.Sprintf("%s-%d", kind, n), nil
}
