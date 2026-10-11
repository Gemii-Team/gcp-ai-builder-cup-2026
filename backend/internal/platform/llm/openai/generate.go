package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/google/jsonschema-go/jsonschema"

	"kiwi/internal/platform/llm"
)

const (
	// reasoningHeadroom is added to the output limit a caller asks for. The models
	// reason before they answer, and the reasoning counts against max_tokens: a
	// one-sentence answer spends 80 to 110 tokens thinking, a summary of a long
	// meeting about 900, and with too little left the content comes back empty.
	reasoningHeadroom = 2048

	// structuredAttempts bounds how often a call that needs JSON from the pro model is
	// asked again after an answer that was cut off or does not fit the schema.
	structuredAttempts = 3
)

// chatRequest is a Chat Completions request. Nothing in it is specific to one
// provider's models: there is no top_k, no safety setting.
type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []message       `json:"messages"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Tools          []toolDef       `json:"tools,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	StreamOptions  *streamOptions  `json:"stream_options,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // a string for the system prompt, []contentPart for the user
}

type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema,omitempty"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type toolDef struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatResponse is the part of a Chat Completions answer the adapter reads. It has no
// field for reasoning_content, so that cannot reach a caller.
type chatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (u *usage) toLLM() llm.Usage {
	if u == nil {
		return llm.Usage{}
	}
	return llm.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
}

// prepared is a request ready to send, with what is needed to read its answer.
type prepared struct {
	body chatRequest
	// schema is set when the answer is JSON that must fit it; check is set as well
	// when this client validates it (the pro model).
	schema bool
	check  *jsonschema.Resolved
}

// prepare builds the request for req.
func (c *client) prepare(req llm.Request, stream bool) (prepared, error) {
	model, err := c.modelFor(req.Tier)
	if err != nil {
		return prepared{}, err
	}
	parts, err := contentParts(req.Parts)
	if err != nil {
		return prepared{}, err
	}
	if len(parts) == 0 && req.System == "" {
		return prepared{}, errors.New("openai: the request has no system prompt and no parts")
	}

	p := prepared{body: chatRequest{Model: model}}
	system := req.System
	if req.Schema != nil {
		if req.Schema.Name == "" || !json.Valid(req.Schema.JSON) {
			return prepared{}, errors.New("openai: a schema needs a name and valid JSON")
		}
		p.schema = true
		if model == c.pro {
			// json_schema on this model is unreliable: it runs away to the limit and
			// scrambles fields. json_object with every field described in the prompt,
			// and the answer checked here, is not.
			prompt, check, err := structured(req.Schema.JSON)
			if err != nil {
				return prepared{}, fmt.Errorf("openai: schema %q: %w", req.Schema.Name, err)
			}
			p.body.ResponseFormat = &responseFormat{Type: "json_object"}
			system = strings.TrimSpace(system + "\n\n" + prompt)
			if !stream {
				p.check = check
			}
		} else {
			p.body.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: &jsonSchema{Name: req.Schema.Name, Schema: req.Schema.JSON, Strict: true}}
		}
	}
	if system != "" {
		p.body.Messages = append(p.body.Messages, message{Role: "system", Content: system})
	}
	if len(parts) > 0 {
		p.body.Messages = append(p.body.Messages, message{Role: "user", Content: parts})
	}
	if req.MaxOutputTokens > 0 {
		p.body.MaxTokens = req.MaxOutputTokens + reasoningHeadroom
	}
	for _, t := range req.Tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		p.body.Tools = append(p.body.Tools, toolDef{Type: "function", Function: toolFunction{Name: t.Name, Description: t.Description, Parameters: params}})
	}
	if stream {
		p.body.Stream = true
		p.body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return p, nil
}

// answer is what one request produced.
type answer struct {
	text   string // content with its leading blank lines trimmed
	calls  []llm.ToolCall
	finish string
	usage  llm.Usage
}

// Generate implements llm.Client.
func (c *client) Generate(ctx context.Context, req llm.Request) (llm.Result, error) {
	p, err := c.prepare(req, false)
	if err != nil {
		return llm.Result{}, err
	}
	if p.check != nil {
		return c.generateChecked(ctx, p)
	}
	a, err := c.complete(ctx, p)
	if err != nil {
		return llm.Result{}, err
	}
	if err := p.truncated(a); err != nil {
		return llm.Result{}, err
	}
	return llm.Result{Text: a.text, ToolCalls: a.calls, Usage: a.usage}, nil
}

// complete sends p and reads the answer.
func (c *client) complete(ctx context.Context, p prepared) (answer, error) {
	var resp chatResponse
	if err := c.postJSON(ctx, c.base+"/v1/chat/completions", true, p.body, &resp); err != nil {
		return answer{}, err
	}
	if len(resp.Choices) == 0 {
		return answer{}, errors.New("openai: the answer has no choice")
	}
	ch := resp.Choices[0]
	a := answer{
		text:   strings.TrimLeftFunc(ch.Message.Content, unicode.IsSpace),
		finish: ch.FinishReason,
		usage:  resp.Usage.toLLM(),
	}
	for _, tc := range ch.Message.ToolCalls {
		call, err := toolCall(tc.ID, tc.Function.Name, tc.Function.Arguments)
		if err != nil {
			return answer{}, err
		}
		a.calls = append(a.calls, call)
	}
	return a, nil
}

// toolCall builds an llm.ToolCall from what the model wrote; its arguments are JSON in
// a string, and a model that calls a tool with none may leave the string empty.
func toolCall(id, name, args string) (llm.ToolCall, error) {
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return llm.ToolCall{}, fmt.Errorf("openai: the arguments of the call to %q are not JSON", name)
	}
	return llm.ToolCall{ID: id, Name: name, Args: json.RawMessage(args)}, nil
}

// truncated reports llm.ErrTruncated for an answer that hit the token limit with
// nothing usable: no content and no tool call (the reasoning used the budget), or
// JSON cut off half way. Text the caller capped is returned as it is.
func (p prepared) truncated(a answer) error {
	if a.finish != "length" {
		return nil
	}
	if !p.schema && (a.text != "" || len(a.calls) > 0) {
		return nil
	}
	return fmt.Errorf("openai: %s stopped at its %d-token limit with no complete answer (reasoning counts against it): %w", p.body.Model, p.body.MaxTokens, llm.ErrTruncated)
}

// generateChecked asks the pro model for JSON and checks it against the schema. An
// answer that ran to the limit or does not fit is asked for again, structuredAttempts
// times in all: the model fails about one call in seven. A failing HTTP status is not
// repeated here; llm.WithRetry does that.
func (c *client) generateChecked(ctx context.Context, p prepared) (llm.Result, error) {
	var total llm.Usage
	var last error
	for range structuredAttempts {
		a, err := c.complete(ctx, p)
		if err != nil {
			return llm.Result{}, err
		}
		total.InputTokens += a.usage.InputTokens
		total.OutputTokens += a.usage.OutputTokens
		if len(a.calls) > 0 {
			return llm.Result{Text: a.text, ToolCalls: a.calls, Usage: total}, nil
		}
		if last = p.truncated(a); last != nil {
			continue
		}
		if last = check(p.check, a.text); last == nil {
			return llm.Result{Text: a.text, Usage: total}, nil
		}
	}
	return llm.Result{}, fmt.Errorf("openai: %s gave no answer that fits the schema in %d attempts: %w", p.body.Model, structuredAttempts, last)
}
