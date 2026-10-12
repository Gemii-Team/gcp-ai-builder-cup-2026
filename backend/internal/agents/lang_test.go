package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"kiwi/internal/knowledge"
	"kiwi/internal/platform/llm"
)

// The assistant searches with a question the model writes, and a model often rewrites a Thai question into English. What the
// person is answered in is the language of what they wrote (the run's question), whatever the model searched with. These
// tests run whole runs (the Ask panel's and a mention's) with a scripted model that does exactly that.

const (
	// The question the model searches with: an English rewrite of the Thai one below.
	englishRewrite = "How many pallets does the Bang Na warehouse hold?"
	thaiQuestion   = "คลังสินค้าบางนาเก็บพาเลทได้กี่ที่"
	thaiAnswer     = "คลังสินค้าบางนาเก็บพาเลทได้ 4,000 ที่"
)

// directives are the instruction that ends the system prompt of an answer call in Thai and in English, as the knowledge
// domain writes them: they are read from a call it makes, not copied here, so a change of wording changes both sides. A
// domain that gave the two languages the same directive fails here, before a test could pass without proving anything.
func (f *fixture) directives() (thai, english string) {
	f.t.Helper()
	for lang, into := range map[string]*string{"th": &thai, "en": &english} {
		if _, err := f.kb.Ask(f.t.Context(), f.agentFor(f.alice), knowledge.AskInput{Question: englishRewrite, AnswerLang: lang}); err != nil {
			f.t.Fatalf("Ask in %q: %v", lang, err)
		}
		calls := f.llm.GenerateCalls()
		system := calls[len(calls)-1].System
		*into = system[strings.LastIndex(system, "\n\n")+2:]
	}
	if thai == english || thai == "" || english == "" {
		f.t.Fatalf("directives: Thai %q, English %q, want two different instructions", thai, english)
	}
	return thai, english
}

// writesInTheLanguageItIsAsked makes the model of the answer call write Thai when its prompt carries the Thai directive and
// English otherwise, and cite what it was shown either way.
func (a *agentFixture) writesInTheLanguageItIsAsked(thaiDirective string) {
	a.llm.HandleGenerate(func(ctx context.Context, req llm.Request) (llm.Result, error) {
		res, err := citeEveryChunk(ctx, req)
		if err == nil && strings.HasSuffix(req.System, thaiDirective) {
			res.Text = strings.Replace(res.Text, "Here is what the chunks say.", thaiAnswer, 1)
		}
		return res, err
	})
}

// answerCalls are the calls that wrote an answer after the first n calls of the test.
func (a *agentFixture) answerCalls(skip int) []llm.Request {
	var out []llm.Request
	for _, c := range a.llm.GenerateCalls()[skip:] {
		if c.Schema != nil && c.Schema.Name == "kb_answer" {
			out = append(out, c)
		}
	}
	return out
}

func TestRun_ThaiQuestionSearchedInEnglishIsAnsweredInThai(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func(a *agentFixture) string // runs the question and returns the text of the slip that answered it
	}{
		{"an Ask panel run", func(a *agentFixture) string {
			svc := New(a.deps)
			return a.answerOf(svc, a.ask(svc, thaiQuestion)).GetBodyMarkdown()
		}},
		{"a mention run", func(a *agentFixture) string {
			trigger := a.mention(a.public, a.alice, "@assistant "+thaiQuestion)
			if err := a.job()(a.t.Context(), a.delivery(a.invokeOf(trigger, uuid.New()), "evt-1", 1)); err != nil {
				a.t.Fatalf("agent.run: %v", err)
			}
			replies := a.replies(a.public, a.alice)
			if len(replies) != 1 || replies[0].GetArtifact() == nil {
				a.t.Fatalf("replies = %v, want one with its slip", replies)
			}
			return replies[0].GetArtifact().GetBodyMarkdown()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newAgentFixture(t)
			a.seedWarehouse()
			thai, english := a.directives()
			a.writesInTheLanguageItIsAsked(thai)
			asked := len(a.llm.GenerateCalls())
			// The model rewrites the Thai question into English and searches with that.
			a.model.script(
				callTool("search_knowledge", map[string]any{"question": englishRewrite}),
				say("The Bang Na warehouse holds 4,000 pallets."),
			)

			body := c.run(a)

			calls := a.answerCalls(asked)
			if len(calls) != 1 {
				t.Fatalf("%d answer calls, want the one of the model's search", len(calls))
			}
			if !strings.HasSuffix(calls[0].System, thai) || strings.Contains(calls[0].System, english) {
				t.Errorf("the answer was asked for in the wrong language: the system prompt ends %q, want the Thai directive %q (not %q)",
					calls[0].System[strings.LastIndex(calls[0].System, "\n\n")+2:], thai, english)
			}
			if knowledge.DetectLang(body) != "th" || !strings.Contains(body, thaiAnswer) {
				t.Errorf("the slip says %q, want the Thai answer %q", body, thaiAnswer)
			}
		})
	}
}
