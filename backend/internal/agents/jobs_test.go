package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/genai"

	agentsv1 "kiwi/gen/kiwi/agents/v1"
	chatv1 "kiwi/gen/kiwi/chat/v1"
	realtimev1 "kiwi/gen/kiwi/realtime/v1"
	"kiwi/internal/chat"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
)

// ---- helpers ---------------------------------------------------------------------

// runtimeFunc is a Runtime that is a function.
type runtimeFunc func(ctx context.Context, spec Spec, input string, sess Session, emit func(string)) (Final, error)

func (f runtimeFunc) Run(ctx context.Context, spec Spec, input string, sess Session, emit func(string)) (Final, error) {
	return f(ctx, spec, input, sess, emit)
}

// testChat is the chat service with a hand on the post of a reply: it can fail it and it
// can stop it where it is.
type testChat struct {
	chat.Service
	failPost atomic.Bool
	// beforePost, when set, runs at the start of every post of a message in a transaction.
	beforePost func()
}

func (c *testChat) PostMessageTx(ctx context.Context, tx pgx.Tx, a authz.Actor, req *chatv1.SendMessageRequest) (*chatv1.Message, error) {
	if c.beforePost != nil {
		c.beforePost()
	}
	if c.failPost.Load() {
		return nil, errors.New("chat is down")
	}
	return c.Service.PostMessageTx(ctx, tx, a, req)
}

// toolCalls are the calls recorded on a run.
func (a *agentFixture) toolCalls(run uuid.UUID) []ToolCall {
	a.t.Helper()
	return callsOf(a.t, a.run(run).ToolCalls)
}

func callsOf(t *testing.T, raw []byte) []ToolCall {
	t.Helper()
	var calls []ToolCall
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &calls); err != nil {
			t.Fatalf("tool_calls %s: %v", raw, err)
		}
	}
	return calls
}

// ansCounters is how many ANS counters exist: one appears with the first REF of the
// kind and never goes away, so none means no REF was ever issued.
func (a *agentFixture) ansCounters() int {
	return a.count(`SELECT count(*) FROM platform.ref_counters WHERE kind = 'ANS'`)
}

// requestParts is every part of every content of the model request at index i.
func (a *agentFixture) requestParts(i int) []*genai.Part {
	a.t.Helper()
	reqs := a.model.requests()
	if i < 0 {
		i += len(reqs)
	}
	var parts []*genai.Part
	for _, c := range reqs[i].Contents {
		parts = append(parts, c.Parts...)
	}
	return parts
}

// ---- time and steps (I1a, I1b) ---------------------------------------------------

func TestInvoke_ModelThatNeverStopsCallingTools_FailsAtOnceWithTooManySteps(t *testing.T) {
	a := newAgentFixture(t)
	logs := captureLogs(t)
	w := a.watch("ch:" + a.public.String())
	loop := callTool("list_tasks", map[string]any{"channel_id": a.public.String()})
	loop.forever = true
	a.model.script(loop)
	trigger := a.mention(a.public, a.alice, "@assistant keep looking")
	runID := uuid.New()

	// One delivery: the run is settled, not nacked for the bus to try again.
	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v, want the failure recorded and the message acked", err)
	}
	run := a.run(runID)
	if run.Status != "failed" || run.FailureReason == nil || *run.FailureReason != "too_many_steps" {
		t.Fatalf("run = %+v, want failed with too_many_steps", run)
	}
	if n := len(a.model.requests()); n != defaultMaxTurns {
		t.Errorf("model calls = %d, want %d: the run is stopped at its last turn, not after it", n, defaultMaxTurns)
	}
	if n := len(a.toolCalls(runID)); n != defaultMaxTurns-1 {
		t.Errorf("tool calls recorded = %d, want %d: what the last turn asked for is not run", n, defaultMaxTurns-1)
	}
	if run.Ref != nil || a.ansCounters() != 0 || len(a.replies(a.public, a.alice)) != 0 {
		t.Errorf("a stopped run left a REF or a reply: %+v", run)
	}
	w.waitFor("the FAILED frame", hasState(realtimev1.AgentRunState_AGENT_RUN_STATE_FAILED))
	if !strings.Contains(logs.String(), "too_many_steps") {
		t.Errorf("log = %q, want the stop logged with its reason", logs.String())
	}
}

func TestInvoke_MaxTurnsComesFromDeps(t *testing.T) {
	a := newAgentFixture(t)
	captureLogs(t)
	a.deps.MaxTurns = 3
	loop := callTool("list_tasks", map[string]any{"channel_id": a.public.String()})
	loop.forever = true
	a.model.script(loop)
	trigger := a.mention(a.public, a.alice, "@assistant keep looking")
	runID := uuid.New()

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatal(err)
	}
	if n := len(a.model.requests()); n != 3 {
		t.Errorf("model calls = %d, want the 3 of Deps.MaxTurns", n)
	}
	if run := a.run(runID); run.Status != "failed" || *run.FailureReason != "too_many_steps" {
		t.Errorf("run = %+v", run)
	}
}

func TestInvoke_ModelThatNeverAnswers_FailsAtOnceWithTimeout(t *testing.T) {
	a := newAgentFixture(t)
	logs := captureLogs(t)
	a.deps.RunTimeout = 1500 * time.Millisecond // long enough that loading the session is never what runs out
	a.model.script(step{block: true})
	trigger := a.mention(a.public, a.alice, "@assistant hello?")
	runID := uuid.New()
	w := a.watch("ch:" + a.public.String())

	// One delivery: the deadline is permanent, so there is no second attempt to wait for.
	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v, want the failure recorded and the message acked", err)
	}
	run := a.run(runID)
	if run.Status != "failed" || run.FailureReason == nil || *run.FailureReason != "timeout" {
		t.Fatalf("run = %+v, want failed with timeout", run)
	}
	if n := len(a.model.requests()); n != 1 {
		t.Errorf("model calls = %d, want 1: the run is not tried again", n)
	}
	if n := len(a.replies(a.public, a.alice)); n != 0 || a.ansCounters() != 0 {
		t.Errorf("a timed-out run left a reply or a REF")
	}
	w.waitFor("the FAILED frame", hasState(realtimev1.AgentRunState_AGENT_RUN_STATE_FAILED))
	if !strings.Contains(logs.String(), "timeout") {
		t.Errorf("log = %q, want the stop logged with its reason", logs.String())
	}
}

// ---- one budget for every attempt of a run (I1c) ---------------------------------

func TestInvoke_RetryAfterATransientFailure_SharesTheBudgetAndRepeatsNoDraft(t *testing.T) {
	a := newAgentFixture(t)
	captureLogs(t)
	trigger := a.mention(a.public, a.alice, "@assistant draft a task for each of these five things")
	runID := uuid.New()
	ev := a.invokeOf(trigger, runID)
	titles := []string{"Book the dock", "Call the carrier", "Order pallets", "Print labels", "Email the buyer"}
	draft := func(title string) step {
		return callTool("create_task_draft", map[string]any{"channel_id": a.public.String(), "title": title})
	}

	// The first attempt makes the five drafts the run may make, then the provider fails.
	for _, title := range titles {
		a.model.script(draft(title))
	}
	a.model.script(step{err: errors.New("provider is down")})
	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 1)); err == nil {
		t.Fatal("attempt 1 failed but was acked: nothing would deliver it again")
	}
	if n := a.count(`SELECT count(*) FROM tasks.tasks WHERE state = 'draft'`); n != 5 {
		t.Fatalf("drafts after attempt 1 = %d, want 5", n)
	}
	if got := a.toolCalls(runID); len(got) != 5 {
		t.Fatalf("tool calls recorded after a failed attempt = %d, want the 5 it made: they are how the next attempt knows its budget", len(got))
	}

	// The second attempt proposes one of them again, and a sixth.
	a.model.script(draft(titles[0]), draft("Pay the invoice"), say("Done; five drafts wait for confirmation."))
	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 2)); err != nil {
		t.Fatalf("attempt 2: %v", err)
	}
	if n := a.count(`SELECT count(*) FROM tasks.tasks WHERE state = 'draft'`); n != 5 {
		t.Errorf("drafts after attempt 2 = %d, want still 5: a retry neither repeats a draft nor gets a budget of its own", n)
	}
	calls := a.toolCalls(runID)
	if len(calls) != 7 {
		t.Fatalf("tool calls recorded = %d, want 7 (5 + 2 of the retry)", len(calls))
	}
	first, repeat, sixth := calls[0], calls[5], calls[6]
	if repeat.Error != "" || repeat.Result == "" || !strings.Contains(repeat.Result, `"title":"`+titles[0]+`"`) {
		t.Errorf("the repeated draft = %+v, want the draft that exists handed back", repeat)
	}
	refOf := func(c ToolCall) string {
		var res struct {
			Ref string `json:"ref"`
		}
		if err := json.Unmarshal([]byte(c.Result), &res); err != nil {
			t.Fatalf("result %q: %v", c.Result, err)
		}
		return res.Ref
	}
	if refOf(first) == "" || refOf(first) != refOf(repeat) {
		t.Errorf("repeat ref %q != first ref %q, want the same draft", refOf(repeat), refOf(first))
	}
	if !strings.Contains(sixth.Error, "budget") {
		t.Errorf("the sixth draft = %+v, want it refused for the run's budget", sixth)
	}
	if run := a.run(runID); run.Status != "done" {
		t.Errorf("run = %+v, want done", run)
	}
	// The retry's session starts clean: the turn the failed attempt left open is not
	// part of the conversation.
	for _, p := range a.requestParts(6) {
		if p.FunctionCall != nil || p.FunctionResponse != nil {
			t.Errorf("the retry's prompt holds a call of the failed attempt: %+v", p)
		}
	}
}

func TestTools_PriorCarriesTheBudgetAndHandsBackDrafts(t *testing.T) {
	f := newFixture(t)
	msg := f.say(f.public, f.alice, "@assistant tasks please")
	trigger := triggerOf(msg, "Alice")
	ctx := f.as(f.agentFor(f.alice))
	newTools := func(p Prior) (draft func(title string) (map[string]any, error), list func() error) {
		tools := NewTools(ToolDeps{Knowledge: f.kb, Tasks: f.tasks, Chat: f.chat, LLM: f.llm, Collector: &Collector{}, Trigger: trigger, Prior: p})
		draft = func(title string) (map[string]any, error) {
			return f.call(ctx, tools, "create_task_draft", map[string]any{"channel_id": f.public.String(), "title": title})
		}
		list = func() error {
			_, err := f.call(ctx, tools, "list_tasks", map[string]any{"channel_id": f.public.String()})
			return err
		}
		return draft, list
	}

	// Calls: 19 were spent by earlier attempts, so one more is allowed and no more.
	_, list := newTools(Prior{Calls: maxToolCallsPerRun - 1})
	if err := list(); err != nil {
		t.Fatalf("the last call of the budget: %v", err)
	}
	wantErr(t, list(), errs.ErrInvalid, "the call beyond the budget")

	// Drafts: five were made, so a new one is refused, and one of them is handed back.
	prior := Prior{Calls: 5}
	for i := range maxDraftsPerRun {
		prior.Drafts = append(prior.Drafts, PriorDraft{ChannelID: f.public.String(), Title: fmt.Sprintf("Made before %d", i), Ref: fmt.Sprintf("TSK-%d", 1001+i), State: "DRAFT"})
	}
	draft, _ := newTools(prior)
	_, err := draft("A sixth")
	wantErr(t, err, errs.ErrInvalid, "a draft beyond the run's drafts")
	got, err := draft("Made before 2")
	if err != nil || got["ref"] != "TSK-1003" || got["state"] != "DRAFT" {
		t.Fatalf("a draft an earlier attempt made = %v, %v, want it handed back", got, err)
	}
	if n := f.count(`SELECT count(*) FROM tasks.tasks`); n != 0 {
		t.Errorf("tasks = %d, want none: nothing was created", n)
	}

	// A prior draft that matches no call (its result was cut when recorded) still spends the budget.
	prior = Prior{Drafts: make([]PriorDraft, maxDraftsPerRun)}
	draft, _ = newTools(prior)
	_, err = draft("Anything")
	wantErr(t, err, errs.ErrInvalid, "a draft after five unmatched ones")

	// And within one attempt a model that asks twice gets one draft.
	draft, _ = newTools(Prior{})
	one, err := draft("Book the dock")
	if err != nil {
		t.Fatal(err)
	}
	two, err := draft("Book the dock")
	if err != nil || one["ref"] != two["ref"] {
		t.Errorf("the same draft twice = %v and %v (%v), want one draft", one, two, err)
	}
	if n := f.count(`SELECT count(*) FROM tasks.tasks`); n != 1 {
		t.Errorf("tasks = %d, want 1", n)
	}
}

// ---- the identity of a run (I2) --------------------------------------------------

func TestInvoke_EventNamingAnotherMemberAsTheInvoker_FailsWithoutAModelCallOrAPost(t *testing.T) {
	a := newAgentFixture(t)
	captureLogs(t)
	// Alice wrote the mention; the event claims Bob, who belongs to the channel, asked.
	trigger := a.mention(a.public, a.alice, "@assistant what did the team decide?")
	runID := uuid.New()
	ev := a.invokeOf(trigger, runID)
	ev.OnBehalfOf = a.bob.String()

	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v, want the failure recorded and the message acked", err)
	}
	run := a.run(runID)
	if run.Status != "failed" || run.FailureReason == nil || *run.FailureReason != "no_access" {
		t.Errorf("run = %+v, want failed with no_access", run)
	}
	if n := len(a.model.requests()); n != 0 {
		t.Errorf("model calls = %d, want none: a message is answered only for its author", n)
	}
	if n := a.count(`SELECT count(*) FROM chat.messages WHERE author_kind = 'agent'`); n != 0 {
		t.Errorf("agent messages = %d, want none", n)
	}
	if n := a.count(`SELECT count(*) FROM agents.sessions`); n != 0 {
		t.Errorf("sessions = %d, want none", n)
	}
}

func TestInvoke_AskRunDeliveredWithAnEventNamingAnotherUser_RunsAsItsOwner(t *testing.T) {
	a := newAgentFixture(t)
	a.llm.HandleGenerate(citeEveryChunk)
	a.indexedMeeting(a.public, a.bob, "Warehouse review", warehouseFact)
	a.indexedMeeting(a.private, a.bob, "Pay review", "The senior engineer salary band is 90,000 baht.")
	a.model.script(
		callTool("search_knowledge", map[string]any{"question": "What are the facts the team keeps?"}),
		say("Here is what I found [1]."),
	)
	svc := New(a.deps)
	run, ev := a.startAsk(svc, "What are the facts the team keeps?")
	runID := uuid.MustParse(run.GetId())
	// A forged event: another user, another agent, and Bob's private channel.
	ev.OnBehalfOf, ev.Agent, ev.ChannelId = a.bob.String(), "nobody", a.private.String()
	wAlice, wBob := a.watch("u:"+a.alice.String()), a.watch("u:"+a.bob.String())

	if err := a.job()(t.Context(), a.delivery(ev, "evt-ask", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}

	got, err := svc.GetRun(t.Context(), a.person(a.alice), runID)
	if err != nil || got.GetStatus() != agentsv1.RunStatus_RUN_STATUS_DONE || got.GetAnswer().GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER {
		t.Fatalf("GetRun as alice = %v, %v, want her answer", got, err)
	}
	if !slices.Equal(got.GetAnswer().GetScopeChannelIds(), idStrings(a.public)) {
		t.Errorf("channels searched = %v, want only alice's (%v): the run is hers, whatever the event says", got.GetAnswer().GetScopeChannelIds(), a.public)
	}
	for _, c := range a.llm.GenerateCalls() {
		if strings.Contains(promptOf(c), "90,000") {
			t.Errorf("the answer model was shown Bob's private chunk:\n%s", promptOf(c))
		}
	}
	row := a.run(runID)
	if row.OnBehalfOf != a.alice || row.Agent != "assistant" || row.ChannelID != nil {
		t.Errorf("run = %+v, want alice's run of the assistant with no channel, as it was started", row)
	}
	if _, err := svc.GetRun(t.Context(), a.person(a.bob), runID); !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("GetRun as bob = %v, want ErrForbidden", err)
	}
	wAlice.waitFor("the DONE frame on alice's topic", hasState(realtimev1.AgentRunState_AGENT_RUN_STATE_DONE))
	if frames := wBob.snapshot(); len(frames) != 0 {
		t.Errorf("frames on bob's topic = %v, want none: the run is private to alice", frames)
	}
}

// ---- no REF without a reply (I3) -------------------------------------------------

func TestInvoke_ReplyFailsAfterTheSlipIsBuilt_TakesNoRefAndSettlesTheRunAtTheLastAttempt(t *testing.T) {
	a := newAgentFixture(t)
	captureLogs(t)
	post := &testChat{Service: a.chat}
	post.failPost.Store(true)
	a.deps.Chat = post
	a.seedWarehouse()
	search := func() []step {
		return []step{callTool("search_knowledge", map[string]any{"question": "pallets?"}), say("The warehouse holds 4,000 pallets [1].")}
	}
	trigger := a.mention(a.public, a.alice, "@assistant pallets?")
	runID := uuid.New()
	ev := a.invokeOf(trigger, runID)
	w := a.watch("ch:" + a.public.String())

	// Attempt 4: the run reaches its slip and the post fails, so the delivery is nacked
	// and the transaction, which had taken ANS-1001, is rolled back with everything.
	a.model.script(search()...)
	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", defaultFinalAttempt-1)); err == nil {
		t.Fatal("attempt 4 failed to post but was acked: nothing would deliver it again")
	}
	if n := a.ansCounters(); n != 0 {
		t.Errorf("ANS counters = %d after a rolled-back reply, want none: the number was never given out", n)
	}
	if n := len(a.replies(a.public, a.alice)); n != 0 {
		t.Errorf("agent replies = %d, want none", n)
	}
	if run := a.run(runID); run.Status != "running" || run.Ref != nil || len(run.Result) != 0 {
		t.Errorf("run after attempt 4 = %+v, want running, with no REF and no answer, for the next delivery", run)
	}
	if n := a.count(`SELECT count(*) FROM platform.processed_events WHERE consumer = 'agent.run'`); n != 0 {
		t.Errorf("processed_events = %d, want none: the redelivery starts over", n)
	}

	// Attempt 5: the same again, and now the run is settled instead of left running.
	a.model.script(search()...)
	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", defaultFinalAttempt)); err != nil {
		t.Fatalf("attempt 5: %v, want the failure recorded and the message acked", err)
	}
	run := a.run(runID)
	if run.Status != "failed" || run.Ref != nil || len(run.Result) != 0 || run.FailureReason == nil || *run.FailureReason != "model_unavailable" {
		t.Errorf("run = %+v, want failed (model_unavailable) with no REF and no answer", run)
	}
	if n := a.ansCounters(); n != 0 {
		t.Errorf("ANS counters = %d after a failed run, want none: a failed run issues no REF", n)
	}
	w.waitFor("the FAILED frame", hasState(realtimev1.AgentRunState_AGENT_RUN_STATE_FAILED))

	// The next answer is ANS-1001: the failed run took no number, so there is no gap.
	post.failPost.Store(false)
	a.model.script(search()...)
	again := a.mention(a.public, a.alice, "@assistant pallets, once more?")
	if err := a.job()(t.Context(), a.delivery(a.invokeOf(again, uuid.New()), "evt-2", 1)); err != nil {
		t.Fatalf("the next run: %v", err)
	}
	replies := a.replies(a.public, a.alice)
	if len(replies) != 1 || replies[0].GetArtifact().GetRef() != "ANS-1001" {
		t.Errorf("replies = %v, want one with ANS-1001", replies)
	}
}

// ---- overlapping deliveries of one run (M2) --------------------------------------

func TestInvoke_OverlappingDeliveriesOfOneRun_SecondTakesNoRefAndChangesNothing(t *testing.T) {
	a := newAgentFixture(t)
	a.seedWarehouse()

	var runs atomic.Int32
	secondRan := make(chan struct{})
	a.deps.Runtime = runtimeFunc(func(ctx context.Context, _ Spec, _ string, sess Session, _ func(string)) (Final, error) {
		// Both deliveries find the pallets: each builds a slip.
		if _, err := a.call(ctx, sess.Tools, "search_knowledge", map[string]any{"question": "pallets?"}); err != nil {
			return Final{}, err
		}
		n := runs.Add(1)
		if n == 2 {
			close(secondRan)
		}
		return Final{Text: fmt.Sprintf("answer of delivery %d", n)}, nil
	})
	// The first delivery is held inside its transaction, after it took ANS-1001, until the
	// second has run to its own result.
	inApply, proceed := make(chan struct{}), make(chan struct{})
	var first sync.Once
	hold := &testChat{Service: a.chat}
	hold.beforePost = func() {
		first.Do(func() {
			close(inApply)
			<-proceed
		})
	}
	a.deps.Chat = hold

	trigger := a.mention(a.public, a.alice, "@assistant pallets?")
	runID := uuid.New()
	ev := a.invokeOf(trigger, runID)

	results := make(chan error, 2)
	go func() { results <- a.job()(t.Context(), a.delivery(ev, "evt-1", 1)) }()
	<-inApply
	go func() { results <- a.job()(t.Context(), a.delivery(ev, "evt-2", 1)) }()
	<-secondRan
	close(proceed)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("a delivery failed: %v, want both acked (the late one is simply over)", err)
		}
	}

	run := a.run(runID)
	if run.Status != "done" || run.Ref == nil || *run.Ref != "ANS-1001" {
		t.Errorf("run = %+v, want done with ANS-1001, as the first delivery left it", run)
	}
	if n := a.count(`SELECT last FROM platform.ref_counters WHERE kind = 'ANS'`); n != 1001 {
		t.Errorf("ANS counter = %d, want 1001: the late delivery's number was rolled back, not burned", n)
	}
	replies := a.replies(a.public, a.alice)
	if len(replies) != 1 || replies[0].GetArtifact().GetRef() != "ANS-1001" || replies[0].GetBody() != "answer of delivery 1" {
		t.Errorf("replies = %v, want the one reply of the first delivery", replies)
	}
}

// ---- the model's answer ends at the token limit (M1) -----------------------------

func TestInvoke_AnswerCutAtTheTokenLimit_IsRetriedOnceThenFailed(t *testing.T) {
	t.Run("a second attempt that answers is posted", func(t *testing.T) {
		a := newAgentFixture(t)
		captureLogs(t)
		trigger := a.mention(a.public, a.alice, "@assistant tell me a long story")
		runID := uuid.New()
		ev := a.invokeOf(trigger, runID)

		a.model.script(step{text: "Once upon a", truncated: true})
		if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 1)); err == nil {
			t.Fatal("a cut-off answer was acked: it would have been posted as if it were whole")
		}
		if n := len(a.replies(a.public, a.alice)); n != 0 {
			t.Fatalf("replies = %d after a truncated answer, want none", n)
		}
		a.model.script(say("A short story."))
		if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 2)); err != nil {
			t.Fatalf("attempt 2: %v", err)
		}
		if replies := a.replies(a.public, a.alice); len(replies) != 1 || replies[0].GetBody() != "A short story." {
			t.Errorf("replies = %v, want the whole answer of the second attempt", replies)
		}
	})
	t.Run("a second cut-off settles the run", func(t *testing.T) {
		a := newAgentFixture(t)
		logs := captureLogs(t)
		trigger := a.mention(a.public, a.alice, "@assistant tell me a long story")
		runID := uuid.New()
		ev := a.invokeOf(trigger, runID)

		a.model.script(step{text: "Once upon a", truncated: true}, step{text: "It was a dark", truncated: true})
		if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 1)); err == nil {
			t.Fatal("attempt 1 was acked")
		}
		if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 2)); err != nil {
			t.Fatalf("attempt 2: %v, want the failure recorded and the message acked: a third try would be cut the same way", err)
		}
		run := a.run(runID)
		if run.Status != "failed" || run.FailureReason == nil || *run.FailureReason != "truncated" {
			t.Errorf("run = %+v, want failed with truncated", run)
		}
		if n := len(a.replies(a.public, a.alice)); n != 0 || a.ansCounters() != 0 {
			t.Errorf("a failed run left a reply or a REF")
		}
		if !strings.Contains(logs.String(), "truncated") {
			t.Errorf("log = %q, want the reason", logs.String())
		}
	})
}

// ---- progress (M3) ---------------------------------------------------------------

func TestInvoke_ProgressSaysRunningAfterEveryToolCall(t *testing.T) {
	a := newAgentFixture(t)
	a.model.script(
		callTool("list_tasks", map[string]any{"channel_id": a.public.String()}),
		callTool("list_tasks", map[string]any{"channel_id": a.public.String()}),
		say("The board is empty."),
	)
	trigger := a.mention(a.public, a.alice, "@assistant what is on the board?")
	runID := uuid.New()
	w := a.watch("ch:" + a.public.String())

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}
	frames := w.waitFor("the DONE frame", hasState(realtimev1.AgentRunState_AGENT_RUN_STATE_DONE))
	var states []string
	for _, p := range frames {
		s := strings.TrimPrefix(p.GetState().String(), "AGENT_RUN_STATE_")
		if len(states) == 0 || states[len(states)-1] != s {
			states = append(states, s)
		}
	}
	// A repeated state collapses: the three RUNNING frames (the start and one per tool call)
	// are consecutive, so the whole stream is RUNNING, then the answer's text, then DONE.
	if want := []string{"RUNNING", "STREAMING", "DONE"}; !slices.Equal(states, want) {
		t.Errorf("states = %v, want %v", states, want)
	}
	running := 0
	for _, p := range frames {
		if p.GetState() == realtimev1.AgentRunState_AGENT_RUN_STATE_RUNNING {
			running++
		}
	}
	if running != 3 {
		t.Errorf("RUNNING frames = %d, want 3: the start, and one after each of the 2 tool calls", running)
	}
}

// ---- finding the message of a mention (M4) ---------------------------------------

// pile adds n filler messages to channel after its latest one, straight into the table: a
// busy channel without n posts through chat.
func (a *agentFixture) pile(channel uuid.UUID, n int) {
	a.t.Helper()
	var last int64
	a.scan(&last, `SELECT last_seq FROM chat.channels WHERE id = $1`, channel)
	a.exec(`
		INSERT INTO chat.messages (id, workspace_id, channel_id, seq, author_kind, author_id, body, client_msg_id)
		SELECT gen_random_uuid(), $1::uuid, $2::uuid, g, 'user', $3::uuid, 'filler ' || g, 'filler-' || g
		FROM generate_series($4::bigint, $5::bigint) g`,
		a.ws, channel, a.bob, last+1, last+int64(n))
	a.exec(`UPDATE chat.channels SET last_seq = $2 WHERE id = $1`, channel, last+int64(n))
}

func TestInvoke_MentionFurtherBackThanOnePage_IsFound(t *testing.T) {
	a := newAgentFixture(t)
	a.model.script(say("Seen."))
	trigger := a.mention(a.public, a.alice, "@assistant are you there?")
	a.pile(a.public, 2*triggerPage+50) // the mention is on the third page, from the newest
	runID := uuid.New()

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}
	if run := a.run(runID); run.Status != "done" || run.Question != "@assistant are you there?" {
		t.Errorf("run = %+v, want done, with the mention as its question", run)
	}
}

func TestInvoke_MentionOlderThanTheLatestMessages_FailsWithItsOwnReason(t *testing.T) {
	a := newAgentFixture(t)
	captureLogs(t)
	trigger := a.mention(a.public, a.alice, "@assistant are you there?")
	a.pile(a.public, triggerPages*triggerPage+10) // past the last page that is read
	runID := uuid.New()

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v, want the failure recorded and the message acked", err)
	}
	run := a.run(runID)
	if run.Status != "failed" || run.FailureReason == nil || *run.FailureReason != "trigger_too_old" {
		t.Errorf("run = %+v, want failed with trigger_too_old, not a claim that the person lost access", run)
	}
	if n := len(a.model.requests()); n != 0 {
		t.Errorf("model calls = %d, want none", n)
	}
}

// ---- the question is bounded (M5) ------------------------------------------------

func TestInvoke_LongMention_IsCutToTheQuestionLimit(t *testing.T) {
	a := newAgentFixture(t)
	var input string
	a.deps.Runtime = runtimeFunc(func(_ context.Context, _ Spec, in string, _ Session, _ func(string)) (Final, error) {
		input = in
		return Final{Text: "ok"}, nil
	})
	body := "@assistant " + strings.Repeat("é", maxBodyRunes-1000)
	trigger := a.mention(a.public, a.alice, body)
	runID := uuid.New()

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}
	run := a.run(runID)
	if n := utf8.RuneCountInString(run.Question); n != maxQuestionRunes || !strings.HasSuffix(run.Question, "…") {
		t.Errorf("question = %d runes, want %d with the cut marked", n, maxQuestionRunes)
	}
	if !strings.HasSuffix(input, run.Question) {
		t.Errorf("the model was not given the stored question")
	}
	if run.Status != "done" {
		t.Errorf("run = %+v", run)
	}
}

func TestInvoke_FinalAttemptComesFromDeps(t *testing.T) {
	a := newAgentFixture(t)
	captureLogs(t)
	a.deps.FinalAttempt = 3
	a.deps.Runtime = runtimeFunc(func(context.Context, Spec, string, Session, func(string)) (Final, error) {
		return Final{}, errors.New("provider is down")
	})
	trigger := a.mention(a.public, a.alice, "@assistant hi")
	runID := uuid.New()
	ev := a.invokeOf(trigger, runID)

	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 2)); err == nil {
		t.Fatal("attempt 2 of 3 was acked")
	}
	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 3)); err != nil {
		t.Fatalf("attempt 3: %v, want the failure recorded and the message acked", err)
	}
	if run := a.run(runID); run.Status != "failed" {
		t.Errorf("run = %+v, want failed at the final attempt Deps names", run)
	}
}

// TestJobs_FinalAttemptIsTheLastDeliveryOfTheBus ties the default to the bus: a run
// whose model always fails is settled by the handler on the delivery before the bus gives
// up, so nothing is dead-lettered with a run left running.
func TestJobs_FinalAttemptIsTheLastDeliveryOfTheBus(t *testing.T) {
	a := newAgentFixture(t)
	captureLogs(t)
	var calls atomic.Int32
	a.deps.Runtime = runtimeFunc(func(context.Context, Spec, string, Session, func(string)) (Final, error) {
		calls.Add(1)
		return Final{}, errors.New("provider is down")
	})
	var dead atomic.Int32
	mem := bus.NewMemory(bus.MemoryOptions{Backoff: []time.Duration{0}, OnDead: func(string, bus.Message, error) { dead.Add(1) }})
	for _, j := range Jobs(a.deps) {
		mem.Register(j.Consumer, j.Topics, j.Handler)
	}
	trigger := a.mention(a.public, a.alice, "@assistant hi")
	runID := uuid.New()
	ev := a.invokeOf(trigger, runID)
	msg := a.delivery(ev, "evt-1", 0)
	msg.Topic = events.TopicAgentInvoke

	if err := mem.Publish(t.Context(), msg); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := calls.Load(); got != defaultFinalAttempt {
		t.Errorf("runtime calls = %d, want %d: one per delivery the bus makes by default", got, defaultFinalAttempt)
	}
	if got := dead.Load(); got != 0 {
		t.Errorf("dead letters = %d, want none: the last delivery settles the run", got)
	}
	if run := a.run(runID); run.Status != "failed" {
		t.Errorf("run = %+v, want failed", run)
	}
}
