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

// The defaults are what the mock sign-in signs in: its provider mints sub = "seed:" +
// email for alice and bob, so their first sign-in adopts the seeded placeholder users.
// Alice's token carries hd = demo.example and bob's carries none (he signs in because he
// is seeded); carol@elsewhere.example (its outsider) has no hd and is not seeded, so she
// is turned away.
const (
	defaultDomain  = "demo.example"
	defaultMembers = "alice@demo.example,bob@demo.example"
)

// seedDomain is the domain to seed, given the flags that were set. Neither flag: the
// mock's demo.example. --domain X: X, with the default members unless --members is set.
// --members alone, or --domain "" with --members: the empty domain, the one workspace
// without a domain, which is for Google accounts that carry no hd and whose members sign
// in because they were invited. --domain "" alone is refused: the default members are
// the mock's users, who must not be invited into that workspace.
func seedDomain(domain string, given map[string]bool) (string, error) {
	domain = strings.TrimSpace(domain)
	switch {
	case !given["domain"] && given["members"]:
		return "", nil
	case !given["domain"]:
		return defaultDomain, nil
	case domain == "" && !given["members"]:
		return "", errors.New(`--domain "" seeds the workspace without a domain and needs --members: the people to invite`)
	}
	return domain, nil
}

// runSeed seeds a workspace with the demo corpus, which is what `task seed -- <flags>` runs
// inside the pod. It reads the environment of `kiwi serve`, indexes the corpus before it
// returns (seed.Options.IndexInline), and can be run again: a second run changes nothing.
//
// What is seeded follows the flags (seedDomain): none, the mock sign-in's workspace;
// --domain, that Google domain; --members alone, the one workspace without a domain, where
// seed.DemoPeople come first and the members given follow.
func runSeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	var usage bytes.Buffer
	fs.SetOutput(&usage)
	domain := fs.String("domain", defaultDomain, "the workspace's Google domain (the hd of the people who sign in). With only --members, or an empty value with --members, the one workspace without a domain is seeded instead, for Google accounts that carry no hd")
	members := fs.String("members", defaultMembers, "comma-separated member emails; the first owns the channels and is the only member of #hr-private. "+
		"Each is invited: an account without hd signs in as its address's placeholder user, in any workspace (also one with a domain), "+
		"and until that first sign-in binds it any Google account that proves that exact address can claim it; placeholders never expire. "+
		"An address that already belongs to another workspace is refused: run once; use the same --members each time. "+
		"In the one workspace without a domain "+strings.Join(seed.DemoPeople, " and ")+" are added in front of them: demo people on a reserved .invalid address, "+
		"which no Google account can have, so nobody can sign in as them or claim them. They own the channels and write the demo conversation, "+
		"and the members given are plain members of the public channels, who cannot read #hr-private")
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
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	workspaceDomain, err := seedDomain(*domain, given)
	if err != nil {
		return err
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
		Domain: workspaceDomain, MemberEmails: emails, IndexInline: true,
	})
}
