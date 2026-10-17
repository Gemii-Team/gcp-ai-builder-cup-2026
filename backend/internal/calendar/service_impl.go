package calendar

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	calendarv1 "kiwi/gen/kiwi/calendar/v1"
	chatv1 "kiwi/gen/kiwi/chat/v1"
	"kiwi/internal/calendar/internal/store"
	"kiwi/internal/chat"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/google"
	"kiwi/internal/platform/refs"
)

// Values of calendar.events.status.
const (
	statusScheduled = "scheduled"
	statusCanceled  = "canceled"
)

const (
	// slipAuthor is the agent whose name the scheduling messages are posted under, as
	// in the meetings domain.
	slipAuthor = "assistant"
	// maxTitleRunes is the longest meeting title ScheduleMeeting accepts.
	maxTitleRunes = 200
	// minDuration and maxDuration bound a meeting's length in minutes.
	minDuration = 1
	maxDuration = 480
)

// bangkok is the zone meeting times are shown in, matching the meetings domain:
// Thailand has a fixed +07:00 offset, so no zone database is needed.
var bangkok = time.FixedZone("Asia/Bangkok", 7*60*60)

// Deps are what New builds the calendar domain from.
type Deps struct {
	DB *db.DB
	// Authz decides every channel-scoped action.
	Authz *authz.Authz
	// Google creates and cancels the Calendar event as the organizer.
	Google google.Client
	// Chat posts the scheduling slip, in the transaction that records the event.
	Chat chat.Service
	// MemberChannels lists the channels a user belongs to; New wires it to
	// chat.Service.MemberChannelIDs so calendar never imports chat for it. It bounds
	// the workspace-wide ListUpcoming.
	MemberChannels func(ctx context.Context, workspaceID, userID uuid.UUID) ([]uuid.UUID, error)
	// Clock tells the time; nil means time.Now.
	Clock func() time.Time
}

type service struct {
	db             *db.DB
	authz          *authz.Authz
	g              google.Client
	chat           chat.Service
	memberChannels func(ctx context.Context, workspaceID, userID uuid.UUID) ([]uuid.UUID, error)
	clock          func() time.Time
}

// New returns the calendar domain.
func New(d Deps) Service {
	clock := d.Clock
	if clock == nil {
		clock = time.Now
	}
	return &service{db: d.DB, authz: d.Authz, g: d.Google, chat: d.Chat, memberChannels: d.MemberChannels, clock: clock}
}

var _ Service = (*service)(nil)

func (s *service) can(ctx context.Context, a authz.Actor, act authz.Action, channelID uuid.UUID) error {
	return s.authz.Can(ctx, a, act, authz.Resource{ChannelID: channelID})
}

// requireUser refuses a principal that is not a signed-in user: only a person
// schedules, lists or cancels a meeting on their own Google calendar.
func requireUser(a authz.Actor) error {
	if a.Kind != authz.ActorUser {
		return fmt.Errorf("calendar: a %s actor may not use the calendar: %w", a.Kind, errs.ErrForbidden)
	}
	if a.WorkspaceID == uuid.Nil || a.UserID == uuid.Nil {
		return fmt.Errorf("calendar: actor has no workspace or user: %w", errs.ErrForbidden)
	}
	return nil
}

func parseID(raw, what string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("calendar: %s %q is not a UUID: %w", what, raw, errs.ErrInvalid)
	}
	return id, nil
}

func validTitle(title string) bool {
	return title != "" && utf8.RuneCountInString(title) <= maxTitleRunes && utf8.ValidString(title) && !strings.ContainsRune(title, 0)
}

// ScheduleMeeting implements Service. The Calendar event is created first, with the
// organizer's own grant, so Google mints the Meet link; the event row and the slip
// that carries the link are then committed together.
func (s *service) ScheduleMeeting(ctx context.Context, a authz.Actor, req *calendarv1.ScheduleMeetingRequest) (*calendarv1.CalendarEvent, error) {
	if err := requireUser(a); err != nil {
		return nil, err
	}
	channelID, err := parseID(req.GetChannelId(), "channel id")
	if err != nil {
		return nil, err
	}
	if err := s.can(ctx, a, authz.ActMeetingStart, channelID); err != nil {
		return nil, err
	}
	title := strings.TrimSpace(req.GetTitle())
	if !validTitle(title) {
		return nil, fmt.Errorf("calendar: a meeting title must be 1-%d characters: %w", maxTitleRunes, errs.ErrInvalid)
	}
	if req.GetStartAt() == nil {
		return nil, fmt.Errorf("calendar: a scheduled meeting needs a start time: %w", errs.ErrInvalid)
	}
	start := req.GetStartAt().AsTime()
	if !start.After(s.clock()) {
		return nil, fmt.Errorf("calendar: the start time must be in the future: %w", errs.ErrInvalid)
	}
	minutes := req.GetDurationMinutes()
	if minutes < minDuration || minutes > maxDuration {
		return nil, fmt.Errorf("calendar: the duration must be %d-%d minutes: %w", minDuration, maxDuration, errs.ErrInvalid)
	}
	end := start.Add(time.Duration(minutes) * time.Minute)

	// Google first: a refused or unconsented call fails before any row is written.
	created, err := s.g.ScheduleCalendarEvent(ctx, a.UserID, google.CalendarEventInput{Title: title, StartAt: start, EndAt: end})
	if err != nil {
		// A *google.ConsentRequiredError or *google.MeetError reaches the caller as it
		// is: the API shell tells the person why, and the log keeps the full error.
		return nil, err
	}

	var row store.CalendarEvent
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		ref, err := refs.Next(ctx, tx, "EVT")
		if err != nil {
			return fmt.Errorf("calendar: %w", err)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("calendar: new event id: %w", err)
		}
		row, err = store.New(tx).InsertEvent(ctx, store.InsertEventParams{
			ID: id, WorkspaceID: a.WorkspaceID, ChannelID: channelID, OwnerUserID: a.UserID,
			Ref: &ref, Title: title, GcalEventID: created.ID, HtmlLink: created.HTMLLink, MeetUri: created.MeetURI,
			StartAt: timestamp(start), EndAt: timestamp(end), OrgWide: req.GetOrgWide(),
		})
		if err != nil {
			return fmt.Errorf("calendar: insert event: %w", err)
		}
		_, err = s.chat.PostMessageTx(ctx, tx, systemActor(row), agentPost(row, "event-scheduled-"+row.ID.String(), schedulingSlip(row)))
		if err != nil {
			return fmt.Errorf("calendar: post scheduling slip: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return eventProto(row), nil
}

// ListUpcoming implements Service.
func (s *service) ListUpcoming(ctx context.Context, a authz.Actor, channelID *uuid.UUID) ([]*calendarv1.CalendarEvent, error) {
	if err := requireUser(a); err != nil {
		return nil, err
	}
	now := s.clock()
	var rows []store.CalendarEvent
	if channelID != nil {
		if err := s.can(ctx, a, authz.ActMeetingRead, *channelID); err != nil {
			return nil, err
		}
		err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
			rows, err = store.New(tx).ListUpcomingByChannel(ctx, store.ListUpcomingByChannelParams{ChannelID: *channelID, Now: timestamp(now)})
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("calendar: list upcoming in channel: %w", err)
		}
		return eventProtos(rows), nil
	}

	channels, err := s.memberChannels(ctx, a.WorkspaceID, a.UserID)
	if err != nil {
		return nil, fmt.Errorf("calendar: list the actor's channels: %w", err)
	}
	if len(channels) == 0 {
		return nil, nil
	}
	ids := make([]string, len(channels))
	for i, id := range channels {
		ids[i] = id.String()
	}
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		rows, err = store.New(tx).ListUpcomingInChannels(ctx, store.ListUpcomingInChannelsParams{ChannelIds: ids, Now: timestamp(now)})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("calendar: list upcoming: %w", err)
	}
	return eventProtos(rows), nil
}

// CancelEvent implements Service. The organizer check is on top of authz: a member who
// could start a meeting still cannot cancel one someone else scheduled.
func (s *service) CancelEvent(ctx context.Context, a authz.Actor, eventID uuid.UUID) error {
	if err := requireUser(a); err != nil {
		return err
	}
	row, err := s.load(ctx, a.WorkspaceID, eventID)
	if err != nil {
		return err
	}
	if err := s.can(ctx, a, authz.ActMeetingStart, row.ChannelID); err != nil {
		return err
	}
	if row.OwnerUserID != a.UserID {
		return fmt.Errorf("calendar: only the organizer cancels event %s: %w", eventID, errs.ErrForbidden)
	}

	// Google first: if the cancel there fails, the row stays scheduled and the person
	// can try again. A gone event is success (see google.CancelCalendarEvent).
	if err := s.g.CancelCalendarEvent(ctx, a.UserID, row.GcalEventID); err != nil {
		return err
	}
	return s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		canceled, err := store.New(tx).CancelEvent(ctx, eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("calendar: event %s is not scheduled: %w", eventID, errs.ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("calendar: cancel event: %w", err)
		}
		_, err = s.chat.PostMessageTx(ctx, tx, systemActor(canceled), agentPost(canceled, "event-canceled-"+canceled.ID.String(),
			"Meeting canceled: "+canceled.Title))
		if err != nil {
			return fmt.Errorf("calendar: post cancellation slip: %w", err)
		}
		return nil
	})
}

// load reads an event; one of another workspace is as good as missing (RLS hides it).
func (s *service) load(ctx context.Context, ws, id uuid.UUID) (row store.CalendarEvent, err error) {
	err = s.db.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) (err error) {
		row, err = store.New(tx).GetEvent(ctx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.CalendarEvent{}, fmt.Errorf("calendar: event %s: %w", id, errs.ErrNotFound)
	}
	if err != nil {
		return store.CalendarEvent{}, fmt.Errorf("calendar: read event: %w", err)
	}
	return row, nil
}

// systemActor is the job-like actor that posts the slip for the organizer, in the
// organizer's workspace. Posting a message needs only a workspace and a user.
func systemActor(row store.CalendarEvent) authz.Actor {
	return authz.Actor{Kind: authz.ActorSystem, WorkspaceID: row.WorkspaceID, UserID: row.OwnerUserID}
}

// agentPost is a message the assistant posts into the event's channel.
func agentPost(row store.CalendarEvent, clientMsgID, body string) *chatv1.SendMessageRequest {
	return &chatv1.SendMessageRequest{
		ChannelId:   row.ChannelID.String(),
		ClientMsgId: clientMsgID,
		Body:        body,
		AuthorKind:  chatv1.AuthorKind_AUTHOR_KIND_AGENT.Enum(),
		AuthorId:    proto.String(authz.AgentID(slipAuthor).String()),
	}
}

// schedulingSlip is the message that announces a scheduled meeting: its title, the
// local time it starts and the Meet link.
func schedulingSlip(row store.CalendarEvent) string {
	when := row.StartAt.Time.In(bangkok).Format("2 Jan 2006, 15:04")
	return fmt.Sprintf("Meeting scheduled: %s on %s — %s", row.Title, when, row.MeetUri)
}

func eventProtos(rows []store.CalendarEvent) []*calendarv1.CalendarEvent {
	out := make([]*calendarv1.CalendarEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, eventProto(r))
	}
	return out
}

func eventProto(r store.CalendarEvent) *calendarv1.CalendarEvent {
	return &calendarv1.CalendarEvent{
		Id: r.ID.String(), ChannelId: r.ChannelID.String(), Ref: deref(r.Ref), Title: r.Title,
		StartAt: timestampProto(r.StartAt), EndAt: timestampProto(r.EndAt),
		MeetUri: r.MeetUri, HtmlLink: r.HtmlLink, Status: statusEnum(r.Status), OrgWide: r.OrgWide,
	}
}

func statusEnum(status string) calendarv1.EventStatus {
	switch status {
	case statusScheduled:
		return calendarv1.EventStatus_EVENT_STATUS_SCHEDULED
	case statusCanceled:
		return calendarv1.EventStatus_EVENT_STATUS_CANCELED
	}
	return calendarv1.EventStatus_EVENT_STATUS_UNSPECIFIED
}

func timestamp(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func timestampProto(t pgtype.Timestamptz) *timestamppb.Timestamp {
	if !t.Valid {
		return nil
	}
	return timestamppb.New(t.Time)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
