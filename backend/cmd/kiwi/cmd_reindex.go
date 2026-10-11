package main

import (
	"context"
	"fmt"
	"os"

	"kiwi/internal/knowledge"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/llm/llmfactory"
)

func init() { register("reindex", runReindex) }

// runReindex re-embeds every knowledge chunk whose vector was made by another model
// than KIWI_EMBED_MODEL, which is what `task reindex` runs after that variable changes.
// It reads the environment of `kiwi serve`, so `kubectl exec` into the running pod
// is enough, and it can be run again after an interruption: it continues with the chunks
// still on the old model.
func runReindex(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("takes no arguments, got %q", args)
	}
	cfg, err := config.Load(nil, os.Getenv)
	if err != nil {
		return err
	}
	model, err := llmfactory.New(cfg)
	if err != nil {
		return err
	}
	d, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close()
	return knowledge.New(knowledge.Deps{DB: d, LLM: model}).Reindex(ctx)
}
