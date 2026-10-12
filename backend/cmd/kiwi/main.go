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

	"kiwi/internal/app"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/llm/llmfactory"
)

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
// run (llmfactory.MustNew exits on one) and then runs the roles of the process through
// the composition root until SIGINT or SIGTERM. The order is the guard: the
// configuration is checked first (config.Load refuses a prod environment that is not
// Google's sign-in), the provider second, and only then does anything dial Postgres,
// Valkey or the sign-in provider. Keep it so.
func serve(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("takes no arguments, got %q", args)
	}
	cfg, err := config.Load(nil, os.Getenv)
	if err != nil {
		return err
	}
	model := llmfactory.MustNew(cfg)
	slog.Info("starting", "config", cfg) // the secrets of the configuration are redacted
	a, err := app.New(ctx, cfg, app.WithLLM(model))
	if err != nil {
		return err
	}
	defer a.Close()
	return a.Run(ctx)
}
