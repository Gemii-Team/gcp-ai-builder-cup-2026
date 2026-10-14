// Package cache is the ephemeral state of a K.I.W.I. process: one-time realtime
// tickets, OAuth login state, the channel-membership cache and the sharded pub/sub
// fan-out that carries frames to the WebSocket gateway, plus the browser sessions
// that sign-in needs. Nothing durable lives here: losing it costs sessions (when
// they live in Valkey) and a few in-flight sign-ins, never data.
//
// The state is a port with two adapters, chosen by KIWI_CACHE_PROVIDER:
//
//   - Valkey (valkey*.go) is shared by every process of a deployment, so the api,
//     realtime, worker and agents roles can run apart, and it keeps the sessions.
//   - Memory (memory.go) is the process's own, for one process that runs every
//     role. It keeps no sessions: Memory does not implement Sessions on purpose, so
//     a session can never end up somewhere a restart loses it. The sessions of a
//     memory deployment live in Postgres (identity.NewSessions).
//
// Valkey keys follow spec §3.3, where {x} stands for the value of x: sess:{id},
// rtt:{ticket}, members:{channel}. OAuth login state, which §3.3 does not list, is
// oauth:{state}, and the invalidation counter that keeps a stale members fill out is
// members:{channel}:epoch.
package cache

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Defaults for the zero fields of Options.
const (
	DefaultSessionSliding  = 7 * 24 * time.Hour
	DefaultSessionAbsolute = 30 * 24 * time.Hour
	DefaultTicketTTL       = 30 * time.Second
)

// oauthStateTTL bounds how long a user may take at the identity provider.
const oauthStateTTL = 10 * time.Minute

// Options tunes lifetimes; a zero field takes its default.
type Options struct {
	// SessionSliding is how long a session lives after its last read.
	SessionSliding time.Duration
	// SessionAbsolute is how long a session lives at most, however often read.
	SessionAbsolute time.Duration
	// TicketTTL is how long a realtime ticket can be redeemed.
	TicketTTL time.Duration
}

// WithDefaults returns o with every zero field replaced by its default.
func (o Options) WithDefaults() Options {
	if o.SessionSliding <= 0 {
		o.SessionSliding = DefaultSessionSliding
	}
	if o.SessionAbsolute <= 0 {
		o.SessionAbsolute = DefaultSessionAbsolute
	}
	if o.TicketTTL <= 0 {
		o.TicketTTL = DefaultTicketTTL
	}
	return o
}

// SessionRecord is what a session id resolves to.
type SessionRecord struct {
	UserID      uuid.UUID `json:"user_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	// CreatedAt starts the absolute lifetime; PutSession fills a zero value.
	CreatedAt time.Time `json:"created_at"`
}

// OAuthState is what the login callback needs to finish the flow it started.
type OAuthState struct {
	Verifier string `json:"verifier"`
	ReturnTo string `json:"return_to"`
	// Nonce is sent in the authorization request and must come back in the ID token.
	Nonce string `json:"nonce"`
	// Consent marks the flow of the Meet consent route: its callback tells the person
	// when no Meet grant came out of it, which a plain sign-in never does.
	Consent bool `json:"consent,omitempty"`
}

// TicketRecord is who a realtime ticket was issued to.
type TicketRecord struct {
	UserID      uuid.UUID `json:"user_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
}

// Frame is one message received on a topic.
type Frame struct {
	Topic string
	Data  []byte
}

// Sessions stores browser sessions. A session id is a bearer credential: no adapter
// logs it. The Postgres store (identity.NewSessions) keeps only its SHA-256; the Valkey
// adapter keeps the legacy key sess:{id}, which hashing would change and so sign
// everyone out on the rollout that does it.
type Sessions interface {
	// PutSession stores the session under id for the sliding lifetime, shortened to
	// what is left of the absolute one. A zero CreatedAt means now; an empty id, or a
	// CreatedAt already past the absolute lifetime, is errs.ErrInvalid.
	PutSession(ctx context.Context, id string, s SessionRecord) error
	// GetSession returns the session and slides its lifetime forward, never past the
	// absolute cap. A missing, expired or capped session is errs.ErrNotFound.
	GetSession(ctx context.Context, id string) (SessionRecord, error)
	// DeleteSession removes the session; removing an absent one is not an error.
	DeleteSession(ctx context.Context, id string) error
}

// OAuthStates holds the state of a login flow between the redirect to the provider
// and its callback.
type OAuthStates interface {
	// PutOAuthState stores the login state for ten minutes. An empty state is
	// errs.ErrInvalid.
	PutOAuthState(ctx context.Context, state string, v OAuthState) error
	// TakeOAuthState returns the state and deletes it in one step, so a replayed
	// callback, and every concurrent one but the first, finds nothing:
	// errs.ErrNotFound.
	TakeOAuthState(ctx context.Context, state string) (OAuthState, error)
}

// Tickets are the one-time credentials of a WebSocket connection.
type Tickets interface {
	// IssueTicket stores a one-time realtime ticket for t and returns it. It expires
	// after Options.TicketTTL.
	IssueTicket(ctx context.Context, t TicketRecord) (string, error)
	// RedeemTicket returns the record a ticket was issued for and deletes it in one
	// step: of any number of concurrent redeems exactly one succeeds, the rest and
	// every later one get errs.ErrNotFound.
	RedeemTicket(ctx context.Context, ticket string) (TicketRecord, error)
}

// Members is the cache of the member ids of a channel, read through by
// chat.NewMembership. An adapter may cache nothing at all: Members then always
// misses and the caller reads Postgres, which is always correct.
type Members interface {
	// Members returns the cached member ids of a channel. hit is false when nothing is
	// cached, and the caller reads Postgres and fills the set with SetMembersAtEpoch.
	// A cached empty set reads as a miss.
	Members(ctx context.Context, channelID uuid.UUID) (ids []uuid.UUID, hit bool, err error)
	// SetMembers replaces the cached member set of a channel unconditionally. A reader
	// that filled the cache from Postgres must use SetMembersAtEpoch instead: this
	// write, landing after an InvalidateMembers, would bring back a removed member.
	SetMembers(ctx context.Context, channelID uuid.UUID, ids []uuid.UUID) error
	// MembersEpoch returns how many times the channel's member set has been
	// invalidated. A cache-aside reader samples it before it reads Postgres and hands
	// it to SetMembersAtEpoch with the result.
	MembersEpoch(ctx context.Context, channelID uuid.UUID) (int64, error)
	// SetMembersAtEpoch replaces the cached member set with ids, but only if no
	// InvalidateMembers happened since the reader sampled epoch; otherwise the
	// reader's data may predate a membership change and is dropped. stored reports
	// whether the set was written.
	SetMembersAtEpoch(ctx context.Context, channelID uuid.UUID, epoch int64, ids []uuid.UUID) (stored bool, err error)
	// InvalidateMembers drops the cached member set and advances the channel's epoch,
	// so the next read goes to Postgres and any fill still in flight from before this
	// call is refused. Call it on every membership change, after the change committed.
	InvalidateMembers(ctx context.Context, channelID uuid.UUID) error
}

// Fanout carries realtime frames to every subscriber of a topic, for example
// ch:{channelId} or u:{userId}. Frames are hints: Postgres is the source of truth and
// clients resync by seq.
type Fanout interface {
	// SPublish sends frame to every subscriber of topic. A topic nobody follows drops it.
	SPublish(ctx context.Context, topic string, frame []byte) error
	// SSubscribe opens a Subscription with no topics yet; Add them. It fails if the
	// backing store cannot be reached.
	SSubscribe(ctx context.Context) (Subscription, error)
}

// Subscription follows topics. Its methods are safe for concurrent use.
type Subscription interface {
	// Add starts following topics. It returns once the request is made, which for a
	// remote store is before the store has confirmed it.
	Add(topics ...string)
	// Remove stops following topics; frames already on their way may still arrive.
	// With no topics it does nothing (it never unsubscribes from all).
	Remove(topics ...string)
	// C delivers the frames of every followed topic, in publish order per topic. It is
	// closed after Close. Read it promptly: a consumer that falls behind loses frames.
	C() <-chan Frame
	// Close drops every topic and then closes C. It may be called more than once.
	Close()
}

// Cache is the ephemeral state of a process, without sessions.
type Cache interface {
	OAuthStates
	Tickets
	Members
	Fanout
	// Ping checks that the backing store answers, for readiness probes. The memory
	// cache has none and always answers.
	Ping(ctx context.Context) error
	// Close releases the cache. Close every Subscription first.
	Close() error
}
