// Command kiwi is the single K.I.W.I. binary. `kiwi` with no argument runs
// `serve`; other subcommands register themselves (see commands.go).
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"kiwi/internal/platform/config"
	"kiwi/internal/platform/httpx"
	"kiwi/internal/platform/llm/llmfactory"
)

const listenAddr = ":8080"

func init() { register("serve", serve) }

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

// run dispatches args to a registered subcommand and returns the exit code:
// 0 on success, 1 when the command fails, 2 for an unknown command.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	name := "serve"
	if len(args) > 0 {
		name, args = args[0], args[1:]
	}
	cmd, ok := commands[name]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "kiwi: unknown command %q; registered commands: %s\n", name, strings.Join(commandNames(), ", "))
		return 2
	}
	if err := cmd(ctx, args); err != nil {
		_, _ = fmt.Fprintf(stderr, "kiwi %s: %v\n", name, err)
		return 1
	}
	return 0
}

// serve loads the configuration for KIWI_ROLES, refuses a model provider that must not
// run (llmfactory.MustNew exits on one), and serves the health endpoints until SIGINT or
// SIGTERM. The provider check comes right after the configuration is loaded and before
// anything is dialled; keep it there when serve starts opening Postgres and Valkey.
func serve(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("takes no arguments, got %q", args)
	}
	cfg, err := config.Load(nil, os.Getenv)
	if err != nil {
		return err
	}
	_ = llmfactory.MustNew(cfg) // L27 hands the client to the composition root
	srv := httpx.New(listenAddr, nil)
	srv.SetReady(true)
	slog.Info("serving", "roles", cfg.Roles, "addr", listenAddr)
	return srv.Run(ctx)
}
