// Package identity owns sign-in, sessions, users and workspaces. Other domains reach
// it only through Service.
package identity

import (
	"context"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	identityv1 "kiwi/gen/kiwi/identity/v1"
	"kiwi/internal/platform/authz"
)

// Session is a signed-in browser session as stored in the cache.
type Session struct {
	ID          string
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// Service is the identity domain. Methods that take an authz.Actor act for that
// actor; none of them is channel-scoped, so none needs an authz action beyond the
// actor being a user of its workspace.
type Service interface {
	// LoginURL returns the Google authorization URL for a sign-in that ends at
	// returnTo. Needs no actor: the caller is not signed in yet.
	LoginURL(ctx context.Context, returnTo string) (authURL string, err error)
	// HandleCallback completes a sign-in from Google's code and state and returns the
	// new session and the path to return to. Needs no actor: the caller is not signed
	// in yet.
	HandleCallback(ctx context.Context, code, state string) (session Session, returnTo string, err error)
	// Session returns the live session sessionID. Needs no actor: it is how the API
	// shell builds one.
	Session(ctx context.Context, sessionID string) (Session, error)
	// Logout ends the session sessionID. Needs no actor.
	Logout(ctx context.Context, sessionID string) error
	// Me returns the actor's user and workspace. Needs no authz action.
	Me(ctx context.Context, a authz.Actor) (*identityv1.User, *identityv1.Workspace, error)
	// ListUsers returns the users of the actor's workspace. Needs no authz action.
	ListUsers(ctx context.Context, a authz.Actor) ([]*identityv1.User, error)
	// IssueRealtimeTicket returns a single-use ticket for the actor's WebSocket
	// connection. Needs no authz action.
	IssueRealtimeTicket(ctx context.Context, a authz.Actor) (string, error)
	// UserTokenSource returns a token source for userID's own Google grant covering
	// scope, for adapters that call Google as that user. Needs no actor; it returns a
	// *google.ConsentRequiredError when the user has not granted the scope.
	UserTokenSource(ctx context.Context, userID uuid.UUID, scope string) (oauth2.TokenSource, error)
}
