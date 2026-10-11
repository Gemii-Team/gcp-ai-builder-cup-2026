// Package chat owns channels, membership and messages. Other domains reach it only
// through Service.
package chat

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	"kiwi/internal/platform/authz"
)

// Service is the chat domain. The comment on each method names the authz.Action it
// checks for the actor on the channel concerned.
type Service interface {
	// CreateChannel creates a channel in the actor's workspace with the actor as its
	// first member. There is no channel yet to check, so it needs no authz action.
	CreateChannel(ctx context.Context, a authz.Actor, req *chatv1.CreateChannelRequest) (*chatv1.Channel, error)
	// ListChannels returns the channels of the actor's workspace that the actor can
	// see. It is not channel-scoped, so it needs no authz action.
	ListChannels(ctx context.Context, a authz.Actor) ([]*chatv1.Channel, error)
	// JoinChannel adds the actor to a public channel; it needs authz.ActChannelJoin.
	JoinChannel(ctx context.Context, a authz.Actor, channelID uuid.UUID) error
	// AddMember adds userID to the channel; it needs authz.ActChannelManage.
	AddMember(ctx context.Context, a authz.Actor, channelID, userID uuid.UUID) error
	// RemoveMember removes userID from the channel; it needs authz.ActChannelManage.
	RemoveMember(ctx context.Context, a authz.Actor, channelID, userID uuid.UUID) error
	// ListMembers returns the members of the channel; it needs authz.ActChannelRead.
	ListMembers(ctx context.Context, a authz.Actor, channelID uuid.UUID) ([]*chatv1.Member, error)
	// PostMessage appends a message to its channel; it needs authz.ActMessagePost.
	PostMessage(ctx context.Context, a authz.Actor, req *chatv1.SendMessageRequest) (*chatv1.Message, error)
	// PostMessageTx is PostMessage inside the caller's transaction tx, so a pipeline
	// can commit the message together with its own effects; it needs
	// authz.ActMessagePost.
	PostMessageTx(ctx context.Context, tx pgx.Tx, a authz.Actor, req *chatv1.SendMessageRequest) (*chatv1.Message, error)
	// ListMessages pages a channel's history DESCENDING by seq: it returns up to limit
	// messages with seq < beforeSeq, newest first, and beforeSeq 0 means the latest
	// page. It needs authz.ActChannelRead.
	ListMessages(ctx context.Context, a authz.Actor, channelID uuid.UUID, beforeSeq int64, limit int) ([]*chatv1.Message, error)
	// SyncChannels returns what each cursor's channel gained since the client's last
	// seq (or a reload); it needs authz.ActChannelRead on every channel in cursors.
	SyncChannels(ctx context.Context, a authz.Actor, cursors []*chatv1.Cursor) ([]*chatv1.ChannelDelta, error)
	// IsMember reports whether userID belongs to the channel. It is the lookup behind
	// authz.Membership, so it takes no actor and needs no authz action.
	IsMember(ctx context.Context, channelID, userID uuid.UUID) (bool, error)
	// MemberChannelIDs returns the ids of the channels userID belongs to in the
	// workspace. It takes no actor and needs no authz action; callers have already
	// authorized the user.
	MemberChannelIDs(ctx context.Context, workspaceID, userID uuid.UUID) ([]uuid.UUID, error)
}
