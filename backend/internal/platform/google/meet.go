package google

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	meet "google.golang.org/api/meet/v2"
)

// pageSize is the largest page every Meet list call accepts; asking for it keeps the
// number of round trips low.
const pageSize = 100

// autoTranscription is the space config that makes Meet start a transcript without
// anyone pressing "Transcribe":
//
//	config.artifactConfig.transcriptionConfig.autoTranscriptionGeneration = "ON"
//
// It is the one place that knows this path. Spike S1 (a real meeting) is deferred, so
// the path comes from the generated google.golang.org/api/meet/v2 client rather than
// from a live call; the plan's inferred name, autoGenerationType, is not a field of
// that API. If S1 shows the path differs, change only this function.
func autoTranscription() *meet.SpaceConfig {
	return &meet.SpaceConfig{ArtifactConfig: &meet.ArtifactConfig{
		TranscriptionConfig: &meet.TranscriptionConfig{AutoTranscriptionGeneration: "ON"},
	}}
}

// CreateMeetSpace implements Client.
func (c *client) CreateMeetSpace(ctx context.Context, userID uuid.UUID, enableTranscription bool) (Space, error) {
	space, err := c.createMeetSpace(ctx, userID, enableTranscription)
	if err != nil {
		return Space{}, wrap(err)
	}
	return space, nil
}

func (c *client) createMeetSpace(ctx context.Context, userID uuid.UUID, enableTranscription bool) (Space, error) {
	svc, err := c.service(ctx, userID)
	if err != nil {
		return Space{}, err
	}
	req := &meet.Space{}
	if enableTranscription {
		req.Config = autoTranscription()
	}
	created, err := svc.Spaces.Create(req).Context(ctx).Do()
	if err != nil && enableTranscription && autoTranscriptionRefused(err) {
		// Meet does not let this account have auto-transcription (a personal Google
		// account). The meeting is still worth having: make the space without the
		// setting, once, and say so in the result.
		enableTranscription = false
		created, err = svc.Spaces.Create(&meet.Space{}).Context(ctx).Do()
	}
	if err != nil {
		return Space{}, fmt.Errorf("spaces.create: %w", err)
	}
	return Space{Name: created.Name, MeetingURI: created.MeetingUri, Code: created.MeetingCode, TranscriptionAutomatic: enableTranscription}, nil
}

// ListConferences implements Client.
func (c *client) ListConferences(ctx context.Context, userID uuid.UUID, spaceName string) ([]Conference, error) {
	confs, err := c.listConferences(ctx, userID, spaceName)
	if err != nil {
		return nil, wrap(err)
	}
	return confs, nil
}

func (c *client) listConferences(ctx context.Context, userID uuid.UUID, spaceName string) ([]Conference, error) {
	svc, err := c.service(ctx, userID)
	if err != nil {
		return nil, err
	}
	var records []*meet.ConferenceRecord
	err = svc.ConferenceRecords.List().
		Filter(fmt.Sprintf("space.name = %q", spaceName)).
		PageSize(pageSize).
		Pages(ctx, func(page *meet.ListConferenceRecordsResponse) error {
			records = append(records, page.ConferenceRecords...)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("conferenceRecords.list: %w", err)
	}

	confs := make([]Conference, 0, len(records))
	for _, rec := range records {
		conf, err := conferenceOf(rec)
		if err != nil {
			return nil, err
		}
		err = svc.ConferenceRecords.Transcripts.List(rec.Name).
			PageSize(pageSize).
			Pages(ctx, func(page *meet.ListTranscriptsResponse) error {
				for _, tr := range page.Transcripts {
					ref, err := transcriptRefOf(tr)
					if err != nil {
						return err
					}
					conf.Transcripts = append(conf.Transcripts, ref)
				}
				return nil
			})
		if err != nil {
			return nil, fmt.Errorf("transcripts.list of %s: %w", rec.Name, err)
		}
		confs = append(confs, conf)
	}
	return confs, nil
}

// ListTranscriptEntries implements Client. Entries get Idx 0, 1, 2, ... in the order
// Meet returns them, and offsets relative to the start of the transcript.
func (c *client) ListTranscriptEntries(ctx context.Context, userID uuid.UUID, transcriptName string) ([]Entry, error) {
	entries, err := c.listTranscriptEntries(ctx, userID, transcriptName)
	if err != nil {
		return nil, wrap(err)
	}
	return entries, nil
}

func (c *client) listTranscriptEntries(ctx context.Context, userID uuid.UUID, transcriptName string) ([]Entry, error) {
	svc, err := c.service(ctx, userID)
	if err != nil {
		return nil, err
	}
	tr, err := svc.ConferenceRecords.Transcripts.Get(transcriptName).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("transcripts.get: %w", err)
	}
	transcriptStart, err := optTime("transcript startTime", tr.StartTime)
	if err != nil {
		return nil, err
	}

	speakers := make(map[string]string) // participant resource name -> display name, for this call only
	var out []Entry
	err = svc.ConferenceRecords.Transcripts.Entries.List(transcriptName).
		PageSize(pageSize).
		Pages(ctx, func(page *meet.ListTranscriptEntriesResponse) error {
			for _, e := range page.TranscriptEntries {
				entry := Entry{Idx: len(out), Text: e.Text}
				if e.Participant != "" {
					name, ok := speakers[e.Participant]
					if !ok {
						p, err := svc.ConferenceRecords.Participants.Get(e.Participant).Context(ctx).Do()
						if err != nil {
							return fmt.Errorf("participants.get: %w", err)
						}
						name = displayName(p)
						speakers[e.Participant] = name
					}
					entry.Speaker = name
				}
				var err error
				if entry.StartMS, err = offsetMS("entry startTime", e.StartTime, transcriptStart); err != nil {
					return err
				}
				if entry.EndMS, err = offsetMS("entry endTime", e.EndTime, transcriptStart); err != nil {
					return err
				}
				out = append(out, entry)
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("transcriptEntries.list of %s: %w", transcriptName, err)
	}
	return out, nil
}

func conferenceOf(rec *meet.ConferenceRecord) (Conference, error) {
	start, err := optTime("conference startTime", rec.StartTime)
	if err != nil {
		return Conference{}, err
	}
	if start == nil {
		return Conference{}, fmt.Errorf("conference %s has no startTime", rec.Name)
	}
	end, err := optTime("conference endTime", rec.EndTime)
	if err != nil {
		return Conference{}, err
	}
	return Conference{Name: rec.Name, StartTime: *start, EndTime: end}, nil
}

func transcriptRefOf(tr *meet.Transcript) (TranscriptRef, error) {
	// The API's state names are the TranscriptState values.
	state := TranscriptState(tr.State)
	switch state {
	case StateStarted, StateEnded, StateFileGenerated:
	default:
		return TranscriptRef{}, fmt.Errorf("transcript %s has unexpected state %q", tr.Name, tr.State)
	}
	end, err := optTime("transcript endTime", tr.EndTime)
	if err != nil {
		return TranscriptRef{}, err
	}
	return TranscriptRef{Name: tr.Name, State: state, EndTime: end}, nil
}

// displayName is how Meet shows the participant: the account name of a signed-in
// user, the name an anonymous user typed, or the redacted number of a phone caller.
func displayName(p *meet.Participant) string {
	switch {
	case p.SignedinUser != nil:
		return p.SignedinUser.DisplayName
	case p.AnonymousUser != nil:
		return p.AnonymousUser.DisplayName
	case p.PhoneUser != nil:
		return p.PhoneUser.DisplayName
	}
	return ""
}

// optTime parses an RFC 3339 timestamp. Meet leaves a field out until it has a value,
// so "" and the zero time both come back as nil.
func optTime(field, s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", field, s, err)
	}
	if t.IsZero() {
		return nil, nil
	}
	t = t.UTC()
	return &t, nil
}

// offsetMS is s as milliseconds after base; nil when either has no value.
func offsetMS(field, s string, base *time.Time) (*int64, error) {
	t, err := optTime(field, s)
	if err != nil || t == nil || base == nil {
		return nil, err
	}
	ms := t.Sub(*base).Milliseconds()
	return &ms, nil
}
