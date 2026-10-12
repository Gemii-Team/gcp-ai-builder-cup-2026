// Package llm is the language-model port: one Client for generation, embeddings and
// transcription, whatever provider sits behind it. This file holds the types and the
// interface only; adapters and wrappers live in sub-packages and sibling files.
package llm

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/adk/v2/model"
)

// Tier picks the model class of a request; the adapter maps it to a configured model id.
type Tier string

// Model tiers.
const (
	// TierFlash is the fast, cheap model.
	TierFlash Tier = "flash"
	// TierPro is the stronger model.
	TierPro Tier = "pro"
)

// Part is one piece of request content: text, inline bytes with their MIME type, or
// a provider-readable URI (see blob.Store.URI).
type Part struct {
	Text string
	Data []byte
	MIME string
	URI  string
}

// Schema asks for output that conforms to a JSON Schema; Name identifies it to the
// provider.
type Schema struct {
	Name string
	JSON json.RawMessage
}

// Tool declares a function the model may call; Parameters is its JSON Schema.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Request is one generation call.
type Request struct {
	Tier   Tier
	System string
	Parts  []Part
	// Schema, when set, constrains the output to JSON of that shape.
	Schema          *Schema
	Tools           []Tool
	MaxOutputTokens int
}

// ToolCall is the model asking for Name to be called with Args.
type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

// Usage is the token count the provider reported for a call.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Result is the answer to a Request: text, tool calls, or both.
type Result struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
}

// EmbedTask says what an embedding is for; models that distinguish documents from
// queries embed them differently.
type EmbedTask int

// Embedding tasks.
const (
	// EmbedDocument embeds text that will be stored and searched.
	EmbedDocument EmbedTask = iota
	// EmbedQuery embeds a search query.
	EmbedQuery
)

// Audio is a recording to transcribe: its bytes, MIME type and file name.
type Audio struct {
	Data []byte
	MIME string
	Name string
}

// TranscriptEntry is one utterance of a transcription. StartMS and EndMS are
// millisecond offsets, nil when the provider returns no timing; Speaker is empty when
// it does not tell speakers apart.
type TranscriptEntry struct {
	Speaker string
	StartMS *int64
	EndMS   *int64
	Text    string
}

// Errors every adapter reports the same way; test them with errors.Is.
var (
	// ErrUnsupportedByProvider means the provider cannot do what was asked, for
	// example reading a scanned PDF.
	ErrUnsupportedByProvider = errors.New("llm: not supported by the provider")
	// ErrTruncated means the output hit the token limit before it was complete.
	ErrTruncated = errors.New("llm: output truncated at the token limit")
	// ErrBudgetExhausted means a spending budget is used up. It is unused until the
	// GCP phase, when the budget guard that returns it is built.
	ErrBudgetExhausted = errors.New("llm: budget exhausted")
)

// Client is the model port.
type Client interface {
	// Generate runs one request to completion.
	Generate(ctx context.Context, req Request) (Result, error)
	// Stream runs one request and calls onDelta with each piece of text as it
	// arrives; the Result is the complete answer.
	Stream(ctx context.Context, req Request, onDelta func(delta string)) (Result, error)
	// Embed returns one vector per text, in order, for task.
	Embed(ctx context.Context, texts []string, task EmbedTask) ([][]float32, error)
	// Transcribe turns a recording into transcript entries.
	Transcribe(ctx context.Context, audio Audio) ([]TranscriptEntry, error)
	// EmbeddingModel names the embedding model; it is stored with every vector so a
	// model change is detectable.
	EmbeddingModel() string
	// ADKModel returns the model for tier as an ADK model for agent runtimes.
	ADKModel(tier Tier) (model.LLM, error)
}
