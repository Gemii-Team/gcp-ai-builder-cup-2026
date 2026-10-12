// Package knowledge owns the search index over chat, meetings and documents, and the
// cited answers built on it. Other domains reach it only through Service and
// Retriever.
package knowledge

import (
	"context"
	"time"

	"github.com/google/uuid"

	knowledgev1 "kiwi/gen/kiwi/knowledge/v1"
	"kiwi/internal/platform/authz"
)

// IndexRequest names one source to (re)index, mirroring the embed.index event.
// SourceKind is "chat", "meeting" or "doc"; ChannelID is nil for a workspace-wide
// source; WindowStart is set for a chat window.
type IndexRequest struct {
	SourceKind  string
	SourceID    uuid.UUID
	ChannelID   *uuid.UUID
	WindowStart *time.Time
}

// AskInput is a question; a non-nil ChannelID scopes the answer to that channel.
type AskInput struct {
	Question  string
	ChannelID *uuid.UUID
	// AnswerLang is the language the answer is written in, "th" or "en". Empty (or anything else) is the language
	// of Question. A caller that asks on behalf of a person, with a question it wrote itself, passes the language of the
	// person's own words: the answer follows the person, the search follows the text it searches with.
	AnswerLang string
}

// Query is a search. Lang is "th" or "en"; Limit caps the hits; a non-nil ChannelID
// scopes the search to that channel.
type Query struct {
	Text      string
	Lang      string
	ChannelID *uuid.UUID
	Limit     int
}

// Hit is one retrieved chunk with the source it came from. SourceAnchor locates it
// inside the source, for example "seq:10-14", "entries:3-9" or "page:2"; ChannelID is
// nil for chunks of workspace-wide sources.
type Hit struct {
	ChunkID      uuid.UUID
	ChannelID    *uuid.UUID
	SourceKind   string
	SourceID     uuid.UUID
	SourceAnchor string
	Lang         string
	Content      string
	Score        float64
}

// Retriever searches the index on behalf of an actor. It returns only chunks from
// channels the actor is a member of and from workspace-wide sources.
type Retriever interface {
	// Search returns the best hits for q; it needs authz.ActKnowledgeSearch.
	Search(ctx context.Context, a authz.Actor, q Query) ([]Hit, error)
}

// Service is the knowledge domain.
type Service interface {
	// Index embeds the source named by req and stores its chunks, skipping work
	// when nothing changed; it needs authz.ActKnowledgeIndex.
	Index(ctx context.Context, a authz.Actor, req IndexRequest) error
	// Ask answers in.Question from the sources the actor can read, with citations, or
	// reports that nothing was found; it needs authz.ActKnowledgeSearch.
	Ask(ctx context.Context, a authz.Actor, in AskInput) (*knowledgev1.Answer, error)
	// Reindex re-embeds every chunk, for when the embedding model changed. It is an
	// operator command (`kiwi reindex`) with no actor, so it needs no authz action.
	Reindex(ctx context.Context) error
}
