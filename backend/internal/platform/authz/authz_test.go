package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/errs"
)

var allActions = []authz.Action{
	authz.ActChannelRead, authz.ActChannelJoin, authz.ActChannelManage, authz.ActMessagePost,
	authz.ActTaskRead, authz.ActTaskWriteDraft, authz.ActTaskWrite, authz.ActTaskConfirm,
	authz.ActFileUpload, authz.ActFileRead, authz.ActFileWrite,
	authz.ActKnowledgeSearch, authz.ActKnowledgeIndex,
	authz.ActMeetingStart, authz.ActMeetingRead, authz.ActMeetingWrite,
}

type lookup struct {
	ctx     context.Context
	channel uuid.UUID
	user    uuid.UUID
}

// fakeMembership answers from a set of (channel, user) pairs and records every
// lookup so tests can see whose membership was asked for.
type fakeMembership struct {
	members map[[2]uuid.UUID]bool
	err     error
	lookups []lookup
}

func newMembership(channel uuid.UUID, users ...uuid.UUID) *fakeMembership {
	m := &fakeMembership{members: map[[2]uuid.UUID]bool{}}
	for _, u := range users {
		m.members[[2]uuid.UUID{channel, u}] = true
	}
	return m
}

func (f *fakeMembership) IsMember(ctx context.Context, channelID, userID uuid.UUID) (bool, error) {
	f.lookups = append(f.lookups, lookup{ctx, channelID, userID})
	if f.err != nil {
		return false, f.err
	}
	return f.members[[2]uuid.UUID{channelID, userID}], nil
}

type fixture struct {
	ws, channel, member, outsider uuid.UUID
	m                             *fakeMembership
	z                             *authz.Authz
}

func newFixture() fixture {
	f := fixture{ws: uuid.New(), channel: uuid.New(), member: uuid.New(), outsider: uuid.New()}
	f.m = newMembership(f.channel, f.member)
	f.z = authz.New(f.m)
	return f
}

func (f fixture) user(id uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorUser, WorkspaceID: f.ws, UserID: id}
}

func (f fixture) agentFor(id uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorAgent, WorkspaceID: f.ws, UserID: uuid.New(), OnBehalfOf: id, Agent: "assistant"}
}

func (f fixture) system(origin uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorSystem, WorkspaceID: f.ws, UserID: origin}
}

func (f fixture) resource(public bool) authz.Resource {
	return authz.Resource{ChannelID: f.channel, ChannelPublic: public}
}

func wantForbidden(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("%s: err = %v, want errs.ErrForbidden", what, err)
	}
}

func wantAllowed(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: err = %v, want nil", what, err)
	}
}

func TestUser_NonMemberForbiddenOnEveryChannelAction(t *testing.T) {
	f := newFixture()
	for _, public := range []bool{false, true} {
		for _, act := range allActions {
			if act == authz.ActChannelJoin && public {
				continue // TestUser_JoinOnlyPublic
			}
			err := f.z.Can(t.Context(), f.user(f.outsider), act, f.resource(public))
			wantForbidden(t, err, string(act))
		}
	}
}

func TestUser_MemberAllowed(t *testing.T) {
	f := newFixture()
	for _, act := range allActions {
		if act == authz.ActChannelJoin {
			continue // TestUser_JoinOnlyPublic
		}
		wantAllowed(t, f.z.Can(t.Context(), f.user(f.member), act, f.resource(false)), string(act))
	}
}

func TestUser_JoinOnlyPublic(t *testing.T) {
	f := newFixture()
	tests := []struct {
		name   string
		public bool
		allow  bool
	}{
		{"non-member joins a public channel", true, true},
		{"non-member cannot join a private channel", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := f.z.Can(t.Context(), f.user(f.outsider), authz.ActChannelJoin, f.resource(tc.public))
			if tc.allow {
				wantAllowed(t, err, "join")
			} else {
				wantForbidden(t, err, "join")
			}
		})
	}
	if len(f.m.lookups) != 0 {
		t.Fatalf("join must not depend on a membership lookup; got %d", len(f.m.lookups))
	}
}

func TestAgent_UsesInvokingUsersMembership(t *testing.T) {
	agentActions := []authz.Action{
		authz.ActChannelRead, authz.ActMessagePost, authz.ActTaskRead, authz.ActTaskWriteDraft, authz.ActKnowledgeSearch,
	}
	t.Run("on behalf of a member", func(t *testing.T) {
		f := newFixture()
		// The agent's own UserID is not a member: only OnBehalfOf counts.
		for _, act := range agentActions {
			wantAllowed(t, f.z.Can(t.Context(), f.agentFor(f.member), act, f.resource(false)), string(act))
		}
		for _, l := range f.m.lookups {
			if l.user != f.member || l.channel != f.channel {
				t.Fatalf("looked up (%s, %s), want (%s, %s)", l.channel, l.user, f.channel, f.member)
			}
		}
		if len(f.m.lookups) != len(agentActions) {
			t.Fatalf("lookups = %d, want %d", len(f.m.lookups), len(agentActions))
		}
	})
	t.Run("on behalf of a non-member", func(t *testing.T) {
		f := newFixture()
		// Even when the actor's UserID is a member, the invoking user is what is checked.
		a := f.agentFor(f.outsider)
		a.UserID = f.member
		for _, act := range agentActions {
			wantForbidden(t, f.z.Can(t.Context(), a, act, f.resource(true)), string(act))
		}
	})
}

func TestAgent_CannotConfirmOrWriteConfirmedTasks(t *testing.T) {
	f := newFixture()
	allowed := map[authz.Action]bool{
		authz.ActChannelRead: true, authz.ActMessagePost: true, authz.ActTaskRead: true,
		authz.ActTaskWriteDraft: true, authz.ActKnowledgeSearch: true,
	}
	for _, act := range []authz.Action{
		authz.ActTaskWrite, authz.ActTaskConfirm, authz.ActChannelJoin, authz.ActChannelManage, authz.ActFileUpload,
	} {
		if allowed[act] {
			t.Fatalf("test table error: %s is in the agent allowed set", act)
		}
	}
	for _, act := range allActions {
		err := f.z.Can(t.Context(), f.agentFor(f.member), act, f.resource(true))
		if allowed[act] {
			wantAllowed(t, err, string(act))
		} else {
			wantForbidden(t, err, string(act))
		}
	}
}

func TestSystem_AllowedSetOnly(t *testing.T) {
	f := newFixture()
	allowed := map[authz.Action]bool{
		authz.ActChannelRead: true, authz.ActMessagePost: true, authz.ActTaskWriteDraft: true,
		authz.ActFileWrite: true, authz.ActMeetingWrite: true, authz.ActKnowledgeIndex: true,
	}
	// The origin user is not a member: a system actor has no membership check
	// (row-level security still bounds the workspace).
	for _, act := range allActions {
		err := f.z.Can(t.Context(), f.system(f.outsider), act, f.resource(false))
		if allowed[act] {
			wantAllowed(t, err, string(act))
		} else {
			wantForbidden(t, err, string(act))
		}
	}
	if len(f.m.lookups) != 0 {
		t.Fatalf("system actor must not trigger membership lookups; got %d", len(f.m.lookups))
	}
}

func TestZeroActor_Forbidden(t *testing.T) {
	f := newFixture()
	tests := []struct {
		name  string
		actor authz.Actor
	}{
		{"zero value", authz.Actor{}},
		{"user without a workspace", authz.Actor{Kind: authz.ActorUser, UserID: f.member}},
		{"agent without a workspace", authz.Actor{Kind: authz.ActorAgent, OnBehalfOf: f.member, Agent: "assistant"}},
		{"system without a workspace", authz.Actor{Kind: authz.ActorSystem, UserID: f.member}},
		{"workspace without a kind", authz.Actor{WorkspaceID: f.ws, UserID: f.member}},
		{"unknown kind", authz.Actor{Kind: "robot", WorkspaceID: f.ws, UserID: f.member}},
		{"user without an id", authz.Actor{Kind: authz.ActorUser, WorkspaceID: f.ws}},
		{"agent without an invoking user", authz.Actor{Kind: authz.ActorAgent, WorkspaceID: f.ws, UserID: f.member, Agent: "assistant"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, public := range []bool{false, true} {
				for _, act := range allActions {
					wantForbidden(t, f.z.Can(t.Context(), tc.actor, act, f.resource(public)), string(act))
				}
			}
		})
	}
	t.Run("unknown action", func(t *testing.T) {
		for _, a := range []authz.Actor{f.user(f.member), f.agentFor(f.member), f.system(f.member)} {
			wantForbidden(t, f.z.Can(t.Context(), a, authz.Action("task.delete"), f.resource(true)), string(a.Kind))
		}
	})
}

type ctxKey struct{}

func TestCan_PassesActorWorkspaceToMembershipContext(t *testing.T) {
	f := newFixture()
	if got := authz.WorkspaceFrom(t.Context()); got != uuid.Nil {
		t.Fatalf("WorkspaceFrom(empty ctx) = %s, want uuid.Nil", got)
	}
	// A different workspace already in the context must not win over the actor's.
	ctx := authz.WithWorkspace(context.WithValue(t.Context(), ctxKey{}, "kept"), uuid.New())
	for _, a := range []authz.Actor{f.user(f.member), f.agentFor(f.member)} {
		f.m.lookups = nil
		wantAllowed(t, f.z.Can(ctx, a, authz.ActChannelRead, f.resource(false)), string(a.Kind))
		if len(f.m.lookups) != 1 {
			t.Fatalf("%s: lookups = %d, want 1", a.Kind, len(f.m.lookups))
		}
		got := f.m.lookups[0].ctx
		if ws := authz.WorkspaceFrom(got); ws != f.ws {
			t.Fatalf("%s: membership saw workspace %s, want %s", a.Kind, ws, f.ws)
		}
		if got.Value(ctxKey{}) != "kept" {
			t.Fatalf("%s: caller's context values were dropped", a.Kind)
		}
	}
}

func TestMembershipLookupError_FailsClosed(t *testing.T) {
	f := newFixture()
	boom := errors.New("store down")
	f.m.err = boom
	// The member is a real member: only the lookup error stands between them and access.
	for _, a := range []authz.Actor{f.user(f.member), f.agentFor(f.member)} {
		err := f.z.Can(t.Context(), a, authz.ActChannelRead, f.resource(false))
		wantForbidden(t, err, string(a.Kind))
		if !errors.Is(err, boom) {
			t.Fatalf("%s: err = %v, want it to keep the store error as the cause", a.Kind, err)
		}
	}
}
