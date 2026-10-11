// Package meetings owns Meet meetings, their transcripts and AI summaries. Other
// domains reach it only through Service.
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
	// authz.ActMeetingStart. A *google.ConsentRequiredError passes through when the
	// actor has not granted Meet access.
	StartMeeting(ctx context.Context, a authz.Actor, channelID uuid.UUID) (*meetingsv1.Meeting, error)
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
