package meetings

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	eventsv1 "kiwi/gen/kiwi/events/v1"
	"kiwi/internal/meetings/internal/store"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/google"
)

const (
	// pollWindow is how long after it was made a meeting is still asked about.
	pollWindow = 24 * time.Hour
	// transcriptCheckDelay is how long after a conference ended Meet has to produce a
	// transcript before the meeting is declared transcript-less.
	transcriptCheckDelay = 30 * time.Minute
)

// PollMeetings asks Meet about every meeting that was made in the last 24 hours with a
// Meet space and is still waiting for its summary, as the meeting's owner, and moves
// each along:
//
//   - the latest conference record and its start are remembered, and the first time it
//     is seen to have ended its end is too, and a meeting.transcript_check is scheduled
//     for half an hour later;
//   - a pending meeting whose conference has a transcript in StateFileGenerated becomes
//     ingesting and one meeting.ingest names the earliest such transcript (the others are
//     ignored). A transcript that is still STARTED or ENDED is not read: its entries are
//     not complete until Meet has generated the file.
//
// An owner whose Google access is gone (consent missing or revoked, or Meet answering
// 401 or 403) fails the meeting as meet_access. Any other error from Meet is logged and
// the meeting is asked about again on the next call.
//
// This file is on the db.System allowlist: the enumeration is the one thing that has to
// see every workspace, and it only reads. Every write goes through db.InWorkspace.
func PollMeetings(ctx context.Context, d *db.DB, g google.Client, now time.Time) error {
	var due []store.ListPollableMeetingsRow
	err := d.System(ctx, func(ctx context.Context, tx pgx.Tx) (err error) {
		due, err = store.New(tx).ListPollableMeetings(ctx, timestamp(now.Add(-pollWindow)))
		return err
	})
	if err != nil {
		return fmt.Errorf("meetings: list meetings to poll: %w", err)
	}
	for _, m := range due {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := pollOne(ctx, d, g, m); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "meetings: poll failed, will ask again", "meeting_id", m.ID, "workspace_id", m.WorkspaceID, "error", err)
		}
	}
	return nil
}

// pollOne asks Meet about one meeting and records what it learned.
func pollOne(ctx context.Context, d *db.DB, g google.Client, m store.ListPollableMeetingsRow) error {
	confs, err := g.ListConferences(ctx, m.OwnerUserID, *m.MeetSpace)
	if google.IsAccessDenied(err) {
		slog.WarnContext(ctx, "meetings: the owner's Meet access is gone", "meeting_id", m.ID, "workspace_id", m.WorkspaceID, "error", err)
		return d.InWorkspace(ctx, m.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
			return failMeeting(ctx, tx, m.ID, reasonMeetAccess)
		})
	}
	if err != nil {
		return fmt.Errorf("list conferences: %w", err)
	}
	conf, ok := latestConference(confs)
	if !ok {
		return nil // nobody has joined yet
	}

	return d.InWorkspace(ctx, m.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.LockMeeting(ctx, m.ID)
		if err != nil {
			return fmt.Errorf("lock meeting: %w", err)
		}
		if cur.Status != statusPending && cur.Status != statusIngesting {
			return nil // it settled while Meet was being asked
		}
		var ended pgtype.Timestamptz
		if conf.EndTime != nil {
			ended = timestamp(*conf.EndTime)
		}
		row, err := q.RecordConference(ctx, store.RecordConferenceParams{
			ID: m.ID, ConferenceRecord: &conf.Name, StartedAt: timestamp(conf.StartTime), EndedAt: ended,
		})
		if err != nil {
			return fmt.Errorf("record conference: %w", err)
		}

		if !cur.EndedAt.Valid && conf.EndTime != nil {
			if err := enqueue(ctx, tx, events.TopicMeetingTranscriptCheck, row, conf.EndTime.Add(transcriptCheckDelay), &eventsv1.MeetingTranscriptCheck{
				WorkspaceId: row.WorkspaceID.String(), MeetingId: row.ID.String(),
			}); err != nil {
				return err
			}
		}

		if row.Status != statusPending {
			return nil
		}
		name, ok := earliestGenerated(conf)
		if !ok {
			return nil
		}
		started, err := q.StartIngest(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("start ingest: %w", err)
		}
		return enqueue(ctx, tx, events.TopicMeetingIngest, started, time.Time{}, &eventsv1.MeetingIngest{
			WorkspaceId: started.WorkspaceID.String(), MeetingId: started.ID.String(), TranscriptName: name, Attempt: 1,
		})
	})
}

// latestConference is the conference of a space that started last; the second result
// is false when the space has none.
func latestConference(confs []google.Conference) (google.Conference, bool) {
	var latest google.Conference
	found := false
	for _, c := range confs {
		if !found || c.StartTime.After(latest.StartTime) {
			latest, found = c, true
		}
	}
	return latest, found
}

// earliestGenerated is the name of the transcript of c whose file Meet has generated
// and that ended first; a transcript without an end time sorts last, and ties keep
// Meet's order. The second result is false when there is none.
func earliestGenerated(c google.Conference) (string, bool) {
	var best *google.TranscriptRef
	for i := range c.Transcripts {
		t := &c.Transcripts[i]
		if t.State != google.StateFileGenerated {
			continue
		}
		if best == nil || endsBefore(t.EndTime, best.EndTime) {
			best = t
		}
	}
	if best == nil {
		return "", false
	}
	return best.Name, true
}

// endsBefore reports whether a is an earlier end than b, a missing end being the latest.
func endsBefore(a, b *time.Time) bool {
	return a != nil && (b == nil || a.Before(*b))
}
