package identity

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	identityv1 "kiwi/gen/kiwi/identity/v1"
	"kiwi/gen/kiwi/identity/v1/identityv1connect"
	"kiwi/internal/platform/authz"
)

// Handler serves IdentityService over Connect and returns the path to mount it
// at. actorFrom reads the actor the API shell's session interceptor put in the
// request context. Domain errors come back unmapped; the shell maps them to
// Connect codes.
func Handler(svc Service, actorFrom func(context.Context) authz.Actor, opts ...connect.HandlerOption) (string, http.Handler) {
	return identityv1connect.NewIdentityServiceHandler(&connectServer{svc: svc, actor: actorFrom}, opts...)
}

type connectServer struct {
	svc   Service
	actor func(context.Context) authz.Actor
}

func (s *connectServer) GetMe(ctx context.Context, _ *connect.Request[identityv1.GetMeRequest]) (*connect.Response[identityv1.GetMeResponse], error) {
	u, w, err := s.svc.Me(ctx, s.actor(ctx))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&identityv1.GetMeResponse{User: u, Workspace: w}), nil
}

func (s *connectServer) ListUsers(ctx context.Context, _ *connect.Request[identityv1.ListUsersRequest]) (*connect.Response[identityv1.ListUsersResponse], error) {
	users, err := s.svc.ListUsers(ctx, s.actor(ctx))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&identityv1.ListUsersResponse{Users: users}), nil
}

func (s *connectServer) IssueRealtimeTicket(ctx context.Context, _ *connect.Request[identityv1.IssueRealtimeTicketRequest]) (*connect.Response[identityv1.IssueRealtimeTicketResponse], error) {
	ticket, err := s.svc.IssueRealtimeTicket(ctx, s.actor(ctx))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&identityv1.IssueRealtimeTicketResponse{Ticket: ticket}), nil
}
