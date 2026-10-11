// Package fakellm is a scriptable llm.Client for tests. Each of Generate, Embed and
// Transcribe answers from its queue of replies first, then from its handler, and
// fails with ErrUnscripted when it has neither (Embed instead falls back to the
// deterministic Vector). Every call is recorded. All methods are safe for concurrent
// use.
package fakellm

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"sync"

	"google.golang.org/adk/v2/model"

	"kiwi/internal/platform/llm"
)

// EmbedDim is the dimension of the vectors Embed returns when unscripted; it matches
// the vector(768) column.
const EmbedDim = 768

// ErrUnscripted is returned by a call that has no queued reply and no handler.
var ErrUnscripted = errors.New("fakellm: no scripted reply")

// GenerateReply is one scripted outcome of Generate or Stream.
type GenerateReply struct {
	Result llm.Result
	Err    error
}

// EmbedReply is one scripted outcome of Embed.
type EmbedReply struct {
	Vectors [][]float32
	Err     error
}

// TranscribeReply is one scripted outcome of Transcribe.
type TranscribeReply struct {
	Entries []llm.TranscriptEntry
	Err     error
}

// EmbedCall records one Embed call.
type EmbedCall struct {
	Texts []string
	Task  llm.EmbedTask
}

// Client implements llm.Client.
type Client struct {
	mu sync.Mutex

	generateQueue   []GenerateReply
	embedQueue      []EmbedReply
	transcribeQueue []TranscribeReply

	onGenerate   func(ctx context.Context, req llm.Request) (llm.Result, error)
	onEmbed      func(ctx context.Context, texts []string, task llm.EmbedTask) ([][]float32, error)
	onTranscribe func(ctx context.Context, audio llm.Audio) ([]llm.TranscriptEntry, error)
	onADKModel   func(tier llm.Tier) (model.LLM, error)

	embeddingModel string

	generateCalls   []llm.Request
	streamCalls     []llm.Request
	embedCalls      []EmbedCall
	transcribeCalls []llm.Audio
}

var _ llm.Client = (*Client)(nil)

// New returns a Client with no script; EmbeddingModel is "fake-embedding".
func New() *Client {
	return &Client{embeddingModel: "fake-embedding"}
}

// QueueGenerate appends replies for the next Generate and Stream calls, consumed in
// order. Stream draws from the same script as Generate.
func (c *Client) QueueGenerate(replies ...GenerateReply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generateQueue = append(c.generateQueue, replies...)
}

// HandleGenerate sets the function that answers Generate and Stream once the queue
// is empty.
func (c *Client) HandleGenerate(f func(ctx context.Context, req llm.Request) (llm.Result, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onGenerate = f
}

// QueueEmbed appends replies for the next Embed calls, consumed in order.
func (c *Client) QueueEmbed(replies ...EmbedReply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.embedQueue = append(c.embedQueue, replies...)
}

// HandleEmbed sets the function that answers Embed once the queue is empty; without
// one Embed returns Vector(text) for every text.
func (c *Client) HandleEmbed(f func(ctx context.Context, texts []string, task llm.EmbedTask) ([][]float32, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onEmbed = f
}

// QueueTranscribe appends replies for the next Transcribe calls, consumed in order.
func (c *Client) QueueTranscribe(replies ...TranscribeReply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.transcribeQueue = append(c.transcribeQueue, replies...)
}

// HandleTranscribe sets the function that answers Transcribe once the queue is empty.
func (c *Client) HandleTranscribe(f func(ctx context.Context, audio llm.Audio) ([]llm.TranscriptEntry, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onTranscribe = f
}

// HandleADKModel sets the function that answers ADKModel; without one ADKModel fails
// with ErrUnscripted.
func (c *Client) HandleADKModel(f func(tier llm.Tier) (model.LLM, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onADKModel = f
}

// SetEmbeddingModel sets what EmbeddingModel returns.
func (c *Client) SetEmbeddingModel(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.embeddingModel = name
}

// GenerateCalls returns the requests of every Generate call so far.
func (c *Client) GenerateCalls() []llm.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.generateCalls)
}

// StreamCalls returns the requests of every Stream call so far.
func (c *Client) StreamCalls() []llm.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.streamCalls)
}

// EmbedCalls returns every Embed call so far.
func (c *Client) EmbedCalls() []EmbedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.embedCalls)
}

// TranscribeCalls returns the audio of every Transcribe call so far.
func (c *Client) TranscribeCalls() []llm.Audio {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.transcribeCalls)
}

// Generate implements llm.Client.
func (c *Client) Generate(ctx context.Context, req llm.Request) (llm.Result, error) {
	c.mu.Lock()
	c.generateCalls = append(c.generateCalls, req)
	reply, queued := pop(&c.generateQueue)
	handler := c.onGenerate
	c.mu.Unlock()
	return answer(ctx, req, reply, queued, handler)
}

// Stream implements llm.Client. It sends the whole reply text as a single delta.
func (c *Client) Stream(ctx context.Context, req llm.Request, onDelta func(delta string)) (llm.Result, error) {
	c.mu.Lock()
	c.streamCalls = append(c.streamCalls, req)
	reply, queued := pop(&c.generateQueue)
	handler := c.onGenerate
	c.mu.Unlock()
	res, err := answer(ctx, req, reply, queued, handler)
	if err == nil && res.Text != "" && onDelta != nil {
		onDelta(res.Text)
	}
	return res, err
}

func answer(ctx context.Context, req llm.Request, reply GenerateReply, queued bool, handler func(context.Context, llm.Request) (llm.Result, error)) (llm.Result, error) {
	if err := ctx.Err(); err != nil {
		return llm.Result{}, err
	}
	switch {
	case queued:
		return reply.Result, reply.Err
	case handler != nil:
		return handler(ctx, req)
	}
	return llm.Result{}, ErrUnscripted
}

// Embed implements llm.Client.
func (c *Client) Embed(ctx context.Context, texts []string, task llm.EmbedTask) ([][]float32, error) {
	c.mu.Lock()
	c.embedCalls = append(c.embedCalls, EmbedCall{Texts: slices.Clone(texts), Task: task})
	reply, queued := pop(&c.embedQueue)
	handler := c.onEmbed
	c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch {
	case queued:
		return reply.Vectors, reply.Err
	case handler != nil:
		return handler(ctx, texts, task)
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = Vector(t)
	}
	return out, nil
}

// Transcribe implements llm.Client.
func (c *Client) Transcribe(ctx context.Context, audio llm.Audio) ([]llm.TranscriptEntry, error) {
	c.mu.Lock()
	c.transcribeCalls = append(c.transcribeCalls, audio)
	reply, queued := pop(&c.transcribeQueue)
	handler := c.onTranscribe
	c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch {
	case queued:
		return reply.Entries, reply.Err
	case handler != nil:
		return handler(ctx, audio)
	}
	return nil, ErrUnscripted
}

// EmbeddingModel implements llm.Client.
func (c *Client) EmbeddingModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.embeddingModel
}

// ADKModel implements llm.Client.
func (c *Client) ADKModel(tier llm.Tier) (model.LLM, error) {
	c.mu.Lock()
	handler := c.onADKModel
	c.mu.Unlock()
	if handler == nil {
		return nil, ErrUnscripted
	}
	return handler(tier)
}

// Vector is the unit-length EmbedDim-dimensional vector Embed returns for text when
// unscripted: the same text always maps to the same vector and different texts to
// different, nearly orthogonal ones.
func Vector(text string) []float32 {
	v := make([]float32, EmbedDim)
	seed := sha256.Sum256([]byte(text))
	var buf [sha256.Size + 4]byte
	copy(buf[:], seed[:])
	var norm float64
	for i := range v {
		binary.LittleEndian.PutUint32(buf[sha256.Size:], uint32(i))
		sum := sha256.Sum256(buf[:])
		x := float64(binary.LittleEndian.Uint32(sum[:4]))/math.MaxUint32*2 - 1
		v[i] = float32(x)
		norm += x * x
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}

func pop[T any](queue *[]T) (T, bool) {
	var zero T
	if len(*queue) == 0 {
		return zero, false
	}
	next := (*queue)[0]
	*queue = (*queue)[1:]
	return next, true
}
