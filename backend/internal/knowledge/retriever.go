package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"kiwi/internal/knowledge/internal/store"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/llm"
)

const (
	// candidates is how many chunks each of the two searches, by vector and by keyword,
	// hands to the fusion.
	candidates = 40
	// defaultHits is how many fused hits a Query without a Limit gets.
	defaultHits = 12
	// rrfK is the rank constant of the fusion; 60 is the value of the method's paper.
	rrfK = 60
	// queryEmbedTimeout bounds the embedding of one question; the adapter has no
	// timeout of its own.
	queryEmbedTimeout = 30 * time.Second
)

// NewRetriever returns the Retriever of the knowledge domain, built from the Deps that
// New takes; it needs DB, LLM, Authz and Chat.
func NewRetriever(d Deps) Retriever {
	return newService(d)
}

var _ Retriever = (*service)(nil)

// retrieval is what a search found, and what Ask needs to answer from it.
type retrieval struct {
	hits []Hit
	// meta is the display snapshot of every hit's source, by chunk id.
	meta map[uuid.UUID]sourceMeta
	// scope is the channels that were searched, in id order.
	scope []uuid.UUID
}

// Search implements Retriever. It searches the channels the actor's user belongs to, or
// only q.ChannelID when that is set and the user belongs to it, and the sources that
// belong to no channel; the membership is read from the table, not from the members
// cache, so a user who has just left a channel no longer finds its chunks. The question
// is embedded as a query; the best 40 chunks by vector and the best 40 by keyword are
// fused and the first q.Limit of them (12 when it is zero) returned. q.Lang filters
// nothing: the embeddings are cross-lingual, so a Thai question finds an English chunk
// and the other way round, and the keyword side reads every language the same way.
func (s *service) Search(ctx context.Context, a authz.Actor, q Query) ([]Hit, error) {
	r, err := s.retrieve(ctx, a, q)
	if err != nil {
		return nil, err
	}
	return r.hits, nil
}

// canSearch reports a service built without what a search reads from.
func (s *service) canSearch() error {
	if s.db == nil || s.llm == nil || s.authz == nil || s.chat == nil {
		return fmt.Errorf("knowledge: searching needs the database, the model, Authz and Chat; this Service was built without them: %w", errs.ErrInvalid)
	}
	return nil
}

func (s *service) retrieve(ctx context.Context, a authz.Actor, q Query) (retrieval, error) {
	if err := s.canSearch(); err != nil {
		return retrieval{}, err
	}
	if err := requireSearcher(a); err != nil {
		return retrieval{}, err
	}
	text := strings.TrimSpace(q.Text)
	if text == "" {
		return retrieval{}, fmt.Errorf("knowledge: the question is empty: %w", errs.ErrInvalid)
	}
	scope, workspaceWide, err := s.searchScope(ctx, a, q.ChannelID)
	if err != nil {
		return retrieval{}, err
	}
	vec, err := s.embedQuery(ctx, text)
	if err != nil {
		return retrieval{}, err
	}

	channelIDs := make([]string, len(scope))
	for i, c := range scope {
		channelIDs[i] = c.String()
	}
	model := s.llm.EmbeddingModel()
	var byVector []store.SearchChunksByVectorRow
	var byKeyword []store.SearchChunksByKeywordRow
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		qs := store.New(tx)
		version, err := qs.PgvectorVersion(ctx)
		if err != nil {
			return fmt.Errorf("read the pgvector version: %w", err)
		}
		if _, err := tx.Exec(ctx, annSetting(version)); err != nil {
			return fmt.Errorf("tune the vector index: %w", err)
		}
		if byVector, err = qs.SearchChunksByVector(ctx, store.SearchChunksByVectorParams{
			Embedding: pgvector.NewVector(vec), EmbeddingModel: model,
			ChannelIds: channelIDs, WorkspaceWide: workspaceWide, RowLimit: candidates,
		}); err != nil {
			return fmt.Errorf("search by vector: %w", err)
		}
		if byKeyword, err = qs.SearchChunksByKeyword(ctx, store.SearchChunksByKeywordParams{
			QueryText: text, EmbeddingModel: model,
			ChannelIds: channelIDs, WorkspaceWide: workspaceWide, RowLimit: candidates,
		}); err != nil {
			return fmt.Errorf("search by keyword: %w", err)
		}
		return nil
	})
	if err != nil {
		return retrieval{}, fmt.Errorf("knowledge: search: %w", err)
	}

	meta := make(map[uuid.UUID]sourceMeta, len(byVector)+len(byKeyword))
	vectorHits := make([]Hit, len(byVector))
	for i, row := range byVector {
		vectorHits[i], meta[row.ID] = hitOf(row)
	}
	keywordHits := make([]Hit, len(byKeyword))
	for i, row := range byKeyword {
		keywordHits[i], meta[row.ID] = hitOf(store.SearchChunksByVectorRow(row))
	}
	hits := RRF([][]Hit{vectorHits, keywordHits}, rrfK)
	limit := q.Limit
	if limit <= 0 {
		limit = defaultHits
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return retrieval{hits: hits, meta: meta, scope: scope}, nil
}

// requireSearcher refuses a principal that holds knowledge.search for no channel at
// all: a job (authz does not grant it the action), a kind authz does not know, or an
// actor without a workspace or the user whose channels it would search.
func requireSearcher(a authz.Actor) error {
	switch a.Kind {
	case authz.ActorUser, authz.ActorAgent:
	case authz.ActorSystem:
		return fmt.Errorf("knowledge: a %s actor may not search: %w", a.Kind, errs.ErrForbidden)
	default:
		return fmt.Errorf("knowledge: unknown actor kind %q: %w", a.Kind, errs.ErrForbidden)
	}
	if a.WorkspaceID == uuid.Nil || a.EffectiveUserID() == uuid.Nil {
		return fmt.Errorf("knowledge: actor has no workspace or user: %w", errs.ErrForbidden)
	}
	return nil
}

// searchScope returns the channels a search by a covers, and whether it also covers the
// chunks that belong to no channel. Without a channel that is every channel the user
// belongs to: the bound to them stands in for knowledge.search of each, which authz
// grants users and agents. With a channel it is that channel alone, which authz must
// grant and the table must confirm: authz asks the members cache, which may still say
// "yes" to a user who has just left, and the table, which cannot, has the last word.
func (s *service) searchScope(ctx context.Context, a authz.Actor, channel *uuid.UUID) (scope []uuid.UUID, workspaceWide bool, err error) {
	member, err := s.chat.MemberChannelIDs(ctx, a.WorkspaceID, a.EffectiveUserID())
	if err != nil {
		return nil, false, fmt.Errorf("knowledge: list member channels: %w", err)
	}
	if channel == nil {
		return member, true, nil
	}
	if err := s.authz.Can(ctx, a, authz.ActKnowledgeSearch, authz.Resource{ChannelID: *channel}); err != nil {
		return nil, false, err
	}
	if !slices.Contains(member, *channel) {
		return nil, false, fmt.Errorf("knowledge: search in channel %s: the user is not a member: %w", *channel, errs.ErrForbidden)
	}
	return []uuid.UUID{*channel}, false, nil
}

// embedQuery embeds a question as a query, under a deadline of its own.
func (s *service) embedQuery(ctx context.Context, text string) ([]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, queryEmbedTimeout)
	defer cancel()
	vecs, err := s.llm.Embed(ctx, []string{text}, llm.EmbedQuery)
	if err != nil {
		return nil, fmt.Errorf("knowledge: embed the question: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("knowledge: embed the question: the model returned %d vectors for one text", len(vecs))
	}
	if len(vecs[0]) != embedDim {
		return nil, fmt.Errorf("knowledge: embed the question: the vector has %d dimensions, the index holds %d", len(vecs[0]), embedDim)
	}
	return vecs[0], nil
}

// annSetting is the statement that keeps a nearest-neighbour search through the HNSW
// index from running dry. The index returns a fixed number of rows before the filter
// on channel and model is applied, so a filter that most chunks fail leaves too few: from
// pgvector 0.8 an iterative scan keeps reading until it has enough, and before it the
// only help is a wider search. A version that cannot be read is treated as the old one.
func annSetting(version string) string {
	major, rest, _ := strings.Cut(version, ".")
	minor, _, _ := strings.Cut(rest, ".")
	maj, err1 := strconv.Atoi(major)
	mnr, err2 := strconv.Atoi(minor)
	if err1 == nil && err2 == nil && (maj > 0 || mnr >= 8) {
		return "SET LOCAL hnsw.iterative_scan = 'relaxed_order'"
	}
	return "SET LOCAL hnsw.ef_search = 200"
}

// hitOf reads a row of either search: the hit and the display snapshot of its source.
func hitOf(r store.SearchChunksByVectorRow) (Hit, sourceMeta) {
	var meta sourceMeta
	if len(r.SourceMeta) > 0 {
		// A snapshot that cannot be read is empty: the chunk is still a valid source,
		// only without its label.
		_ = json.Unmarshal(r.SourceMeta, &meta)
	}
	return Hit{
		ChunkID: r.ID, ChannelID: r.ChannelID, SourceKind: r.SourceKind, SourceID: r.SourceID,
		SourceAnchor: r.SourceAnchor, Lang: r.Lang, Content: r.Content, Score: r.Score,
	}, meta
}
