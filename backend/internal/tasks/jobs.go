package tasks

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	tasksv1 "kiwi/gen/kiwi/tasks/v1"
	"kiwi/internal/meetings"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/outbox"
)

const consumerDraft = "task.draft"

// Jobs returns the consumers of the tasks domain for the composition root to register:
// task.draft, which turns the action items of a summarized meeting into draft tasks.
func Jobs(d Deps) []bus.Job {
	if d.ChannelMembers == nil || d.MemberChannels == nil {
		panic("tasks: Deps.ChannelMembers and Deps.MemberChannels are required by the task.draft job")
	}
	j := &drafter{svc: New(d), members: d.ChannelMembers}
	return []bus.Job{{
		Consumer: consumerDraft,
		Topics:   []string{events.TopicTaskDraft},
		Handler:  outbox.Once(d.DB, consumerDraft, j.prepare),
	}}
}

type drafter struct {
	svc     Service
	members func(ctx context.Context, a authz.Actor, channelID uuid.UUID) ([]*chatv1.Member, error)
}

// prepare is the task.draft job. The model only wrote down what the transcript said about
// who and when; here code decides. An owner hint becomes the one member of the meeting's
// channel it names, and stays empty when it names none or several (ResolveOwner); a due
// hint becomes a day in Asia/Bangkok counted from the meeting's date, and stays empty
// when it is "none", is not in the grammar, lies far from the meeting, or the event has
// no date (ResolveDue). Every action item becomes a draft in the channel, in one
// transaction, with the meeting as its source: the transcript entry it was said in as
// the anchor (none for an item that names no entry, meetings.NoSourceEntry) and that
// entry's start as the offset (none for an entry without timing). Drafts are made by the
// job as the agent acting for the meeting's owner; a person confirms them, which is the
// only way they become tasks.
//
// An item without a title is skipped. The write is exactly-once per event, and all
// drafts or none: an error, such as a member who left the channel between the lookup and
// the write, makes the redelivery start over with a fresh member list.
func (j *drafter) prepare(ctx context.Context, m bus.Message) (outbox.Apply, error) {
	var ev eventsv1.TaskDraftRequested
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(m.Data, &ev); err != nil {
		return nil, fmt.Errorf("tasks: decode %s: %v: %w", m.Topic, err, errs.ErrInvalid)
	}
	ws, err := parseID(ev.GetWorkspaceId(), "workspace id")
	if err != nil {
		return nil, err
	}
	meeting, err := parseID(ev.GetMeetingId(), "meeting id")
	if err != nil {
		return nil, err
	}
	channel, err := parseID(ev.GetChannelId(), "channel id")
	if err != nil {
		return nil, err
	}
	owner, err := parseID(ev.GetOwnerUserId(), "owner user id")
	if err != nil {
		return nil, err
	}

	var items []*eventsv1.TaskDraftRequested_Item
	for _, it := range ev.GetItems() {
		if strings.TrimSpace(it.GetTitle()) != "" {
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		return nil, nil
	}

	actor := authz.Actor{Kind: authz.ActorSystem, WorkspaceID: ws, UserID: owner}
	members, err := j.members(ctx, actor, channel)
	if err != nil {
		return nil, fmt.Errorf("tasks: list the members of channel %s: %w", channel, err)
	}
	var date *time.Time
	if ts := ev.GetMeetingDate(); ts != nil && ts.CheckValid() == nil {
		date = new(ts.AsTime())
	}

	reqs := make([]*tasksv1.CreateTaskRequest, 0, len(items))
	for _, it := range items {
		req := &tasksv1.CreateTaskRequest{
			ChannelId: channel.String(),
			Title:     it.GetTitle(),
			Source: &chatv1.Source{
				Kind: chatv1.SourceKind_SOURCE_KIND_MEETING, Id: meeting.String(), Label: ev.GetMeetingTitle(), ChannelId: channel.String(),
			},
		}
		if it.GetEntryIdx() >= 0 {
			req.Source.Anchor = strconv.Itoa(int(it.GetEntryIdx()))
			req.Source.OffsetMs = it.OffsetMs
		}
		if id := meetings.ResolveOwner(it.GetOwnerHint(), members); id != nil {
			req.AssigneeId = proto.String(id.String())
		}
		if date != nil {
			// ResolveDue keeps a date near the meeting, but a meeting at the edge of what a
			// timestamp can hold has a neighbourhood that is not all valid: such a date is
			// left off. It is never the reason a whole meeting's drafts are refused.
			if due := meetings.ResolveDue(it.GetDueHint(), *date); due != nil {
				if ts := timestamppb.New(*due); ts.CheckValid() == nil {
					req.DueAt = ts
				}
			}
		}
		reqs = append(reqs, req)
	}
	return func(ctx context.Context, tx pgx.Tx) error {
		for _, req := range reqs {
			if _, err := j.svc.CreateDraftTx(ctx, tx, actor, req); err != nil {
				return fmt.Errorf("tasks: draft %q of meeting %s: %w", req.GetTitle(), meeting, err)
			}
		}
		return nil
	}, nil
}
