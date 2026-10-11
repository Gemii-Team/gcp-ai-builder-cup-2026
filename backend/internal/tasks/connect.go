package tasks

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	tasksv1 "kiwi/gen/kiwi/tasks/v1"
	"kiwi/gen/kiwi/tasks/v1/tasksv1connect"
	"kiwi/internal/platform/authz"
)

// Handler serves TaskService over Connect and returns the path to mount it at.
// actorFrom reads the actor the API shell's session interceptor put in the request
// context. Domain errors come back unmapped; the shell maps them to Connect codes.
//
// The handler always requires the Connect-Protocol-Version header. With POST-only
// RPCs and the origin check, it is what keeps a cross-site request from reaching a
// cookie-authenticated RPC, and a built http.Handler cannot be given the option
// afterwards, so no caller is left to forget it.
func Handler(svc Service, actorFrom func(context.Context) authz.Actor, opts ...connect.HandlerOption) (string, http.Handler) {
	opts = append([]connect.HandlerOption{connect.WithRequireConnectProtocolHeader()}, opts...)
	return tasksv1connect.NewTaskServiceHandler(&connectServer{svc: svc, actor: actorFrom}, opts...)
}

type connectServer struct {
	svc   Service
	actor func(context.Context) authz.Actor
}

func (s *connectServer) ListTasks(ctx context.Context, req *connect.Request[tasksv1.ListTasksRequest]) (*connect.Response[tasksv1.ListTasksResponse], error) {
	q := ListTasksQuery{States: req.Msg.GetStates()}
	if raw := req.Msg.GetChannelId(); raw != "" {
		id, err := parseID(raw, "channel id")
		if err != nil {
			return nil, err
		}
		q.ChannelID = &id
	}
	tasks, err := s.svc.ListTasks(ctx, s.actor(ctx), q)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&tasksv1.ListTasksResponse{Tasks: tasks}), nil
}

func (s *connectServer) CreateTask(ctx context.Context, req *connect.Request[tasksv1.CreateTaskRequest]) (*connect.Response[tasksv1.CreateTaskResponse], error) {
	task, err := s.svc.CreateTask(ctx, s.actor(ctx), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&tasksv1.CreateTaskResponse{Task: task}), nil
}

func (s *connectServer) UpdateTask(ctx context.Context, req *connect.Request[tasksv1.UpdateTaskRequest]) (*connect.Response[tasksv1.UpdateTaskResponse], error) {
	task, err := s.svc.UpdateTask(ctx, s.actor(ctx), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&tasksv1.UpdateTaskResponse{Task: task}), nil
}

func (s *connectServer) MoveTask(ctx context.Context, req *connect.Request[tasksv1.MoveTaskRequest]) (*connect.Response[tasksv1.MoveTaskResponse], error) {
	task, err := s.svc.MoveTask(ctx, s.actor(ctx), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&tasksv1.MoveTaskResponse{Task: task}), nil
}

func (s *connectServer) ConfirmDraft(ctx context.Context, req *connect.Request[tasksv1.ConfirmDraftRequest]) (*connect.Response[tasksv1.ConfirmDraftResponse], error) {
	id, err := parseID(req.Msg.GetId(), "task id")
	if err != nil {
		return nil, err
	}
	task, err := s.svc.ConfirmDraft(ctx, s.actor(ctx), id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&tasksv1.ConfirmDraftResponse{Task: task}), nil
}

func (s *connectServer) DiscardDraft(ctx context.Context, req *connect.Request[tasksv1.DiscardDraftRequest]) (*connect.Response[tasksv1.DiscardDraftResponse], error) {
	id, err := parseID(req.Msg.GetId(), "task id")
	if err != nil {
		return nil, err
	}
	if err := s.svc.DiscardDraft(ctx, s.actor(ctx), id); err != nil {
		return nil, err
	}
	return connect.NewResponse(&tasksv1.DiscardDraftResponse{}), nil
}
