package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"kiwi/internal/platform/llm"
)

const (
	// adkMaxOutputTokens bounds one model call of an agent. The models behind the
	// adapter reason before they answer and the reasoning is paid for out of this
	// limit: with less than about 1100 a call can end with an answer that has no text.
	adkMaxOutputTokens = 4096
	// defaultMaxTurns is how many model calls one attempt of a run may make, the last of
	// which has to be the answer: a model that keeps calling tools is stopped, and what
	// it asked for in its last call is not run.
	defaultMaxTurns = 8
	// maxRecordedResult cuts the result of a tool in agents.runs.tool_calls; the
	// model has read all of it and the record only has to show what happened.
	maxRecordedResult = 2000
)

// errTooManySteps is a run whose model was still calling tools after its last turn.
var errTooManySteps = errors.New("the agent made too many steps")

// Runtime runs one turn of an agent: it gives the model the instruction of spec, the
// conversation in sess and input as the person's newest message, lets it call the
// tools of sess that spec allows until it answers, and returns the answer. emit gets
// each piece of the answer's text as the model writes it; the text it gets is only a
// preview, Final.Text is the answer. It reports every call of a tool to sess.Record.
// On an error, Final holds what was used before it.
type Runtime interface {
	Run(ctx context.Context, spec Spec, input string, sess Session, emit func(delta string)) (Final, error)
}

// Final is what a run ended with.
type Final struct {
	// Text is what the agent said last.
	Text string
	// Usage is the tokens every model call of the run used.
	Usage llm.Usage
}

// ToolCall is one call of a tool as agents.runs records it: the arguments the model
// wrote and either what the tool answered (cut when long) or why it failed.
type ToolCall struct {
	Name   string         `json:"name"`
	Args   map[string]any `json:"args,omitempty"`
	Result string         `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// adkRuntime is the Runtime over the ADK: llmagent with the model of the spec's tier
// and the runner over the session service of the run.
type adkRuntime struct {
	llm      llm.Client
	maxTurns int
}

var _ Runtime = adkRuntime{}

// Run implements Runtime. A model call that ends at the token limit is an error
// wrapping llm.ErrTruncated, not an answer: its text is cut off, and a call of a tool
// that ends there has cut-off arguments. A model still calling tools after maxTurns
// calls ends the run with errTooManySteps before the tools of its last call run.
func (r adkRuntime) Run(ctx context.Context, spec Spec, input string, sess Session, emit func(string)) (Final, error) {
	m, err := r.llm.ADKModel(spec.Tier)
	if err != nil {
		return Final{}, fmt.Errorf("agents: model of %s: %w", spec.Name, err)
	}
	tools := make([]tool.Tool, 0, len(spec.Tools))
	for _, name := range spec.Tools {
		t, ok := sess.Tools[name]
		if !ok {
			return Final{}, fmt.Errorf("agents: %s may use tool %q and the run has none: %w", spec.Name, name, errNoTool)
		}
		tools = append(tools, t)
	}
	a, err := llmagent.New(llmagent.Config{
		Name:  spec.Name,
		Model: m,
		// A provider, not Instruction: Instruction is a template in which "{name}" is
		// replaced by state, and the registry's text is never one.
		InstructionProvider:   func(agent.ReadonlyContext) (string, error) { return spec.Instruction, nil },
		Tools:                 tools,
		GenerateContentConfig: &genai.GenerateContentConfig{MaxOutputTokens: adkMaxOutputTokens},
	})
	if err != nil {
		return Final{}, fmt.Errorf("agents: build %s: %w", spec.Name, err)
	}
	rn, err := runner.New(runner.Config{AppName: appName, Agent: a, SessionService: sess.Service, AutoCreateSession: true})
	if err != nil {
		return Final{}, fmt.Errorf("agents: runner of %s: %w", spec.Name, err)
	}

	var (
		out     Final
		turns   int
		pending = map[string]*ToolCall{} // by call id, until its response arrives
	)
	msg := genai.NewContentFromText(input, genai.RoleUser)
	for ev, err := range rn.Run(ctx, sess.UserID, sess.ID, msg, agent.RunConfig{StreamingMode: agent.StreamingModeSSE}) {
		if err != nil {
			return out, fmt.Errorf("agents: %s run: %w", spec.Name, err)
		}
		if ev == nil {
			continue
		}
		if ev.Partial {
			if ev.Content != nil {
				for _, p := range ev.Content.Parts {
					if p.Text != "" && !p.Thought {
						emit(p.Text)
					}
				}
			}
			continue
		}
		if u := ev.UsageMetadata; u != nil {
			out.Usage.InputTokens += int(u.PromptTokenCount)
			out.Usage.OutputTokens += int(u.CandidatesTokenCount)
		}
		if ev.FinishReason == genai.FinishReasonMaxTokens {
			return out, fmt.Errorf("agents: %s: a model call ended at the token limit: %w", spec.Name, llm.ErrTruncated)
		}
		if ev.Content == nil {
			continue
		}
		if ev.Content.Role == genai.RoleModel {
			turns++
			if turns >= r.maxTurns && callsATool(ev.Content) {
				return out, fmt.Errorf("agents: %s: still calling tools at its turn %d: %w", spec.Name, turns, errTooManySteps)
			}
		}
		for _, p := range ev.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				pending[p.FunctionCall.ID] = &ToolCall{Name: p.FunctionCall.Name, Args: p.FunctionCall.Args}
			case p.FunctionResponse != nil:
				call := pending[p.FunctionResponse.ID]
				if call == nil {
					call = &ToolCall{Name: p.FunctionResponse.Name}
				}
				delete(pending, p.FunctionResponse.ID)
				call.Result, call.Error = recordResult(p.FunctionResponse.Response)
				if sess.Record != nil {
					if err := sess.Record(*call); err != nil {
						return out, err
					}
				}
			}
		}
		if ev.IsFinalResponse() && ev.Author == spec.Name {
			if text := textOf(ev.Content); text != "" {
				out.Text = text
			}
		}
	}
	return out, nil
}

// callsATool reports whether c asks for a tool to be run.
func callsATool(c *genai.Content) bool {
	for _, p := range c.Parts {
		if p.FunctionCall != nil {
			return true
		}
	}
	return false
}

// errNoTool is a registry that allows a tool the run was not given: a mistake of the
// program that wires the agents, never of a person.
var errNoTool = errors.New("tool not available")

// textOf is the text of c's parts, without the model's thoughts.
func textOf(c *genai.Content) string {
	var b strings.Builder
	for _, p := range c.Parts {
		if p.Text != "" && !p.Thought {
			b.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// recordResult is what a tool answered as agents.runs keeps it: the error a tool
// failed with (ADK hands it to the model as {"error": ...}), or the answer as JSON.
func recordResult(resp map[string]any) (result, failure string) {
	if msg, ok := resp["error"].(string); ok && len(resp) == 1 {
		return "", clip(msg, maxRecordedResult)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return "", "the result cannot be recorded"
	}
	return clip(string(raw), maxRecordedResult), ""
}
