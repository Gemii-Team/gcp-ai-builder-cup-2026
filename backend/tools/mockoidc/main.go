// Command mockoidc is the mock sign-in provider of the local stack: an OpenID
// Connect provider shaped like Google's that lets a developer pick which seeded
// user to sign in as (or an outsider without a hosted domain). It serves
// /mockoidc on :8090 and is configured by MOCKOIDC_* variables; see server.go.
// It exits 1 when KIWI_ENV is anything but unset or "local".
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Getenv, os.Stderr)
	stop()
	os.Exit(code)
}
