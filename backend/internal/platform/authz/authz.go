package authz

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"kiwi/internal/platform/errs"
)

// agentActions is everything an agent may do, always as the user it acts for.
// An agent never writes or confirms tasks, joins or manages channels, or uploads
// files: it drafts, and a person confirms.
var agentActions = map[Action]bool{
	ActChannelRead:     true,
	ActMessagePost:     true,
	ActTaskRead:        true,
	ActTaskWriteDraft:  true,
	ActKnowledgeSearch: true,
}

// systemActions is everything a background job may do. It needs no membership:
// the job runs for the person whose action started it and row-level security
// still bounds it to the workspace.
var systemActions = map[Action]bool{
	ActChannelRead:    true,
	ActMessagePost:    true,
	ActTaskWriteDraft: true,
	ActFileWrite:      true,
	ActMeetingWrite:   true,
	ActKnowledgeIndex: true,
}

// userActions is every action a channel member may do. Channel roles are not
// modelled, so any member may manage the channel. ActChannelJoin is absent: it
// is decided by the channel being public, not by membership.
var userActions = map[Action]bool{
	ActChannelRead:     true,
	ActChannelManage:   true,
	ActMessagePost:     true,
	ActTaskRead:        true,
	ActTaskWriteDraft:  true,
	ActTaskWrite:       true,
	ActTaskConfirm:     true,
	ActFileUpload:      true,
	ActFileRead:        true,
	ActFileWrite:       true,
	ActKnowledgeSearch: true,
	ActKnowledgeIndex:  true,
	ActMeetingStart:    true,
	ActMeetingRead:     true,
	ActMeetingWrite:    true,
}

// Authz decides whether an Actor may perform an Action on a Resource. It is the
// single rule table: domains call Can and nothing else grants access.
type Authz struct {
	members Membership
}

// New returns an Authz that asks m about channel membership.
func New(m Membership) *Authz { return &Authz{members: m} }

// Can returns nil when a may perform act on r, otherwise an error wrapping
// errs.ErrForbidden. It denies by default: an unknown actor kind, an unknown
// action, an actor without a workspace or identity, and a failed membership
// lookup are all forbidden.
func (z *Authz) Can(ctx context.Context, a Actor, act Action, r Resource) error {
	if a.WorkspaceID == uuid.Nil {
		return forbidden(a, act, "actor has no workspace")
	}
	switch a.Kind {
	case ActorUser:
		if a.UserID == uuid.Nil {
			return forbidden(a, act, "user has no id")
		}
		if act == ActChannelJoin {
			if !r.ChannelPublic {
				return forbidden(a, act, "channel is private")
			}
			return nil
		}
		if !userActions[act] {
			return forbidden(a, act, "unknown action")
		}
		return z.requireMember(ctx, a, act, r)
	case ActorAgent:
		if !agentActions[act] {
			return forbidden(a, act, "agents may not do this")
		}
		if a.OnBehalfOf == uuid.Nil {
			return forbidden(a, act, "agent has no invoking user")
		}
		return z.requireMember(ctx, a, act, r)
	case ActorSystem:
		if !systemActions[act] {
			return forbidden(a, act, "jobs may not do this")
		}
		return nil
	default:
		return forbidden(a, act, "unknown actor kind")
	}
}

// requireMember checks the channel membership of the user the actor exercises.
func (z *Authz) requireMember(ctx context.Context, a Actor, act Action, r Resource) error {
	ok, err := z.members.IsMember(WithWorkspace(ctx, a.WorkspaceID), r.ChannelID, a.EffectiveUserID())
	if err != nil {
		return fmt.Errorf("authz: %s %s: membership lookup: %w: %w", a.Kind, act, errs.ErrForbidden, err)
	}
	if !ok {
		return forbidden(a, act, "not a channel member")
	}
	return nil
}

func forbidden(a Actor, act Action, why string) error {
	return fmt.Errorf("authz: %s %s: %s: %w", a.Kind, act, why, errs.ErrForbidden)
}

type workspaceKey struct{}

// WithWorkspace returns a context carrying the workspace a Membership lookup
// must open its transaction in.
func WithWorkspace(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, workspaceKey{}, id)
}

// WorkspaceFrom returns the workspace WithWorkspace stored, or uuid.Nil.
func WorkspaceFrom(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(workspaceKey{}).(uuid.UUID)
	return id
}
