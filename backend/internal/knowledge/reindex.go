package knowledge

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"kiwi/internal/identity"
	"kiwi/internal/knowledge/internal/store"
	"kiwi/internal/platform/errs"
)

// Reindex implements Service. It re-embeds, workspace by workspace and in batches, every
// chunk whose vector was made by another model than the configured one, from the text
// stored with it, and leaves the chunks of the current model as they are. Each batch is
// committed on its own and a re-embedded chunk drops out of the work, so a run that is
// interrupted, or fails on one batch, is continued by running it again.
func (s *service) Reindex(ctx context.Context) error {
	if s.db == nil || s.llm == nil {
		return fmt.Errorf("knowledge: reindex needs a database and a model: %w", errs.ErrInvalid)
	}
	workspaces, err := identity.ListWorkspaceIDs(ctx, s.db)
	if err != nil {
		return fmt.Errorf("knowledge: list workspaces: %w", err)
	}
	model := s.llm.EmbeddingModel()
	var total int
	for _, ws := range workspaces {
		n, err := s.reindexWorkspace(ctx, ws, model)
		total += n
		if err != nil {
			return fmt.Errorf("knowledge: reindex workspace %s (%d chunks done): %w", ws, n, err)
		}
		if n > 0 {
			slog.Info("knowledge: re-embedded chunks", "workspace_id", ws, "chunks", n, "model", model)
		}
	}
	slog.Info("knowledge: reindex finished", "workspaces", len(workspaces), "chunks", total, "model", model)
	return nil
}

// reindexWorkspace re-embeds the chunks of ws that model did not make, and returns how
// many it did.
func (s *service) reindexWorkspace(ctx context.Context, ws uuid.UUID, model string) (int, error) {
	var done int
	for {
		var rows []store.ListChunksToReembedRow
		err := s.db.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) (err error) {
			rows, err = store.New(tx).ListChunksToReembed(ctx, store.ListChunksToReembedParams{EmbeddingModel: model, RowLimit: embedBatch})
			return err
		})
		if err != nil {
			return done, fmt.Errorf("read chunks to re-embed: %w", err)
		}
		if len(rows) == 0 {
			return done, nil
		}
		texts := make([]string, len(rows))
		for i, r := range rows {
			texts[i] = r.Content
		}
		vecs, err := s.embed(ctx, texts)
		if err != nil {
			return done, err
		}
		err = s.db.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
			q := store.New(tx)
			for i, r := range rows {
				// A chunk indexing rewrote since it was read already holds a vector of its new
				// text, and a vector of the old one must not replace it: no row is updated.
				if _, err := q.UpdateChunkEmbedding(ctx, store.UpdateChunkEmbeddingParams{
					ID: r.ID, ContentSha256: r.ContentSha256, EmbeddingModel: model, Embedding: pgvector.NewVector(vecs[i]),
				}); err != nil {
					return fmt.Errorf("update chunk %s: %w", r.ID, err)
				}
			}
			return nil
		})
		if err != nil {
			return done, fmt.Errorf("store %d re-embedded chunks: %w", len(rows), err)
		}
		done += len(rows)
	}
}
