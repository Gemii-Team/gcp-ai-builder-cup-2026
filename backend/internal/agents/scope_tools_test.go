package agents

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/tool"

	tasksv1 "kiwi/gen/kiwi/tasks/v1"
	"kiwi/internal/platform/llm"
)

// The scope of a run is the scope of every tool that reads a channel for it, not only of
// the search: the reply of a run is read by the channel it is posted in, so a summary or a
// board of another channel would be a republication of that channel all the same.

const scopeRefusal = "in this conversation you can only look at the channel it was asked in"

// scopedTools are the tools of a run asked in channel (nil: an Ask-panel run with no
// channel), started by trigger (nil for an Ask-panel run).
func (f *fixture) scopedTools(channel *uuid.UUID, trigger *Trigger) (map[string]tool.Tool, *Collector) {
	col := &Collector{}
	return NewTools(ToolDeps{Knowledge: f.kb, Tasks: f.tasks, Chat: f.chat, LLM: f.llm, Collector: col, Trigger: trigger, Channel: channel}), col
}

// twoChannels is alice in public and in hr, each with a message and a task of its own, and a
// model that summarizes whatever it is shown by citing the first message of its channel.
func twoChannels(t *testing.T) (f *fixture, hr uuid.UUID) {
	t.Helper()
	f = newFixture(t)
	hr = f.channel("private", f.alice, f.bob)
	f.say(f.public, f.alice, "Hello team")
	f.say(hr, f.bob, hrFact)
	for channel, title := range map[uuid.UUID]string{f.public: "Book the dock", hr: "Pay the HR bonus"} {
		if _, err := f.tasks.CreateTask(t.Context(), f.person(f.bob), &tasksv1.CreateTaskRequest{ChannelId: channel.String(), Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	f.llm.HandleGenerate(func(context.Context, llm.Request) (llm.Result, error) {
		return llm.Result{Text: `{"summary":"They said hello.","source_seqs":[1]}`}, nil
	})
	return f, hr
}

func TestScopedRun_ChannelReadingToolsRefuseAnotherChannelWithAFixedError(t *testing.T) {
	f, hr := twoChannels(t)
	trigger := triggerOf(f.say(f.public, f.alice, "@assistant do things"), "Alice")
	scope := f.public
	tools, col := f.scopedTools(&scope, trigger)
	alice := f.as(f.agentFor(f.alice))

	for name, args := range map[string]map[string]any{
		"summarize_channel": {"channel_id": hr.String()},
		"list_tasks":        {"channel_id": hr.String()},
		"create_task_draft": {"channel_id": hr.String(), "title": "Pay the HR bonus again"},
	} {
		t.Run(name+" of another channel of the person", func(t *testing.T) {
			_, err := f.call(alice, tools, name, args)
			if err == nil || !strings.Contains(err.Error(), scopeRefusal) {
				t.Fatalf("%s of #hr-private from a run asked in #general = %v, want the refusal %q", name, err, scopeRefusal)
			}
			for _, leak := range []string{hr.String(), "90,000", "Pay the HR bonus"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("the error %q names %q of the other channel", err, leak)
				}
			}
		})
	}
	if n := len(f.llm.GenerateCalls()); n != 0 || col.Summary() != nil {
		t.Errorf("the refused summary asked the model %d times and left %v, want no read at all", n, col.Summary())
	}
	if n := f.count(`SELECT count(*) FROM tasks.tasks WHERE channel_id = $1 AND state = 'draft'`, hr); n != 0 {
		t.Errorf("%d drafts in #hr-private, want none", n)
	}

	t.Run("the channel the run was asked in works", func(t *testing.T) {
		if _, err := f.call(alice, tools, "summarize_channel", map[string]any{"channel_id": f.public.String()}); err != nil {
			t.Errorf("summarize_channel of the scoped channel: %v", err)
		}
		listed, err := f.call(alice, tools, "list_tasks", map[string]any{"channel_id": f.public.String()})
		if err != nil || !strings.Contains(fmt.Sprint(listed), "Book the dock") {
			t.Errorf("list_tasks of the scoped channel = %v, %v, want its board", listed, err)
		}
		if _, err := f.call(alice, tools, "create_task_draft", map[string]any{"channel_id": f.public.String(), "title": "Order pallets"}); err != nil {
			t.Errorf("create_task_draft in the scoped channel: %v", err)
		}
	})
}

func TestScopedRun_AnAskWithNoChannelStillReadsEveryChannelOfThePerson(t *testing.T) {
	f, hr := twoChannels(t)
	tools, col := f.scopedTools(nil, nil)
	alice := f.as(f.agentFor(f.alice))

	if _, err := f.call(alice, tools, "summarize_channel", map[string]any{"channel_id": hr.String()}); err != nil {
		t.Fatalf("summarize_channel of a channel of alice's from an unscoped Ask: %v", err)
	}
	if sum := col.Summary(); sum == nil || sum.ChannelID != hr {
		t.Errorf("collector summary = %+v, want the summary of #hr-private", sum)
	}
	listed, err := f.call(alice, tools, "list_tasks", map[string]any{"channel_id": hr.String()})
	if err != nil || !strings.Contains(fmt.Sprint(listed), "Pay the HR bonus") {
		t.Errorf("list_tasks of #hr-private from an unscoped Ask = %v, %v, want its board", listed, err)
	}
}

// A run asked in #general whose model reads #hr-private for the person publishes nothing of
// it: not the slip, the reply, the stored result, the recorded calls or the streamed frames.
func TestMention_SummarizeChannelOfAnotherChannel_IsRefusedAndLeaksNothing(t *testing.T) {
	a, hr := scopeFixture(t, false)
	a.say(hr, a.bob, hrFact)
	w := a.watch("ch:" + a.public.String())
	a.model.script(callTool("summarize_channel", map[string]any{"channel_id": hr.String()}), say("I cannot look there."))
	trigger := a.mention(a.public, a.alice, "@assistant summarize the HR channel")
	runID := uuid.New()

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}

	run := a.run(runID)
	calls := a.recordedCalls(runID)
	if len(calls) != 1 || calls[0].Name != "summarize_channel" || !strings.Contains(calls[0].Error, scopeRefusal) {
		t.Fatalf("tool_calls = %+v, want the summary refused with %q", calls, scopeRefusal)
	}
	where := map[string]string{"the stored result": string(run.Result), "the recorded calls": string(run.ToolCalls)}
	for _, r := range a.replies(a.public, a.alice) {
		where["the reply"] = r.String()
	}
	for i, f := range w.snapshot() {
		where[fmt.Sprintf("streamed frame %d", i)] = f.String()
	}
	for i, g := range a.llm.GenerateCalls() {
		where[fmt.Sprintf("prompt %d of the model", i)] = promptOf(g)
	}
	for name, got := range where {
		leaks := []string{"90,000", hrTitle, hr.String()}
		if name == "the recorded calls" {
			leaks = leaks[:2] // the operator's record keeps the id the model itself wrote as the argument
		}
		for _, leak := range leaks {
			if strings.Contains(got, leak) {
				t.Errorf("%s holds %q of #hr-private", name, leak)
			}
		}
	}
}
