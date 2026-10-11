package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	tasksv1 "kiwi/gen/kiwi/tasks/v1"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/outbox"
	"kiwi/internal/platform/refs"
	"kiwi/internal/tasks/internal/store"
)

// maxQuoteRunes is the longest source quote a task keeps, in runes.
const maxQuoteRunes = 500

// Values of the text columns.
const (
	stateDraft     = "draft"
	stateConfirmed = "confirmed"

	statusTodo  = "todo"
	statusDoing = "doing"
	statusDone  = "done"

	creatorUser  = "user"
	creatorAgent = "agent"

	sourceMeeting = "meeting"
	sourceFile    = "file"
	sourceMessage = "message"
)

// jobCreator names the creator of a draft a background job makes without naming
// an agent: created_by_id is authz.AgentID(jobCreator).
const jobCreator = "system"

// fromRefPattern is the REF of the answer a task was made from: three capitals, a
// dash and the number refs.Next hands out, which is unpadded and unbounded, so it
// has four digits only until the workspace's 9999th REF of that kind.
var fromRefPattern = regexp.MustCompile(`^[A-Z]{3}-\d{4,}$`)

// protoJSON is how events are carried on the outbox: field names as in the .proto,
// which is what outbox.Enqueue reads workspace_id by.
var protoJSON = protojson.MarshalOptions{UseProtoNames: true}

// Deps are what New builds the tasks domain from.
type Deps struct {
	DB *db.DB
	// Authz decides every channel-scoped action.
	Authz *authz.Authz
	// MemberChannels lists the channels userID belongs to; New wires it to
	// chat.Service.MemberChannelIDs so that tasks never imports chat. It bounds
	// the workspace-wide ListTasks and decides who may be assigned a task.
	MemberChannels func(ctx context.Context, workspaceID, userID uuid.UUID) ([]uuid.UUID, error)
}

type service struct {
	db             *db.DB
	authz          *authz.Authz
	memberChannels func(ctx context.Context, workspaceID, userID uuid.UUID) ([]uuid.UUID, error)
}

// New returns the tasks domain.
func New(d Deps) Service {
	return &service{db: d.DB, authz: d.Authz, memberChannels: d.MemberChannels}
}

var _ Service = (*service)(nil)

// can asks authz whether a may perform act on the channel.
func (s *service) can(ctx context.Context, a authz.Actor, act authz.Action, channelID uuid.UUID) error {
	return s.authz.Can(ctx, a, act, authz.Resource{ChannelID: channelID})
}

// requireActor refuses a principal the methods that need no authz action cannot act
// for: authz.Can denies by default and so does this.
func requireActor(a authz.Actor) error {
	switch a.Kind {
	case authz.ActorUser, authz.ActorAgent, authz.ActorSystem:
	default:
		return fmt.Errorf("tasks: unknown actor kind %q: %w", a.Kind, errs.ErrForbidden)
	}
	if a.WorkspaceID == uuid.Nil || a.EffectiveUserID() == uuid.Nil {
		return fmt.Errorf("tasks: actor has no workspace or user: %w", errs.ErrForbidden)
	}
	return nil
}

// authorize reads task id and checks that a may perform act on its channel, which
// never changes, so the check holds for the write that follows.
func (s *service) authorize(ctx context.Context, a authz.Actor, id uuid.UUID, act authz.Action) (store.TasksTask, error) {
	if err := requireActor(a); err != nil {
		return store.TasksTask{}, err
	}
	var row store.TasksTask
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		row, err = store.New(tx).GetTask(ctx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.TasksTask{}, fmt.Errorf("tasks: task %s: %w", id, errs.ErrNotFound)
	}
	if err != nil {
		return store.TasksTask{}, fmt.Errorf("tasks: read task: %w", err)
	}
	if err := s.can(ctx, a, act, row.ChannelID); err != nil {
		return store.TasksTask{}, err
	}
	return row, nil
}

// requireMember is errs.ErrInvalid unless userID belongs to the channel: only
// members of a channel can be given its tasks.
func (s *service) requireMember(ctx context.Context, ws, channelID, userID uuid.UUID) error {
	ids, err := s.memberChannels(ctx, ws, userID)
	if err != nil {
		return fmt.Errorf("tasks: list the channels of %s: %w", userID, err)
	}
	if !slices.Contains(ids, channelID) {
		return fmt.Errorf("tasks: assignee %s is not a member of channel %s: %w", userID, channelID, errs.ErrInvalid)
	}
	return nil
}

// ---- list -------------------------------------------------------------------

// ListTasks implements Service. A state that is unspecified in the filter is
// errs.ErrInvalid. A system actor holds no task.read, so it may list no tasks, in
// one channel or in all.
func (s *service) ListTasks(ctx context.Context, a authz.Actor, q ListTasksQuery) ([]*tasksv1.Task, error) {
	states, err := stateTexts(q.States)
	if err != nil {
		return nil, err
	}

	var channels []uuid.UUID
	if q.ChannelID != nil {
		if err := s.can(ctx, a, authz.ActTaskRead, *q.ChannelID); err != nil {
			return nil, err
		}
		channels = []uuid.UUID{*q.ChannelID}
	} else {
		if err := requireActor(a); err != nil {
			return nil, err
		}
		// The bound to the actor's channels stands in for task.read of each of them,
		// which the authz table grants users and agents but not jobs.
		if a.Kind == authz.ActorSystem {
			return nil, fmt.Errorf("tasks: a %s actor may not list tasks: %w", a.Kind, errs.ErrForbidden)
		}
		if channels, err = s.memberChannels(ctx, a.WorkspaceID, a.EffectiveUserID()); err != nil {
			return nil, fmt.Errorf("tasks: list member channels: %w", err)
		}
		if len(channels) == 0 {
			return []*tasksv1.Task{}, nil
		}
	}
	ids := make([]string, len(channels))
	for i, c := range channels {
		ids[i] = c.String()
	}

	var rows []store.TasksTask
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		rows, err = store.New(tx).ListTasks(ctx, store.ListTasksParams{ChannelIds: ids, States: states})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("tasks: list tasks: %w", err)
	}
	out := make([]*tasksv1.Task, len(rows))
	for i, r := range rows {
		if out[i], err = taskProto(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---- create -----------------------------------------------------------------

// newTask is a create request after the rules have been applied to it.
type newTask struct {
	channelID   uuid.UUID
	title       string
	description string
	assignee    *uuid.UUID
	dueAt       pgtype.Timestamptz
	source      *taskSource
	fromRef     *string
}

// taskSource is the typed reference a task came from and its display snapshot.
type taskSource struct {
	kind   string
	id     uuid.UUID
	anchor string
	meta   json.RawMessage
}

// sourceMeta is the display snapshot stored in source_meta, so a task renders its
// source without a cross-domain call.
type sourceMeta struct {
	Label    string     `json:"label"`
	At       *time.Time `json:"at,omitempty"`
	OffsetMs *int64     `json:"offset_ms,omitempty"`
	Page     *int32     `json:"page,omitempty"`
	Quote    *string    `json:"quote,omitempty"`
}

// CreateTask implements Service. A request without a state, or with CONFIRMED,
// makes a confirmed task; DRAFT is CreateDraft.
func (s *service) CreateTask(ctx context.Context, a authz.Actor, req *tasksv1.CreateTaskRequest) (*tasksv1.Task, error) {
	switch req.GetState() {
	case tasksv1.TaskState_TASK_STATE_DRAFT:
		return s.CreateDraft(ctx, a, req)
	case tasksv1.TaskState_TASK_STATE_UNSPECIFIED, tasksv1.TaskState_TASK_STATE_CONFIRMED:
	default:
		return nil, fmt.Errorf("tasks: task state %s: %w", req.GetState(), errs.ErrInvalid)
	}
	n, err := s.newTask(ctx, a, req, authz.ActTaskWrite, false)
	if err != nil {
		return nil, err
	}
	var task *tasksv1.Task
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		task, err = insert(ctx, tx, a, stateConfirmed, n)
		return err
	})
	if err != nil {
		return nil, err
	}
	return task, nil
}

// CreateDraft implements Service. It always makes a draft, whatever the request
// says about its state, and the request must name a source.
func (s *service) CreateDraft(ctx context.Context, a authz.Actor, req *tasksv1.CreateTaskRequest) (*tasksv1.Task, error) {
	n, err := s.newTask(ctx, a, req, authz.ActTaskWriteDraft, true)
	if err != nil {
		return nil, err
	}
	var task *tasksv1.Task
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		task, err = insert(ctx, tx, a, stateDraft, n)
		return err
	})
	if err != nil {
		return nil, err
	}
	return task, nil
}

// CreateDraftTx implements Service. tx must be bound to the actor's workspace
// (db.InWorkspace). The authorization and assignee lookups run on their own pooled
// connections, not on tx.
//
// A pipeline must draft into ONE channel per transaction. insert locks the channel's
// To do lane and then allocates the REF, and the TSK counter row stays locked until
// tx ends: a transaction that drafts into a second channel asks for that lane's lock
// while holding the counter, and deadlocks against a concurrent draft that holds the
// second lane and waits for the counter.
func (s *service) CreateDraftTx(ctx context.Context, tx pgx.Tx, a authz.Actor, req *tasksv1.CreateTaskRequest) (*tasksv1.Task, error) {
	n, err := s.newTask(ctx, a, req, authz.ActTaskWriteDraft, true)
	if err != nil {
		return nil, err
	}
	return insert(ctx, tx, a, stateDraft, n)
}

// newTask authorizes act on the request's channel and turns the request into a
// newTask:
//   - the title is trimmed and must not be empty;
//   - an assignee must be a member of the channel;
//   - from_ref is empty or an answer REF such as ANS-2209;
//   - a source names its kind and a UUID, its quote has at most 500 runes, its
//     channel, when it names one, is the task's, and at belongs to a message;
//   - a draft needs a source; a task made by hand has none.
func (s *service) newTask(ctx context.Context, a authz.Actor, req *tasksv1.CreateTaskRequest, act authz.Action, draft bool) (newTask, error) {
	channelID, err := parseID(req.GetChannelId(), "channel id")
	if err != nil {
		return newTask{}, err
	}
	if err := s.can(ctx, a, act, channelID); err != nil {
		return newTask{}, err
	}

	n := newTask{channelID: channelID, title: strings.TrimSpace(req.GetTitle()), description: req.GetDescription()}
	if n.title == "" || !validText(n.title) {
		return newTask{}, fmt.Errorf("tasks: the title must be text: %w", errs.ErrInvalid)
	}
	if !validText(n.description) {
		return newTask{}, fmt.Errorf("tasks: the description must be text: %w", errs.ErrInvalid)
	}
	if n.assignee, err = s.assignee(ctx, a, channelID, req.GetAssigneeId()); err != nil {
		return newTask{}, err
	}
	if n.dueAt, err = dueAt(req.GetDueAt()); err != nil {
		return newTask{}, err
	}
	switch {
	case req.GetSource() != nil:
		if n.source, err = parseSource(channelID, req.GetSource()); err != nil {
			return newTask{}, err
		}
	case draft:
		return newTask{}, fmt.Errorf("tasks: a draft needs the source it was proposed from: %w", errs.ErrInvalid)
	}
	if ref := req.GetFromRef(); ref != "" {
		if !fromRefPattern.MatchString(ref) {
			return newTask{}, fmt.Errorf("tasks: from_ref %q is not a REF like ANS-2209: %w", ref, errs.ErrInvalid)
		}
		n.fromRef = &ref
	}
	return n, nil
}

// assignee parses a request's assignee_id; an empty one is no assignee.
func (s *service) assignee(ctx context.Context, a authz.Actor, channelID uuid.UUID, raw string) (*uuid.UUID, error) {
	if raw == "" {
		return nil, nil
	}
	id, err := parseID(raw, "assignee id")
	if err != nil {
		return nil, err
	}
	if err := s.requireMember(ctx, a.WorkspaceID, channelID, id); err != nil {
		return nil, err
	}
	return &id, nil
}

func dueAt(ts *timestamppb.Timestamp) (pgtype.Timestamptz, error) {
	if ts == nil {
		return pgtype.Timestamptz{}, nil
	}
	if err := ts.CheckValid(); err != nil {
		return pgtype.Timestamptz{}, fmt.Errorf("tasks: due_at: %v: %w", err, errs.ErrInvalid)
	}
	return pgtype.Timestamptz{Time: ts.AsTime(), Valid: true}, nil
}

func parseSource(channelID uuid.UUID, s *chatv1.Source) (*taskSource, error) {
	var kind string
	switch s.GetKind() {
	case chatv1.SourceKind_SOURCE_KIND_MEETING:
		kind = sourceMeeting
	case chatv1.SourceKind_SOURCE_KIND_FILE:
		kind = sourceFile
	case chatv1.SourceKind_SOURCE_KIND_MESSAGE:
		kind = sourceMessage
	default:
		return nil, fmt.Errorf("tasks: source kind %s: %w", s.GetKind(), errs.ErrInvalid)
	}
	id, err := parseID(s.GetId(), "source id")
	if err != nil {
		return nil, err
	}
	if id == uuid.Nil {
		return nil, fmt.Errorf("tasks: the source id is empty: %w", errs.ErrInvalid)
	}
	if !validText(s.GetAnchor()) || !validText(s.GetLabel()) || !validText(s.GetQuote()) {
		return nil, fmt.Errorf("tasks: the source anchor, label and quote must be text: %w", errs.ErrInvalid)
	}
	if utf8.RuneCountInString(s.GetQuote()) > maxQuoteRunes {
		return nil, fmt.Errorf("tasks: the source quote has more than %d characters: %w", maxQuoteRunes, errs.ErrInvalid)
	}
	if raw := s.GetChannelId(); raw != "" {
		ch, err := parseID(raw, "source channel id")
		if err != nil {
			return nil, err
		}
		if ch != channelID {
			return nil, fmt.Errorf("tasks: the source is in channel %s, not the task's %s: %w", ch, channelID, errs.ErrInvalid)
		}
	}

	meta := sourceMeta{Label: s.GetLabel(), OffsetMs: s.OffsetMs, Page: s.Page, Quote: s.Quote}
	if s.At != nil {
		if kind != sourceMessage {
			return nil, fmt.Errorf("tasks: at belongs to a message source, not a %s: %w", kind, errs.ErrInvalid)
		}
		if err := s.At.CheckValid(); err != nil {
			return nil, fmt.Errorf("tasks: source at: %v: %w", err, errs.ErrInvalid)
		}
		at := s.At.AsTime()
		meta.At = &at
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("tasks: encode the source snapshot: %w", err)
	}
	return &taskSource{kind: kind, id: id, anchor: s.GetAnchor(), meta: raw}, nil
}

// creator is who the task is recorded as made by: a person, or for an agent and
// for a background job the agent, which a job without one calls jobCreator.
func creator(a authz.Actor) (kind string, id uuid.UUID) {
	if a.Kind == authz.ActorUser {
		return creatorUser, a.UserID
	}
	name := a.Agent
	if name == "" {
		name = jobCreator
	}
	return creatorAgent, authz.AgentID(name)
}

// insert stores n as a new task at the end of the To do lane of its channel and
// announces it, all in tx. The lane is locked first so that concurrent creations
// get distinct positions, and the REF is allocated last so that its counter row is
// held for as short a time as possible. The lock order is lane, then counter: see
// CreateDraftTx for what that means for a caller that drafts into several channels.
func insert(ctx context.Context, tx pgx.Tx, a authz.Actor, state string, n newTask) (*tasksv1.Task, error) {
	q := store.New(tx)
	if err := q.LockLane(ctx, laneKey(n.channelID, statusTodo)); err != nil {
		return nil, fmt.Errorf("tasks: lock lane: %w", err)
	}
	last, err := lastPosition(ctx, q, n.channelID, statusTodo, uuid.Nil)
	if err != nil {
		return nil, err
	}
	position, err := positionBetween(last, "")
	if err != nil {
		return nil, fmt.Errorf("tasks: position of a new task: %w", err)
	}
	ref, err := refs.Next(ctx, tx, "TSK")
	if err != nil {
		return nil, fmt.Errorf("tasks: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("tasks: new task id: %w", err)
	}
	kind, by := creator(a)

	p := store.InsertTaskParams{
		ID: id, WorkspaceID: a.WorkspaceID, ChannelID: n.channelID, Ref: &ref,
		Title: n.title, Description: n.description, State: state, Status: statusTodo,
		AssigneeID: n.assignee, DueAt: n.dueAt, Position: position, FromRef: n.fromRef,
		CreatedByKind: kind, CreatedByID: by,
	}
	if n.source != nil {
		p.SourceKind, p.SourceID, p.SourceAnchor, p.SourceMeta = &n.source.kind, &n.source.id, &n.source.anchor, n.source.meta
	}
	row, err := q.InsertTask(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("tasks: insert task: %w", err)
	}
	return changed(ctx, tx, a.WorkspaceID, row)
}

// ---- update, move, confirm, discard -----------------------------------------

// UpdateTask implements Service. Only the fields the request sets change; a title,
// when set, is trimmed and must not be empty, an assignee must be a member of the
// task's channel, and setting and clearing the same field is errs.ErrInvalid.
func (s *service) UpdateTask(ctx context.Context, a authz.Actor, req *tasksv1.UpdateTaskRequest) (*tasksv1.Task, error) {
	id, err := parseID(req.GetId(), "task id")
	if err != nil {
		return nil, err
	}
	cur, err := s.authorize(ctx, a, id, authz.ActTaskWrite)
	if err != nil {
		return nil, err
	}

	p := store.UpdateTaskParams{ID: id, ClearAssignee: req.GetClearAssignee(), ClearDue: req.GetClearDue()}
	if req.Title != nil {
		title := strings.TrimSpace(req.GetTitle())
		if title == "" || !validText(title) {
			return nil, fmt.Errorf("tasks: the title must be text: %w", errs.ErrInvalid)
		}
		p.Title = &title
	}
	if req.Description != nil {
		if !validText(req.GetDescription()) {
			return nil, fmt.Errorf("tasks: the description must be text: %w", errs.ErrInvalid)
		}
		p.Description = req.Description
	}
	if req.GetClearAssignee() && req.GetAssigneeId() != "" || req.GetClearDue() && req.DueAt != nil {
		return nil, fmt.Errorf("tasks: a request cannot set and clear the same field: %w", errs.ErrInvalid)
	}
	if p.AssigneeID, err = s.assignee(ctx, a, cur.ChannelID, req.GetAssigneeId()); err != nil {
		return nil, err
	}
	if p.DueAt, err = dueAt(req.GetDueAt()); err != nil {
		return nil, err
	}

	return s.write(ctx, a, func(ctx context.Context, q *store.Queries) (store.TasksTask, error) {
		return q.UpdateTask(ctx, p)
	})
}

// MoveTask implements Service. The card goes after the task after_id and before the
// task before_id, both of which must be in the destination status of the card's
// channel; an empty one is an open end, and two empty ones append the card to the
// lane. The ids are where the client believes the card goes, and the lane may have
// changed since: the card is placed between the two cards that really are adjacent,
// drafts included. A single named neighbour takes the lane's real card on the other
// side as the other bound, and two named neighbours with a card between them are
// errs.ErrConflict. Only the moved row is written.
func (s *service) MoveTask(ctx context.Context, a authz.Actor, req *tasksv1.MoveTaskRequest) (*tasksv1.Task, error) {
	id, err := parseID(req.GetId(), "task id")
	if err != nil {
		return nil, err
	}
	status, err := statusText(req.GetStatus())
	if err != nil {
		return nil, err
	}
	before, err := optionalID(req.GetBeforeId(), "before_id")
	if err != nil {
		return nil, err
	}
	after, err := optionalID(req.GetAfterId(), "after_id")
	if err != nil {
		return nil, err
	}
	if before == id || after == id {
		return nil, fmt.Errorf("tasks: a card cannot be placed next to itself: %w", errs.ErrInvalid)
	}
	cur, err := s.authorize(ctx, a, id, authz.ActTaskWrite)
	if err != nil {
		return nil, err
	}

	return s.write(ctx, a, func(ctx context.Context, q *store.Queries) (store.TasksTask, error) {
		if err := q.LockLane(ctx, laneKey(cur.ChannelID, status)); err != nil {
			return store.TasksTask{}, fmt.Errorf("tasks: lock lane: %w", err)
		}
		position, err := placement(ctx, q, cur.ChannelID, status, id, after, before)
		if err != nil {
			return store.TasksTask{}, fmt.Errorf("tasks: move task %s: %w", id, err)
		}
		return q.MoveTask(ctx, store.MoveTaskParams{ID: id, Status: status, Position: position})
	})
}

// ConfirmDraft implements Service. A draft without an assignee is errs.ErrInvalid
// and a task that is not a draft, including one a concurrent confirm just sealed,
// is errs.ErrConflict. The sealed card lands at the top of To do, in front of every
// card of the lane, drafts included, and the task.changed event carries that
// position.
func (s *service) ConfirmDraft(ctx context.Context, a authz.Actor, id uuid.UUID) (*tasksv1.Task, error) {
	cur, err := s.authorize(ctx, a, id, authz.ActTaskConfirm)
	if err != nil {
		return nil, err
	}
	var task *tasksv1.Task
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.LockLane(ctx, laneKey(cur.ChannelID, statusTodo)); err != nil {
			return fmt.Errorf("tasks: lock lane: %w", err)
		}
		first, err := orOpen(q.FirstPosition(ctx, store.FirstPositionParams{ChannelID: cur.ChannelID, Status: statusTodo, ExcludingID: id}))
		if err != nil {
			return err
		}
		position, err := positionBetween("", first)
		if err != nil {
			return fmt.Errorf("tasks: position of a confirmed task: %w", err)
		}
		row, err := q.ConfirmDraft(ctx, store.ConfirmDraftParams{ID: id, Position: position})
		if errors.Is(err, pgx.ErrNoRows) {
			return explainConfirm(ctx, q, id)
		}
		if err != nil {
			return fmt.Errorf("tasks: confirm draft: %w", err)
		}
		task, err = changed(ctx, tx, a.WorkspaceID, row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return task, nil
}

// explainConfirm says why ConfirmDraft changed no row.
func explainConfirm(ctx context.Context, q *store.Queries, id uuid.UUID) error {
	row, err := q.GetTask(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("tasks: task %s: %w", id, errs.ErrNotFound)
	case err != nil:
		return fmt.Errorf("tasks: read task: %w", err)
	case row.State != stateDraft:
		return fmt.Errorf("tasks: task %s is already confirmed: %w", id, errs.ErrConflict)
	}
	return fmt.Errorf("tasks: choose an owner first: %w", errs.ErrInvalid)
}

// DiscardDraft implements Service. A task that is not a draft is errs.ErrConflict.
// No task.changed event follows: the event carries a task, and a discarded draft
// no longer exists.
func (s *service) DiscardDraft(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	if _, err := s.authorize(ctx, a, id, authz.ActTaskWrite); err != nil {
		return err
	}
	return s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.DeleteDraft(ctx, id)
		if err != nil {
			return fmt.Errorf("tasks: discard draft: %w", err)
		}
		if n > 0 {
			return nil
		}
		if _, err := q.GetTask(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("tasks: task %s: %w", id, errs.ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("tasks: read task: %w", err)
		}
		return fmt.Errorf("tasks: task %s is confirmed, only a draft can be discarded: %w", id, errs.ErrConflict)
	})
}

// write runs change in a transaction of the actor's workspace and announces the
// task it returns. A task that disappeared since authorize read it is
// errs.ErrNotFound.
func (s *service) write(ctx context.Context, a authz.Actor, change func(context.Context, *store.Queries) (store.TasksTask, error)) (*tasksv1.Task, error) {
	var task *tasksv1.Task
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		row, err := change(ctx, store.New(tx))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("tasks: the task is gone: %w", errs.ErrNotFound)
		}
		if err != nil {
			return err
		}
		task, err = changed(ctx, tx, a.WorkspaceID, row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return task, nil
}

// changed announces the task row as task.changed in tx and returns it as a message.
func changed(ctx context.Context, tx pgx.Tx, ws uuid.UUID, row store.TasksTask) (*tasksv1.Task, error) {
	task, err := taskProto(row)
	if err != nil {
		return nil, err
	}
	payload, err := protoJSON.Marshal(&eventsv1.TaskChanged{WorkspaceId: ws.String(), ChannelId: task.GetChannelId(), Task: task})
	if err != nil {
		return nil, fmt.Errorf("tasks: encode %s event: %w", events.TopicTaskChanged, err)
	}
	if err := outbox.Enqueue(ctx, tx, outbox.Event{Topic: events.TopicTaskChanged, Key: task.GetChannelId(), Payload: payload}); err != nil {
		return nil, fmt.Errorf("tasks: %w", err)
	}
	return task, nil
}

// ---- lanes ------------------------------------------------------------------

// laneKey identifies a lane: the cards of one status of one channel.
func laneKey(channelID uuid.UUID, status string) string { return channelID.String() + "/" + status }

// lastPosition is the position of the last card of the lane other than excluding;
// empty for an empty lane.
func lastPosition(ctx context.Context, q *store.Queries, channelID uuid.UUID, status string, excluding uuid.UUID) (string, error) {
	return orOpen(q.LastPosition(ctx, store.LastPositionParams{ChannelID: channelID, Status: status, ExcludingID: excluding}))
}

// orOpen turns the result of a lane position query into a position, the empty open
// end when the lane has no such card.
func orOpen(pos string, err error) (string, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("tasks: read lane: %w", err)
	}
	return pos, nil
}

// placement is the position of a card moving into the lane (channel, status) after
// the card after and before the card before, either of which is uuid.Nil for an
// open end. The caller holds the lane lock.
//
// The named cards say where the client wants the card; the lane is what decides.
// Every card of the lane counts, the drafts the board hides included, and the moving
// card does not: it is leaving its spot. One named card takes the lane's real
// neighbour on its other side as the other bound, none takes the end of the lane,
// and two with a card between them mean the client's view is stale (errs.ErrConflict).
// So the key lies strictly between two cards that are adjacent right now, and no
// card of the lane ever holds the same position as another.
func placement(ctx context.Context, q *store.Queries, channelID uuid.UUID, status string, moving, after, before uuid.UUID) (string, error) {
	lo, err := neighbour(ctx, q, channelID, status, after)
	if err != nil {
		return "", err
	}
	hi, err := neighbour(ctx, q, channelID, status, before)
	if err != nil {
		return "", err
	}

	switch {
	case after != uuid.Nil && before != uuid.Nil:
		if lo >= hi {
			return "", fmt.Errorf("tasks: after_id sorts behind before_id: %w", errs.ErrInvalid)
		}
		next, err := orOpen(q.NextPosition(ctx, store.NextPositionParams{ChannelID: channelID, Status: status, ExcludingID: moving, AfterPosition: lo}))
		if err != nil {
			return "", err
		}
		if next != hi {
			return "", fmt.Errorf("tasks: another card lies between after_id and before_id now: %w", errs.ErrConflict)
		}
	case after != uuid.Nil:
		if hi, err = orOpen(q.NextPosition(ctx, store.NextPositionParams{ChannelID: channelID, Status: status, ExcludingID: moving, AfterPosition: lo})); err != nil {
			return "", err
		}
	case before != uuid.Nil:
		if lo, err = orOpen(q.PrevPosition(ctx, store.PrevPositionParams{ChannelID: channelID, Status: status, ExcludingID: moving, BeforePosition: hi})); err != nil {
			return "", err
		}
	default:
		if lo, err = lastPosition(ctx, q, channelID, status, moving); err != nil {
			return "", err
		}
	}

	position, err := positionBetween(lo, hi)
	if err != nil {
		return "", err
	}
	taken, err := q.PositionTaken(ctx, store.PositionTakenParams{ChannelID: channelID, Status: status, ExcludingID: moving, Position: position})
	if err != nil {
		return "", fmt.Errorf("tasks: read lane: %w", err)
	}
	if taken {
		return "", fmt.Errorf("tasks: position %q is already held by a card of the lane: %w", position, errs.ErrConflict)
	}
	return position, nil
}

// neighbour is the position of the card id, which must be in the lane; empty for
// uuid.Nil, an open end.
func neighbour(ctx context.Context, q *store.Queries, channelID uuid.UUID, status string, id uuid.UUID) (string, error) {
	if id == uuid.Nil {
		return "", nil
	}
	row, err := q.GetTask(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("tasks: neighbour %s does not exist: %w", id, errs.ErrInvalid)
	}
	if err != nil {
		return "", fmt.Errorf("tasks: read neighbour: %w", err)
	}
	if row.ChannelID != channelID || row.Status != status {
		return "", fmt.Errorf("tasks: neighbour %s is not a %s card of this channel: %w", id, status, errs.ErrInvalid)
	}
	return row.Position, nil
}

// ---- conversions ------------------------------------------------------------

func statusText(s tasksv1.TaskStatus) (string, error) {
	switch s {
	case tasksv1.TaskStatus_TASK_STATUS_TODO:
		return statusTodo, nil
	case tasksv1.TaskStatus_TASK_STATUS_DOING:
		return statusDoing, nil
	case tasksv1.TaskStatus_TASK_STATUS_DONE:
		return statusDone, nil
	}
	return "", fmt.Errorf("tasks: status must be todo, doing or done, got %s: %w", s, errs.ErrInvalid)
}

// stateTexts turns a ListTasks filter into the values of the state column. The
// result is never nil: a nil slice would reach Postgres as NULL.
func stateTexts(states []tasksv1.TaskState) ([]string, error) {
	out := make([]string, 0, len(states))
	for _, st := range states {
		switch st {
		case tasksv1.TaskState_TASK_STATE_DRAFT:
			out = append(out, stateDraft)
		case tasksv1.TaskState_TASK_STATE_CONFIRMED:
			out = append(out, stateConfirmed)
		default:
			return nil, fmt.Errorf("tasks: state filter must be draft or confirmed, got %s: %w", st, errs.ErrInvalid)
		}
	}
	return out, nil
}

func taskProto(r store.TasksTask) (*tasksv1.Task, error) {
	t := &tasksv1.Task{
		Id: r.ID.String(), ChannelId: r.ChannelID.String(), Title: r.Title, Description: r.Description,
		State: tasksv1.TaskState_TASK_STATE_CONFIRMED, Status: tasksv1.TaskStatus_TASK_STATUS_TODO,
		CreatedAt: timestamppb.New(r.CreatedAt.Time), Position: r.Position, FromRef: r.FromRef,
		CreatedByKind: tasksv1.CreatedByKind_CREATED_BY_KIND_USER, CreatedById: r.CreatedByID.String(),
	}
	if r.Ref != nil {
		t.Ref = *r.Ref
	}
	if r.State == stateDraft {
		t.State = tasksv1.TaskState_TASK_STATE_DRAFT
	}
	switch r.Status {
	case statusDoing:
		t.Status = tasksv1.TaskStatus_TASK_STATUS_DOING
	case statusDone:
		t.Status = tasksv1.TaskStatus_TASK_STATUS_DONE
	}
	if r.CreatedByKind == creatorAgent {
		t.CreatedByKind = tasksv1.CreatedByKind_CREATED_BY_KIND_AGENT
	}
	if r.AssigneeID != nil {
		t.AssigneeId = proto.String(r.AssigneeID.String())
	}
	if r.DueAt.Valid {
		t.DueAt = timestamppb.New(r.DueAt.Time)
	}
	if r.DoneAt.Valid {
		t.DoneAt = timestamppb.New(r.DoneAt.Time)
	}
	if r.SourceKind != nil {
		src, err := sourceProto(r)
		if err != nil {
			return nil, err
		}
		t.Source = src
	}
	return t, nil
}

// sourceProto rebuilds Task.source from the typed reference and the snapshot; its
// channel is the task's.
func sourceProto(r store.TasksTask) (*chatv1.Source, error) {
	var meta sourceMeta
	if len(r.SourceMeta) > 0 {
		if err := json.Unmarshal(r.SourceMeta, &meta); err != nil {
			return nil, fmt.Errorf("tasks: task %s source snapshot: %w", r.ID, err)
		}
	}
	src := &chatv1.Source{
		Kind: chatv1.SourceKind_SOURCE_KIND_UNSPECIFIED, Label: meta.Label, ChannelId: r.ChannelID.String(),
		OffsetMs: meta.OffsetMs, Page: meta.Page, Quote: meta.Quote,
	}
	switch *r.SourceKind {
	case sourceMeeting:
		src.Kind = chatv1.SourceKind_SOURCE_KIND_MEETING
	case sourceFile:
		src.Kind = chatv1.SourceKind_SOURCE_KIND_FILE
	case sourceMessage:
		src.Kind = chatv1.SourceKind_SOURCE_KIND_MESSAGE
	}
	if r.SourceID != nil {
		src.Id = r.SourceID.String()
	}
	if r.SourceAnchor != nil {
		src.Anchor = *r.SourceAnchor
	}
	if meta.At != nil {
		src.At = timestamppb.New(*meta.At)
	}
	return src, nil
}

// ---- helpers ----------------------------------------------------------------

func parseID(raw, what string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("tasks: %s %q is not a UUID: %w", what, raw, errs.ErrInvalid)
	}
	return id, nil
}

// optionalID is parseID for a field that may be empty, which is uuid.Nil.
func optionalID(raw, what string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.Nil, nil
	}
	return parseID(raw, what)
}

// validText reports whether s can be stored in a Postgres text column: valid UTF-8
// without NUL.
func validText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
