package agents

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	agentsv1 "kiwi/gen/kiwi/agents/v1"
	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
)

// Retrieval scope follows the audience: a run that has a channel context searches only
// that channel. A reply to an @mention is read by the channel it is posted in, so a member of
// a private channel who mentions the assistant elsewhere must not get that channel's content
// republished there; an Ask run whose request names a channel is told "Searching only
// #X" by the page. An Ask run without one is private to the asker and searches every channel
// of theirs.

const (
	hrFact     = "The payroll salary band is 90,000 baht."
	hrTitle    = "HR salary review"
	hrQuestion = "What is the payroll salary band?"
)

// scopeFixture is alice with two channels, public (her #general) and hr (a private channel of hers and
// bob's), and the two channels she is not in, each with a decoy meeting about the warehouse. hr holds
// a meeting about the payroll; public holds the warehouse meeting when withWarehouse is set, and
// nothing otherwise, so that a search of it alone finds nothing.
func scopeFixture(t *testing.T, withWarehouse bool) (a *agentFixture, hr uuid.UUID) {
	t.Helper()
	a = newAgentFixture(t)
	a.llm.HandleGenerate(citeEveryChunk)
	hr = a.channel("private", a.alice, a.bob)
	a.indexedMeeting(hr, a.bob, hrTitle, hrFact)
	if withWarehouse {
		a.indexedMeeting(a.public, a.bob, "Warehouse review", warehouseFact)
	}
	a.indexedMeeting(a.private, a.bob, "Decoy private warehouse review", "The private warehouse holds 9,500 pallets.")
	a.indexedMeeting(a.elsewhere, a.carol, "Decoy elsewhere warehouse review", "The other warehouse holds 7,000 pallets.")
	return a, hr
}

// startAskIn asks the Ask panel as alice with the request naming channel.
func (a *agentFixture) startAskIn(svc Service, channel uuid.UUID, question string) (*agentsv1.Run, *eventsv1.AgentInvoke) {
	a.t.Helper()
	run, err := svc.StartRun(a.t.Context(), a.person(a.alice), &agentsv1.StartRunRequest{
		Agent: "assistant", Question: question, ChannelId: proto.String(channel.String()),
	})
	if err != nil {
		a.t.Fatalf("StartRun in %s: %v", channel, err)
	}
	return run, a.queuedInvoke(run)
}

// citedChannels is every channel a citation of slip comes from.
func citedChannels(slip *chatv1.Artifact) []string {
	var out []string
	for _, c := range slip.GetCitations() {
		if id := c.GetSource().GetChannelId(); !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// neverShown fails the test for each of what that any prompt of the answer model carried.
func (a *agentFixture) neverShown(what ...string) {
	a.t.Helper()
	for _, g := range a.llm.GenerateCalls() {
		for _, w := range what {
			if strings.Contains(promptOf(g), w) {
				a.t.Errorf("the answer model was shown %q from a channel the run was not scoped to:\n%s", w, promptOf(g))
			}
		}
	}
}

func TestMention_SearchIsScopedToTheMentionedChannel(t *testing.T) {
	t.Run("the content lives in another channel of the person: the slip says nothing was found and names only the channel", func(t *testing.T) {
		a, hr := scopeFixture(t, false)
		w := a.watch("ch:" + a.public.String())
		a.model.script(callTool("search_knowledge", map[string]any{"question": hrQuestion}), say("Nothing found."))
		trigger := a.mention(a.public, a.alice, "@assistant "+hrQuestion)
		runID := uuid.New()

		if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
			t.Fatalf("agent.run: %v", err)
		}

		replies := a.replies(a.public, a.alice)
		if len(replies) != 1 {
			t.Fatalf("agent replies = %d, want 1", len(replies))
		}
		slip := replies[0].GetArtifact()
		if slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_NOT_FOUND || len(slip.GetCitations()) != 0 {
			t.Fatalf("slip = %v, want a NOT_FOUND slip: the answer lives in #hr-private, which is not where it was asked", slip)
		}
		if !slices.Equal(slip.GetScopeChannelIds(), idStrings(a.public)) {
			t.Errorf("scope_channel_ids = %v, want only the channel the mention was typed in, %v", slip.GetScopeChannelIds(), a.public)
		}

		// Nothing of #hr-private is anywhere the mentioned channel, a later reader or the page could read it.
		run := a.run(runID)
		where := map[string]string{
			"the reply":          replies[0].String(),
			"the stored result":  string(run.Result),
			"the recorded calls": string(run.ToolCalls),
		}
		for i, f := range w.snapshot() {
			where[fmt.Sprintf("streamed frame %d", i)] = f.String()
		}
		for i, g := range a.llm.GenerateCalls() {
			where[fmt.Sprintf("prompt %d of the answer model", i)] = promptOf(g)
		}
		for name, got := range where {
			for _, leak := range []string{"90,000", hrTitle, hr.String()} {
				if strings.Contains(got, leak) {
					t.Errorf("%s holds %q of #hr-private", name, leak)
				}
			}
		}
	})

	t.Run("the same mention in the channel that has it is answered with a citation", func(t *testing.T) {
		a, hr := scopeFixture(t, true)
		a.model.script(callTool("search_knowledge", map[string]any{"question": hrQuestion}), say("The band is 90,000 baht [1]."))
		trigger := a.mention(hr, a.alice, "@assistant "+hrQuestion)

		if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, uuid.New()), "evt-1", 1)); err != nil {
			t.Fatalf("agent.run: %v", err)
		}

		replies := a.replies(hr, a.alice)
		if len(replies) != 1 {
			t.Fatalf("agent replies = %d, want 1", len(replies))
		}
		slip := replies[0].GetArtifact()
		if slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER || !slices.Equal(citedChannels(slip), idStrings(hr)) {
			t.Fatalf("slip = %v, want an ANSWER cited from #hr-private alone", slip)
		}
		if !slices.Equal(slip.GetScopeChannelIds(), idStrings(hr)) {
			t.Errorf("scope_channel_ids = %v, want only %v", slip.GetScopeChannelIds(), hr)
		}
		a.neverShown("4,000", "9,500", "7,000")
	})
}

// modes are the two ways an Ask run reaches its retrieval: the model calls search_knowledge, or
// the program makes the search because the model did not.
var modes = []struct {
	name   string
	script func(a *agentFixture)
}{
	{"the model searches", func(a *agentFixture) {
		a.model.script(callTool("search_knowledge", map[string]any{"question": palletsQuestion}), say("Here you go."))
	}},
	{"the program forces the search", func(a *agentFixture) { a.model.script(say(fromMemory)) }},
}

func TestAsk_ChannelScope_SearchesOnlyThatChannel(t *testing.T) {
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			a, hr := scopeFixture(t, true)
			mode.script(a)
			svc := New(a.deps)
			run, ev := a.startAskIn(svc, hr, hrQuestion)

			if err := a.job()(t.Context(), a.delivery(ev, "evt-ask", 1)); err != nil {
				t.Fatalf("agent.run: %v", err)
			}

			slip := a.answerOf(svc, uuid.MustParse(run.GetId()))
			if slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER || !slices.Equal(citedChannels(slip), idStrings(hr)) {
				t.Fatalf("slip = %v, want an ANSWER cited from the named channel alone", slip)
			}
			if !slices.Equal(slip.GetScopeChannelIds(), idStrings(hr)) {
				t.Errorf("scope_channel_ids = %v, want only the named channel %v", slip.GetScopeChannelIds(), hr)
			}
			a.neverShown("4,000", "9,500", "7,000")
		})
	}
}

func TestAsk_ChannelScope_NotFoundNamesOnlyThatChannel(t *testing.T) {
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			a, _ := scopeFixture(t, false)
			mode.script(a)
			svc := New(a.deps)
			run, ev := a.startAskIn(svc, a.public, hrQuestion) // the payroll band is in #hr-private, which alice also reads

			if err := a.job()(t.Context(), a.delivery(ev, "evt-ask", 1)); err != nil {
				t.Fatalf("agent.run: %v", err)
			}

			slip := a.answerOf(svc, uuid.MustParse(run.GetId()))
			if slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_NOT_FOUND || !slices.Equal(slip.GetScopeChannelIds(), idStrings(a.public)) {
				t.Fatalf("slip = %v, want NOT_FOUND naming only the channel the Ask was scoped to", slip)
			}
			if strings.Contains(slip.String(), "90,000") {
				t.Errorf("slip = %v, holds the payroll of #hr-private", slip)
			}
		})
	}
}

func TestAsk_NoScope_StillSearchesEveryChannelOfThePerson(t *testing.T) {
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			a, hr := scopeFixture(t, true)
			mode.script(a)
			svc := New(a.deps)

			id := a.ask(svc, hrQuestion)

			slip := a.answerOf(svc, id)
			if slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER || !slices.Equal(citedChannels(slip), idStrings(a.public, hr)) {
				t.Fatalf("slip = %v, want an ANSWER cited from both channels of alice and no other", slip)
			}
			if !slices.Equal(slip.GetScopeChannelIds(), idStrings(a.public, hr)) {
				t.Errorf("scope_channel_ids = %v, want every channel alice belongs to: %v and %v", slip.GetScopeChannelIds(), a.public, hr)
			}
			a.neverShown("9,500", "7,000")
		})
	}
}
