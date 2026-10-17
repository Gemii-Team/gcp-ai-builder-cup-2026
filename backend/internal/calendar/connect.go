package calendar

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	calendarv1 "kiwi/gen/kiwi/calendar/v1"
	"kiwi/gen/kiwi/calendar/v1/calendarv1connect"
	"kiwi/internal/platform/authz"
)

// Handler serves CalendarService over Connect and returns the path to mount it at.
// actorFrom reads the actor the API shell's session interceptor put in the request
// context. Domain errors come back unmapped; the shell maps them to Connect codes.
//
// Like the other domains' handlers it always requires the Connect-Protocol-Version
// header: with POST-only RPCs and the origin check, that is what keeps a cross-site
// request from reaching a cookie-authenticated RPC.
func Handler(svc Service, actorFrom func(context.Context) authz.Actor, opts ...connect.HandlerOption) (string, http.Handler) {
	opts = append([]connect.HandlerOption{connect.WithRequireConnectProtocolHeader()}, opts...)
	return calendarv1connect.NewCalendarServiceHandler(&connectServer{svc: svc, actor: actorFrom}, opts...)
}

type connectServer struct {
	svc   Service
	actor func(context.Context) authz.Actor
}

func (s *connectServer) ScheduleMeeting(ctx context.Context, req *connect.Request[calendarv1.ScheduleMeetingRequest]) (*connect.Response[calendarv1.ScheduleMeetingResponse], error) {
	event, err := s.svc.ScheduleMeeting(ctx, s.actor(ctx), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&calendarv1.ScheduleMeetingResponse{Event: event}), nil
}

func (s *connectServer) ListUpcoming(ctx context.Context, req *connect.Request[calendarv1.ListUpcomingRequest]) (*connect.Response[calendarv1.ListUpcomingResponse], error) {
	var channelID *uuid.UUID
	if raw := req.Msg.GetChannelId(); raw != "" {
		id, err := parseID(raw, "channel id")
		if err != nil {
			return nil, err
		}
		channelID = &id
	}
	events, err := s.svc.ListUpcoming(ctx, s.actor(ctx), channelID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&calendarv1.ListUpcomingResponse{Events: events}), nil
}

func (s *connectServer) CancelEvent(ctx context.Context, req *connect.Request[calendarv1.CancelEventRequest]) (*connect.Response[calendarv1.CancelEventResponse], error) {
	id, err := parseID(req.Msg.GetEventId(), "event id")
	if err != nil {
		return nil, err
	}
	if err := s.svc.CancelEvent(ctx, s.actor(ctx), id); err != nil {
		return nil, err
	}
	return connect.NewResponse(&calendarv1.CancelEventResponse{}), nil
}
