// Package meetings owns Meet meetings, their transcripts and AI summaries. Other
// domains reach it only through Service.
//
// Meet is polled (PollMeetings), not pushed: the poll finds the transcript and the
// meeting.ingest job reads it until two reads agree. An uploaded recording is the other
// way in (CreateFromRecording): the meeting.transcribe job has the model transcribe it
// and then goes on as meeting.ingest does. Both summarize with one call to the model and
// publish the slip and the draft tasks; code, not the model, resolves who and when. The
// jobs settle what they can: a message the bus is about to dead-letter ends its meeting
// failed with a reason (see settleOnFinalDelivery). The one residual is a database that
// stays unreachable for the whole of the bus's retry window: the meeting then stays
// ingesting, and only a future pass that recovers stale meetings would re-drive it.
//
// A third way in is not a Service method, because only an operator command uses it:
// ImportMeeting records a finished meeting whose transcript and summary the caller
// already has (the demo workspace of `kiwi seed`), through the write the ingest ends in.
package meetings

import (
	"context"

	"github.com/google/uuid"

	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
	"kiwi/internal/platform/authz"
)

// Service is the meetings domain. The comment on each method names the authz.Action
// it checks for the actor on the meeting's channel.
type Service interface {
	// StartMeeting creates a Meet space and a meeting in the channel; it needs
	// authz.ActMeetingStart. The response says whether Meet will start the transcript
	// by itself: a Google account that cannot have that setting gets the space
	// without it, and its host has to turn transcription on. A
	// *google.ConsentRequiredError passes through when the actor has not granted Meet
	// access, and a *google.MeetError when Meet fails for any other reason a person
	// can be told about.
	StartMeeting(ctx context.Context, a authz.Actor, channelID uuid.UUID) (*meetingsv1.StartMeetingResponse, error)
	// CreateFromRecording creates a meeting named title from an uploaded recording
	// instead of a Meet transcript; it needs authz.ActMeetingStart.
	CreateFromRecording(ctx context.Context, a authz.Actor, channelID, fileID uuid.UUID, title string) (*meetingsv1.Meeting, error)
	// GetMeeting returns the meeting with its status and summary; it needs
	// authz.ActMeetingRead.
	GetMeeting(ctx context.Context, a authz.Actor, id uuid.UUID) (*meetingsv1.Meeting, error)
	// GetTranscriptExcerpt returns transcript entry entryIdx with radius entries on
	// each side; it needs authz.ActMeetingRead.
	GetTranscriptExcerpt(ctx context.Context, a authz.Actor, meetingID uuid.UUID, entryIdx, radius int32) (*meetingsv1.Excerpt, error)
	// TranscriptEntries returns every transcript entry of the meeting in order, for
	// indexing. For a system actor (the indexer) it needs authz.ActKnowledgeIndex; for
	// a user or agent actor it needs the ordinary membership-based
	// authz.ActMeetingRead on the meeting's channel.
	TranscriptEntries(ctx context.Context, a authz.Actor, meetingID uuid.UUID) ([]*meetingsv1.Entry, error)
	// RetrySummary queues the summary of a failed meeting again; it needs
	// authz.ActMeetingWrite.
	RetrySummary(ctx context.Context, a authz.Actor, meetingID uuid.UUID) error
}
