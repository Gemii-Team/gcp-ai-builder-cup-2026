// Package google is the port to the Google Meet REST API. Every call runs with the
// Google grant of the user it names, never with a service account.
package google

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Client reads and creates Meet resources as one user. Every method returns a
// *ConsentRequiredError when that user has not granted Meet access (or revoked it),
// and a *MeetError (found with errors.As) for any other failure of Meet that a person
// can be told about: see MeetError.
type Client interface {
	// CreateMeetSpace creates a meeting space for userID, with transcript
	// auto-generation switched on when enableTranscription is true. When Meet refuses
	// that setting for this user (a personal Google account has no auto-transcription)
	// the space is created without it, once, and Space.TranscriptionAutomatic is false.
	CreateMeetSpace(ctx context.Context, userID uuid.UUID, enableTranscription bool) (Space, error)
	// ListConferences returns every conference record of the space, each with its
	// transcripts, so the caller can poll for the end of a meeting and the state of
	// its transcript.
	ListConferences(ctx context.Context, userID uuid.UUID, spaceName string) ([]Conference, error)
	// ListTranscriptEntries returns all entries of a transcript in order, following
	// pagination to the end.
	ListTranscriptEntries(ctx context.Context, userID uuid.UUID, transcriptName string) ([]Entry, error)
}

// Space is a Meet meeting space: Name is its resource name ("spaces/..."), MeetingURI
// the link people join through and Code the short meeting code. TranscriptionAutomatic
// is true when Meet starts the transcript by itself and false when the space has no
// such setting, so the host has to turn transcription on in Meet.
type Space struct {
	Name                   string
	MeetingURI             string
	Code                   string
	TranscriptionAutomatic bool
}

// Conference is one run of a space, from its first participant joining to the last
// one leaving. EndTime is nil while it is still running.
type Conference struct {
	Name        string
	StartTime   time.Time
	EndTime     *time.Time
	Transcripts []TranscriptRef
}

// TranscriptState is the lifecycle state of a Meet transcript.
type TranscriptState string

// Transcript states; entries are only complete once the state is StateFileGenerated.
const (
	StateStarted       TranscriptState = "STARTED"
	StateEnded         TranscriptState = "ENDED"
	StateFileGenerated TranscriptState = "FILE_GENERATED"
)

// TranscriptRef points at a transcript of a conference. EndTime is nil until the
// transcript has ended.
type TranscriptRef struct {
	Name    string
	State   TranscriptState
	EndTime *time.Time
}

// Entry is one utterance of a transcript. StartMS and EndMS are offsets in
// milliseconds from the start of the transcript, nil when the source has no timing.
type Entry struct {
	Idx     int
	Speaker string
	StartMS *int64
	EndMS   *int64
	Text    string
}

// ConsentRequiredError means the user has not granted, or has revoked, the Google
// access a call needs; URL is where they can grant it. It is returned as
// *ConsentRequiredError.
type ConsentRequiredError struct {
	URL string
}

// Error implements error. It leaves URL out so the consent link does not end up in logs.
func (e *ConsentRequiredError) Error() string {
	return "google: the user has not granted the required Google access"
}
