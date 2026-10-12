package agents

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	"kiwi/internal/platform/bus"
)

// ---- events ------------------------------------------------------------------------

// callEvent is the agent's call of a tool, as ADK appends it before the tool runs.
func callEvent(t *testing.T, id, name string) *session.Event {
	t.Helper()
	ev := session.NewEvent(t.Context(), "inv")
	ev.Author = "assistant"
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: map[string]any{}}}}}
	return ev
}

// responseEvent is a tool's answer; payload is the size of what it answers.
func responseEvent(t *testing.T, id, name string, payload int) *session.Event {
	t.Helper()
	ev := session.NewEvent(t.Context(), "inv")
	ev.Author = "assistant"
	ev.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
		ID: id, Name: name, Response: map[string]any{"data": strings.Repeat("x", payload)},
	}}}}
	return ev
}

// kinds describes events as user, call, resp and text, in order.
func kinds(evs session.Events) []string {
	var out []string
	for ev := range evs.All() {
		switch {
		case ev.Author == "user":
			out = append(out, "user")
		case ev.Content != nil && slices.ContainsFunc(ev.Content.Parts, func(p *genai.Part) bool { return p.FunctionCall != nil }):
			out = append(out, "call")
		case ev.Content != nil && slices.ContainsFunc(ev.Content.Parts, func(p *genai.Part) bool { return p.FunctionResponse != nil }):
			out = append(out, "resp")
		default:
			out = append(out, "text")
		}
	}
	return out
}

// texts is the text parts of events, in order.
func texts(evs session.Events) []string {
	var out []string
	for ev := range evs.All() {
		if ev.Content != nil {
			if text := textOf(ev.Content); text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

// seeded is a session service over a fresh database with a session of alice's, and a way
// to append to it.
func seeded(t *testing.T, id string) (svc session.Service, f *fixture, add func(evs ...*session.Event)) {
	t.Helper()
	f = newFixture(t)
	svc = newSessionService(f.db, f.ws)
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: testApp, UserID: f.alice.String(), SessionID: id})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return svc, f, func(evs ...*session.Event) {
		t.Helper()
		for _, ev := range evs {
			if err := svc.AppendEvent(t.Context(), created.Session, ev); err != nil {
				t.Fatalf("AppendEvent: %v", err)
			}
		}
	}
}

func history(t *testing.T, svc session.Service, f *fixture, id string) session.Events {
	t.Helper()
	got, err := svc.Get(t.Context(), &session.GetRequest{AppName: testApp, UserID: f.alice.String(), SessionID: id})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return got.Session.Events()
}

// ---- what a run may read of the past (I4b, M5) -----------------------------------

func TestSessionService_HistoryLeavesOutTurnsThatNeverFinished(t *testing.T) {
	svc, f, add := seeded(t, "turns")
	add(turn(t, "user", "q1"), callEvent(t, "c1", "list_tasks"), responseEvent(t, "c1", "list_tasks", 1), turn(t, "assistant", "a1"))
	add(turn(t, "user", "q2 dangling call"), callEvent(t, "c2", "list_tasks")) // a run stopped before its tool answered
	add(turn(t, "user", "q3 no answer"), callEvent(t, "c3", "list_tasks"), responseEvent(t, "c3", "list_tasks", 1))
	add(turn(t, "user", "q4 alone"))
	add(turn(t, "user", "q5 orphan response"), responseEvent(t, "nobody-called", "list_tasks", 1), turn(t, "assistant", "a5"))
	add(turn(t, "user", "q6"), turn(t, "assistant", "a6"))

	evs := history(t, svc, f, "turns")
	if got, want := texts(evs), []string{"q1", "a1", "q6", "a6"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want only the turns that finished: %v", got, want)
	}
	if got, want := kinds(evs), []string{"user", "call", "resp", "text", "user", "text"}; !slices.Equal(got, want) {
		t.Errorf("history kinds = %v, want %v: every call of a tool is followed by its response", got, want)
	}
	// What is stored is untouched: the history is a view of it.
	if n := f.count(`SELECT count(*) FROM agents.session_events WHERE session_id = 'turns'`); n != 15 {
		t.Errorf("stored events = %d, want all 15", n)
	}
	// A request that names how many events it wants gets them as they are.
	got, err := svc.Get(t.Context(), &session.GetRequest{AppName: testApp, UserID: f.alice.String(), SessionID: "turns", NumRecentEvents: 3})
	if err != nil || got.Session.Events().Len() != 3 {
		t.Errorf("Get with NumRecentEvents 3 = %v, %v, want exactly 3 events", got, err)
	}
}

func TestSessionService_HistoryIsCappedBySizeNotOnlyByEventCount(t *testing.T) {
	// Five turns whose tool results weigh 18 KiB each: only the newest three fit in 64 KiB.
	svc, f, add := seeded(t, "heavy")
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("c%d", i)
		add(turn(t, "user", fmt.Sprintf("q%d", i)), callEvent(t, id, "list_tasks"), responseEvent(t, id, "list_tasks", 18<<10), turn(t, "assistant", fmt.Sprintf("a%d", i)))
	}
	if got, want := texts(history(t, svc, f, "heavy")), []string{"q3", "a3", "q4", "a4", "q5", "a5"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want the newest three turns %v: the older ones do not fit in %d bytes", got, want, maxHistoryBytes)
	}

	// An old turn that is too heavy goes, and the light ones after it stay.
	svc, f, add = seeded(t, "old")
	add(turn(t, "user", "q0"), callEvent(t, "c0", "list_tasks"), responseEvent(t, "c0", "list_tasks", 100<<10), turn(t, "assistant", "a0"))
	add(turn(t, "user", "q1"), turn(t, "assistant", "a1"))
	if got, want := texts(history(t, svc, f, "old")), []string{"q1", "a1"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}

	// And a newest turn that is too heavy by itself leaves nothing: never a part of a turn.
	svc, f, add = seeded(t, "huge")
	add(turn(t, "user", "q0"), turn(t, "assistant", "a0"))
	add(turn(t, "user", "q1"), callEvent(t, "c1", "list_tasks"), responseEvent(t, "c1", "list_tasks", 100<<10), turn(t, "assistant", "a1"))
	if got := texts(history(t, svc, f, "huge")); len(got) != 0 {
		t.Errorf("history = %v, want none: the newest turn does not fit, and a turn is never cut", got)
	}
}

// ---- a failed run does not poison the next one (I4b) -----------------------------

func TestSession_FailedRunLeavesNothingTheNextRunReplays(t *testing.T) {
	a := newAgentFixture(t)
	captureLogs(t)
	a.deps.MaxTurns = 2
	loop := callTool("list_tasks", map[string]any{"channel_id": a.public.String()})
	loop.forever = true
	a.model.script(loop)

	// The first run is stopped at its second turn: ADK had already appended the call of
	// its second turn, and the tool never ran.
	first := a.mention(a.public, a.alice, "@assistant first question")
	firstRun := uuid.New()
	if err := a.job()(t.Context(), a.delivery(a.invokeOf(first, firstRun), "evt-1", 1)); err != nil {
		t.Fatal(err)
	}
	if run := a.run(firstRun); run.Status != "failed" {
		t.Fatalf("run = %+v, want failed", run)
	}
	if got := kinds(rawEvents(t, a, "assistant:"+a.alice.String()+":"+a.public.String())); got[len(got)-1] != "call" {
		t.Fatalf("stored kinds = %v, want a call with no response last: the case this test is about", got)
	}

	// The next run in the same conversation starts from a clean past.
	a.model.reset()
	a.model.script(say("A fine answer."))
	second := a.mention(a.public, a.alice, "@assistant second question")
	secondRun := uuid.New()
	if err := a.job()(t.Context(), a.delivery(a.invokeOf(second, secondRun), "evt-2", 1)); err != nil {
		t.Fatal(err)
	}
	if run := a.run(secondRun); run.Status != "done" {
		t.Fatalf("run = %+v, want done", run)
	}
	var prompt strings.Builder
	for _, p := range a.requestParts(-1) {
		if p.FunctionCall != nil || p.FunctionResponse != nil {
			t.Errorf("the second run's prompt holds a call or a response of the stopped run: %+v", p)
		}
		prompt.WriteString(p.Text)
	}
	if strings.Contains(prompt.String(), "first question") || !strings.Contains(prompt.String(), "second question") {
		t.Errorf("the second run's prompt = %q, want the second question and not the first, which was never answered", prompt.String())
	}

	// And the run after that continues from the second one's turn.
	a.model.script(say("Third."))
	third := a.mention(a.public, a.alice, "@assistant third question")
	if err := a.job()(t.Context(), a.delivery(a.invokeOf(third, uuid.New()), "evt-3", 1)); err != nil {
		t.Fatal(err)
	}
	prompt.Reset()
	for _, p := range a.requestParts(-1) {
		prompt.WriteString(p.Text + "\n")
	}
	for _, want := range []string{"second question", "A fine answer.", "third question"} {
		if !strings.Contains(prompt.String(), want) {
			t.Errorf("the third run's prompt lacks %q:\n%s", want, prompt.String())
		}
	}
}

// rawEvents is the events of a session as they are stored.
func rawEvents(t *testing.T, a *agentFixture, id string) session.Events {
	t.Helper()
	got, err := newSessionService(a.db, a.ws).Get(t.Context(), &session.GetRequest{AppName: appName, UserID: a.alice.String(), SessionID: id, NumRecentEvents: 1000})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return got.Session.Events()
}

// ---- runs of one conversation take turns (I4a) -----------------------------------

// overlapRuntime is a runtime that notes how many runs are inside it at once.
type overlapRuntime struct {
	mu                 sync.Mutex
	active, peak, runs int
	sessions           map[string]bool
}

func (r *overlapRuntime) Run(ctx context.Context, _ Spec, _ string, sess Session, _ func(string)) (Final, error) {
	r.mu.Lock()
	r.active++
	r.runs++
	r.peak = max(r.peak, r.active)
	if r.sessions == nil {
		r.sessions = map[string]bool{}
	}
	r.sessions[sess.ID] = true
	r.mu.Unlock()
	select { // the work of a run
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
	}
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	return Final{Text: "ok"}, nil
}

func TestSession_RunsOfOneConversationNeverOverlap(t *testing.T) {
	for name, shared := range map[string]bool{
		// One process: its runs queue on a mutex.
		"one process": true,
		// Many processes: each has its own mutex, and the runs queue on the database.
		"separate processes": false,
	} {
		t.Run(name, func(t *testing.T) {
			a := newAgentFixture(t)
			rt := &overlapRuntime{}
			a.deps.Runtime = rt
			const n = 6
			one := a.job()
			var wg sync.WaitGroup
			runs := make([]uuid.UUID, n)
			errs := make([]error, n)
			handlers := make([]bus.Handler, n)
			deliveries := make([]bus.Message, n)
			for i := range n {
				handlers[i] = one
				if !shared {
					handlers[i] = a.job()
				}
				runs[i] = uuid.New()
				m := a.mention(a.public, a.alice, fmt.Sprintf("@assistant question %d", i))
				deliveries[i] = a.delivery(a.invokeOf(m, runs[i]), fmt.Sprintf("evt-%d", i), 1)
			}
			for i := range n {
				wg.Go(func() { errs[i] = handlers[i](t.Context(), deliveries[i]) })
			}
			wg.Wait()

			for i, err := range errs {
				if err != nil {
					t.Fatalf("delivery %d: %v", i, err)
				}
				if run := a.run(runs[i]); run.Status != "done" {
					t.Errorf("run %d = %+v, want done", i, run)
				}
			}
			rt.mu.Lock()
			defer rt.mu.Unlock()
			if rt.runs != n || len(rt.sessions) != 1 {
				t.Fatalf("runs = %d over %d sessions, want %d runs of one session", rt.runs, len(rt.sessions), n)
			}
			if rt.peak != 1 {
				t.Errorf("most runs of the conversation inside the runtime at once = %d, want 1: they take turns", rt.peak)
			}
		})
	}
}

func TestSession_RunsOfDifferentConversationsRunTogether(t *testing.T) {
	a := newAgentFixture(t)
	var inside atomic.Int32
	ready := make(chan struct{})
	a.deps.Runtime = runtimeFunc(func(ctx context.Context, _ Spec, _ string, _ Session, _ func(string)) (Final, error) {
		if inside.Add(1) == 2 {
			close(ready)
		}
		select {
		case <-ready:
			return Final{Text: "ok"}, nil
		case <-time.After(10 * time.Second):
			return Final{}, errors.New("the other conversation's run never started: they are serialised against each other")
		}
	})
	forAlice := a.mention(a.public, a.alice, "@assistant from alice")
	forBob := a.mention(a.public, a.bob, "@assistant from bob")
	handler := a.job()

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, m := range []*chatv1.Message{forAlice, forBob} {
		wg.Go(func() {
			results[i] = handler(t.Context(), a.delivery(a.invokeOf(m, uuid.New()), fmt.Sprintf("evt-%d", i), 1))
		})
	}
	wg.Wait()
	for i, err := range results {
		if err != nil {
			t.Errorf("delivery %d: %v", i, err)
		}
	}
}

func TestSession_TwoMentionsAtOnce_LeaveWholeTurnsInOrder(t *testing.T) {
	a := newAgentFixture(t)
	// Both runs are scripted alike, so which of them reads which step does not matter:
	// the steps are played in the order the model is asked, and the runs take turns.
	a.model.script(
		callTool("list_tasks", map[string]any{"channel_id": a.public.String()}), say("First."),
		callTool("list_tasks", map[string]any{"channel_id": a.public.String()}), say("Second."),
	)
	first := a.mention(a.public, a.alice, "@assistant what is on the board?")
	second := a.mention(a.public, a.alice, "@assistant and now?")
	handler := a.job()

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, m := range []*chatv1.Message{first, second} {
		wg.Go(func() {
			results[i] = handler(t.Context(), a.delivery(a.invokeOf(m, uuid.New()), fmt.Sprintf("evt-%d", i), 1))
		})
	}
	wg.Wait()
	for i, err := range results {
		if err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}

	got := kinds(rawEvents(t, a, "assistant:"+a.alice.String()+":"+a.public.String()))
	want := []string{"user", "call", "resp", "text", "user", "call", "resp", "text"}
	if !slices.Equal(got, want) {
		t.Errorf("stored events = %v, want two whole turns one after the other: %v", got, want)
	}
}
