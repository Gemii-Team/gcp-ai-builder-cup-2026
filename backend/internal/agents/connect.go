package agents

import (
	"context"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	agentsv1 "kiwi/gen/kiwi/agents/v1"
	"kiwi/gen/kiwi/agents/v1/agentsv1connect"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/errs"
)

// Handler serves AgentService over Connect and returns the path to mount it at.
// actorFrom reads the actor the API shell's session interceptor put in the request
// context. Domain errors come back unmapped; the shell maps them to Connect codes.
//
// The handler always requires the Connect-Protocol-Version header. With POST-only
// RPCs and the origin check, it is what keeps a cross-site request from reaching a
// cookie-authenticated RPC, and a built http.Handler cannot be given the option
// afterwards, so no caller is left to forget it.
func Handler(svc Service, actorFrom func(context.Context) authz.Actor, opts ...connect.HandlerOption) (string, http.Handler) {
	opts = append([]connect.HandlerOption{connect.WithRequireConnectProtocolHeader()}, opts...)
	return agentsv1connect.NewAgentServiceHandler(&connectServer{svc: svc, actor: actorFrom}, opts...)
}

type connectServer struct {
	svc   Service
	actor func(context.Context) authz.Actor
}

var _ agentsv1connect.AgentServiceHandler = (*connectServer)(nil)

func (s *connectServer) StartRun(ctx context.Context, req *connect.Request[agentsv1.StartRunRequest]) (*connect.Response[agentsv1.StartRunResponse], error) {
	run, err := s.svc.StartRun(ctx, s.actor(ctx), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentsv1.StartRunResponse{Run: run}), nil
}

func (s *connectServer) GetRun(ctx context.Context, req *connect.Request[agentsv1.GetRunRequest]) (*connect.Response[agentsv1.GetRunResponse], error) {
	id, err := uuid.Parse(req.Msg.GetRunId())
	if err != nil || id == uuid.Nil {
		return nil, fmt.Errorf("agents: run id %q is not a UUID: %w", req.Msg.GetRunId(), errs.ErrInvalid)
	}
	run, err := s.svc.GetRun(ctx, s.actor(ctx), id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentsv1.GetRunResponse{Run: run}), nil
}
