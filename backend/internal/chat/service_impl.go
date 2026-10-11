package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	"kiwi/internal/chat/internal/store"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/outbox"
)

// Limits. The body limit is spec decision D10; the others keep a request from
// asking for more than a screen of chat can show.
const (
	// maxBody is the longest message body, in runes after trimming.
	maxBody = 8000
	// maxClientMsgID is the longest client_msg_id, in bytes: it is a key of a unique index.
	maxClientMsgID = 128
	// maxChannelName is the longest channel name, in runes.
	maxChannelName = 80
	// defaultPage is the page ListMessages returns when asked for none, and the
	// page a resync reload carries.
	defaultPage = 50
	// maxPage is the largest page ListMessages returns.
	maxPage = 200
	// maxCatchUp is how far behind a cursor may be and still catch up by delta;
	// a cursor further behind reloads (spec decision D10).
	maxCatchUp = 200
	// invalidateTimeout bounds the cache write that follows a committed membership
	// change; it runs on a context that outlives the request, so a client that gave
	// up cannot leave a removed member in the cache.
	invalidateTimeout = 5 * time.Second
)

// Values of the text columns.
const (
	authorUser  = "user"
	authorAgent = "agent"

	channelPublic  = "public"
	channelPrivate = "private"

	roleMember = "member"
	roleOwner  = "owner"
)

// Postgres error codes the service turns into domain errors.
const (
	pgForeignKeyViolation = "23503"
	pgUniqueViolation     = "23505"
)

// protoJSON is how messages are stored in jsonb columns and carried in events:
// field names as in the .proto, which is what outbox.Enqueue reads workspace_id by.
var protoJSON = protojson.MarshalOptions{UseProtoNames: true}

// Deps are what New builds the chat domain from.
type Deps struct {
	DB *db.DB
	// Authz decides every channel-scoped action; its Membership is normally
	// NewMembership over the same DB and Cache.
	Authz *authz.Authz
	// Cache holds members:{channel}.
	Cache *cache.Cache
	// KnownAgent reports whether name is a registered agent; an @mention of one
	// that is not stays plain text. Nil means no agent is known.
	KnownAgent func(name string) bool
}

type service struct {
	db         *db.DB
	authz      *authz.Authz
	cache      *cache.Cache
	knownAgent func(name string) bool
	members    authz.Membership
}

// New returns the chat domain.
func New(d Deps) Service {
	return &service{db: d.DB, authz: d.Authz, cache: d.Cache, knownAgent: d.KnownAgent, members: NewMembership(d.DB, d.Cache)}
}

var _ Service = (*service)(nil)

// can asks authz whether a may perform act on the channel.
func (s *service) can(ctx context.Context, a authz.Actor, act authz.Action, channelID uuid.UUID) error {
	return s.authz.Can(ctx, a, act, authz.Resource{ChannelID: channelID})
}

// requireActor refuses a principal the methods that need no authz action cannot
// act for: authz.Can denies by default and so does this.
func requireActor(a authz.Actor) error {
	switch a.Kind {
	case authz.ActorUser, authz.ActorAgent, authz.ActorSystem:
	default:
		return fmt.Errorf("chat: unknown actor kind %q: %w", a.Kind, errs.ErrForbidden)
	}
	if a.WorkspaceID == uuid.Nil || a.EffectiveUserID() == uuid.Nil {
		return fmt.Errorf("chat: actor has no workspace or user: %w", errs.ErrForbidden)
	}
	return nil
}

// ---- channels ---------------------------------------------------------------

// CreateChannel implements Service. Only a person creates channels: the name is
// trimmed and must be 1-80 runes, and the kind must be public or private.
func (s *service) CreateChannel(ctx context.Context, a authz.Actor, req *chatv1.CreateChannelRequest) (*chatv1.Channel, error) {
	if err := requireActor(a); err != nil {
		return nil, err
	}
	if a.Kind != authz.ActorUser {
		return nil, fmt.Errorf("chat: a %s actor may not create a channel: %w", a.Kind, errs.ErrForbidden)
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" || !validText(name) || utf8.RuneCountInString(name) > maxChannelName {
		return nil, fmt.Errorf("chat: channel name must be 1-%d characters: %w", maxChannelName, errs.ErrInvalid)
	}
	kind, err := channelKind(req.GetKind())
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("chat: new channel id: %w", err)
	}

	var row store.ChatChannel
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		if row, err = store.New(tx).InsertChannel(ctx, store.InsertChannelParams{ID: id, WorkspaceID: a.WorkspaceID, Name: name, Kind: kind}); err != nil {
			return fmt.Errorf("chat: create channel: %w", err)
		}
		return addMemberTx(ctx, tx, a.WorkspaceID, id, a.UserID, roleOwner)
	})
	if err != nil {
		return nil, err
	}
	return channelProto(row), nil
}

// ListChannels implements Service.
func (s *service) ListChannels(ctx context.Context, a authz.Actor) ([]*chatv1.Channel, error) {
	if err := requireActor(a); err != nil {
		return nil, err
	}
	var rows []store.ChatChannel
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		rows, err = store.New(tx).ListVisibleChannels(ctx, a.EffectiveUserID())
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("chat: list channels: %w", err)
	}
	out := make([]*chatv1.Channel, len(rows))
	for i, r := range rows {
		out[i] = channelProto(r)
	}
	return out, nil
}

// JoinChannel implements Service. Joining a channel one is already in changes
// nothing.
func (s *service) JoinChannel(ctx context.Context, a authz.Actor, channelID uuid.UUID) error {
	if err := requireActor(a); err != nil {
		return err
	}
	var ch store.ChatChannel
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		ch, err = store.New(tx).GetChannel(ctx, channelID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("chat: join channel %s: %w", channelID, errs.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("chat: join channel: %w", err)
	}
	if err := s.authz.Can(ctx, a, authz.ActChannelJoin, authz.Resource{ChannelID: channelID, ChannelPublic: ch.Kind == channelPublic}); err != nil {
		return err
	}
	return s.addMember(ctx, a.WorkspaceID, channelID, a.UserID)
}

// AddMember implements Service. Adding a member the channel already has changes
// nothing; a user the workspace does not have is errs.ErrNotFound.
func (s *service) AddMember(ctx context.Context, a authz.Actor, channelID, userID uuid.UUID) error {
	if err := s.can(ctx, a, authz.ActChannelManage, channelID); err != nil {
		return err
	}
	return s.addMember(ctx, a.WorkspaceID, channelID, userID)
}

// addMember commits the membership and then drops the cached member set. The
// invalidation runs even when the row already existed, so a repeated call heals a
// first one whose invalidation failed.
func (s *service) addMember(ctx context.Context, ws, channelID, userID uuid.UUID) error {
	err := s.db.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		return addMemberTx(ctx, tx, ws, channelID, userID, roleMember)
	})
	if err != nil {
		return err
	}
	return s.invalidateMembers(ctx, channelID)
}

// RemoveMember implements Service. The cached member set is dropped after the
// delete has committed (a reader that began before the commit is refused its cache
// fill, see membership), and membership.changed{REMOVED} tells the realtime
// gateway, which then stops the user's connections from following the channel.
// Removing someone who is not a member changes nothing, but still invalidates.
func (s *service) RemoveMember(ctx context.Context, a authz.Actor, channelID, userID uuid.UUID) error {
	if err := s.can(ctx, a, authz.ActChannelManage, channelID); err != nil {
		return err
	}
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		n, err := store.New(tx).DeleteMember(ctx, store.DeleteMemberParams{ChannelID: channelID, UserID: userID})
		if err != nil {
			return fmt.Errorf("chat: remove member: %w", err)
		}
		if n == 0 {
			return nil
		}
		return enqueueMembership(ctx, tx, a.WorkspaceID, channelID, userID, chatv1.MembershipChange_MEMBERSHIP_CHANGE_REMOVED)
	})
	if err != nil {
		return err
	}
	return s.invalidateMembers(ctx, channelID)
}

// ListMembers implements Service.
func (s *service) ListMembers(ctx context.Context, a authz.Actor, channelID uuid.UUID) ([]*chatv1.Member, error) {
	if err := s.can(ctx, a, authz.ActChannelRead, channelID); err != nil {
		return nil, err
	}
	var rows []store.ListMembersRow
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		rows, err = store.New(tx).ListMembers(ctx, channelID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("chat: list members: %w", err)
	}
	out := make([]*chatv1.Member, len(rows))
	for i, r := range rows {
		role := chatv1.MemberRole_MEMBER_ROLE_MEMBER
		if r.Role == roleOwner {
			role = chatv1.MemberRole_MEMBER_ROLE_OWNER
		}
		out[i] = &chatv1.Member{UserId: r.UserID.String(), Name: r.Name, AvatarUrl: r.AvatarUrl, Role: role}
	}
	return out, nil
}

// IsMember implements Service through the cache-aside membership lookup. ctx must
// carry the workspace (authz.WithWorkspace).
func (s *service) IsMember(ctx context.Context, channelID, userID uuid.UUID) (bool, error) {
	return s.members.IsMember(ctx, channelID, userID)
}

// MemberChannelIDs implements Service.
func (s *service) MemberChannelIDs(ctx context.Context, workspaceID, userID uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.db.InWorkspace(ctx, workspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		ids, err = store.New(tx).ListMemberChannelIDs(ctx, store.ListMemberChannelIDsParams{WorkspaceID: workspaceID, UserID: userID})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("chat: list member channels: %w", err)
	}
	return ids, nil
}

// invalidateMembers drops members:{channel} after a membership change has
// committed.
func (s *service) invalidateMembers(ctx context.Context, channelID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), invalidateTimeout)
	defer cancel()
	if err := s.cache.InvalidateMembers(ctx, channelID); err != nil {
		return fmt.Errorf("chat: membership changed but the members cache was not invalidated, repeat the call: %w", err)
	}
	return nil
}

// addMemberTx inserts the membership in tx and announces it when it is new. A user
// the workspace does not have is errs.ErrNotFound.
func addMemberTx(ctx context.Context, tx pgx.Tx, ws, channelID, userID uuid.UUID, role string) error {
	n, err := store.New(tx).InsertMember(ctx, store.InsertMemberParams{ChannelID: channelID, UserID: userID, WorkspaceID: ws, Role: role})
	if pgCode(err) == pgForeignKeyViolation {
		return fmt.Errorf("chat: add member: user %s is not in the workspace: %w", userID, errs.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("chat: add member: %w", err)
	}
	if n == 0 {
		return nil
	}
	return enqueueMembership(ctx, tx, ws, channelID, userID, chatv1.MembershipChange_MEMBERSHIP_CHANGE_ADDED)
}

func enqueueMembership(ctx context.Context, tx pgx.Tx, ws, channelID, userID uuid.UUID, change chatv1.MembershipChange) error {
	return enqueue(ctx, tx, events.TopicMembershipChanged, channelID, &eventsv1.MembershipChanged{
		WorkspaceId: ws.String(), ChannelId: channelID.String(), UserId: userID.String(), Change: change,
	})
}

// enqueue writes ev to the outbox in tx, keyed by the channel it concerns.
func enqueue(ctx context.Context, tx pgx.Tx, topic string, channelID uuid.UUID, ev proto.Message) error {
	payload, err := protoJSON.Marshal(ev)
	if err != nil {
		return fmt.Errorf("chat: encode %s event: %w", topic, err)
	}
	if err := outbox.Enqueue(ctx, tx, outbox.Event{Topic: topic, Key: channelID.String(), Payload: payload}); err != nil {
		return fmt.Errorf("chat: %w", err)
	}
	return nil
}

// ---- messages ---------------------------------------------------------------

// message is a SendMessageRequest after the rules have been applied to it: who
// the author is, and what a user actor may not set cleared.
type message struct {
	channelID   uuid.UUID
	clientMsgID string
	body        string
	mentions    []*chatv1.Mention
	attachments []*chatv1.FileRef
	artifact    *chatv1.Artifact
	authorKind  string
	authorID    uuid.UUID
}

// newMessage authorizes the post and turns the request into a message:
//   - the body is trimmed and must be 1-8000 runes of valid text;
//   - client_msg_id must be 1-128 bytes, it is the idempotency key;
//   - a user posts as themself, and attachments, artifact, author_kind and author_id
//     of the request are ignored, so nobody forges an AI slip, an attachment or
//     another author;
//   - an agent posts as an agent, with the author_id of the request (the agents
//     runtime fills authz.AgentID(name));
//   - a system actor posts as the author_kind and author_id of the request.
func (s *service) newMessage(ctx context.Context, a authz.Actor, req *chatv1.SendMessageRequest) (message, error) {
	channelID, err := parseID(req.GetChannelId(), "channel id")
	if err != nil {
		return message{}, err
	}
	if err := s.can(ctx, a, authz.ActMessagePost, channelID); err != nil {
		return message{}, err
	}

	m := message{channelID: channelID, clientMsgID: req.GetClientMsgId(), body: strings.TrimSpace(req.GetBody()), mentions: req.GetMentions()}
	if m.body == "" || !validText(m.body) || utf8.RuneCountInString(m.body) > maxBody {
		return message{}, fmt.Errorf("chat: message body must be 1-%d characters: %w", maxBody, errs.ErrInvalid)
	}
	if m.clientMsgID == "" || len(m.clientMsgID) > maxClientMsgID || !validText(m.clientMsgID) {
		return message{}, fmt.Errorf("chat: client_msg_id must be 1-%d bytes: %w", maxClientMsgID, errs.ErrInvalid)
	}
	if len(m.mentions) > maxMentions {
		return message{}, fmt.Errorf("chat: a message carries at most %d mentions: %w", maxMentions, errs.ErrInvalid)
	}

	switch a.Kind {
	case authz.ActorUser:
		m.authorKind, m.authorID = authorUser, a.UserID
	case authz.ActorAgent, authz.ActorSystem:
		m.attachments, m.artifact = req.GetAttachments(), req.GetArtifact()
		if m.authorID, err = uuid.Parse(req.GetAuthorId()); err != nil || m.authorID == uuid.Nil {
			return message{}, fmt.Errorf("chat: a %s post needs the author_id of its author: %w", a.Kind, errs.ErrInvalid)
		}
		switch {
		case a.Kind == authz.ActorAgent:
			m.authorKind = authorAgent
		case req.GetAuthorKind() == chatv1.AuthorKind_AUTHOR_KIND_USER:
			m.authorKind = authorUser
		case req.GetAuthorKind() == chatv1.AuthorKind_AUTHOR_KIND_AGENT:
			m.authorKind = authorAgent
		default:
			return message{}, fmt.Errorf("chat: a system post needs the author_kind of its author: %w", errs.ErrInvalid)
		}
	}
	return m, nil
}

// PostMessage implements Service.
func (s *service) PostMessage(ctx context.Context, a authz.Actor, req *chatv1.SendMessageRequest) (*chatv1.Message, error) {
	m, err := s.newMessage(ctx, a, req)
	if err != nil {
		return nil, err
	}
	var msg *chatv1.Message
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		msg, err = s.post(ctx, tx, a.WorkspaceID, m)
		return err
	})
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// PostMessageTx implements Service. tx must be bound to the actor's workspace
// (db.InWorkspace). The authorization lookup runs on its own pooled connection,
// not on tx.
func (s *service) PostMessageTx(ctx context.Context, tx pgx.Tx, a authz.Actor, req *chatv1.SendMessageRequest) (*chatv1.Message, error) {
	m, err := s.newMessage(ctx, a, req)
	if err != nil {
		return nil, err
	}
	return s.post(ctx, tx, a.WorkspaceID, m)
}

// post stores the message in tx and enqueues its event there.
//
// The channel row is locked first. That serialises every writer of the channel, so
// a retry racing its original finds the original after the lock instead of failing
// on UNIQUE(author_id, client_msg_id) halfway through, and a seq is only taken for
// a message that is really inserted: seq stays gapless, and rolls back with tx.
func (s *service) post(ctx context.Context, tx pgx.Tx, ws uuid.UUID, m message) (*chatv1.Message, error) {
	q := store.New(tx)
	if _, err := q.LockChannel(ctx, m.channelID); errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("chat: post to channel %s: %w", m.channelID, errs.ErrNotFound)
	} else if err != nil {
		return nil, fmt.Errorf("chat: lock channel: %w", err)
	}

	original, err := q.GetMessageByClientID(ctx, store.GetMessageByClientIDParams{AuthorID: m.authorID, ClientMsgID: m.clientMsgID})
	switch {
	case err == nil:
		if original.ChannelID != m.channelID {
			return nil, fmt.Errorf("chat: client_msg_id %q is already used in another channel: %w", m.clientMsgID, errs.ErrConflict)
		}
		return messageProto(original)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("chat: look up client_msg_id: %w", err)
	}

	mentions, err := s.resolveMentions(ctx, q, m.channelID, m.mentions)
	if err != nil {
		return nil, err
	}
	mentionsJSON, err := encodeList(mentions)
	if err != nil {
		return nil, err
	}
	attachmentsJSON, err := encodeList(m.attachments)
	if err != nil {
		return nil, err
	}
	var artifactJSON json.RawMessage
	if m.artifact != nil {
		if artifactJSON, err = protoJSON.Marshal(m.artifact); err != nil {
			return nil, fmt.Errorf("chat: encode artifact: %w", err)
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("chat: new message id: %w", err)
	}

	seq, err := q.BumpChannelSeq(ctx, m.channelID)
	if err != nil {
		return nil, fmt.Errorf("chat: assign seq: %w", err)
	}
	row, err := q.InsertMessage(ctx, store.InsertMessageParams{
		ID: id, WorkspaceID: ws, ChannelID: m.channelID, Seq: seq,
		AuthorKind: m.authorKind, AuthorID: m.authorID, Body: m.body,
		Mentions: mentionsJSON, Attachments: attachmentsJSON, Artifact: artifactJSON,
		ClientMsgID: m.clientMsgID,
	})
	if pgCode(err) == pgUniqueViolation {
		// Only another channel's message by the same author with the same client_msg_id
		// gets here: the lock above ruled out a race inside this channel.
		return nil, fmt.Errorf("chat: client_msg_id %q is already used in another channel: %w", m.clientMsgID, errs.ErrConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("chat: insert message: %w", err)
	}
	msg, err := messageProto(row)
	if err != nil {
		return nil, err
	}
	if err := enqueue(ctx, tx, events.TopicChatMessageCreated, m.channelID, &eventsv1.ChatMessageCreated{
		WorkspaceId: ws.String(), ChannelId: m.channelID.String(), Message: msg,
	}); err != nil {
		return nil, err
	}
	return msg, nil
}

// ListMessages implements Service. A limit of zero or less is a default page and
// one above 200 is 200.
func (s *service) ListMessages(ctx context.Context, a authz.Actor, channelID uuid.UUID, beforeSeq int64, limit int) ([]*chatv1.Message, error) {
	if err := s.can(ctx, a, authz.ActChannelRead, channelID); err != nil {
		return nil, err
	}
	if beforeSeq < 0 {
		return nil, fmt.Errorf("chat: before_seq must not be negative: %w", errs.ErrInvalid)
	}
	if limit <= 0 {
		limit = defaultPage
	}
	limit = min(limit, maxPage)

	var rows []store.ChatMessage
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		q := store.New(tx)
		if beforeSeq == 0 {
			rows, err = q.ListLatestMessages(ctx, store.ListLatestMessagesParams{ChannelID: channelID, Limit: int32(limit)})
		} else {
			rows, err = q.ListMessagesBefore(ctx, store.ListMessagesBeforeParams{ChannelID: channelID, Seq: beforeSeq, Limit: int32(limit)})
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("chat: list messages: %w", err)
	}
	return messagesProto(rows)
}

// SyncChannels implements Service. One delta comes back per cursor, in the
// cursors' order, with its messages oldest first: the messages after the cursor, or
// when it is more than 200 behind, a reload carrying the latest page. One channel
// the actor may not read refuses the whole request.
func (s *service) SyncChannels(ctx context.Context, a authz.Actor, cursors []*chatv1.Cursor) ([]*chatv1.ChannelDelta, error) {
	ids := make([]uuid.UUID, len(cursors))
	for i, c := range cursors {
		id, err := parseID(c.GetChannelId(), "cursor channel id")
		if err != nil {
			return nil, err
		}
		if c.GetLastSeq() < 0 {
			return nil, fmt.Errorf("chat: cursor last_seq must not be negative: %w", errs.ErrInvalid)
		}
		ids[i] = id
	}
	for _, id := range ids {
		if err := s.can(ctx, a, authz.ActChannelRead, id); err != nil {
			return nil, err
		}
	}

	deltas := make([]*chatv1.ChannelDelta, len(cursors))
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		for i, c := range cursors {
			ch, err := q.GetChannel(ctx, ids[i])
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("chat: sync channel %s: %w", ids[i], errs.ErrNotFound)
			}
			if err != nil {
				return fmt.Errorf("chat: sync channel: %w", err)
			}

			var rows []store.ChatMessage
			d := &chatv1.ChannelDelta{ChannelId: ids[i].String()}
			switch behind := ch.LastSeq - c.GetLastSeq(); {
			case behind > maxCatchUp:
				d.Reload = true
				if rows, err = q.ListLatestMessages(ctx, store.ListLatestMessagesParams{ChannelID: ids[i], Limit: defaultPage}); err == nil {
					slices.Reverse(rows)
				}
			case behind > 0:
				rows, err = q.ListMessagesBetween(ctx, store.ListMessagesBetweenParams{ChannelID: ids[i], AfterSeq: c.GetLastSeq(), ThroughSeq: ch.LastSeq})
			}
			if err != nil {
				return fmt.Errorf("chat: sync channel: %w", err)
			}
			if d.Messages, err = messagesProto(rows); err != nil {
				return err
			}
			deltas[i] = d
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return deltas, nil
}

// ---- conversions ------------------------------------------------------------

func channelKind(k chatv1.ChannelKind) (string, error) {
	switch k {
	case chatv1.ChannelKind_CHANNEL_KIND_PUBLIC:
		return channelPublic, nil
	case chatv1.ChannelKind_CHANNEL_KIND_PRIVATE:
		return channelPrivate, nil
	}
	return "", fmt.Errorf("chat: channel kind must be public or private, got %s: %w", k, errs.ErrInvalid)
}

func channelProto(c store.ChatChannel) *chatv1.Channel {
	kind := chatv1.ChannelKind_CHANNEL_KIND_PRIVATE
	if c.Kind == channelPublic {
		kind = chatv1.ChannelKind_CHANNEL_KIND_PUBLIC
	}
	return &chatv1.Channel{Id: c.ID.String(), Name: c.Name, Kind: kind, LastSeq: c.LastSeq}
}

func messagesProto(rows []store.ChatMessage) ([]*chatv1.Message, error) {
	out := make([]*chatv1.Message, len(rows))
	for i, r := range rows {
		m, err := messageProto(r)
		if err != nil {
			return nil, err
		}
		out[i] = m
	}
	return out, nil
}

func messageProto(m store.ChatMessage) (*chatv1.Message, error) {
	mentions, err := decodeList[chatv1.Mention](m.Mentions)
	if err != nil {
		return nil, fmt.Errorf("chat: message %s mentions: %w", m.ID, err)
	}
	files, err := decodeList[chatv1.FileRef](m.Attachments)
	if err != nil {
		return nil, fmt.Errorf("chat: message %s attachments: %w", m.ID, err)
	}
	var artifact *chatv1.Artifact
	if len(m.Artifact) > 0 {
		artifact = &chatv1.Artifact{}
		if err := unmarshalJSON(m.Artifact, artifact); err != nil {
			return nil, fmt.Errorf("chat: message %s artifact: %w", m.ID, err)
		}
	}
	kind := chatv1.AuthorKind_AUTHOR_KIND_USER
	if m.AuthorKind == authorAgent {
		kind = chatv1.AuthorKind_AUTHOR_KIND_AGENT
	}
	return &chatv1.Message{
		Id: m.ID.String(), ChannelId: m.ChannelID.String(), Seq: m.Seq,
		AuthorKind: kind, AuthorId: m.AuthorID.String(), Body: m.Body,
		Mentions: mentions, ClientMsgId: m.ClientMsgID, CreatedAt: timestamppb.New(m.CreatedAt.Time),
		Files: files, Artifact: artifact,
	}, nil
}

// encodeList stores messages of one kind as a JSON array of their protojson.
func encodeList[M proto.Message](items []M) (json.RawMessage, error) {
	parts := make([]json.RawMessage, len(items))
	for i, it := range items {
		b, err := protoJSON.Marshal(it)
		if err != nil {
			return nil, fmt.Errorf("chat: encode %T: %w", it, err)
		}
		parts[i] = b
	}
	return json.Marshal(parts)
}

// decodeList reads what encodeList wrote.
func decodeList[T any, P interface {
	*T
	proto.Message
}](raw json.RawMessage) ([]P, error) {
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	out := make([]P, len(parts))
	for i, part := range parts {
		out[i] = P(new(T))
		if err := unmarshalJSON(part, out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// unmarshalJSON skips fields it does not know, so a row written by a newer version
// still reads during a rolling deploy.
func unmarshalJSON(raw []byte, into proto.Message) error {
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, into)
}

// ---- helpers ----------------------------------------------------------------

func parseID(raw, what string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("chat: %s %q is not a UUID: %w", what, raw, errs.ErrInvalid)
	}
	return id, nil
}

// validText reports whether s can be stored in a Postgres text column: valid UTF-8
// without NUL.
func validText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// pgCode is the SQLSTATE of a Postgres error, or "" for any other error.
func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}
