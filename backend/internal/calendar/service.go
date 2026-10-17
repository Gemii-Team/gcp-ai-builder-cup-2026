// Package calendar owns scheduled meetings: Google Calendar events with a Meet link,
// on the organizer's own calendar. Other domains reach it only through Service.
//
// A scheduled meeting is not a Meet meeting yet: the event holds the time and the
// link, and the person joins at that time, when StartMeeting (the meetings domain)
// takes over. The channel sees a scheduling slip the assistant posts, which is an
// ordinary chat message, so the realtime stream carries it with no calendar-specific
// frame. ListUpcoming is pull-based: a panel of coming meetings reads it.
package calendar

import (
	"context"

	"github.com/google/uuid"

	calendarv1 "kiwi/gen/kiwi/calendar/v1"
	"kiwi/internal/platform/authz"
)

// Service is the calendar domain. Scheduling and cancelling a meeting are the same
// permission as starting one (authz.ActMeetingStart); listing is authz.ActMeetingRead.
// Only the organizer cancels their own event, which Service checks on top of authz.
type Service interface {
	// ScheduleMeeting creates a Calendar event with a Meet link in the channel and
	// posts the scheduling slip; it needs authz.ActMeetingStart. A
	// *google.ConsentRequiredError passes through when the actor has not granted
	// Calendar access, and a *google.MeetError when Google fails for another reason a
	// person can be told about.
	ScheduleMeeting(ctx context.Context, a authz.Actor, req *calendarv1.ScheduleMeetingRequest) (*calendarv1.CalendarEvent, error)
	// ListUpcoming returns events that have not ended, soonest first: of one channel
	// when channelID is set (needs authz.ActMeetingRead on it), or of every channel the
	// actor belongs to when it is nil.
	ListUpcoming(ctx context.Context, a authz.Actor, channelID *uuid.UUID) ([]*calendarv1.CalendarEvent, error)
	// CancelEvent cancels a scheduled event in Google Calendar and marks it canceled;
	// it needs authz.ActMeetingStart and that the actor is the event's organizer.
	CancelEvent(ctx context.Context, a authz.Actor, eventID uuid.UUID) error
}
