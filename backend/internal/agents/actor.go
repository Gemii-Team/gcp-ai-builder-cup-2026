package agents

import (
	"context"

	"kiwi/internal/platform/authz"
)

type actorKey struct{}

// WithActor returns a context that carries a, the agent actor (agent acting on
// behalf of the user who started the run) that the tools of the run act as. It is the
// only source of identity a tool has: no argument of a tool call ever names a user.
func WithActor(ctx context.Context, a authz.Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the actor WithActor stored in ctx, and false when there is none.
func ActorFrom(ctx context.Context) (authz.Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(authz.Actor)
	return a, ok
}
