package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"kiwi/internal/platform/blob"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/llm/llmfactory"
	"kiwi/internal/seed"
)

func init() { register("seed", runSeed) }

// The defaults are what the mock sign-in signs in: its provider mints hd = demo.example
// and sub = "seed:" + email for alice and bob, so their first sign-in adopts the seeded
// placeholder users, and carol@elsewhere.example (its outsider) is not seeded.
const (
	defaultDomain  = "demo.example"
	defaultMembers = "alice@demo.example,bob@demo.example"
)

// runSeed seeds the demo workspace, which is what `task seed -- <flags>` runs inside the
// pod. It reads the environment of `kiwi serve`, indexes the corpus before it returns
// (seed.Options.IndexInline), and can be run again: a second run changes nothing.
func runSeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	var usage bytes.Buffer
	fs.SetOutput(&usage)
	domain := fs.String("domain", defaultDomain, "the workspace's Google domain (the hd of the people who sign in)")
	members := fs.String("members", defaultMembers, "comma-separated member emails; the first owns the channels and is the only member of #hr-private")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, werr := io.WriteString(os.Stdout, usage.String())
			return werr
		}
		return errors.New(strings.TrimSpace(usage.String()))
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("takes no arguments, got %q", fs.Args())
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
	c, err := cache.Open(ctx, cfg.ValkeyURL, cache.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	store, err := blob.Open(ctx, cfg)
	if err != nil {
		return err
	}
	if closer, ok := store.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}

	emails := strings.Split(*members, ",")
	return seed.Run(ctx, seed.Deps{DB: d, Cache: c, Blob: store, LLM: model}, seed.Options{
		Domain: *domain, MemberEmails: emails, IndexInline: true,
	})
}
