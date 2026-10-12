package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/llm"
	"kiwi/internal/platform/outbox"
)

// consumerAnalyze is the job that summarises an uploaded file.
const consumerAnalyze = "doc.analyze"

// finalAttempt is the delivery the bus dead-letters a message after: an analysis that
// still fails on it is recorded as failed, because nothing will deliver the message
// again and the file would show a spinner for ever.
const finalAttempt = 5

// Jobs returns the consumers of the docs domain for the composition root to register:
// doc.analyze, which reads a file when doc.uploaded announces it and posts its summary.
func Jobs(d Deps) []bus.Job {
	a := &analyzer{db: d.DB, blob: d.Blob, chat: d.Chat, llm: d.LLM}
	a.once = outbox.Once(d.DB, consumerAnalyze, a.prepare)
	return []bus.Job{{
		Consumer: consumerAnalyze,
		Topics:   []string{events.TopicDocUploaded},
		Handler:  a.handle,
	}}
}

// handle is the doc.analyze handler: the analysis, exactly once per message, and a last
// resort for the final delivery. prepare settles most failures itself, but an error it
// cannot foresee, one raised while the result is written (a database that refuses it) or
// while the file is looked up, fails the delivery whatever the file is: on the final
// attempt, with nothing left to deliver it again, the file is recorded as failed with a
// reason instead of being left analyzing for ever. A caller that has gone away, a
// shutdown, and a model budget that has run out are not the file's fault and are
// delivered again.
func (a *analyzer) handle(ctx context.Context, m bus.Message) error {
	err := a.once(ctx, m)
	if err == nil || m.Attempt < finalAttempt || ctx.Err() != nil || errors.Is(err, llm.ErrBudgetExhausted) {
		return err
	}
	ws, fileID, derr := decodeEvent(m)
	if derr != nil {
		return err // a message about no file: there is nothing to settle
	}
	slog.WarnContext(ctx, "docs: giving up on a file at the final attempt", "file_id", fileID, "attempt", m.Attempt, "error", err)
	settle := outbox.OnceFor(ctx, a.db, consumerAnalyze, m.ID, ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := failAnalysis(ctx, tx, fileID, reasonModelUnavailable)
		return err
	})
	if settle != nil {
		return fmt.Errorf("docs: fail file %s at the final attempt: %w (the analysis failed with: %v)", fileID, settle, err)
	}
	return nil
}
