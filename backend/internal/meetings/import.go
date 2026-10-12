package meetings

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
	"kiwi/internal/meetings/internal/store"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/refs"
)

// Import is a finished meeting that was neither held on Meet nor recorded, whose
// transcript and summary the caller already has. It is how an operator command (`kiwi
// seed`) brings in a meeting without a Meet space to poll or a recording to transcribe.
type Import struct {
	// ID names the meeting. The caller derives it from what identifies the meeting (a
	// UUIDv5 of the workspace and a name), so that importing it again changes nothing.
	ID        uuid.UUID
	ChannelID uuid.UUID
	Title     string
	// Entries is the whole transcript, numbered from 0 without gaps.
	Entries []*meetingsv1.Entry
	// Summary is what the summarizer would have found. Its action items are announced
	// for the task.draft job, as a Meet meeting's are.
	Summary Summary
	// InTx, when set, runs in the transaction that records a new meeting, after the
	// meeting and its slip are written, so that what it writes (a task drafted from
	// the meeting) commits with the meeting or not at all. A meeting that was already
	// imported does not run it again.
	InTx func(ctx context.Context, tx pgx.Tx) error
}

// ImportMeeting records in as a ready meeting of the actor, who must be a member of
// its channel (authz.ActMeetingStart) and becomes its owner. It writes what the ingest
// job writes once a summary is ready, through the same code: the transcript, the summary
// and the MTG reference, the slip in the channel, the events that start the indexing and
// refresh the channel, and the drafting of tasks when the summary has action items. The
// meeting has no Meet space, so the poll never looks at it. A meeting already imported
// under in.ID is left exactly as it is.
func ImportMeeting(ctx context.Context, d Deps, a authz.Actor, in Import) error {
	if d.DB == nil || d.Authz == nil || d.Chat == nil {
		return errors.New("meetings: ImportMeeting needs Deps.DB, Deps.Authz and Deps.Chat")
	}
	s := &service{db: d.DB, authz: d.Authz, chat: d.Chat, clock: clockOf(d)}
	if err := requireActor(a); err != nil {
		return err
	}
	if err := s.can(ctx, a, authz.ActMeetingStart, in.ChannelID); err != nil {
		return err
	}
	title := strings.TrimSpace(in.Title)
	if in.ID == uuid.Nil || !validTitle(title) {
		return fmt.Errorf("meetings: an imported meeting needs an id and a title of 1-%d characters: %w", maxTitleRunes, errs.ErrInvalid)
	}
	entries, err := importedEntries(in.Entries)
	if err != nil {
		return err
	}
	sum := cleanSummary(in.Summary)
	if strings.TrimSpace(sum.Summary) == "" {
		return fmt.Errorf("meetings: an imported meeting needs a summary: %w", errs.ErrInvalid)
	}

	p := &pipeline{db: d.DB, chat: d.Chat, clock: s.clock}
	return d.DB.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		// Two runs at once make one meeting: the second waits here and then finds it.
		if err := q.LockRecording(ctx, "import/"+in.ID.String()); err != nil {
			return fmt.Errorf("meetings: lock import: %w", err)
		}
		if _, err := q.GetMeeting(ctx, in.ID); err == nil {
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("meetings: look up the imported meeting: %w", err)
		}

		ref, err := refs.Next(ctx, tx, "MTG")
		if err != nil {
			return fmt.Errorf("meetings: %w", err)
		}
		row, err := q.InsertMeeting(ctx, store.InsertMeetingParams{
			ID: in.ID, WorkspaceID: a.WorkspaceID, ChannelID: in.ChannelID, OwnerUserID: a.EffectiveUserID(), Ref: &ref, Title: title,
		})
		if err != nil {
			return fmt.Errorf("meetings: insert meeting: %w", err)
		}
		// The ingest writes a meeting that is ingesting; this one skips the reading.
		if row, err = q.StartIngest(ctx, row.ID); err != nil {
			return fmt.Errorf("meetings: start ingest: %w", err)
		}
		if err := p.ready(row, entries, sum)(ctx, tx); err != nil {
			return err
		}
		if in.InTx != nil {
			return in.InTx(ctx, tx)
		}
		return nil
	})
}

// importedEntries checks a transcript that comes from the caller and returns it as the
// ingest would have stored it: numbered from 0 without gaps, with its text cleaned and
// never empty, and the speaker left unset when it is blank.
func importedEntries(in []*meetingsv1.Entry) ([]*meetingsv1.Entry, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("meetings: an imported meeting needs a transcript: %w", errs.ErrInvalid)
	}
	out := make([]*meetingsv1.Entry, len(in))
	for i, e := range in {
		text := strings.TrimSpace(cleanText(e.GetText()))
		if int(e.GetIdx()) != i || text == "" {
			return nil, fmt.Errorf("meetings: transcript entry %d must be numbered %d and have text: %w", i, i, errs.ErrInvalid)
		}
		c := &meetingsv1.Entry{Idx: e.GetIdx(), Text: text, StartMs: e.StartMs, EndMs: e.EndMs}
		if speaker := strings.TrimSpace(cleanText(e.GetSpeaker())); speaker != "" {
			c.Speaker = proto.String(speaker)
		}
		out[i] = c
	}
	return out, nil
}
