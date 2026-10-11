// Package containers starts the Docker containers the test suites need
// (Postgres, Valkey) and is the only place under backend/ that may do so;
// TestNoPackageStartsContainersDirectly enforces it.
//
// Rootless Docker allocates the host port of a published container port without
// knowing which ports other processes already hold, so when several test
// packages start containers at once a start can fail before any test code runs
// with a host-port bind error. Start and StartPostgres retry that one error
// (see portBindRace) and nothing else: any other failure is returned at once,
// and test assertions are never retried.
package containers

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const (
	// maxAttempts is the total number of starts tried, the first included.
	maxAttempts = 3

	minBackoff = 200 * time.Millisecond
	maxBackoff = 600 * time.Millisecond

	// cleanupTimeout bounds the removal of a failed attempt's container.
	cleanupTimeout = 30 * time.Second
)

// portBindRace is the text of the one start failure that is retried. The
// RootlessKit port manager of rootless Docker fails to listen on the host port
// the daemon picked because something else holds it; observed on this host, the
// whole error of a start is
//
//	generic container: start container: container start: Error response from
//	daemon: failed to set up container networking: driver failed programming
//	external connectivity on endpoint <name> (<id>): error while calling
//	RootlessKit PortManager.AddPort(): listen tcp4 0.0.0.0:33354: bind:
//	address already in use
//
// A fixed-port clash ("port is already allocated") or a name clash ("is already
// in use by container") is a real configuration error and does not match.
const portBindRace = "address already in use"

// container is what the retry needs of a started container: the Container
// interface to remove a failed attempt's leftover, and comparability to tell a
// nil container from a real one without reflection.
type container interface {
	testcontainers.Container
	comparable
}

// retrier is the retry policy; the zero value is not usable, start from
// defaultRetrier. Tests replace sleep and log.
type retrier struct {
	attempts int
	sleep    func(ctx context.Context, d time.Duration) error
	log      *slog.Logger
}

var defaultRetrier = retrier{attempts: maxAttempts, sleep: sleep}

// Start runs the container that testcontainers.Run would, with the opts it
// would take, and retries a start that fails on the host-port bind race. On
// error no container is returned and none is left behind.
func Start(ctx context.Context, image string, opts ...testcontainers.ContainerCustomizer) (*testcontainers.DockerContainer, error) {
	return start(ctx, defaultRetrier, func(ctx context.Context) (*testcontainers.DockerContainer, error) {
		return testcontainers.Run(ctx, image, opts...)
	})
}

// StartPostgres is Start for the Postgres module container.
func StartPostgres(ctx context.Context, image string, opts ...testcontainers.ContainerCustomizer) (*tcpostgres.PostgresContainer, error) {
	return start(ctx, defaultRetrier, func(ctx context.Context) (*tcpostgres.PostgresContainer, error) {
		return tcpostgres.Run(ctx, image, opts...)
	})
}

// start calls run until it succeeds, fails with something other than the
// port-bind race, the context ends, or r.attempts starts have failed. Every
// failed attempt's container (testcontainers returns the created one alongside
// the error) is removed first, so the caller has nothing to clean up.
func start[C container](ctx context.Context, r retrier, run func(context.Context) (C, error)) (C, error) {
	var none C
	log := r.log
	if log == nil {
		log = slog.Default()
	}

	for attempt := 1; ; attempt++ {
		ctr, err := run(ctx)
		if err == nil {
			return ctr, nil
		}
		if ctr != none {
			discard(ctx, log, ctr, attempt)
		}

		if !strings.Contains(err.Error(), portBindRace) {
			return none, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return none, abandoned(attempt, err, ctxErr)
		}
		if attempt >= r.attempts {
			return none, fmt.Errorf("container start failed after %d attempts: %w", attempt, err)
		}

		pause := backoff()
		log.Warn("retrying container start after the rootless-Docker port-bind race",
			"attempt", attempt, "max_attempts", r.attempts, "backoff", pause, "error", err)
		if sleepErr := r.sleep(ctx, pause); sleepErr != nil {
			return none, abandoned(attempt, err, sleepErr)
		}
	}
}

// abandoned reports a retry given up because the context ended, keeping both
// the start error and the cancellation visible to errors.Is.
func abandoned(attempt int, startErr, ctxErr error) error {
	return fmt.Errorf("container start attempt %d failed and the context ended, not retrying: %w: %w", attempt, startErr, ctxErr)
}

// discard removes the container a failed attempt left behind. It runs on its
// own bounded context because the attempt's context may be what ended; a
// failure is logged and never replaces the start error.
func discard(ctx context.Context, log *slog.Logger, ctr testcontainers.Container, attempt int) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := ctr.Terminate(ctx); err != nil {
		log.Warn("could not remove the container of a failed start attempt", "attempt", attempt, "error", err)
	}
}

// backoff is the pause before a retry: jittered so that packages that collided
// do not collide again in lockstep.
func backoff() time.Duration {
	return minBackoff + rand.N(maxBackoff-minBackoff)
}

// sleep waits d or until ctx ends, whichever is first, and returns ctx's error
// in the second case.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
