package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pgvector/pgvector-go"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	"kiwi/internal/knowledge/internal/store"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/llm"
)

// The kinds of source, as they are spelled in IndexRequest, in chunk ids and in the
// source_kind column.
const (
	kindChat    = "chat"
	kindMeeting = "meeting"
	kindDoc     = "doc"
)

const (
	// chatPage is how many messages one read of a channel's history asks for.
	chatPage = 200
	// authorAssistant labels what an agent said; only one agent exists.
	authorAssistant = "assistant"
	// authorUnknown labels a message whose author the directory no longer knows.
	authorUnknown = "unknown"
	// statusReady is a meeting whose transcript and summary are complete.
	statusReady = "ready"
)

// window is one five-minute span of a channel's clock, from start up to end.
type window struct{ start, end time.Time }

// windowAt is the window t falls in; windows are aligned to the clock, so every message
// of one span lands in the same one.
func windowAt(t time.Time) window {
	start := t.UTC().Truncate(chatWindow)
	return window{start: start, end: start.Add(chatWindow)}
}

// source is what one run writes: all the chunks of a meeting or a document, or those of
// one window of a chat channel. channel is the channel whose members may retrieve them.
type source struct {
	kind    string
	id      uuid.UUID
	channel uuid.UUID
	window  *window
}

// Index implements Service. It reads the source, cuts it into chunks, embeds those
// that are new or changed and removes those the source no longer produces. A chunk
// whose text and embedding model are what is stored is left alone, and every batch of
// embedded chunks is committed on its own, so a run that stops half way is continued
// by the next one and a run on an unchanged source embeds nothing.
//
// Its one caller is the embed.index job (Jobs), which acts as a system actor, and it
// must stay job-only. A meeting or a file is read before the actor is authorised on it,
// because the channel that authorisation needs is the source's own (a channel in req is
// only checked against it), so a person calling Index would learn whether a source
// exists before being refused; a caller that is a person has to authorise first. Runs of
// one source do not overlap within one Service (keyedLock), not within one process: Jobs
// builds the Service whose job is the only caller, and a second caller would need to
// share that lock, for instance through Deps.
//
// The other caller is the operator command `kiwi seed` (package seed), which acts as the
// same kind of system actor in a process that no request reaches, so it opens nothing to
// a person. Like `kiwi reindex` it builds a Service of its own and does not share the
// job's lock; the writes are upserts of deterministic chunk ids, so runs of one source
// that overlap end with the chunks one run alone would leave.
func (s *service) Index(ctx context.Context, a authz.Actor, req IndexRequest) error {
	if err := s.complete(); err != nil {
		return err
	}
	src, load, err := s.resolve(ctx, a, req)
	if err != nil {
		return err
	}
	unlock, err := s.locks.lock(ctx, lockKey(src.kind, src.id, src.window))
	if err != nil {
		return err
	}
	defer unlock()

	drafts, err := load(ctx)
	if err != nil {
		return err
	}
	return s.write(ctx, a.WorkspaceID, src, drafts)
}

// resolve finds out which source req names and checks that a may index it. The
// function it returns reads the source's text, which a run does once it holds the
// source's lock.
func (s *service) resolve(ctx context.Context, a authz.Actor, req IndexRequest) (source, func(context.Context) ([]draft, error), error) {
	if req.SourceID == uuid.Nil {
		return source{}, nil, fmt.Errorf("knowledge: index %s: no source id: %w", req.SourceKind, errs.ErrInvalid)
	}
	switch req.SourceKind {
	case kindChat:
		return s.resolveChat(ctx, a, req)
	case kindMeeting:
		return s.resolveMeeting(ctx, a, req)
	case kindDoc:
		return s.resolveDoc(ctx, a, req)
	}
	return source{}, nil, fmt.Errorf("knowledge: unknown source kind %q: %w", req.SourceKind, errs.ErrInvalid)
}

// canIndex asks authz whether a may index what lives in channelID.
func (s *service) canIndex(ctx context.Context, a authz.Actor, channelID uuid.UUID) error {
	return s.authz.Can(ctx, a, authz.ActKnowledgeIndex, authz.Resource{ChannelID: channelID})
}

// checkChannel refuses a request that names another channel than the source belongs
// to: the channel decides who may retrieve the chunks, so it is taken from the source
// and a request that disagrees is a bug in whoever made it.
func checkChannel(req IndexRequest, actual uuid.UUID) error {
	if req.ChannelID != nil && *req.ChannelID != actual {
		return fmt.Errorf("knowledge: index %s %s: the request names channel %s, the source is in %s: %w",
			req.SourceKind, req.SourceID, *req.ChannelID, actual, errs.ErrInvalid)
	}
	return nil
}

// withOrigin gives a system actor the user it acts for. A job reacts to events that
// carry no user, but the indexer reads through services that want one, so it acts for
// the person the source belongs to: the owner of a meeting, the uploader of a file.
func withOrigin(a authz.Actor, user uuid.UUID) authz.Actor {
	if a.Kind == authz.ActorSystem && a.UserID == uuid.Nil {
		a.UserID = user
	}
	return a
}

// ---- chat -------------------------------------------------------------------

func (s *service) resolveChat(ctx context.Context, a authz.Actor, req IndexRequest) (source, func(context.Context) ([]draft, error), error) {
	if req.WindowStart == nil {
		return source{}, nil, fmt.Errorf("knowledge: index chat %s: no window start: %w", req.SourceID, errs.ErrInvalid)
	}
	if err := checkChannel(req, req.SourceID); err != nil {
		return source{}, nil, err
	}
	if err := s.canIndex(ctx, a, req.SourceID); err != nil {
		return source{}, nil, err
	}
	w := windowAt(*req.WindowStart)
	src := source{kind: kindChat, id: req.SourceID, channel: req.SourceID, window: &w}
	return src, func(ctx context.Context) ([]draft, error) {
		msgs, err := s.windowMessages(ctx, a, src.channel, w)
		if err != nil {
			return nil, err
		}
		author, err := s.authorNames(ctx, a.WorkspaceID, msgs)
		if err != nil {
			return nil, err
		}
		return chatDrafts(msgs, author), nil
	}, nil
}

// windowMessages returns the messages of channel sent in w, oldest first. The
// history is paged newest first, so it reads back until it reaches a page that begins
// before the window.
func (s *service) windowMessages(ctx context.Context, a authz.Actor, channel uuid.UUID, w window) ([]*chatv1.Message, error) {
	var msgs []*chatv1.Message
	for before := int64(0); ; {
		page, err := s.chat.ListMessages(ctx, a, channel, before, chatPage)
		if err != nil {
			return nil, fmt.Errorf("knowledge: read channel %s: %w", channel, err)
		}
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			if at := m.GetCreatedAt().AsTime(); !at.Before(w.start) && at.Before(w.end) {
				msgs = append(msgs, m)
			}
		}
		oldest := page[len(page)-1]
		if oldest.GetCreatedAt().AsTime().Before(w.start) {
			break
		}
		before = oldest.GetSeq()
	}
	slices.Reverse(msgs)
	return msgs, nil
}

// authorNames returns how a message's author is named: a user by the directory's
// name for them, an agent as "assistant".
func (s *service) authorNames(ctx context.Context, ws uuid.UUID, msgs []*chatv1.Message) (func(*chatv1.Message) string, error) {
	var ids []string
	for _, m := range msgs {
		if m.GetAuthorKind() == chatv1.AuthorKind_AUTHOR_KIND_USER && !slices.Contains(ids, m.GetAuthorId()) {
			ids = append(ids, m.GetAuthorId())
		}
	}
	names := make(map[string]string, len(ids))
	if len(ids) > 0 {
		var rows []store.ListUserNamesRow
		err := s.db.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) (err error) {
			rows, err = store.New(tx).ListUserNames(ctx, ids)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("knowledge: read author names: %w", err)
		}
		for _, r := range rows {
			names[r.ID.String()] = strings.TrimSpace(r.Name)
		}
	}
	return func(m *chatv1.Message) string {
		if m.GetAuthorKind() == chatv1.AuthorKind_AUTHOR_KIND_AGENT {
			return authorAssistant
		}
		if name := names[m.GetAuthorId()]; name != "" {
			return name
		}
		return authorUnknown
	}, nil
}

// ---- meetings ---------------------------------------------------------------

// resolveMeeting reads the meeting's row, then authorises the actor on the channel the
// row names. That order is for the indexer, a system actor that needs no membership; a
// caller that is a person would have to authorise first, or learn that the meeting exists
// before being refused (see Index).
func (s *service) resolveMeeting(ctx context.Context, a authz.Actor, req IndexRequest) (source, func(context.Context) ([]draft, error), error) {
	var row store.GetMeetingSourceRow
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		row, err = store.New(tx).GetMeetingSource(ctx, req.SourceID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return source{}, nil, fmt.Errorf("knowledge: meeting %s: %w", req.SourceID, errs.ErrNotFound)
	}
	if err != nil {
		return source{}, nil, fmt.Errorf("knowledge: read meeting %s: %w", req.SourceID, err)
	}
	if err := checkChannel(req, row.ChannelID); err != nil {
		return source{}, nil, err
	}
	if err := s.canIndex(ctx, a, row.ChannelID); err != nil {
		return source{}, nil, err
	}
	if row.Status != statusReady {
		return source{}, nil, fmt.Errorf("knowledge: meeting %s is %s, not ready: %w", row.ID, row.Status, errs.ErrConflict)
	}
	src := source{kind: kindMeeting, id: row.ID, channel: row.ChannelID}
	return src, func(ctx context.Context) ([]draft, error) {
		entries, err := s.meetings.TranscriptEntries(ctx, withOrigin(a, row.OwnerUserID), row.ID)
		if err != nil {
			return nil, fmt.Errorf("knowledge: read transcript of meeting %s: %w", row.ID, err)
		}
		drafts := entryDrafts(row.Title, entries)
		sum, err := s.meetingSummary(ctx, a.WorkspaceID, row.ID)
		if err != nil {
			return nil, err
		}
		return append(drafts, summaryDrafts(row.Title, sum)...), nil
	}, nil
}

// meetingSummary reads the summary of a meeting; a meeting without one has an empty
// summary, and so no summary chunk. Decisions or open questions that cannot be read are
// left out of the chunk with a warning that names the meeting: the summary and the
// transcript are still worth indexing.
func (s *service) meetingSummary(ctx context.Context, ws, id uuid.UUID) (meetingSummary, error) {
	var row store.GetMeetingSummaryRow
	err := s.db.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) (err error) {
		row, err = store.New(tx).GetMeetingSummary(ctx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return meetingSummary{}, nil
	}
	if err != nil {
		return meetingSummary{}, fmt.Errorf("knowledge: read summary of meeting %s: %w", id, err)
	}
	return meetingSummary{
		Summary:       row.Summary,
		Decisions:     itemTexts(id, "decisions", row.Decisions),
		OpenQuestions: itemTexts(id, "open_questions", row.OpenQuestions),
	}, nil
}

// itemTexts reads the text of each item of a stored list of decisions or open
// questions, [{text, entry_refs}]. A list that cannot be read has none, and the warning
// says which list of which meeting it was.
func itemTexts(meeting uuid.UUID, list string, raw json.RawMessage) []string {
	var items []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		slog.Warn("knowledge: summary items are unreadable and are not indexed", "meeting_id", meeting, "list", list, "error", err)
		return nil
	}
	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = it.Text
	}
	return texts
}

// ---- documents --------------------------------------------------------------

// resolveDoc reads the file's row, then authorises the actor on the channel the row names.
// That order is for the indexer, a system actor that needs no membership; a caller that is
// a person would have to authorise first, or learn that the file exists before being
// refused (see Index).
func (s *service) resolveDoc(ctx context.Context, a authz.Actor, req IndexRequest) (source, func(context.Context) ([]draft, error), error) {
	var row store.GetFileSourceRow
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		row, err = store.New(tx).GetFileSource(ctx, req.SourceID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return source{}, nil, fmt.Errorf("knowledge: file %s: %w", req.SourceID, errs.ErrNotFound)
	}
	if err != nil {
		return source{}, nil, fmt.Errorf("knowledge: read file %s: %w", req.SourceID, err)
	}
	if err := checkChannel(req, row.ChannelID); err != nil {
		return source{}, nil, err
	}
	if err := s.canIndex(ctx, a, row.ChannelID); err != nil {
		return source{}, nil, err
	}
	src := source{kind: kindDoc, id: row.ID, channel: row.ChannelID}
	return src, func(ctx context.Context) ([]draft, error) {
		pages, err := s.docs.ExtractPages(ctx, withOrigin(a, row.UploadedBy), row.ID)
		if err != nil {
			return nil, fmt.Errorf("knowledge: read pages of file %s: %w", row.ID, err)
		}
		return pageDrafts(row.Name, pages), nil
	}, nil
}

// ---- writing ----------------------------------------------------------------

// chunk is a draft with the identity and the checksum it is stored under.
type chunk struct {
	draft
	id  uuid.UUID
	sha string
}

// write makes the stored chunks of src what drafts say: it embeds and stores those
// that are new or whose text or model changed, in batches, and then removes the rest.
func (s *service) write(ctx context.Context, ws uuid.UUID, src source, drafts []draft) error {
	model := s.llm.EmbeddingModel()
	chunks := make([]chunk, len(drafts))
	keep := make([]string, len(drafts)) // never nil: a nil array keeps nothing and deletes nothing
	for i, d := range drafts {
		sum := sha256.Sum256([]byte(d.content))
		chunks[i] = chunk{draft: d, id: pieceID(ws, src.kind, src.id, d.anchor, d.piece), sha: hex.EncodeToString(sum[:])}
		keep[i] = chunks[i].id.String()
	}

	var stored []store.ListChunkStatesRow
	err := s.db.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) (err error) {
		stored, err = store.New(tx).ListChunkStates(ctx, keep)
		return err
	})
	if err != nil {
		return fmt.Errorf("knowledge: read stored chunks of %s %s: %w", src.kind, src.id, err)
	}
	upToDate := make(map[uuid.UUID]store.ListChunkStatesRow, len(stored))
	for _, r := range stored {
		upToDate[r.ID] = r
	}
	var todo []chunk
	for _, c := range chunks {
		if r, ok := upToDate[c.id]; !ok || r.ContentSha256 != c.sha || r.EmbeddingModel != model {
			todo = append(todo, c)
		}
	}

	for batch := range slices.Chunk(todo, embedBatch) {
		if err := s.storeBatch(ctx, ws, src, model, batch); err != nil {
			return err
		}
		bus.KeepAlive(ctx)
	}

	var removed int64
	err = s.db.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) (err error) {
		q := store.New(tx)
		if src.window != nil {
			removed, err = q.DeleteStaleWindowChunks(ctx, store.DeleteStaleWindowChunksParams{
				SourceID:    src.id,
				WindowStart: pgtype.Timestamptz{Time: src.window.start, Valid: true},
				WindowEnd:   pgtype.Timestamptz{Time: src.window.end, Valid: true},
				Keep:        keep,
			})
			return err
		}
		removed, err = q.DeleteStaleSourceChunks(ctx, store.DeleteStaleSourceChunksParams{SourceKind: src.kind, SourceID: src.id, Keep: keep})
		return err
	})
	if err != nil {
		return fmt.Errorf("knowledge: remove stale chunks of %s %s: %w", src.kind, src.id, err)
	}
	slog.Debug("knowledge: indexed", "workspace_id", ws, "source_kind", src.kind, "source_id", src.id,
		"chunks", len(chunks), "embedded", len(todo), "removed", removed)
	return nil
}

// storeBatch embeds the chunks of one batch with one call and stores them in one
// transaction.
func (s *service) storeBatch(ctx context.Context, ws uuid.UUID, src source, model string, batch []chunk) error {
	texts := make([]string, len(batch))
	for i, c := range batch {
		texts[i] = c.content
	}
	vecs, err := s.embed(ctx, texts)
	if err != nil {
		return err
	}
	params := make([]store.UpsertChunkParams, len(batch))
	for i, c := range batch {
		meta, err := json.Marshal(c.meta)
		if err != nil {
			return fmt.Errorf("knowledge: encode source meta: %w", err)
		}
		params[i] = store.UpsertChunkParams{
			ID: c.id, WorkspaceID: ws, ChannelID: new(src.channel), SourceKind: src.kind, SourceID: src.id,
			SourceAnchor: c.anchor, Lang: chunkLang(c.content), Content: c.content, ContentSha256: c.sha,
			SourceMeta: meta, EmbeddingModel: model, Embedding: pgvector.NewVector(vecs[i]),
		}
	}
	err = s.db.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		for _, p := range params {
			if err := q.UpsertChunk(ctx, p); err != nil {
				return fmt.Errorf("upsert chunk %s: %w", p.ID, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("knowledge: store %d chunks of %s %s: %w", len(batch), src.kind, src.id, err)
	}
	return nil
}

// embed returns one vector per text, embedded as documents, under a deadline of its
// own: the adapter has none, and a model that hangs would hold the job for ever.
func (s *service) embed(ctx context.Context, texts []string) ([][]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	vecs, err := s.llm.Embed(ctx, texts, llm.EmbedDocument)
	if err != nil {
		return nil, fmt.Errorf("knowledge: embed %d chunks: %w", len(texts), err)
	}
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("knowledge: embed %d chunks: the model returned %d vectors", len(texts), len(vecs))
	}
	for _, v := range vecs {
		if len(v) != embedDim {
			return nil, fmt.Errorf("knowledge: embed %d chunks: a vector has %d dimensions, the index holds %d", len(texts), len(v), embedDim)
		}
	}
	return vecs, nil
}
