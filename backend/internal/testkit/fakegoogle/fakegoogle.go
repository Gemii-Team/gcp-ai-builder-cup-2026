// Package fakegoogle is a scriptable google.Client for tests. Each method answers
// from its queue of replies first, then from its handler, and fails with
// ErrUnscripted when it has neither; to script a poll that changes over time, queue
// one reply per poll. Every call is recorded. All methods are safe for concurrent
// use.
package fakegoogle

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/google/uuid"

	"kiwi/internal/platform/google"
)

// ErrUnscripted is returned by a call that has no queued reply and no handler.
var ErrUnscripted = errors.New("fakegoogle: no scripted reply")

// SpaceReply is one scripted outcome of CreateMeetSpace.
type SpaceReply struct {
	Space google.Space
	Err   error
}

// ConferencesReply is one scripted outcome of ListConferences.
type ConferencesReply struct {
	Conferences []google.Conference
	Err         error
}

// EntriesReply is one scripted outcome of ListTranscriptEntries.
type EntriesReply struct {
	Entries []google.Entry
	Err     error
}

// Call records one call. Name is the space name (ListConferences) or the transcript
// name (ListTranscriptEntries); EnableTranscription is set for CreateMeetSpace.
type Call struct {
	Method              string
	UserID              uuid.UUID
	Name                string
	EnableTranscription bool
}

// Client implements google.Client.
type Client struct {
	mu sync.Mutex

	spaceQueue       []SpaceReply
	conferencesQueue []ConferencesReply
	entriesQueue     []EntriesReply

	onCreateMeetSpace       func(ctx context.Context, userID uuid.UUID, enableTranscription bool) (google.Space, error)
	onListConferences       func(ctx context.Context, userID uuid.UUID, spaceName string) ([]google.Conference, error)
	onListTranscriptEntries func(ctx context.Context, userID uuid.UUID, transcriptName string) ([]google.Entry, error)

	calls []Call
}

var _ google.Client = (*Client)(nil)

// New returns a Client with no script.
func New() *Client { return &Client{} }

// QueueCreateMeetSpace appends replies for the next CreateMeetSpace calls, consumed in order.
func (c *Client) QueueCreateMeetSpace(replies ...SpaceReply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spaceQueue = append(c.spaceQueue, replies...)
}

// HandleCreateMeetSpace sets the function that answers CreateMeetSpace once the queue is empty.
func (c *Client) HandleCreateMeetSpace(f func(ctx context.Context, userID uuid.UUID, enableTranscription bool) (google.Space, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onCreateMeetSpace = f
}

// QueueListConferences appends replies for the next ListConferences calls, consumed in order.
func (c *Client) QueueListConferences(replies ...ConferencesReply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conferencesQueue = append(c.conferencesQueue, replies...)
}

// HandleListConferences sets the function that answers ListConferences once the queue is empty.
func (c *Client) HandleListConferences(f func(ctx context.Context, userID uuid.UUID, spaceName string) ([]google.Conference, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onListConferences = f
}

// QueueListTranscriptEntries appends replies for the next ListTranscriptEntries calls, consumed in order.
func (c *Client) QueueListTranscriptEntries(replies ...EntriesReply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entriesQueue = append(c.entriesQueue, replies...)
}

// HandleListTranscriptEntries sets the function that answers ListTranscriptEntries once the queue is empty.
func (c *Client) HandleListTranscriptEntries(f func(ctx context.Context, userID uuid.UUID, transcriptName string) ([]google.Entry, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onListTranscriptEntries = f
}

// Calls returns every call so far, in order.
func (c *Client) Calls() []Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

// CreateMeetSpace implements google.Client.
func (c *Client) CreateMeetSpace(ctx context.Context, userID uuid.UUID, enableTranscription bool) (google.Space, error) {
	c.mu.Lock()
	c.calls = append(c.calls, Call{Method: "CreateMeetSpace", UserID: userID, EnableTranscription: enableTranscription})
	reply, queued := pop(&c.spaceQueue)
	handler := c.onCreateMeetSpace
	c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return google.Space{}, err
	}
	switch {
	case queued:
		return reply.Space, reply.Err
	case handler != nil:
		return handler(ctx, userID, enableTranscription)
	}
	return google.Space{}, ErrUnscripted
}

// ListConferences implements google.Client.
func (c *Client) ListConferences(ctx context.Context, userID uuid.UUID, spaceName string) ([]google.Conference, error) {
	c.mu.Lock()
	c.calls = append(c.calls, Call{Method: "ListConferences", UserID: userID, Name: spaceName})
	reply, queued := pop(&c.conferencesQueue)
	handler := c.onListConferences
	c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch {
	case queued:
		return reply.Conferences, reply.Err
	case handler != nil:
		return handler(ctx, userID, spaceName)
	}
	return nil, ErrUnscripted
}

// ListTranscriptEntries implements google.Client.
func (c *Client) ListTranscriptEntries(ctx context.Context, userID uuid.UUID, transcriptName string) ([]google.Entry, error) {
	c.mu.Lock()
	c.calls = append(c.calls, Call{Method: "ListTranscriptEntries", UserID: userID, Name: transcriptName})
	reply, queued := pop(&c.entriesQueue)
	handler := c.onListTranscriptEntries
	c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch {
	case queued:
		return reply.Entries, reply.Err
	case handler != nil:
		return handler(ctx, userID, transcriptName)
	}
	return nil, ErrUnscripted
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
