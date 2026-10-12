package agents

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentsv1 "kiwi/gen/kiwi/agents/v1"
	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	realtimev1 "kiwi/gen/kiwi/realtime/v1"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/llm"
	"kiwi/internal/testkit/fakellm"
)

// The Ask panel keeps one conversation per person, and a model that finds the answer in it
// may not search. Every Ask is answered from the retrieval of its own turn: the program
// makes that retrieval itself when the model did not, and what the model wrote instead is
// never the answer.

// ask puts question to the Ask panel as alice and delivers its run once, as the relay does.
func (a *agentFixture) ask(svc Service, question string) uuid.UUID {
	a.t.Helper()
	run, ev := a.startAsk(svc, question)
	if err := a.job()(a.t.Context(), a.delivery(ev, "evt-"+run.GetId(), 1)); err != nil {
		a.t.Fatalf("agent.run of %q: %v", question, err)
	}
	return uuid.MustParse(run.GetId())
}

// queuedInvoke is the agent.invoke event the outbox holds for run, which the relay delivers.
func (a *agentFixture) queuedInvoke(run *agentsv1.Run) *eventsv1.AgentInvoke {
	a.t.Helper()
	var payload string
	a.scan(&payload, `SELECT payload::text FROM platform.outbox WHERE topic = 'agent.invoke' AND key = $1`, run.GetId())
	var ev eventsv1.AgentInvoke
	if err := protojson.Unmarshal([]byte(payload), &ev); err != nil {
		a.t.Fatal(err)
	}
	return &ev
}

// answerOf is the slip GetRun gives alice for a run that is done.
func (a *agentFixture) answerOf(svc Service, run uuid.UUID) *chatv1.Artifact {
	a.t.Helper()
	got, err := svc.GetRun(a.t.Context(), a.person(a.alice), run)
	if err != nil || got.GetStatus() != agentsv1.RunStatus_RUN_STATUS_DONE || got.GetAnswer() == nil {
		a.t.Fatalf("GetRun = %v, %v, want a DONE run with its slip", got, err)
	}
	return got.GetAnswer()
}

// recordedCall is a call of agents.runs.tool_calls as an operator reads it in the database.
type recordedCall struct {
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
	Result string         `json:"result"`
	Error  string         `json:"error"`
	Forced bool           `json:"forced"`
}

func (a *agentFixture) recordedCalls(run uuid.UUID) []recordedCall {
	a.t.Helper()
	var calls []recordedCall
	if err := json.Unmarshal(a.run(run).ToolCalls, &calls); err != nil {
		a.t.Fatalf("tool_calls: %v", err)
	}
	return calls
}

// history is everything the model of request i was shown, as one string.
func (a *agentFixture) history(i int) string {
	a.t.Helper()
	var b strings.Builder
	for _, p := range a.requestParts(i) {
		b.WriteString(p.Text + "\n")
	}
	return b.String()
}

// unseen fails the test when text is anywhere a person could read or keep what the run answered.
func unseen(t *testing.T, text string, where map[string]string) {
	t.Helper()
	for name, got := range where {
		if strings.Contains(got, text) {
			t.Errorf("%s holds the model's own words %q, want only what the retrieval returned", name, text)
		}
	}
}

const (
	palletsQuestion = "How many pallets does the warehouse hold?"
	// A follow-up on the same topic in other words: the conversation already holds its answer.
	followUpQuestion = "And how much stock fits in the Bang Na warehouse, put differently?"
	fromMemory       = "As I said before, the Bang Na warehouse holds 4,000 pallets."
)

func TestAsk_FollowUpAnsweredFromMemory_IsReplacedByRetrieval(t *testing.T) {
	a := newAgentFixture(t)
	a.seedWarehouse()
	svc := New(a.deps)
	w := a.watch("u:" + a.alice.String())
	a.model.script(
		callTool("search_knowledge", map[string]any{"question": palletsQuestion}),
		say("The warehouse holds 4,000 pallets [1]."),
		say(fromMemory), // the follow-up: no tool, answered from the conversation
	)

	first := a.ask(svc, palletsQuestion)
	second := a.ask(svc, followUpQuestion)

	if !strings.Contains(a.history(2), palletsQuestion) {
		t.Fatalf("the model of the follow-up was not shown the conversation: %s", a.history(2))
	}
	slip := a.answerOf(svc, second)
	if slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER || slip.GetRef() != "ANS-1002" || slip.GetRunId() != second.String() {
		t.Fatalf("slip = %v, want an ANSWER with ANS-1002 of the follow-up run", slip)
	}
	if len(slip.GetCitations()) == 0 {
		t.Fatalf("slip has no citations: %v", slip)
	}
	for _, c := range slip.GetCitations() {
		if c.GetSource().GetChannelId() != a.public.String() || c.GetSource().GetLabel() != "Warehouse review" || c.GetChunkId() == "" {
			t.Errorf("citation %d = %v, want a chunk of the warehouse meeting that this turn's search retrieved", c.GetN(), c)
		}
	}
	if !strings.Contains(slip.GetBodyMarkdown(), "[1]") || !slices.Equal(slip.GetScopeChannelIds(), idStrings(a.public)) {
		t.Errorf("slip body %q, scope %v, want the retrieval's answer over alice's channels", slip.GetBodyMarkdown(), slip.GetScopeChannelIds())
	}

	row := a.run(second)
	if row.Status != "done" || row.Ref == nil || *row.Ref != "ANS-1002" || row.FailureReason != nil {
		t.Errorf("run = %+v, want done with ANS-1002", row)
	}
	calls := a.recordedCalls(second)
	if len(calls) != 1 || calls[0].Name != "search_knowledge" || !calls[0].Forced || calls[0].Error != "" ||
		calls[0].Args["question"] != followUpQuestion || !strings.Contains(calls[0].Result, `"found":true`) {
		t.Errorf("tool_calls = %+v, want the one search the program forced, marked as such, with the person's own question", calls)
	}
	if calls := a.recordedCalls(first); len(calls) != 1 || calls[0].Forced {
		t.Errorf("first run's tool_calls = %+v, want the model's own search, not marked as forced", calls)
	}
	done := w.waitFor("the DONE frame of the follow-up", func(frames []*realtimev1.AgentRunProgress) bool {
		return slices.ContainsFunc(frames, func(p *realtimev1.AgentRunProgress) bool {
			return p.GetRunId() == second.String() && p.GetState() == realtimev1.AgentRunState_AGENT_RUN_STATE_DONE
		})
	})
	var frame *realtimev1.AgentRunProgress
	var streamed strings.Builder
	for _, p := range done {
		if p.GetRunId() != second.String() {
			continue
		}
		streamed.WriteString(p.GetTextDelta())
		if p.GetState() == realtimev1.AgentRunState_AGENT_RUN_STATE_DONE {
			frame = p
		}
	}
	unseen(t, fromMemory, map[string]string{
		"the stored result": string(row.Result), "the slip": slip.String(), "the DONE frame": frame.GetAnswer().String(),
		"the streamed preview": streamed.String(),
	})
}

func TestAsk_FollowUpAnsweredFromMemory_NothingFound_IsNotFoundSlip(t *testing.T) {
	a := newAgentFixture(t)
	a.seedWarehouse()
	svc := New(a.deps)
	a.model.script(
		callTool("search_knowledge", map[string]any{"question": palletsQuestion}),
		say("The warehouse holds 4,000 pallets [1]."),
		say(fromMemory),
	)
	a.ask(svc, palletsQuestion)
	// What the conversation remembers is no longer in the channels.
	a.exec(`DELETE FROM knowledge.chunks`)

	second := a.ask(svc, followUpQuestion)

	slip := a.answerOf(svc, second)
	if slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_NOT_FOUND || slip.GetRef() != "ANS-1002" {
		t.Fatalf("slip = %v, want NOT_FOUND with ANS-1002: a completed search that found nothing", slip)
	}
	if len(slip.GetCitations()) != 0 || slip.GetBodyMarkdown() != "Not found in channels you can access." {
		t.Errorf("slip = %v, want the not-found text and no citations", slip)
	}
	if !slices.Equal(slip.GetScopeChannelIds(), idStrings(a.public)) {
		t.Errorf("scope_channel_ids = %v, want the channels that were searched: %v", slip.GetScopeChannelIds(), a.public)
	}
	row := a.run(second)
	if row.Status != "done" || row.Ref == nil || *row.Ref != "ANS-1002" || row.FailureReason != nil {
		t.Errorf("run = %+v, want done with ANS-1002 and no failure", row)
	}
	if calls := a.recordedCalls(second); len(calls) != 1 || !calls[0].Forced || !strings.Contains(calls[0].Result, `"found":false`) {
		t.Errorf("tool_calls = %+v, want the forced search that found nothing", calls)
	}
	unseen(t, fromMemory, map[string]string{"the stored result": string(row.Result), "the slip": slip.String()})
	unseen(t, "4,000", map[string]string{"the stored result": string(row.Result), "the slip": slip.String()})
}

func TestAsk_ChatterIsAnsweredByTheSearchNotByTheModel(t *testing.T) {
	a := newAgentFixture(t)
	a.model.script(say("I can search what your team said."))
	svc := New(a.deps)

	id := a.ask(svc, "what can you do?")

	slip := a.answerOf(svc, id)
	if slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_NOT_FOUND || slip.GetRef() != "ANS-1001" {
		t.Errorf("slip = %v, want NOT_FOUND with ANS-1001: an Ask is a question, and has a source or says it has none", slip)
	}
	if run := a.run(id); run.Status != "done" || run.Ref == nil || *run.Ref != "ANS-1001" {
		t.Errorf("run = %+v, want done with ANS-1001", run)
	}
	unseen(t, "I can search", map[string]string{"the stored result": string(a.run(id).Result), "the slip": slip.String()})
}

func TestAsk_EmptyModelReply_IsStillAnsweredByRetrieval(t *testing.T) {
	a := newAgentFixture(t)
	a.seedWarehouse()
	a.deps.Runtime = &stubRuntime{} // the model ended with nothing to say and no tool call
	svc := New(a.deps)

	id := a.ask(svc, palletsQuestion)

	if slip := a.answerOf(svc, id); slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER || len(slip.GetCitations()) == 0 {
		t.Errorf("slip = %v, want the answer of the forced search: an empty reply is not an error when the program can search", slip)
	}
}

func TestAsk_ModelCallsSearch_NoForcedSearch(t *testing.T) {
	a := newAgentFixture(t)
	a.seedWarehouse()
	svc := New(a.deps)
	a.model.script(
		callTool("search_knowledge", map[string]any{"question": palletsQuestion}),
		say("The warehouse holds 4,000 pallets [1]."),
	)

	id := a.ask(svc, palletsQuestion)

	if slip := a.answerOf(svc, id); slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER || slip.GetRef() != "ANS-1001" {
		t.Errorf("slip = %v, want the ANSWER of the model's own search", slip)
	}
	if calls := a.recordedCalls(id); len(calls) != 1 || calls[0].Forced {
		t.Errorf("tool_calls = %+v, want exactly the model's one search", calls)
	}
	if n := len(a.llm.GenerateCalls()); n != 1 {
		t.Errorf("answer generations = %d, want 1: the normal path searches once, not twice", n)
	}
}

func TestAsk_SummarizeChannelRun_IsNotForced(t *testing.T) {
	a := newAgentFixture(t)
	a.say(a.public, a.alice, "Launch moves to Friday.")
	a.say(a.public, a.bob, "Agreed, QA signs off Thursday.")
	a.llm.HandleGenerate(func(ctx context.Context, req llm.Request) (llm.Result, error) {
		return llm.Result{Text: `{"summary":"The launch moves to Friday; QA signs off Thursday.","source_seqs":[1,2]}`}, nil
	})
	a.model.script(
		callTool("summarize_channel", map[string]any{"channel_id": a.public.String()}),
		say("Here is the catch-up."),
	)
	svc := New(a.deps)
	run, err := svc.StartRun(t.Context(), a.person(a.alice), &agentsv1.StartRunRequest{
		Agent: "assistant", Question: "catch me up", ChannelId: proto.String(a.public.String()),
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := a.job()(t.Context(), a.delivery(a.queuedInvoke(run), "evt-ask", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}
	id := uuid.MustParse(run.GetId())

	slip := a.answerOf(svc, id)
	if slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_CHANNEL_SUMMARY || slip.GetRef() != "ANS-1001" || len(slip.GetSources()) != 2 {
		t.Errorf("slip = %v, want the CHANNEL_SUMMARY of the model's own tool call with its two sources", slip)
	}
	if calls := a.recordedCalls(id); len(calls) != 1 || calls[0].Name != "summarize_channel" || calls[0].Forced {
		t.Errorf("tool_calls = %+v, want the summary call alone: a summary is a legitimate answer of the Ask panel", calls)
	}
	if n := len(a.llm.EmbedCalls()); n != 0 {
		t.Errorf("embed calls = %d, want none: nothing was searched", n)
	}
}

func TestAsk_ForcedSearchRespectsTheInvokingPersonsChannels(t *testing.T) {
	a := newAgentFixture(t)
	a.llm.HandleGenerate(citeEveryChunk)
	a.indexedMeeting(a.public, a.bob, "Warehouse review", warehouseFact)
	// Decoys: the same topic in channels alice does not belong to.
	a.indexedMeeting(a.private, a.bob, "Decoy private warehouse review", "The private warehouse holds 9,000 pallets.")
	a.indexedMeeting(a.elsewhere, a.carol, "Decoy elsewhere warehouse review", "The other warehouse holds 7,000 pallets.")
	a.model.script(say(fromMemory))
	svc := New(a.deps)

	id := a.ask(svc, palletsQuestion)

	slip := a.answerOf(svc, id)
	if slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER || len(slip.GetCitations()) == 0 {
		t.Fatalf("slip = %v, want an ANSWER from the one channel alice may read", slip)
	}
	for _, c := range slip.GetCitations() {
		if c.GetSource().GetChannelId() != a.public.String() || strings.Contains(c.GetSource().GetLabel(), "Decoy") {
			t.Errorf("citation %d = %v, want only a source of alice's channel", c.GetN(), c)
		}
	}
	if !slices.Equal(slip.GetScopeChannelIds(), idStrings(a.public)) {
		t.Errorf("scope_channel_ids = %v, want only alice's channel %v", slip.GetScopeChannelIds(), a.public)
	}
	for _, g := range a.llm.GenerateCalls() {
		if strings.Contains(promptOf(g), "9,000") || strings.Contains(promptOf(g), "7,000") {
			t.Errorf("the answer model was shown a chunk of a channel alice is not in:\n%s", promptOf(g))
		}
	}
	if calls := a.recordedCalls(id); len(calls) != 1 || strings.Contains(calls[0].Result, "Decoy") {
		t.Errorf("tool_calls = %+v, want one forced search that names no decoy", calls)
	}
}

func TestAsk_ForcedSearch_SharesTheRunsToolBudget(t *testing.T) {
	spent := func(n int) string {
		calls := make([]ToolCall, n)
		for i := range calls {
			calls[i] = ToolCall{Name: toolListTasks, Result: "{}"}
		}
		raw, err := json.Marshal(calls)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	// Earlier attempts of the run used some of its budget, as agents.runs.tool_calls records.
	start := func(a *agentFixture, svc Service, used int) (uuid.UUID, *eventsv1.AgentInvoke) {
		run, ev := a.startAsk(svc, palletsQuestion)
		id := uuid.MustParse(run.GetId())
		a.exec(`UPDATE agents.runs SET tool_calls = $1::jsonb WHERE id = $2`, spent(used), id)
		return id, ev
	}
	deliver := func(a *agentFixture, ev *eventsv1.AgentInvoke) {
		t.Helper()
		if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 2)); err != nil {
			t.Fatalf("agent.run: %v", err)
		}
	}

	t.Run("one call left is enough for the search", func(t *testing.T) {
		a := newAgentFixture(t)
		a.seedWarehouse()
		a.model.script(say(fromMemory))
		svc := New(a.deps)
		id, ev := start(a, svc, maxToolCallsPerRun-1)

		deliver(a, ev)

		if slip := a.answerOf(svc, id); slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER {
			t.Errorf("slip = %v, want the forced search's answer", slip)
		}
		calls := a.recordedCalls(id)
		if len(calls) != maxToolCallsPerRun || !calls[len(calls)-1].Forced {
			t.Errorf("tool_calls = %d, last %+v, want the budget of %d spent, the last call the forced search", len(calls), calls[len(calls)-1], maxToolCallsPerRun)
		}
	})

	t.Run("no call left: the run is stopped, nothing is searched and the model's text is not the answer", func(t *testing.T) {
		a := newAgentFixture(t)
		captureLogs(t)
		a.seedWarehouse()
		a.model.script(say(fromMemory))
		svc := New(a.deps)
		id, ev := start(a, svc, maxToolCallsPerRun)
		embeds := len(a.llm.EmbedCalls())

		deliver(a, ev)

		row := a.run(id)
		if row.Status != "failed" || row.FailureReason == nil || *row.FailureReason != "too_many_steps" || row.Ref != nil || len(row.Result) != 0 {
			t.Errorf("run = %+v, want failed with too_many_steps, no REF and no answer", row)
		}
		if n := len(a.llm.EmbedCalls()) - embeds; n != 0 || len(a.llm.GenerateCalls()) != 0 {
			t.Errorf("the program searched past the budget (%d embeds, %d generations), want no search", n, len(a.llm.GenerateCalls()))
		}
		if n := len(a.recordedCalls(id)); n != maxToolCallsPerRun {
			t.Errorf("tool_calls = %d, want the %d of the earlier attempts and no more", n, maxToolCallsPerRun)
		}
		if a.ansCounters() != 0 {
			t.Errorf("a stopped run took a REF")
		}
	})
}

func TestAsk_RedeliveredRun_DoesNotSearchTwice(t *testing.T) {
	a := newAgentFixture(t)
	a.seedWarehouse()
	svc := New(a.deps)
	a.model.script(say(fromMemory))
	run, ev := a.startAsk(svc, palletsQuestion)
	id := uuid.MustParse(run.GetId())
	job := a.job()

	if err := job(t.Context(), a.delivery(ev, "evt-1", 1)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	requests, generations := len(a.model.requests()), len(a.llm.GenerateCalls())
	// The same message again, and another message that names the same run.
	for _, msg := range []string{"evt-1", "evt-2"} {
		if err := a.job()(t.Context(), a.delivery(ev, msg, 2)); err != nil {
			t.Fatalf("redelivery %s: %v", msg, err)
		}
	}

	if n := len(a.model.requests()); n != requests {
		t.Errorf("model calls = %d after redelivery, want the %d of the first delivery", n, requests)
	}
	if n := len(a.llm.GenerateCalls()); n != generations {
		t.Errorf("answer generations = %d after redelivery, want the %d of the first delivery", n, generations)
	}
	if calls := a.recordedCalls(id); len(calls) != 1 || !calls[0].Forced {
		t.Errorf("tool_calls = %+v, want the one forced search", calls)
	}
	if slip := a.answerOf(svc, id); slip.GetRef() != "ANS-1001" || a.ansCounters() != 1 {
		t.Errorf("slip = %v, want ANS-1001 once", slip)
	}
}

func TestAsk_ForcedSearchFails_IsRetriedAndNeverAnsweredFromMemory(t *testing.T) {
	a := newAgentFixture(t)
	logs := captureLogs(t)
	a.seedWarehouse()
	svc := New(a.deps)
	run, ev := a.startAsk(svc, palletsQuestion)
	id := uuid.MustParse(run.GetId())
	// The provider is down for the program's search: the model's text must not stand in for it.
	a.llm.QueueGenerate(fakellm.GenerateReply{Err: errors.New("provider is down")})
	a.model.script(say(fromMemory), say(fromMemory))

	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 1)); err == nil {
		t.Fatal("attempt 1 had no answer but was acked: nothing would deliver it again")
	}
	row := a.run(id)
	if row.Status != "running" || row.Ref != nil || len(row.Result) != 0 {
		t.Errorf("run after attempt 1 = %+v, want running with no REF and no answer", row)
	}
	calls := a.recordedCalls(id)
	if len(calls) != 1 || !calls[0].Forced || calls[0].Error == "" || calls[0].Result != "" || strings.Contains(calls[0].Error, "provider") {
		t.Errorf("tool_calls = %+v, want the failed forced search, with an error that does not repeat the provider's", calls)
	}
	if !strings.Contains(logs.String(), "provider is down") {
		t.Errorf("log = %q, want the real error logged where an operator reads it", logs.String())
	}

	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 2)); err != nil {
		t.Fatalf("attempt 2: %v", err)
	}
	if slip := a.answerOf(svc, id); slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER || len(slip.GetCitations()) == 0 {
		t.Errorf("slip = %v, want the answer of the retry's search", slip)
	}
	if calls := a.recordedCalls(id); len(calls) != 2 || !calls[1].Forced || calls[1].Error != "" {
		t.Errorf("tool_calls = %+v, want the failed search and then the one that answered", calls)
	}
	unseen(t, fromMemory, map[string]string{"the stored result": string(a.run(id).Result)})
}

func TestMention_PlainReply_StillAPlainReply(t *testing.T) {
	a := newAgentFixture(t)
	a.seedWarehouse()
	const reply = "I think about 4,000 pallets, but I did not look it up."
	a.model.script(say(reply))
	trigger := a.mention(a.public, a.alice, "@assistant how many pallets does the warehouse hold?")
	runID := uuid.New()
	embeds := len(a.llm.EmbedCalls()) // the meeting was embedded when it was indexed

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}

	replies := a.replies(a.public, a.alice)
	if len(replies) != 1 || replies[0].GetBody() != reply || replies[0].GetArtifact() != nil {
		t.Fatalf("replies = %v, want the model's own words as a plain message: a mention is not an Ask", replies)
	}
	if replies[0].GetAuthorId() != authz.AgentID("assistant").String() {
		t.Errorf("author_id = %s, want the agent's", replies[0].GetAuthorId())
	}
	run := a.run(runID)
	if run.Status != "done" || run.Ref != nil || len(run.Result) != 0 || string(run.ToolCalls) != "[]" {
		t.Errorf("run = %+v, want done with no REF, no slip and no tool calls: no search is forced in a channel", run)
	}
	if n := len(a.llm.EmbedCalls()) - embeds; n != 0 || len(a.llm.GenerateCalls()) != 0 {
		t.Errorf("the knowledge domain was searched (%d embeds, %d generations), want no search in a channel", n, len(a.llm.GenerateCalls()))
	}
	if a.ansCounters() != 0 {
		t.Errorf("a plain reply took a REF")
	}
}
