package meetings

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
	"kiwi/gen/kiwi/meetings/v1/meetingsv1connect"
	"kiwi/internal/platform/authz"
)

// Handler serves MeetingService over Connect and returns the path to mount it at.
// actorFrom reads the actor the API shell's session interceptor put in the request
// context. Domain errors come back unmapped; the shell maps them to Connect codes.
//
// The handler always requires the Connect-Protocol-Version header. With POST-only
// RPCs and the origin check, it is what keeps a cross-site request from reaching a
// cookie-authenticated RPC, and a built http.Handler cannot be given the option
// afterwards, so no caller is left to forget it.
func Handler(svc Service, actorFrom func(context.Context) authz.Actor, opts ...connect.HandlerOption) (string, http.Handler) {
	opts = append([]connect.HandlerOption{connect.WithRequireConnectProtocolHeader()}, opts...)
	return meetingsv1connect.NewMeetingServiceHandler(&connectServer{svc: svc, actor: actorFrom}, opts...)
}

type connectServer struct {
	svc   Service
	actor func(context.Context) authz.Actor
}

var _ meetingsv1connect.MeetingServiceHandler = (*connectServer)(nil)

func (s *connectServer) StartMeeting(ctx context.Context, req *connect.Request[meetingsv1.StartMeetingRequest]) (*connect.Response[meetingsv1.StartMeetingResponse], error) {
	channelID, err := parseID(req.Msg.GetChannelId(), "channel id")
	if err != nil {
		return nil, err
	}
	res, err := s.svc.StartMeeting(ctx, s.actor(ctx), channelID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (s *connectServer) CreateFromRecording(ctx context.Context, req *connect.Request[meetingsv1.CreateFromRecordingRequest]) (*connect.Response[meetingsv1.CreateFromRecordingResponse], error) {
	channelID, err := parseID(req.Msg.GetChannelId(), "channel id")
	if err != nil {
		return nil, err
	}
	fileID, err := parseID(req.Msg.GetFileId(), "file id")
	if err != nil {
		return nil, err
	}
	m, err := s.svc.CreateFromRecording(ctx, s.actor(ctx), channelID, fileID, req.Msg.GetTitle())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&meetingsv1.CreateFromRecordingResponse{Meeting: m}), nil
}

func (s *connectServer) GetMeeting(ctx context.Context, req *connect.Request[meetingsv1.GetMeetingRequest]) (*connect.Response[meetingsv1.GetMeetingResponse], error) {
	id, err := parseID(req.Msg.GetMeetingId(), "meeting id")
	if err != nil {
		return nil, err
	}
	m, err := s.svc.GetMeeting(ctx, s.actor(ctx), id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&meetingsv1.GetMeetingResponse{Meeting: m}), nil
}

func (s *connectServer) ListMeetings(ctx context.Context, req *connect.Request[meetingsv1.ListMeetingsRequest]) (*connect.Response[meetingsv1.ListMeetingsResponse], error) {
	var channelID *uuid.UUID
	if raw := req.Msg.GetChannelId(); raw != "" {
		id, err := parseID(raw, "channel id")
		if err != nil {
			return nil, err
		}
		channelID = &id
	}
	meetings, err := s.svc.ListMeetings(ctx, s.actor(ctx), channelID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&meetingsv1.ListMeetingsResponse{Meetings: meetings}), nil
}

func (s *connectServer) GetTranscriptExcerpt(ctx context.Context, req *connect.Request[meetingsv1.GetTranscriptExcerptRequest]) (*connect.Response[meetingsv1.GetTranscriptExcerptResponse], error) {
	id, err := parseID(req.Msg.GetMeetingId(), "meeting id")
	if err != nil {
		return nil, err
	}
	ex, err := s.svc.GetTranscriptExcerpt(ctx, s.actor(ctx), id, req.Msg.GetEntryIdx(), req.Msg.GetRadius())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&meetingsv1.GetTranscriptExcerptResponse{Excerpt: ex}), nil
}

func (s *connectServer) RetrySummary(ctx context.Context, req *connect.Request[meetingsv1.RetrySummaryRequest]) (*connect.Response[meetingsv1.RetrySummaryResponse], error) {
	id, err := parseID(req.Msg.GetMeetingId(), "meeting id")
	if err != nil {
		return nil, err
	}
	if err := s.svc.RetrySummary(ctx, s.actor(ctx), id); err != nil {
		return nil, err
	}
	return connect.NewResponse(&meetingsv1.RetrySummaryResponse{}), nil
}
