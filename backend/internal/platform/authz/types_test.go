package authz_test

import (
	"testing"

	"github.com/google/uuid"

	"kiwi/internal/platform/authz"
)

func TestActor_EffectiveUserID(t *testing.T) {
	user, owner := uuid.New(), uuid.New()
	tests := []struct {
		name  string
		actor authz.Actor
		want  uuid.UUID
	}{
		{"user is itself", authz.Actor{Kind: authz.ActorUser, UserID: user}, user},
		{"agent is the user it acts for", authz.Actor{Kind: authz.ActorAgent, UserID: uuid.New(), OnBehalfOf: owner, Agent: "assistant"}, owner},
		{"system is the origin user", authz.Actor{Kind: authz.ActorSystem, UserID: user}, user},
		{"zero actor has no user", authz.Actor{}, uuid.Nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.actor.EffectiveUserID(); got != tc.want {
				t.Fatalf("EffectiveUserID() = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestAgentID_IsStable pins the value: it is the author id stored on every agent
// message, so it must never change.
func TestAgentID_IsStable(t *testing.T) {
	const assistant = "df731520-44a0-534c-b038-51f630f85942"
	if got := authz.AgentID("assistant").String(); got != assistant {
		t.Fatalf("AgentID(assistant) = %s, want %s", got, assistant)
	}
	if authz.AgentID("assistant") == authz.AgentID("other") {
		t.Fatal("different agents must have different ids")
	}
}
