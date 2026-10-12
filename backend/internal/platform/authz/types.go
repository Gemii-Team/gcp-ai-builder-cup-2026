// Package authz holds the authorization vocabulary every domain shares: who is
// acting (Actor), what they want to do (Action) and on what (Resource). It is a leaf
// package so domains can use it without import cycles.
package authz

import (
	"context"

	"github.com/google/uuid"
)

// ActorKind says what kind of principal is acting.
type ActorKind string

// Actor kinds.
const (
	// ActorUser is a signed-in person.
	ActorUser ActorKind = "user"
	// ActorAgent is an AI agent acting on behalf of a user; it never has more rights
	// than that user.
	ActorAgent ActorKind = "agent"
	// ActorSystem is a background job acting for the user whose action started it.
	ActorSystem ActorKind = "system"
)

// Actor is the principal of a request, carried in the context by the API shell and
// passed explicitly to every domain method.
type Actor struct {
	Kind        ActorKind
	WorkspaceID uuid.UUID
	// UserID is the signed-in user, or for a system actor the origin user.
	UserID uuid.UUID
	// OnBehalfOf is the user an agent acts for.
	OnBehalfOf uuid.UUID
	// Agent is the registry name of an agent actor, for example "assistant".
	Agent string
}

// EffectiveUserID is the user whose rights and identity the actor exercises: UserID
// for a user and for a system actor (the origin user), OnBehalfOf for an agent. A
// zero Actor yields uuid.Nil.
func (a Actor) EffectiveUserID() uuid.UUID {
	if a.Kind == ActorAgent {
		return a.OnBehalfOf
	}
	return a.UserID
}

// Action is something an actor may be allowed to do.
type Action string

// Actions an actor can be authorized for. ActKnowledgeIndex also covers what the
// indexer, a system actor, reads to build its chunks (docs.ExtractPages,
// meetings.TranscriptEntries).
const (
	ActChannelRead     Action = "channel.read"
	ActChannelJoin     Action = "channel.join"
	ActChannelManage   Action = "channel.manage"
	ActMessagePost     Action = "message.post"
	ActTaskRead        Action = "task.read"
	ActTaskWriteDraft  Action = "task.write_draft"
	ActTaskWrite       Action = "task.write"
	ActTaskConfirm     Action = "task.confirm"
	ActFileUpload      Action = "file.upload"
	ActFileRead        Action = "file.read"
	ActFileWrite       Action = "file.write"
	ActKnowledgeSearch Action = "knowledge.search"
	ActKnowledgeIndex  Action = "knowledge.index"
	ActMeetingStart    Action = "meeting.start"
	ActMeetingRead     Action = "meeting.read"
	ActMeetingWrite    Action = "meeting.write"
)

// Resource is what an Action applies to. Every resource lives in a channel;
// ChannelPublic is what lets a non-member join.
type Resource struct {
	ChannelID     uuid.UUID
	ChannelPublic bool
}

// Membership answers whether a user belongs to a channel. The chat domain implements
// it; the context carries the workspace so implementations can open a scoped
// transaction.
type Membership interface {
	IsMember(ctx context.Context, channelID, userID uuid.UUID) (bool, error)
}

// agentNamespace is the UUIDv5 namespace of agent ids. Changing it changes every
// stored agent author id.
var agentNamespace = uuid.NewSHA1(uuid.NameSpaceDNS, []byte("agents.kiwi.local"))

// AgentID is the stable author id of the agent called name; every message an agent
// posts carries it. It lives here, in a leaf package, so meetings, docs and agents
// can all use it without an import cycle.
func AgentID(name string) uuid.UUID {
	return uuid.NewSHA1(agentNamespace, []byte(name))
}
