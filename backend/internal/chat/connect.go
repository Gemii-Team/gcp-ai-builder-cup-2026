package chat

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	"kiwi/gen/kiwi/chat/v1/chatv1connect"
	"kiwi/internal/platform/authz"
)

// Handler serves ChatService over Connect and returns the path to mount it at.
// actorFrom reads the actor the API shell's session interceptor put in the request
// context. Domain errors come back unmapped; the shell maps them to Connect codes.
//
// The handler always requires the Connect-Protocol-Version header. With POST-only
// RPCs and the origin check, it is what keeps a cross-site request from reaching a
// cookie-authenticated RPC, and a built http.Handler cannot be given the option
// afterwards, so no caller is left to forget it.
func Handler(svc Service, actorFrom func(context.Context) authz.Actor, opts ...connect.HandlerOption) (string, http.Handler) {
	opts = append([]connect.HandlerOption{connect.WithRequireConnectProtocolHeader()}, opts...)
	return chatv1connect.NewChatServiceHandler(&connectServer{svc: svc, actor: actorFrom}, opts...)
}

type connectServer struct {
	svc   Service
	actor func(context.Context) authz.Actor
}

func (s *connectServer) ListChannels(ctx context.Context, _ *connect.Request[chatv1.ListChannelsRequest]) (*connect.Response[chatv1.ListChannelsResponse], error) {
	channels, err := s.svc.ListChannels(ctx, s.actor(ctx))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chatv1.ListChannelsResponse{Channels: channels}), nil
}

func (s *connectServer) CreateChannel(ctx context.Context, req *connect.Request[chatv1.CreateChannelRequest]) (*connect.Response[chatv1.CreateChannelResponse], error) {
	channel, err := s.svc.CreateChannel(ctx, s.actor(ctx), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chatv1.CreateChannelResponse{Channel: channel}), nil
}

func (s *connectServer) JoinChannel(ctx context.Context, req *connect.Request[chatv1.JoinChannelRequest]) (*connect.Response[chatv1.JoinChannelResponse], error) {
	channelID, err := parseID(req.Msg.GetChannelId(), "channel id")
	if err != nil {
		return nil, err
	}
	if err := s.svc.JoinChannel(ctx, s.actor(ctx), channelID); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chatv1.JoinChannelResponse{}), nil
}

func (s *connectServer) AddMember(ctx context.Context, req *connect.Request[chatv1.AddMemberRequest]) (*connect.Response[chatv1.AddMemberResponse], error) {
	channelID, err := parseID(req.Msg.GetChannelId(), "channel id")
	if err != nil {
		return nil, err
	}
	userID, err := parseID(req.Msg.GetUserId(), "user id")
	if err != nil {
		return nil, err
	}
	if err := s.svc.AddMember(ctx, s.actor(ctx), channelID, userID); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chatv1.AddMemberResponse{}), nil
}

func (s *connectServer) RemoveMember(ctx context.Context, req *connect.Request[chatv1.RemoveMemberRequest]) (*connect.Response[chatv1.RemoveMemberResponse], error) {
	channelID, err := parseID(req.Msg.GetChannelId(), "channel id")
	if err != nil {
		return nil, err
	}
	userID, err := parseID(req.Msg.GetUserId(), "user id")
	if err != nil {
		return nil, err
	}
	if err := s.svc.RemoveMember(ctx, s.actor(ctx), channelID, userID); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chatv1.RemoveMemberResponse{}), nil
}

func (s *connectServer) ListMembers(ctx context.Context, req *connect.Request[chatv1.ListMembersRequest]) (*connect.Response[chatv1.ListMembersResponse], error) {
	channelID, err := parseID(req.Msg.GetChannelId(), "channel id")
	if err != nil {
		return nil, err
	}
	members, err := s.svc.ListMembers(ctx, s.actor(ctx), channelID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chatv1.ListMembersResponse{Members: members}), nil
}

func (s *connectServer) ListMessages(ctx context.Context, req *connect.Request[chatv1.ListMessagesRequest]) (*connect.Response[chatv1.ListMessagesResponse], error) {
	channelID, err := parseID(req.Msg.GetChannelId(), "channel id")
	if err != nil {
		return nil, err
	}
	messages, err := s.svc.ListMessages(ctx, s.actor(ctx), channelID, req.Msg.GetBeforeSeq(), int(req.Msg.GetLimit()))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chatv1.ListMessagesResponse{Messages: messages}), nil
}

func (s *connectServer) SendMessage(ctx context.Context, req *connect.Request[chatv1.SendMessageRequest]) (*connect.Response[chatv1.SendMessageResponse], error) {
	message, err := s.svc.PostMessage(ctx, s.actor(ctx), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chatv1.SendMessageResponse{Message: message}), nil
}

func (s *connectServer) SyncChannels(ctx context.Context, req *connect.Request[chatv1.SyncChannelsRequest]) (*connect.Response[chatv1.SyncChannelsResponse], error) {
	deltas, err := s.svc.SyncChannels(ctx, s.actor(ctx), req.Msg.GetCursors())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chatv1.SyncChannelsResponse{Deltas: deltas}), nil
}
