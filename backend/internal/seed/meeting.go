package seed

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
	tasksv1 "kiwi/gen/kiwi/tasks/v1"
	"kiwi/internal/meetings"
)

// importMeeting records the corpus's meeting in channel as the ingest records one it has
// just summarized (meetings.ImportMeeting: no ASR and no Meet are involved, the
// transcript and the summary are the corpus's), and drafts the tasks of its action items
// in the same transaction, so a meeting never exists without them. It returns the
// meeting's id, derived from the workspace, and does nothing for a meeting that exists.
//
// The action items are not left in the summary for the task.draft job, which would make
// the drafts when a worker next runs: the seed has to leave them behind it. They are made
// here with the request the job would make, the meeting as the source, the transcript
// entry as the anchor with its start as the offset, and a due date from the corpus's
// hint resolved against the meeting's date by the code the job uses (meetings.ResolveDue).
func (s *seeder) importMeeting(ctx context.Context, channel uuid.UUID, spec meetingSpec) (uuid.UUID, error) {
	id := s.derived("meeting", meetingKey)

	entries := make([]*meetingsv1.Entry, len(spec.Entries))
	for i, e := range spec.Entries {
		entries[i] = &meetingsv1.Entry{
			Idx: int32(i), Speaker: proto.String(s.names[s.member(e.Speaker)]),
			StartMs: proto.Int64(e.StartMs), EndMs: proto.Int64(e.EndMs), Text: e.Text,
		}
	}
	summary := meetings.Summary{Summary: spec.Summary, Decisions: items(spec.Decisions), OpenQuestions: items(spec.OpenQuestions)}
	date := time.Now()

	err := meetings.ImportMeeting(ctx, s.meetingDeps, s.owner, meetings.Import{
		ID: id, ChannelID: channel, Title: spec.Title, Entries: entries, Summary: summary,
		InTx: func(ctx context.Context, tx pgx.Tx) error {
			return s.draftTasks(ctx, tx, channel, id, spec, entries, date)
		},
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("seed: import the meeting: %w", err)
	}
	return id, nil
}

func items(in []itemSpec) []meetings.Item {
	out := make([]meetings.Item, len(in))
	for i, it := range in {
		out[i] = meetings.Item{Text: it.Text, EntryRefs: it.EntryRefs}
	}
	return out
}

// draftTasks drafts the meeting's action items in tx, as the job acting for the owner. An
// item with an owner names a member of the channel; one without leaves the draft
// ownerless, for a person to fill in before it can be confirmed.
func (s *seeder) draftTasks(ctx context.Context, tx pgx.Tx, channel, meeting uuid.UUID, spec meetingSpec, entries []*meetingsv1.Entry, date time.Time) error {
	for _, ai := range spec.ActionItems {
		entry := entries[ai.EntryRefs[0]]
		req := &tasksv1.CreateTaskRequest{
			ChannelId: channel.String(),
			Title:     ai.Title,
			Source: &chatv1.Source{
				Kind: chatv1.SourceKind_SOURCE_KIND_MEETING, Id: meeting.String(), Label: spec.Title, ChannelId: channel.String(),
				Anchor: strconv.Itoa(int(entry.GetIdx())), OffsetMs: entry.StartMs,
			},
		}
		if ai.Owner != nil {
			req.AssigneeId = proto.String(s.ids[s.member(*ai.Owner)].String())
		}
		if due := meetings.ResolveDue(ai.Due, date); due != nil {
			req.DueAt = timestamppb.New(*due)
		}
		if _, err := s.tasks.CreateDraftTx(ctx, tx, s.job, req); err != nil {
			return fmt.Errorf("seed: draft %q: %w", ai.Title, err)
		}
	}
	return nil
}
