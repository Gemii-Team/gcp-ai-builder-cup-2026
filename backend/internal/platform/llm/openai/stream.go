package openai

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"kiwi/internal/platform/llm"
)

// maxFrame bounds one line of the event stream.
const maxFrame = 4 << 20

// streamFrame is one data frame of a streamed answer. Reasoning arrives first as
// delta.reasoning_content; there is no field for it here, so only content is read.
type streamFrame struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
}

// partialCall is a tool call as it arrives, in pieces.
type partialCall struct {
	id, name string
	args     strings.Builder
}

// Stream implements llm.Client. It calls onDelta with each piece of content, never
// with reasoning, and never with the blank lines a model starts its answer with.
func (c *client) Stream(ctx context.Context, req llm.Request, onDelta func(delta string)) (llm.Result, error) {
	p, err := c.prepare(req, true)
	if err != nil {
		return llm.Result{}, err
	}
	resp, err := c.post(ctx, c.base+"/v1/chat/completions", true, p.body)
	if err != nil {
		return llm.Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	var (
		text    strings.Builder
		started bool
		finish  string
		u       llm.Usage
		calls   = map[int]*partialCall{}
		done    bool
	)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), maxFrame)
	for !done && sc.Scan() {
		payload, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue // a comment, an event name or a blank line
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			done = true
			continue
		}
		var f streamFrame
		if err := json.Unmarshal([]byte(payload), &f); err != nil {
			return llm.Result{}, fmt.Errorf("openai: a frame of the stream is not JSON: %w", err)
		}
		if f.Usage != nil {
			u = f.Usage.toLLM()
		}
		if len(f.Choices) == 0 {
			continue
		}
		ch := f.Choices[0]
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
		for _, tc := range ch.Delta.ToolCalls {
			pc := calls[tc.Index]
			if pc == nil {
				pc = &partialCall{}
				calls[tc.Index] = pc
			}
			// An id and a name come in the first piece of a tool call only.
			pc.id, pc.name = cmp.Or(tc.ID, pc.id), cmp.Or(tc.Function.Name, pc.name)
			pc.args.WriteString(tc.Function.Arguments)
		}
		delta := ch.Delta.Content
		if !started {
			delta = strings.TrimLeftFunc(delta, unicode.IsSpace)
		}
		if delta == "" {
			continue
		}
		started = true
		text.WriteString(delta)
		if onDelta != nil {
			onDelta(delta)
		}
	}
	if err := sc.Err(); err != nil {
		return llm.Result{}, fmt.Errorf("openai: read stream: %w", err)
	}
	if !done {
		return llm.Result{}, errors.New("openai: the stream ended before [DONE]")
	}

	a := answer{text: text.String(), finish: finish, usage: u}
	for _, i := range slices.Sorted(maps.Keys(calls)) {
		call, err := toolCall(calls[i].id, calls[i].name, calls[i].args.String())
		if err != nil {
			return llm.Result{}, err
		}
		a.calls = append(a.calls, call)
	}
	if err := p.truncated(a); err != nil {
		return llm.Result{}, err
	}
	return llm.Result{Text: a.text, ToolCalls: a.calls, Usage: a.usage}, nil
}
