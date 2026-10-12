// Package seed fills a workspace with a small synthetic demo corpus: three channels (one
// of them private), messages in Thai and English, a text document, and a finished
// meeting with its summary and draft tasks. All of it goes through the same services
// and write paths as real data, and is indexed through the same knowledge.Index, so what
// a demo shows is what the product produces. Everything in it is invented.
//
// Running a seed again is a no-op. The services choose the ids of their rows, so a run
// finds what an earlier one made by the natural key of each thing: a channel by its
// name, a message by a client_msg_id derived from the workspace, channel and position
// (UUIDv5), the document by its file name in its channel, and the meeting by an id
// derived the same way. A run that stopped half way is finished by the next one.
//
// The workspace is that of a Google domain (its members sign in with hd) or, with no
// domain, the one workspace without a domain: its members are personal Google accounts
// that sign in because the seed listed them (identity.EnsureSeedWorkspace).
package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	"kiwi/internal/chat"
	"kiwi/internal/docs"
	"kiwi/internal/identity"
	"kiwi/internal/knowledge"
	"kiwi/internal/meetings"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/blob"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/llm"
	"kiwi/internal/tasks"
)

// namespace derives the ids of seeded things (UUIDv5), so they are the same on every run.
var namespace = uuid.NewSHA1(uuid.NameSpaceDNS, []byte("seed.kiwi.local"))

const (
	// analyzeTimeout bounds the model's summary of the document; the adapter has no
	// timeout of its own.
	analyzeTimeout = 3 * time.Minute
	// indexTimeout bounds the indexing of one source, which embeds on a CPU locally.
	indexTimeout = 5 * time.Minute
	// historyPage is how many messages of a channel are looked through for the document.
	historyPage = 200
)

// Deps are the ports Run builds the domain services from, as the composition root does.
type Deps struct {
	DB    *db.DB
	Cache *cache.Cache
	Blob  blob.Store
	// LLM summarizes the document and embeds what is indexed.
	LLM llm.Client
}

// Options are what to seed. Domain is the workspace's Google domain, created if it does
// not exist; empty, it is the one workspace without a domain, for a team whose Google
// accounts carry no hd claim (personal addresses), whose members sign in because they
// are listed here. MemberEmails are its members, in order: the first one owns the
// channels and is the only member of the private one, the first two write the messages
// and are named as owners of the drafts (a single member writes every message and owns
// the drafts that have an owner).
// Each is a placeholder user until that email signs in. With
// IndexInline Run analyses the document and indexes everything itself, and the workspace
// can be searched when it returns; without it Run leaves both to the doc.analyze and
// embed.index jobs, which pick them up from the outbox when the stack is running.
type Options struct {
	Domain       string
	MemberEmails []string
	IndexInline  bool
}

// clean checks Options and returns the domain (empty for the workspace without one) and
// the emails the way identity keeps them.
func (o Options) clean() (domain string, emails []string, err error) {
	domain = strings.ToLower(strings.TrimSpace(o.Domain))
	if len(o.MemberEmails) == 0 {
		return "", nil, errors.New("seed: at least one member is required: the first one owns the channels")
	}
	for _, raw := range o.MemberEmails {
		email := strings.ToLower(strings.TrimSpace(raw))
		if local, host, ok := strings.Cut(email, "@"); !ok || local == "" || host == "" {
			return "", nil, fmt.Errorf("seed: %q is not an email address", raw)
		}
		if slices.Contains(emails, email) {
			return "", nil, fmt.Errorf("seed: member %s is listed twice", email)
		}
		emails = append(emails, email)
	}
	return domain, emails, nil
}

// Run seeds the workspace of o.Domain, or the one without a domain. See Options and the
// package comment.
func Run(ctx context.Context, d Deps, o Options) error {
	if d.DB == nil || d.Cache == nil || d.Blob == nil || d.LLM == nil {
		return errors.New("seed: Deps needs DB, Cache, Blob and LLM")
	}
	domain, emails, err := o.clean()
	if err != nil {
		return err
	}
	c, err := loadCorpus()
	if err != nil {
		return err
	}

	started := time.Now()
	ws, ids, err := identity.EnsureSeedWorkspace(ctx, d.DB, domain, emails)
	if err != nil {
		return err
	}
	s := newSeeder(d, ws, ids, emails)
	slog.InfoContext(ctx, "seeding the demo workspace", "workspace_id", ws, "domain", domain, "members", len(ids), "index_inline", o.IndexInline)

	channels := make(map[string]uuid.UUID, len(c.channels))
	var posts []post
	for _, spec := range c.channels {
		id, err := s.ensureChannel(ctx, spec)
		if err != nil {
			return err
		}
		channels[spec.Name] = id
		at, err := s.postMessages(ctx, id, spec)
		if err != nil {
			return err
		}
		for _, t := range at {
			posts = append(posts, post{channel: id, at: t})
		}
	}
	fileID, err := s.ensureDocument(ctx, channels[documentChannel], c.document)
	if err != nil {
		return err
	}
	meetingID, err := s.importMeeting(ctx, channels[meetingChannel], c.meeting)
	if err != nil {
		return err
	}
	if !o.IndexInline {
		slog.InfoContext(ctx, "seeded; the workers analyse the document and index the corpus", "workspace_id", ws, "took", time.Since(started).Round(time.Millisecond))
		return nil
	}

	if err := s.analyze(ctx, fileID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "the document is analysed", "file_id", fileID, "took", time.Since(started).Round(time.Millisecond))
	if err := s.index(ctx, posts, channels[documentChannel], fileID, channels[meetingChannel], meetingID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "seeded and indexed", "workspace_id", ws, "took", time.Since(started).Round(time.Millisecond))
	return nil
}

// post is when a seeded message was sent, which is what names the chat window to index.
type post struct {
	channel uuid.UUID
	at      time.Time
}

// seeder is one run: the workspace, its members and the services it writes through.
type seeder struct {
	d     Deps
	ws    uuid.UUID
	ids   []uuid.UUID
	names []string

	// owner is the first member acting as themself, which is who makes channels, adds
	// members and uploads. job is the same user as the background jobs act for them: it
	// posts the messages, naming their authors, and drafts the tasks. indexer has no user.
	owner, job, indexer authz.Actor

	chat        chat.Service
	docs        docs.Service
	tasks       tasks.Service
	docDeps     docs.Deps
	meetingDeps meetings.Deps
	knowledge   knowledge.Service
}

func newSeeder(d Deps, ws uuid.UUID, ids []uuid.UUID, emails []string) *seeder {
	az := authz.New(chat.NewMembership(d.DB, d.Cache))
	cs := chat.New(chat.Deps{DB: d.DB, Authz: az, Cache: d.Cache})
	docDeps := docs.Deps{DB: d.DB, Authz: az, Blob: d.Blob, Chat: cs, LLM: d.LLM}
	docSvc := docs.New(docDeps)
	meetingDeps := meetings.Deps{DB: d.DB, Authz: az, Chat: cs}

	s := &seeder{
		d: d, ws: ws, ids: ids, names: make([]string, len(emails)),
		owner:   authz.Actor{Kind: authz.ActorUser, WorkspaceID: ws, UserID: ids[0]},
		job:     authz.Actor{Kind: authz.ActorSystem, WorkspaceID: ws, UserID: ids[0]},
		indexer: authz.Actor{Kind: authz.ActorSystem, WorkspaceID: ws},
		chat:    cs, docs: docSvc, docDeps: docDeps, meetingDeps: meetingDeps,
		tasks: tasks.New(tasks.Deps{DB: d.DB, Authz: az, MemberChannels: cs.MemberChannelIDs, ChannelMembers: cs.ListMembers}),
		knowledge: knowledge.New(knowledge.Deps{
			DB: d.DB, LLM: d.LLM, Authz: az, Chat: cs, Meetings: meetings.New(meetingDeps), Docs: docSvc,
		}),
	}
	for i, email := range emails {
		s.names[i] = speakerName(email)
	}
	return s
}

// speakerName is how a member is named in the transcript: the name identity gives a
// placeholder user (the part of the address before the @), with a capital.
func speakerName(email string) string {
	local, _, _ := strings.Cut(email, "@")
	r, n := utf8.DecodeRuneInString(local)
	return string(unicode.ToUpper(r)) + local[n:]
}

// member is the user a corpus index points at, wrapping round when fewer members were
// listed than the corpus has voices.
func (s *seeder) member(i int) int { return i % len(s.ids) }

// derived is the UUIDv5 of a seeded thing of this workspace.
func (s *seeder) derived(parts ...string) uuid.UUID {
	return uuid.NewSHA1(namespace, []byte(s.ws.String()+"/"+strings.Join(parts, "/")))
}

// ensureChannel finds the channel by its name or makes it, with the owner as its first
// member. Members join a public channel; a private one keeps only the owner.
func (s *seeder) ensureChannel(ctx context.Context, spec channelSpec) (uuid.UUID, error) {
	kind := chatv1.ChannelKind_CHANNEL_KIND_PUBLIC
	if spec.Kind == kindPrivate {
		kind = chatv1.ChannelKind_CHANNEL_KIND_PRIVATE
	}
	visible, err := s.chat.ListChannels(ctx, s.owner)
	if err != nil {
		return uuid.Nil, fmt.Errorf("seed: list channels: %w", err)
	}
	var id uuid.UUID
	for _, ch := range visible {
		if ch.GetName() != spec.Name {
			continue
		}
		if ch.GetKind() != kind {
			return uuid.Nil, fmt.Errorf("seed: channel #%s exists and is %s, the corpus wants it %s", spec.Name, ch.GetKind(), kind)
		}
		if id, err = uuid.Parse(ch.GetId()); err != nil {
			return uuid.Nil, fmt.Errorf("seed: channel id %q: %w", ch.GetId(), err)
		}
		break
	}
	if id == uuid.Nil {
		created, err := s.chat.CreateChannel(ctx, s.owner, &chatv1.CreateChannelRequest{Name: spec.Name, Kind: kind})
		if err != nil {
			return uuid.Nil, fmt.Errorf("seed: create channel #%s: %w", spec.Name, err)
		}
		if id, err = uuid.Parse(created.GetId()); err != nil {
			return uuid.Nil, fmt.Errorf("seed: channel id %q: %w", created.GetId(), err)
		}
	}
	if kind == chatv1.ChannelKind_CHANNEL_KIND_PRIVATE {
		return id, nil
	}
	// Joining is a no-op for the owner of a channel just made; it covers a channel of that
	// name that somebody else made.
	if err := s.chat.JoinChannel(ctx, s.owner, id); err != nil {
		return uuid.Nil, fmt.Errorf("seed: join #%s: %w", spec.Name, err)
	}
	for _, member := range s.ids[1:] {
		if err := s.chat.AddMember(ctx, s.owner, id, member); err != nil {
			return uuid.Nil, fmt.Errorf("seed: add a member to #%s: %w", spec.Name, err)
		}
	}
	return id, nil
}

// postMessages posts the channel's messages as their authors and returns when each was
// sent. A message that was posted before comes back as it was, so a second run returns
// the times of the first.
func (s *seeder) postMessages(ctx context.Context, channel uuid.UUID, spec channelSpec) ([]time.Time, error) {
	at := make([]time.Time, 0, len(spec.Messages))
	for i, m := range spec.Messages {
		msg, err := s.chat.PostMessage(ctx, s.job, &chatv1.SendMessageRequest{
			ChannelId:   channel.String(),
			ClientMsgId: "seed-" + s.derived("message", spec.Name, strconv.Itoa(i)).String(),
			Body:        m.Body,
			AuthorKind:  chatv1.AuthorKind_AUTHOR_KIND_USER.Enum(),
			AuthorId:    proto.String(s.ids[s.member(m.Author)].String()),
		})
		if err != nil {
			return nil, fmt.Errorf("seed: post message %d of #%s: %w", i+1, spec.Name, err)
		}
		at = append(at, msg.GetCreatedAt().AsTime())
	}
	return at, nil
}

// index indexes what was seeded through knowledge.Index, as the embed.index job does:
// every chat window that has a seeded message, the document and the meeting. Index skips
// what is already stored, so the windows are not worked out here: one call per message
// finds its window, and the calls after the first of a window embed nothing.
func (s *seeder) index(ctx context.Context, posts []post, docChannel, fileID, meetingChannel, meetingID uuid.UUID) error {
	requests := make([]knowledge.IndexRequest, 0, len(posts)+2)
	for _, p := range posts {
		requests = append(requests, knowledge.IndexRequest{SourceKind: "chat", SourceID: p.channel, ChannelID: &p.channel, WindowStart: &p.at})
	}
	requests = append(requests,
		knowledge.IndexRequest{SourceKind: "doc", SourceID: fileID, ChannelID: &docChannel},
		knowledge.IndexRequest{SourceKind: "meeting", SourceID: meetingID, ChannelID: &meetingChannel},
	)
	for _, req := range requests {
		if err := s.indexOne(ctx, req); err != nil {
			return fmt.Errorf("seed: index %s %s: %w", req.SourceKind, req.SourceID, err)
		}
	}
	return nil
}

func (s *seeder) indexOne(ctx context.Context, req knowledge.IndexRequest) error {
	ctx, cancel := context.WithTimeout(ctx, indexTimeout)
	defer cancel()
	return s.knowledge.Index(ctx, s.indexer, req)
}
