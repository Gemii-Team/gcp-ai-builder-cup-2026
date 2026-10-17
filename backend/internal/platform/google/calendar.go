package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
)

// ScheduleCalendarEvent implements Client. The event is created on the user's primary
// calendar with a Meet conference attached (conferenceDataVersion=1), so Google mints
// the Meet link; the created event carries that link back.
func (c *client) ScheduleCalendarEvent(ctx context.Context, userID uuid.UUID, in CalendarEventInput) (CalendarEvent, error) {
	ev, err := c.scheduleCalendarEvent(ctx, userID, in)
	if err != nil {
		return CalendarEvent{}, wrap(err)
	}
	return ev, nil
}

func (c *client) scheduleCalendarEvent(ctx context.Context, userID uuid.UUID, in CalendarEventInput) (CalendarEvent, error) {
	svc, err := c.calendarService(ctx, userID)
	if err != nil {
		return CalendarEvent{}, err
	}
	// requestID keys the conference creation: Google makes one Meet link per distinct
	// id, so a retried insert of the same event asks for the same link.
	requestID, err := uuid.NewV7()
	if err != nil {
		return CalendarEvent{}, fmt.Errorf("new conference request id: %w", err)
	}
	req := &calendar.Event{
		Summary: in.Title,
		Start:   &calendar.EventDateTime{DateTime: in.StartAt.UTC().Format(time.RFC3339)},
		End:     &calendar.EventDateTime{DateTime: in.EndAt.UTC().Format(time.RFC3339)},
		ConferenceData: &calendar.ConferenceData{
			CreateRequest: &calendar.CreateConferenceRequest{
				RequestId:             requestID.String(),
				ConferenceSolutionKey: &calendar.ConferenceSolutionKey{Type: "hangoutsMeet"},
			},
		},
	}
	created, err := svc.Events.Insert("primary", req).ConferenceDataVersion(1).Context(ctx).Do()
	if err != nil {
		return CalendarEvent{}, fmt.Errorf("events.insert: %w", err)
	}
	return CalendarEvent{ID: created.Id, HTMLLink: created.HtmlLink, MeetURI: meetURIOf(created)}, nil
}

// CancelCalendarEvent implements Client. A 404 or 410 is success: the event is already
// gone, which is the state the caller wanted.
func (c *client) CancelCalendarEvent(ctx context.Context, userID uuid.UUID, eventID string) error {
	if err := c.cancelCalendarEvent(ctx, userID, eventID); err != nil {
		return wrap(err)
	}
	return nil
}

func (c *client) cancelCalendarEvent(ctx context.Context, userID uuid.UUID, eventID string) error {
	svc, err := c.calendarService(ctx, userID)
	if err != nil {
		return err
	}
	err = svc.Events.Delete("primary", eventID).Context(ctx).Do()
	var api *googleapi.Error
	if errors.As(err, &api) && (api.Code == http.StatusNotFound || api.Code == http.StatusGone) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("events.delete: %w", err)
	}
	return nil
}

// meetURIOf is the Meet link of a created event: hangoutLink when Google sets it, else
// the first video entry point of the attached conference.
func meetURIOf(ev *calendar.Event) string {
	if ev.HangoutLink != "" {
		return ev.HangoutLink
	}
	if ev.ConferenceData == nil {
		return ""
	}
	for _, ep := range ev.ConferenceData.EntryPoints {
		if ep.EntryPointType == "video" {
			return ep.Uri
		}
	}
	return ""
}
