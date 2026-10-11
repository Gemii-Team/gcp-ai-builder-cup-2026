package meetings

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	eventsv1 "kiwi/gen/kiwi/events/v1"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/outbox"
)

// Jobs returns the consumers of the meetings domain for the composition root to
// register: meeting.ingest, which reads a Meet transcript until two reads agree and
// then summarizes it, and meeting.transcript_check, which ends a meeting whose
// transcript never came. The poll (PollMeetings) is what produces both events.
//
// Every consumer is wrapped by settleOnFinalDelivery, so a message the bus is about to
// dead-letter leaves its meeting failed, not ingesting for ever.
func Jobs(d Deps) []bus.Job {
	p := newPipeline(d)
	return []bus.Job{
		{
			Consumer: consumerIngest,
			Topics:   []string{events.TopicMeetingIngest},
			Handler: settleOnFinalDelivery(d.DB, outbox.Once(d.DB, consumerIngest, p.prepareIngest),
				func(m bus.Message) (uuid.UUID, uuid.UUID, error) { return meetingOf(m, &eventsv1.MeetingIngest{}) }),
		},
		{
			Consumer: consumerTranscriptCheck,
			Topics:   []string{events.TopicMeetingTranscriptCheck},
			Handler: settleOnFinalDelivery(d.DB, outbox.Once(d.DB, consumerTranscriptCheck, p.prepareCheck),
				func(m bus.Message) (uuid.UUID, uuid.UUID, error) {
					return meetingOf(m, &eventsv1.MeetingTranscriptCheck{})
				}),
		},
	}
}

// meetingEvent is an event of the meetings pipeline: it names a workspace and a meeting.
type meetingEvent interface {
	proto.Message
	GetWorkspaceId() string
	GetMeetingId() string
}

// meetingOf decodes m into ev and returns the workspace and meeting it names.
func meetingOf(m bus.Message, ev meetingEvent) (workspace, meeting uuid.UUID, err error) {
	if err = decodeEvent(m, ev); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if workspace, err = parseID(ev.GetWorkspaceId(), "workspace id"); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if meeting, err = parseID(ev.GetMeetingId(), "meeting id"); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return workspace, meeting, nil
}

// settleOnFinalDelivery wraps h, a consumer of the meetings pipeline. The consumers
// settle the failures they understand themselves (a Meet error or a summarizer error
// on the final delivery, no speech); what is left is a failure of the write inside the
// one transaction, or of the database read before it, and the bus redelivers those. On
// the final delivery the bus dead-letters the message and counts it settled, so
// nothing would ever finish the meeting. There the wrapper decodes the event, fails
// the meeting with reason internal in a fresh transaction (failMeeting leaves a meeting
// that already settled alone) and acknowledges the message; the meeting then shows its
// failure and a Retry. A process that is stopping (the context is done) is not a
// verdict: the error is returned and the message redelivered.
//
// Residual: if the database stays unreachable for the whole of the bus's retry window
// (about 2.6 minutes with the default backoff) the fresh transaction fails as well, the
// original error is returned and the meeting stays as it was; only a pass that recovers
// meetings stuck in ingesting would re-drive it, and none exists yet.
func settleOnFinalDelivery(d *db.DB, h bus.Handler, target func(bus.Message) (workspace, meeting uuid.UUID, err error)) bus.Handler {
	return func(ctx context.Context, m bus.Message) error {
		err := h(ctx, m)
		if err == nil || m.Attempt < finalAttempt || ctx.Err() != nil {
			return err
		}
		ws, id, terr := target(m)
		if terr != nil {
			return err
		}
		ferr := d.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
			return failMeeting(ctx, tx, id, reasonInternal)
		})
		if ferr != nil {
			slog.ErrorContext(ctx, "meetings: could not settle a message on its final delivery",
				"topic", m.Topic, "meeting_id", id, "error", err, "settle_error", ferr)
			return err
		}
		slog.WarnContext(ctx, "meetings: message failed on its final delivery, meeting failed",
			"topic", m.Topic, "meeting_id", id, "reason", reasonInternal, "error", err)
		return nil
	}
}
