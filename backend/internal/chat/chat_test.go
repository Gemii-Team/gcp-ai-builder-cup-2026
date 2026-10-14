package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	"kiwi/gen/kiwi/chat/v1/chatv1connect"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/testkit/containers"
	"kiwi/internal/testkit/dbtest"
)

// One plain Valkey serves the package. Every test works on channels it creates
// itself, so the members keys of one test never meet another's.
var (
	valkeyOnce sync.Once
	valkeyURL  string
	valkeyErr  error
	valkeyCtr  testcontainers.Container
)

func TestMain(m *testing.M) {
	code := m.Run()
	if valkeyCtr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = valkeyCtr.Terminate(ctx)
		cancel()
	}
	os.Exit(code)
}

func bootValkey() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ctr, err := containers.Start(ctx, "valkey/valkey:8",
		testcontainers.WithLogger(log.New(io.Discard, "", 0)),
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithCmd("valkey-server", "--save", "", "--appendonly", "no"),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")),
	)
	if err != nil {
		return "", fmt.Errorf("start valkey (is the Docker daemon reachable?): %w", err)
	}
	valkeyCtr = ctr
	host, err := ctr.Host(ctx)
	if err != nil {
		return "", err
	}
	port, err := ctr.MappedPort(ctx, "6379/tcp")
	if err != nil {
		return "", err
	}
	return "redis://" + host + ":" + port.Port(), nil
}

// ---- fixture ----------------------------------------------------------------

// fixture is one migrated database, one Valkey and the chat service over them,
// with a workspace holding Alice, Bob and Carol.
type fixture struct {
	t     *testing.T
	env   *dbtest.Env
	db    *db.DB
	cache *cache.Valkey
	svc   Service

	ws                uuid.UUID
	alice, bob, carol uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	valkeyOnce.Do(func() { valkeyURL, valkeyErr = bootValkey() })
	if valkeyErr != nil {
		t.Fatalf("valkey: %v", valkeyErr)
	}
	env := dbtest.Start(t)
	d, err := db.Open(t.Context(), config.Config{DatabaseURL: env.AppURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(d.Close)
	c, err := cache.OpenValkey(t.Context(), valkeyURL, cache.Options{})
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	f := &fixture{t: t, env: env, db: d, cache: c}
	f.svc = New(Deps{
		DB:         d,
		Authz:      authz.New(NewMembership(d, c)),
		Cache:      c,
		KnownAgent: func(name string) bool { return name == "assistant" },
	})
	f.ws = f.seedWorkspace("acme.example")
	f.alice = f.seedUser(f.ws, "Alice")
	f.bob = f.seedUser(f.ws, "Bob")
	f.carol = f.seedUser(f.ws, "Carol")
	return f
}

// asOwner runs fn as kiwi_owner in a transaction bound to ws: the tests seed and
// inspect tenant rows this way and never through kiwi_system.
func (f *fixture) asOwner(ws uuid.UUID, fn func(ctx context.Context, tx pgx.Tx)) {
	f.t.Helper()
	ctx := f.t.Context()
	conn, err := pgx.Connect(ctx, f.env.OwnerURL)
	if err != nil {
		f.t.Fatalf("connect as owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('kiwi.workspace_id', $1, true)`, ws.String()); err != nil {
		f.t.Fatal(err)
	}
	fn(ctx, tx)
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) exec(ws uuid.UUID, sql string, args ...any) {
	f.t.Helper()
	f.asOwner(ws, func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			f.t.Fatalf("%s: %v", sql, err)
		}
	})
}

func (f *fixture) scalar(ws uuid.UUID, dst any, sql string, args ...any) {
	f.t.Helper()
	f.asOwner(ws, func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			f.t.Fatalf("%s: %v", sql, err)
		}
	})
}

func (f *fixture) seedWorkspace(domain string) uuid.UUID {
	f.t.Helper()
	ws := uuid.New()
	f.exec(ws, `INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, $2, $2)`, ws, domain)
	return ws
}

func (f *fixture) seedUser(ws uuid.UUID, name string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	email := strings.ToLower(name) + "@example.test"
	f.exec(ws, `INSERT INTO identity.users (id, workspace_id, google_sub, email, name) VALUES ($1, $2, $3, $4, $5)`,
		id, ws, "sub-"+id.String(), email, name)
	return id
}

// channel creates a channel of kind in the fixture workspace; the first member is
// its owner.
func (f *fixture) channel(kind string, members ...uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(f.ws, `INSERT INTO chat.channels (id, workspace_id, name, kind) VALUES ($1, $2, $3, $4)`,
		id, f.ws, "ch-"+id.String()[:8], kind)
	for i, m := range members {
		role := "member"
		if i == 0 {
			role = "owner"
		}
		f.exec(f.ws, `INSERT INTO chat.channel_members (channel_id, user_id, workspace_id, role) VALUES ($1, $2, $3, $4)`,
			id, m, f.ws, role)
	}
	return id
}

// seedMessages writes n messages with seq 1..n straight into the table, so a test
// can start from a long history without posting every message.
func (f *fixture) seedMessages(channel, author uuid.UUID, n int) {
	f.t.Helper()
	f.exec(f.ws, `
		INSERT INTO chat.messages (id, workspace_id, channel_id, seq, author_kind, author_id, body, client_msg_id)
		SELECT gen_random_uuid(), $1, $2, g, 'user', $3, 'seed ' || g, 'seed-' || g
		FROM generate_series(1, $4::int) AS g`, f.ws, channel, author, n)
	f.exec(f.ws, `UPDATE chat.channels SET last_seq = $2 WHERE id = $1`, channel, n)
}

func (f *fixture) user(id uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorUser, WorkspaceID: f.ws, UserID: id}
}

func (f *fixture) agentFor(id uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorAgent, WorkspaceID: f.ws, UserID: uuid.New(), OnBehalfOf: id, Agent: "assistant"}
}

func (f *fixture) system(origin uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorSystem, WorkspaceID: f.ws, UserID: origin}
}

// wsCtx is a context carrying the workspace, as a Membership lookup needs.
func (f *fixture) wsCtx() context.Context { return authz.WithWorkspace(f.t.Context(), f.ws) }

func (f *fixture) send(a authz.Actor, channel uuid.UUID, clientMsgID, body string) *chatv1.Message {
	f.t.Helper()
	m, err := f.svc.PostMessage(f.t.Context(), a, &chatv1.SendMessageRequest{
		ChannelId: channel.String(), ClientMsgId: clientMsgID, Body: body,
	})
	if err != nil {
		f.t.Fatalf("PostMessage(%s): %v", clientMsgID, err)
	}
	return m
}

func (f *fixture) messageCount(channel uuid.UUID) int {
	f.t.Helper()
	var n int
	f.scalar(f.ws, &n, `SELECT count(*) FROM chat.messages WHERE channel_id = $1`, channel)
	return n
}

func (f *fixture) lastSeq(channel uuid.UUID) int64 {
	f.t.Helper()
	var n int64
	f.scalar(f.ws, &n, `SELECT last_seq FROM chat.channels WHERE id = $1`, channel)
	return n
}

// outbox returns the payloads enqueued on topic, oldest first.
func (f *fixture) outbox(topic string) []string {
	f.t.Helper()
	var out []string
	f.asOwner(f.ws, func(ctx context.Context, tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT payload::text FROM platform.outbox WHERE topic = $1 ORDER BY id`, topic)
		if err != nil {
			f.t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				f.t.Fatal(err)
			}
			out = append(out, p)
		}
		if err := rows.Err(); err != nil {
			f.t.Fatal(err)
		}
	})
	return out
}

func decode[M proto.Message](t *testing.T, payload string, into M) M {
	t.Helper()
	if err := protojson.Unmarshal([]byte(payload), into); err != nil {
		t.Fatalf("decode %q: %v", payload, err)
	}
	return into
}

func seqs(msgs []*chatv1.Message) []int64 {
	out := make([]int64, len(msgs))
	for i, m := range msgs {
		out[i] = m.GetSeq()
	}
	return out
}

func wantErr(t *testing.T, err, target error, what string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("%s: err = %v, want %v", what, err, target)
	}
}

// thai is n runes of Thai consonants with combining vowel and tone marks, which
// are three bytes each and several runes per visible character.
func thai(n int) string {
	unit := []rune("กิ่") // consonant + vowel sign + tone mark
	out := make([]rune, 0, n)
	for len(out) < n {
		out = append(out, unit[len(out)%len(unit)])
	}
	return string(out)
}

// ---- messages ---------------------------------------------------------------

func TestPostMessage_AssignsGaplessSeqUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)

	const n = 50
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		got  []int64
		errC = make(chan error, n)
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := f.svc.PostMessage(t.Context(), f.user(f.alice), &chatv1.SendMessageRequest{
				ChannelId: ch.String(), ClientMsgId: fmt.Sprintf("c-%d", i), Body: fmt.Sprintf("message %d", i),
			})
			if err != nil {
				errC <- err
				return
			}
			mu.Lock()
			got = append(got, m.GetSeq())
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errC)
	for err := range errC {
		t.Fatalf("PostMessage: %v", err)
	}

	slices.Sort(got)
	want := make([]int64, n)
	for i := range want {
		want[i] = int64(i + 1)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("seqs = %v, want 1..%d with no gap or duplicate", got, n)
	}
	if head := f.lastSeq(ch); head != n {
		t.Fatalf("channel last_seq = %d, want %d", head, n)
	}
	if rows := f.messageCount(ch); rows != n {
		t.Fatalf("messages stored = %d, want %d", rows, n)
	}
}

func TestPostMessage_IdempotentOnClientMsgID_ReturnsOriginal(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)

	first := f.send(f.user(f.alice), ch, "cm-1", "hello")
	again := f.send(f.user(f.alice), ch, "cm-1", "hello, edited")

	if again.GetId() != first.GetId() || again.GetSeq() != first.GetSeq() || again.GetBody() != "hello" {
		t.Fatalf("retry returned id=%s seq=%d body=%q, want the original id=%s seq=%d body=%q",
			again.GetId(), again.GetSeq(), again.GetBody(), first.GetId(), first.GetSeq(), first.GetBody())
	}
	if !proto.Equal(first.GetCreatedAt(), again.GetCreatedAt()) {
		t.Errorf("retry created_at = %v, want the original %v", again.GetCreatedAt(), first.GetCreatedAt())
	}
	if rows := f.messageCount(ch); rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}
	if head := f.lastSeq(ch); head != 1 {
		t.Fatalf("last_seq = %d after a retry, want 1: a retry must not burn a seq", head)
	}
	if evs := f.outbox(events.TopicChatMessageCreated); len(evs) != 1 {
		t.Fatalf("chat.message.created rows = %d, want 1: a retry announces nothing", len(evs))
	}

	// The same client id from another author is another message.
	if err := f.svc.AddMember(t.Context(), f.user(f.alice), ch, f.bob); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	other := f.send(f.user(f.bob), ch, "cm-1", "bob's own")
	if other.GetId() == first.GetId() || other.GetSeq() != 2 {
		t.Fatalf("bob's message = %s seq %d, want a new message at seq 2", other.GetId(), other.GetSeq())
	}
}

func TestPostMessage_ConcurrentDuplicatesCollapseWithoutBurningSeq(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)

	const n = 20
	var (
		wg  sync.WaitGroup
		got = make([]*chatv1.Message, n)
		bad = make([]error, n)
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], bad[i] = f.svc.PostMessage(t.Context(), f.user(f.alice), &chatv1.SendMessageRequest{
				ChannelId: ch.String(), ClientMsgId: "same", Body: fmt.Sprintf("attempt %d", i),
			})
		}()
	}
	wg.Wait()
	for i := range n {
		if bad[i] != nil {
			t.Fatalf("attempt %d: %v", i, bad[i])
		}
		if got[i].GetId() != got[0].GetId() || got[i].GetSeq() != 1 {
			t.Fatalf("attempt %d returned id=%s seq=%d, want the one message %s at seq 1", i, got[i].GetId(), got[i].GetSeq(), got[0].GetId())
		}
	}
	if rows, head := f.messageCount(ch), f.lastSeq(ch); rows != 1 || head != 1 {
		t.Fatalf("rows = %d, last_seq = %d, want 1 and 1", rows, head)
	}
	if evs := f.outbox(events.TopicChatMessageCreated); len(evs) != 1 {
		t.Fatalf("chat.message.created rows = %d, want 1", len(evs))
	}
}

func TestPostMessage_ClientMsgIDOfAnotherChannel_Conflicts(t *testing.T) {
	f := newFixture(t)
	a := f.channel("public", f.alice)
	b := f.channel("public", f.alice)

	f.send(f.user(f.alice), a, "cm-1", "in a")
	_, err := f.svc.PostMessage(t.Context(), f.user(f.alice), &chatv1.SendMessageRequest{
		ChannelId: b.String(), ClientMsgId: "cm-1", Body: "in b",
	})
	wantErr(t, err, errs.ErrConflict, "reusing a client_msg_id in another channel")
	if rows, head := f.messageCount(b), f.lastSeq(b); rows != 0 || head != 0 {
		t.Fatalf("channel b has %d rows and last_seq %d after the refused post, want none", rows, head)
	}
}

func TestPostMessage_WritesOutboxInSameTx(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)

	t.Run("committed", func(t *testing.T) {
		var posted *chatv1.Message
		err := f.db.InWorkspace(t.Context(), f.ws, func(ctx context.Context, tx pgx.Tx) (err error) {
			posted, err = f.svc.PostMessageTx(ctx, tx, f.user(f.alice), &chatv1.SendMessageRequest{
				ChannelId: ch.String(), ClientMsgId: "tx-ok", Body: "committed with its event",
			})
			return err
		})
		if err != nil {
			t.Fatalf("PostMessageTx: %v", err)
		}
		evs := f.outbox(events.TopicChatMessageCreated)
		if len(evs) != 1 {
			t.Fatalf("chat.message.created rows = %d, want 1", len(evs))
		}
		ev := decode(t, evs[0], &eventsv1.ChatMessageCreated{})
		if ev.GetWorkspaceId() != f.ws.String() || ev.GetChannelId() != ch.String() {
			t.Fatalf("event workspace/channel = %s/%s, want %s/%s", ev.GetWorkspaceId(), ev.GetChannelId(), f.ws, ch)
		}
		if !proto.Equal(ev.GetMessage(), posted) {
			t.Fatalf("event message = %v, want the stored message %v", ev.GetMessage(), posted)
		}
	})

	t.Run("rolled back with the caller", func(t *testing.T) {
		boom := errors.New("the pipeline failed after posting")
		err := f.db.InWorkspace(t.Context(), f.ws, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := f.svc.PostMessageTx(ctx, tx, f.user(f.alice), &chatv1.SendMessageRequest{
				ChannelId: ch.String(), ClientMsgId: "tx-failed", Body: "never visible",
			}); err != nil {
				return err
			}
			return boom
		})
		wantErr(t, err, boom, "forced failure")
		if rows := f.messageCount(ch); rows != 1 {
			t.Fatalf("messages = %d, want only the committed one", rows)
		}
		if evs := f.outbox(events.TopicChatMessageCreated); len(evs) != 1 {
			t.Fatalf("outbox rows = %d, want only the committed one", len(evs))
		}
		if head := f.lastSeq(ch); head != 1 {
			t.Fatalf("last_seq = %d, want 1: the rolled-back post must not burn a seq", head)
		}
		// The next message takes the seq the failed one gave back.
		if next := f.send(f.user(f.alice), ch, "after", "next"); next.GetSeq() != 2 {
			t.Fatalf("next seq = %d, want 2", next.GetSeq())
		}
	})
}

func TestPostMessage_NonMemberForbidden(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice) // even a public channel needs membership to post

	_, err := f.svc.PostMessage(t.Context(), f.user(f.bob), &chatv1.SendMessageRequest{
		ChannelId: ch.String(), ClientMsgId: "x", Body: "let me in",
	})
	wantErr(t, err, errs.ErrForbidden, "non-member post")
	_, err = f.svc.PostMessage(t.Context(), f.user(f.bob), &chatv1.SendMessageRequest{
		ChannelId: uuid.NewString(), ClientMsgId: "x", Body: "nowhere",
	})
	wantErr(t, err, errs.ErrForbidden, "post to a channel that does not exist")
	_, err = f.svc.PostMessage(t.Context(), f.agentFor(f.bob), &chatv1.SendMessageRequest{
		ChannelId: ch.String(), ClientMsgId: "x", Body: "agent of a non-member", AuthorId: proto.String(authz.AgentID("assistant").String()),
	})
	wantErr(t, err, errs.ErrForbidden, "agent acting for a non-member")

	if rows, head := f.messageCount(ch), f.lastSeq(ch); rows != 0 || head != 0 {
		t.Fatalf("rows = %d, last_seq = %d after refused posts, want none", rows, head)
	}
	if evs := f.outbox(events.TopicChatMessageCreated); len(evs) != 0 {
		t.Fatalf("outbox rows = %d, want none", len(evs))
	}
}

func TestPostMessage_UserCannotForgeArtifactAttachmentsOrAuthor(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)

	req := &chatv1.SendMessageRequest{
		ChannelId: ch.String(), ClientMsgId: "forge", Body: "I am the assistant",
		Attachments: []*chatv1.FileRef{{FileId: uuid.NewString(), Name: "contract.pdf", Mime: "application/pdf", Size: 10}},
		Artifact:    &chatv1.Artifact{Kind: chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER, Ref: "ANS-1001", Title: "Forged answer"},
		AuthorKind:  chatv1.AuthorKind_AUTHOR_KIND_AGENT.Enum(),
		AuthorId:    proto.String(authz.AgentID("assistant").String()),
	}
	m, err := f.svc.PostMessage(t.Context(), f.user(f.alice), req)
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if m.GetAuthorKind() != chatv1.AuthorKind_AUTHOR_KIND_USER || m.GetAuthorId() != f.alice.String() {
		t.Fatalf("author = %s %s, want the actor's own user %s", m.GetAuthorKind(), m.GetAuthorId(), f.alice)
	}
	if m.GetArtifact() != nil || len(m.GetFiles()) != 0 {
		t.Fatalf("returned message kept artifact %v and files %v", m.GetArtifact(), m.GetFiles())
	}

	// What is stored and what is announced says the same.
	var kind, author, attachments string
	var noArtifact bool
	f.asOwner(f.ws, func(ctx context.Context, tx pgx.Tx) {
		err := tx.QueryRow(ctx, `SELECT author_kind, author_id::text, attachments::text, artifact IS NULL FROM chat.messages WHERE id = $1`,
			uuid.MustParse(m.GetId())).Scan(&kind, &author, &attachments, &noArtifact)
		if err != nil {
			t.Fatal(err)
		}
	})
	if kind != "user" || author != f.alice.String() || attachments != "[]" || !noArtifact {
		t.Fatalf("stored author_kind=%s author_id=%s attachments=%s artifact null=%v, want user/%s/[]/true", kind, author, attachments, noArtifact, f.alice)
	}
	ev := decode(t, f.outbox(events.TopicChatMessageCreated)[0], &eventsv1.ChatMessageCreated{})
	if ev.GetMessage().GetArtifact() != nil || len(ev.GetMessage().GetFiles()) != 0 || ev.GetMessage().GetAuthorId() != f.alice.String() {
		t.Fatalf("event message = %v, want no artifact, no files, author alice", ev.GetMessage())
	}
}

func TestPostMessage_AgentAndSystemAuthors(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice)
	assistant := authz.AgentID("assistant")
	slip := &chatv1.Artifact{Kind: chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER, Ref: "ANS-1001", Title: "An answer"}
	file := &chatv1.FileRef{FileId: uuid.NewString(), Name: "notes.pdf", Mime: "application/pdf", Size: 7}

	t.Run("agent for a member keeps its artifact and posts as itself", func(t *testing.T) {
		m, err := f.svc.PostMessage(t.Context(), f.agentFor(f.alice), &chatv1.SendMessageRequest{
			ChannelId: ch.String(), ClientMsgId: "run-1", Body: "Here is what I found",
			Artifact: slip, Attachments: []*chatv1.FileRef{file},
			AuthorKind: chatv1.AuthorKind_AUTHOR_KIND_USER.Enum(), // an agent actor is always an agent
			AuthorId:   proto.String(assistant.String()),
		})
		if err != nil {
			t.Fatalf("PostMessage: %v", err)
		}
		if m.GetAuthorKind() != chatv1.AuthorKind_AUTHOR_KIND_AGENT || m.GetAuthorId() != assistant.String() {
			t.Fatalf("author = %s %s, want agent %s", m.GetAuthorKind(), m.GetAuthorId(), assistant)
		}
		if !proto.Equal(m.GetArtifact(), slip) || len(m.GetFiles()) != 1 || !proto.Equal(m.GetFiles()[0], file) {
			t.Fatalf("artifact = %v, files = %v, want what the agent sent", m.GetArtifact(), m.GetFiles())
		}
		// The stored copy round-trips.
		page, err := f.svc.ListMessages(t.Context(), f.user(f.alice), ch, 0, 10)
		if err != nil || len(page) != 1 || !proto.Equal(page[0], m) {
			t.Fatalf("ListMessages = %v, %v, want the posted message", page, err)
		}
	})
	t.Run("agent without an author id", func(t *testing.T) {
		_, err := f.svc.PostMessage(t.Context(), f.agentFor(f.alice), &chatv1.SendMessageRequest{
			ChannelId: ch.String(), ClientMsgId: "run-2", Body: "who am I",
		})
		wantErr(t, err, errs.ErrInvalid, "agent request without author_id")
	})
	t.Run("system posts as the author it names, without membership", func(t *testing.T) {
		m, err := f.svc.PostMessage(t.Context(), f.system(f.bob), &chatv1.SendMessageRequest{
			ChannelId: ch.String(), ClientMsgId: "up-1", Body: "uploaded a file",
			Attachments: []*chatv1.FileRef{file},
			AuthorKind:  chatv1.AuthorKind_AUTHOR_KIND_USER.Enum(),
			AuthorId:    proto.String(f.alice.String()),
		})
		if err != nil {
			t.Fatalf("PostMessage: %v", err)
		}
		if m.GetAuthorKind() != chatv1.AuthorKind_AUTHOR_KIND_USER || m.GetAuthorId() != f.alice.String() || len(m.GetFiles()) != 1 {
			t.Fatalf("message = %v, want alice as author with the file", m)
		}
		pipeline, err := f.svc.PostMessage(t.Context(), f.system(f.alice), &chatv1.SendMessageRequest{
			ChannelId: ch.String(), ClientMsgId: "mtg-1", Body: "Meeting summary", Artifact: slip,
			AuthorKind: chatv1.AuthorKind_AUTHOR_KIND_AGENT.Enum(),
			AuthorId:   proto.String(assistant.String()),
		})
		if err != nil {
			t.Fatalf("PostMessage as pipeline: %v", err)
		}
		if pipeline.GetAuthorKind() != chatv1.AuthorKind_AUTHOR_KIND_AGENT || pipeline.GetAuthorId() != assistant.String() || pipeline.GetArtifact() == nil {
			t.Fatalf("pipeline message = %v, want the assistant's slip", pipeline)
		}
	})
	t.Run("system must name a valid author", func(t *testing.T) {
		for name, req := range map[string]*chatv1.SendMessageRequest{
			"no author":      {},
			"no kind":        {AuthorId: proto.String(f.alice.String())},
			"no id":          {AuthorKind: chatv1.AuthorKind_AUTHOR_KIND_USER.Enum()},
			"id not a uuid":  {AuthorKind: chatv1.AuthorKind_AUTHOR_KIND_USER.Enum(), AuthorId: proto.String("alice")},
			"id the nil uid": {AuthorKind: chatv1.AuthorKind_AUTHOR_KIND_USER.Enum(), AuthorId: proto.String(uuid.Nil.String())},
		} {
			req.ChannelId, req.ClientMsgId, req.Body = ch.String(), "bad-"+name, "x"
			_, err := f.svc.PostMessage(t.Context(), f.system(f.alice), req)
			wantErr(t, err, errs.ErrInvalid, name)
		}
	})
	t.Run("system into a channel that does not exist", func(t *testing.T) {
		_, err := f.svc.PostMessage(t.Context(), f.system(f.alice), &chatv1.SendMessageRequest{
			ChannelId: uuid.NewString(), ClientMsgId: "ghost", Body: "x",
			AuthorKind: chatv1.AuthorKind_AUTHOR_KIND_USER.Enum(), AuthorId: proto.String(f.alice.String()),
		})
		wantErr(t, err, errs.ErrNotFound, "unknown channel")
	})
}

func TestPostMessage_EmptyAndOversizeInvalid(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	post := func(clientMsgID, body string) (*chatv1.Message, error) {
		return f.svc.PostMessage(t.Context(), f.user(f.alice), &chatv1.SendMessageRequest{
			ChannelId: ch.String(), ClientMsgId: clientMsgID, Body: body,
		})
	}

	for name, body := range map[string]string{
		"empty":            "",
		"whitespace only":  " \n\t \u00a0\u3000 ",
		"8001 ascii runes": strings.Repeat("x", 8001),
		"8001 thai runes":  thai(8001),
		"NUL byte":         "hello\x00world",
		"invalid UTF-8":    "hello\xffworld",
	} {
		_, err := post("bad-"+name, body)
		wantErr(t, err, errs.ErrInvalid, name)
	}
	_, err := post("", "no client id")
	wantErr(t, err, errs.ErrInvalid, "empty client_msg_id")
	_, err = post(strings.Repeat("c", 129), "long client id")
	wantErr(t, err, errs.ErrInvalid, "client_msg_id over 128 bytes")
	if rows := f.messageCount(ch); rows != 0 {
		t.Fatalf("rows = %d after rejected posts, want 0", rows)
	}

	// 8000 runes are 24 000 bytes of Thai with combining marks: runes count, not bytes.
	body := thai(8000)
	if n := len([]rune(body)); n != 8000 {
		t.Fatalf("test body has %d runes", n)
	}
	m, err := post("thai-8000", body)
	if err != nil {
		t.Fatalf("8000 Thai runes: %v", err)
	}
	if m.GetBody() != body {
		t.Fatalf("stored body differs from the 8000 runes sent")
	}
	if m, err = post("ascii-8000", "  "+strings.Repeat("y", 8000)+"\n"); err != nil || len(m.GetBody()) != 8000 {
		t.Fatalf("8000 runes between whitespace: err = %v, body length = %d, want accepted and trimmed", err, len(m.GetBody()))
	}
}

func TestPostMessage_MentionsKeepMembersAndKnownAgentsOnly(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice, f.bob) // Carol exists in the workspace but is not a member
	stranger := uuid.NewString()

	m, err := f.svc.PostMessage(t.Context(), f.user(f.alice), &chatv1.SendMessageRequest{
		ChannelId: ch.String(), ClientMsgId: "ment", Body: "@Bob @Carol @assistant @ghost @nobody",
		Mentions: []*chatv1.Mention{
			{Kind: chatv1.MentionKind_MENTION_KIND_USER, Id: f.bob.String(), Label: "Not Bob's real name"},
			{Kind: chatv1.MentionKind_MENTION_KIND_USER, Id: f.bob.String(), Label: "Bob again"}, // duplicate
			{Kind: chatv1.MentionKind_MENTION_KIND_USER, Id: f.carol.String(), Label: "Carol"},   // not a member
			{Kind: chatv1.MentionKind_MENTION_KIND_USER, Id: stranger, Label: "nobody"},          // no such user
			{Kind: chatv1.MentionKind_MENTION_KIND_USER, Id: "not-a-uuid", Label: "garbage"},
			{Kind: chatv1.MentionKind_MENTION_KIND_AGENT, Id: "assistant", Label: "assistant"}, // registered
			{Kind: chatv1.MentionKind_MENTION_KIND_AGENT, Id: "ghost", Label: "ghost"},         // not registered
			{Kind: chatv1.MentionKind_MENTION_KIND_UNSPECIFIED, Id: f.alice.String(), Label: "Alice"},
		},
	})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	want := []*chatv1.Mention{
		{Kind: chatv1.MentionKind_MENTION_KIND_USER, Id: f.bob.String(), Label: "Bob"},
		{Kind: chatv1.MentionKind_MENTION_KIND_AGENT, Id: authz.AgentID("assistant").String(), Label: "assistant"},
	}
	if len(m.GetMentions()) != len(want) {
		t.Fatalf("mentions = %v, want %v", m.GetMentions(), want)
	}
	for i := range want {
		if !proto.Equal(m.GetMentions()[i], want[i]) {
			t.Fatalf("mention %d = %v, want %v (member names come from the directory, agents carry their registry name)", i, m.GetMentions()[i], want[i])
		}
	}
	if m.GetBody() != "@Bob @Carol @assistant @ghost @nobody" {
		t.Fatalf("body = %q: dropped mentions stay as plain text", m.GetBody())
	}

	page, err := f.svc.ListMessages(t.Context(), f.user(f.bob), ch, 0, 10)
	if err != nil || len(page) != 1 || !proto.Equal(page[0], m) {
		t.Fatalf("ListMessages = %v, %v, want the posted message with its mentions", page, err)
	}

	// A message may not drown in mentions.
	many := make([]*chatv1.Mention, maxMentions+1)
	for i := range many {
		many[i] = &chatv1.Mention{Kind: chatv1.MentionKind_MENTION_KIND_USER, Id: uuid.NewString()}
	}
	_, err = f.svc.PostMessage(t.Context(), f.user(f.alice), &chatv1.SendMessageRequest{
		ChannelId: ch.String(), ClientMsgId: "many", Body: "x", Mentions: many,
	})
	wantErr(t, err, errs.ErrInvalid, "too many mentions")
}

func TestListMessages_PagesDescendingBySeq(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	for i := 1; i <= 7; i++ {
		f.send(f.user(f.alice), ch, fmt.Sprintf("m-%d", i), fmt.Sprintf("message %d", i))
	}
	page := func(before int64, limit int) []int64 {
		t.Helper()
		msgs, err := f.svc.ListMessages(t.Context(), f.user(f.alice), ch, before, limit)
		if err != nil {
			t.Fatalf("ListMessages(before %d, limit %d): %v", before, limit, err)
		}
		return seqs(msgs)
	}
	for _, tc := range []struct {
		before int64
		limit  int
		want   []int64
	}{
		{0, 3, []int64{7, 6, 5}}, // before_seq 0 is the latest page
		{5, 3, []int64{4, 3, 2}},
		{2, 3, []int64{1}},
		{1, 3, []int64{}},
		{8, 2, []int64{7, 6}},
		{0, 0, []int64{7, 6, 5, 4, 3, 2, 1}}, // the default page holds more than seven
	} {
		if got := page(tc.before, tc.limit); !slices.Equal(got, tc.want) {
			t.Errorf("ListMessages(before %d, limit %d) = %v, want %v", tc.before, tc.limit, got, tc.want)
		}
	}
	if _, err := f.svc.ListMessages(t.Context(), f.user(f.alice), ch, -1, 3); !errors.Is(err, errs.ErrInvalid) {
		t.Errorf("negative before_seq: err = %v, want ErrInvalid", err)
	}

	// The page is bounded: a long history never comes back in one piece.
	long := f.channel("public", f.alice)
	f.seedMessages(long, f.alice, maxPage+30)
	msgs, err := f.svc.ListMessages(t.Context(), f.user(f.alice), long, 0, 10_000)
	if err != nil || len(msgs) != maxPage {
		t.Fatalf("oversize limit returned %d messages, err = %v, want %d", len(msgs), err, maxPage)
	}
	if msgs[0].GetSeq() != maxPage+30 || msgs[len(msgs)-1].GetSeq() != 31 {
		t.Fatalf("oversize limit spans seq %d..%d, want %d..31 (newest first)", msgs[0].GetSeq(), msgs[len(msgs)-1].GetSeq(), maxPage+30)
	}
}

// ---- sync -------------------------------------------------------------------

func TestSyncChannels_ReturnsMissingRangeInOrder(t *testing.T) {
	f := newFixture(t)
	a := f.channel("public", f.alice)
	b := f.channel("private", f.alice)
	for i := 1; i <= 7; i++ {
		f.send(f.user(f.alice), a, fmt.Sprintf("a-%d", i), fmt.Sprintf("a %d", i))
	}
	f.send(f.user(f.alice), b, "b-1", "b 1")

	deltas, err := f.svc.SyncChannels(t.Context(), f.user(f.alice), []*chatv1.Cursor{
		{ChannelId: a.String(), LastSeq: 3},
		{ChannelId: b.String(), LastSeq: 1}, // up to date
		{ChannelId: a.String(), LastSeq: 7}, // up to date
		{ChannelId: b.String(), LastSeq: 0}, // a channel the client has not seen yet
	})
	if err != nil {
		t.Fatalf("SyncChannels: %v", err)
	}
	if len(deltas) != 4 {
		t.Fatalf("deltas = %d, want one per cursor", len(deltas))
	}
	for i, want := range []struct {
		channel uuid.UUID
		seqs    []int64
	}{{a, []int64{4, 5, 6, 7}}, {b, []int64{}}, {a, []int64{}}, {b, []int64{1}}} {
		d := deltas[i]
		if d.GetChannelId() != want.channel.String() || d.GetReload() || !slices.Equal(seqs(d.GetMessages()), want.seqs) {
			t.Errorf("delta %d = channel %s reload %v seqs %v, want channel %s no reload seqs %v",
				i, d.GetChannelId(), d.GetReload(), seqs(d.GetMessages()), want.channel, want.seqs)
		}
	}
	if got := deltas[0].GetMessages()[0].GetBody(); got != "a 4" {
		t.Errorf("first missing message body = %q, want %q", got, "a 4")
	}
}

func TestSyncChannels_TooFarBehindReloadsLatestPage(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	const head = 205
	f.seedMessages(ch, f.alice, head)

	catchUp := func(last int64) *chatv1.ChannelDelta {
		t.Helper()
		deltas, err := f.svc.SyncChannels(t.Context(), f.user(f.alice), []*chatv1.Cursor{{ChannelId: ch.String(), LastSeq: last}})
		if err != nil || len(deltas) != 1 {
			t.Fatalf("SyncChannels(%d) = %v, %v", last, deltas, err)
		}
		return deltas[0]
	}

	// 201 behind: reload with the latest page (oldest first, like every delta).
	d := catchUp(head - 201)
	if !d.GetReload() || len(d.GetMessages()) != defaultPage {
		t.Fatalf("201 behind: reload = %v with %d messages, want a reload of one page (%d)", d.GetReload(), len(d.GetMessages()), defaultPage)
	}
	first, last := d.GetMessages()[0].GetSeq(), d.GetMessages()[len(d.GetMessages())-1].GetSeq()
	if first != head-defaultPage+1 || last != head {
		t.Fatalf("reload page spans seq %d..%d, want %d..%d", first, last, head-defaultPage+1, head)
	}
	if !slices.IsSorted(seqs(d.GetMessages())) {
		t.Fatalf("reload page is not ascending: %v", seqs(d.GetMessages()))
	}

	// Exactly 200 behind still catches up by delta.
	d = catchUp(head - 200)
	if d.GetReload() || len(d.GetMessages()) != 200 || d.GetMessages()[0].GetSeq() != head-199 {
		t.Fatalf("200 behind: reload = %v with %d messages from seq %d, want a plain 200-message delta", d.GetReload(), len(d.GetMessages()), d.GetMessages()[0].GetSeq())
	}
}

func TestSyncChannels_RejectsMalformedCursors(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	for name, c := range map[string]*chatv1.Cursor{
		"channel id not a uuid": {ChannelId: "general", LastSeq: 0},
		"negative last_seq":     {ChannelId: ch.String(), LastSeq: -1},
	} {
		_, err := f.svc.SyncChannels(t.Context(), f.user(f.alice), []*chatv1.Cursor{c})
		wantErr(t, err, errs.ErrInvalid, name)
	}
}

// One request may name at most 200 cursors: every cursor is a channel read and up to 200
// messages in the answer, so an unbounded list is an unbounded query and response.
func TestSyncChannels_MoreThan200Cursors_IsInvalid(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	cursors := func(n int) []*chatv1.Cursor {
		out := make([]*chatv1.Cursor, n)
		for i := range out {
			out[i] = &chatv1.Cursor{ChannelId: ch.String(), LastSeq: 0}
		}
		return out
	}

	deltas, err := f.svc.SyncChannels(t.Context(), f.user(f.alice), cursors(200))
	if err != nil || len(deltas) != 200 {
		t.Fatalf("SyncChannels with 200 cursors = %d deltas, %v, want one per cursor", len(deltas), err)
	}
	_, err = f.svc.SyncChannels(t.Context(), f.user(f.alice), cursors(201))
	wantErr(t, err, errs.ErrInvalid, "201 cursors")
}

// ---- access -----------------------------------------------------------------

func TestPrivateChannel_NonMemberCannotListOrSync(t *testing.T) {
	f := newFixture(t)
	secret := f.channel("private", f.alice)
	open := f.channel("public", f.alice, f.bob)
	f.send(f.user(f.alice), secret, "s-1", "the plan")

	_, err := f.svc.ListMessages(t.Context(), f.user(f.bob), secret, 0, 10)
	wantErr(t, err, errs.ErrForbidden, "ListMessages")
	_, err = f.svc.ListMembers(t.Context(), f.user(f.bob), secret)
	wantErr(t, err, errs.ErrForbidden, "ListMembers")
	_, err = f.svc.SyncChannels(t.Context(), f.user(f.bob), []*chatv1.Cursor{{ChannelId: secret.String(), LastSeq: 0}})
	wantErr(t, err, errs.ErrForbidden, "SyncChannels")
	// One forbidden cursor refuses the whole request: nothing leaks next to a channel the caller may read.
	_, err = f.svc.SyncChannels(t.Context(), f.user(f.bob), []*chatv1.Cursor{
		{ChannelId: open.String(), LastSeq: 0}, {ChannelId: secret.String(), LastSeq: 0},
	})
	wantErr(t, err, errs.ErrForbidden, "SyncChannels with one allowed and one forbidden cursor")

	chans, err := f.svc.ListChannels(t.Context(), f.user(f.bob))
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	var ids []string
	for _, c := range chans {
		ids = append(ids, c.GetId())
	}
	if slices.Contains(ids, secret.String()) || !slices.Contains(ids, open.String()) {
		t.Fatalf("Bob's channels = %v, want the public one and not the private one", ids)
	}

	// Another workspace's user is no member of anything here, public or not.
	other := f.seedWorkspace("other.example")
	mallory := f.seedUser(other, "Mallory")
	intruder := authz.Actor{Kind: authz.ActorUser, WorkspaceID: other, UserID: mallory}
	_, err = f.svc.ListMessages(t.Context(), intruder, open, 0, 10)
	wantErr(t, err, errs.ErrForbidden, "a user of another workspace")
	if chans, err = f.svc.ListChannels(t.Context(), intruder); err != nil || len(chans) != 0 {
		t.Fatalf("another workspace's ListChannels = %v, %v, want none", chans, err)
	}
}

// With the in-process cache a removal is effective at once, in the process that made it
// and in a second one that shares only Postgres, which is what a rollout runs for a
// while: nothing is cached, so no process can answer from a set that predates it.
func TestRemoveMember_WithTheMemoryCache_IsEffectiveInEveryProcessAtOnce(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice, f.bob)
	ctx := f.wsCtx()

	// Two processes: each has a memory cache of its own and they share the database.
	process := func() Service {
		mem := cache.NewMemory(cache.Options{})
		t.Cleanup(func() { _ = mem.Close() })
		return New(Deps{
			DB: f.db, Authz: authz.New(NewMembership(f.db, mem)), Cache: mem,
			KnownAgent: func(name string) bool { return name == "assistant" },
		})
	}
	old, next := process(), process()

	for name, svc := range map[string]Service{"first": old, "second": next} {
		if ok, err := svc.IsMember(ctx, ch, f.bob); err != nil || !ok {
			t.Fatalf("%s process: IsMember(bob) = %v, %v, want true", name, ok, err)
		}
	}
	if err := old.RemoveMember(ctx, f.user(f.alice), ch, f.bob); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	for name, svc := range map[string]Service{"first": old, "second": next} {
		if ok, err := svc.IsMember(ctx, ch, f.bob); err != nil || ok {
			t.Errorf("%s process: IsMember(bob) after the removal = %v, %v, want false at once", name, ok, err)
		}
		_, err := svc.PostMessage(ctx, f.user(f.bob), &chatv1.SendMessageRequest{ChannelId: ch.String(), ClientMsgId: "late-" + name, Body: "still here?"})
		wantErr(t, err, errs.ErrForbidden, name+" process: removed member posts")
	}
}

func TestRemoveMember_InvalidatesCacheAndNotifies(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice, f.bob)
	ctx := f.wsCtx()

	// Warm the cache: the first lookup fills members:{channel}.
	if ok, err := f.svc.IsMember(ctx, ch, f.bob); err != nil || !ok {
		t.Fatalf("IsMember(bob) = %v, %v, want true", ok, err)
	}
	if ids, hit, err := f.cache.Members(ctx, ch); err != nil || !hit || !slices.Contains(ids, f.bob) {
		t.Fatalf("members cache = %v hit %v err %v, want it to list Bob after the first lookup", ids, hit, err)
	}

	if err := f.svc.RemoveMember(ctx, f.user(f.alice), ch, f.bob); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	if ids, hit, err := f.cache.Members(ctx, ch); err != nil || (hit && slices.Contains(ids, f.bob)) {
		t.Fatalf("members cache = %v hit %v err %v, want it to no longer list Bob", ids, hit, err)
	}
	if ok, err := f.svc.IsMember(ctx, ch, f.bob); err != nil || ok {
		t.Fatalf("IsMember(bob) after removal = %v, %v, want false", ok, err)
	}
	_, err := f.svc.PostMessage(ctx, f.user(f.bob), &chatv1.SendMessageRequest{ChannelId: ch.String(), ClientMsgId: "late", Body: "still here?"})
	wantErr(t, err, errs.ErrForbidden, "removed member posts")

	evs := f.outbox(events.TopicMembershipChanged)
	if len(evs) != 1 {
		t.Fatalf("membership.changed rows = %d, want 1", len(evs))
	}
	ev := decode(t, evs[0], &eventsv1.MembershipChanged{})
	if ev.GetWorkspaceId() != f.ws.String() || ev.GetChannelId() != ch.String() || ev.GetUserId() != f.bob.String() ||
		ev.GetChange() != chatv1.MembershipChange_MEMBERSHIP_CHANGE_REMOVED {
		t.Fatalf("event = %v, want REMOVED for Bob in the channel", ev)
	}

	// Removing someone who is gone is not an error and announces nothing more.
	if err := f.svc.RemoveMember(ctx, f.user(f.alice), ch, f.bob); err != nil {
		t.Fatalf("second RemoveMember: %v", err)
	}
	if evs := f.outbox(events.TopicMembershipChanged); len(evs) != 1 {
		t.Fatalf("membership.changed rows = %d after the repeat, want 1", len(evs))
	}
	// Only a member manages the channel.
	wantErr(t, f.svc.RemoveMember(ctx, f.user(f.carol), ch, f.alice), errs.ErrForbidden, "outsider removes a member")
	if ok, _ := f.svc.IsMember(ctx, ch, f.alice); !ok {
		t.Fatalf("Alice lost her membership to an outsider's request")
	}
}

func TestJoinChannel_PublicOnly(t *testing.T) {
	f := newFixture(t)
	open := f.channel("public", f.alice)
	secret := f.channel("private", f.alice)
	ctx := f.wsCtx()

	// A lookup before the join caches the set without Bob; the join must invalidate it.
	if ok, _ := f.svc.IsMember(ctx, open, f.bob); ok {
		t.Fatal("Bob is a member before joining")
	}
	if err := f.svc.JoinChannel(t.Context(), f.user(f.bob), open); err != nil {
		t.Fatalf("JoinChannel(public): %v", err)
	}
	if ok, err := f.svc.IsMember(ctx, open, f.bob); err != nil || !ok {
		t.Fatalf("IsMember(bob) after joining = %v, %v, want true", ok, err)
	}
	f.send(f.user(f.bob), open, "hi", "hello everyone")
	evs := f.outbox(events.TopicMembershipChanged)
	if len(evs) != 1 {
		t.Fatalf("membership.changed rows = %d, want 1", len(evs))
	}
	ev := decode(t, evs[0], &eventsv1.MembershipChanged{})
	if ev.GetUserId() != f.bob.String() || ev.GetChannelId() != open.String() || ev.GetChange() != chatv1.MembershipChange_MEMBERSHIP_CHANGE_ADDED {
		t.Fatalf("event = %v, want ADDED for Bob", ev)
	}

	// Joining twice is the same as joining once.
	if err := f.svc.JoinChannel(t.Context(), f.user(f.bob), open); err != nil {
		t.Fatalf("second JoinChannel: %v", err)
	}
	if evs := f.outbox(events.TopicMembershipChanged); len(evs) != 1 {
		t.Fatalf("membership.changed rows = %d after joining twice, want 1", len(evs))
	}

	wantErr(t, f.svc.JoinChannel(t.Context(), f.user(f.bob), secret), errs.ErrForbidden, "join a private channel")
	wantErr(t, f.svc.JoinChannel(t.Context(), f.user(f.alice), secret), errs.ErrForbidden, "a member re-joining a private channel")
	wantErr(t, f.svc.JoinChannel(t.Context(), f.agentFor(f.bob), open), errs.ErrForbidden, "an agent joins")
	wantErr(t, f.svc.JoinChannel(t.Context(), authz.Actor{}, open), errs.ErrForbidden, "a zero actor joins")
	_, err := f.svc.ListChannels(t.Context(), authz.Actor{})
	wantErr(t, err, errs.ErrForbidden, "a zero actor lists channels")
	wantErr(t, f.svc.JoinChannel(t.Context(), f.user(f.bob), uuid.New()), errs.ErrNotFound, "join a channel that does not exist")
	if ok, _ := f.svc.IsMember(ctx, secret, f.bob); ok {
		t.Fatal("Bob became a member of the private channel")
	}
}

// ---- channels and members ---------------------------------------------------

func TestCreateChannel(t *testing.T) {
	f := newFixture(t)

	ch, err := f.svc.CreateChannel(t.Context(), f.user(f.alice), &chatv1.CreateChannelRequest{
		Name: "  design  ", Kind: chatv1.ChannelKind_CHANNEL_KIND_PRIVATE,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if ch.GetName() != "design" || ch.GetKind() != chatv1.ChannelKind_CHANNEL_KIND_PRIVATE || ch.GetLastSeq() != 0 {
		t.Fatalf("channel = %v, want private 'design' at seq 0", ch)
	}
	id := uuid.MustParse(ch.GetId())
	if ok, err := f.svc.IsMember(f.wsCtx(), id, f.alice); err != nil || !ok {
		t.Fatalf("the creator is not a member: %v, %v", ok, err)
	}
	var role string
	f.scalar(f.ws, &role, `SELECT role FROM chat.channel_members WHERE channel_id = $1 AND user_id = $2`, id, f.alice)
	if role != "owner" {
		t.Fatalf("creator role = %s, want owner", role)
	}
	f.send(f.user(f.alice), id, "first", "welcome")
	if chans, err := f.svc.ListChannels(t.Context(), f.user(f.alice)); err != nil || len(chans) != 1 || chans[0].GetLastSeq() != 1 {
		t.Fatalf("ListChannels = %v, %v, want the new channel at last_seq 1", chans, err)
	}
	if chans, _ := f.svc.ListChannels(t.Context(), f.user(f.bob)); len(chans) != 0 {
		t.Fatalf("Bob sees %v, want nothing: the channel is private", chans)
	}

	for name, req := range map[string]*chatv1.CreateChannelRequest{
		"empty name":       {Name: " ", Kind: chatv1.ChannelKind_CHANNEL_KIND_PUBLIC},
		"name too long":    {Name: strings.Repeat("n", maxChannelName+1), Kind: chatv1.ChannelKind_CHANNEL_KIND_PUBLIC},
		"unspecified kind": {Name: "x", Kind: chatv1.ChannelKind_CHANNEL_KIND_UNSPECIFIED},
	} {
		_, err := f.svc.CreateChannel(t.Context(), f.user(f.alice), req)
		wantErr(t, err, errs.ErrInvalid, name)
	}
	_, err = f.svc.CreateChannel(t.Context(), f.agentFor(f.alice), &chatv1.CreateChannelRequest{Name: "x", Kind: chatv1.ChannelKind_CHANNEL_KIND_PUBLIC})
	wantErr(t, err, errs.ErrForbidden, "an agent creates a channel")
	_, err = f.svc.CreateChannel(t.Context(), authz.Actor{}, &chatv1.CreateChannelRequest{Name: "x", Kind: chatv1.ChannelKind_CHANNEL_KIND_PUBLIC})
	wantErr(t, err, errs.ErrForbidden, "a zero actor creates a channel")

	evs := f.outbox(events.TopicMembershipChanged)
	if len(evs) != 1 || decode(t, evs[0], &eventsv1.MembershipChanged{}).GetUserId() != f.alice.String() {
		t.Fatalf("membership.changed = %v, want one ADDED for the creator", evs)
	}
}

func TestAddMemberAndListMembers(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice)
	ctx := f.wsCtx()

	wantErr(t, f.svc.AddMember(t.Context(), f.user(f.bob), ch, f.bob), errs.ErrForbidden, "an outsider adds themself to a private channel")
	if ok, _ := f.svc.IsMember(ctx, ch, f.alice); !ok { // warms members:{channel} without Bob
		t.Fatal("Alice is not a member of her own channel")
	}
	if err := f.svc.AddMember(t.Context(), f.user(f.alice), ch, f.bob); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if ok, err := f.svc.IsMember(ctx, ch, f.bob); err != nil || !ok {
		t.Fatalf("IsMember(bob) after AddMember = %v, %v, want true", ok, err)
	}
	if err := f.svc.AddMember(t.Context(), f.user(f.alice), ch, f.bob); err != nil {
		t.Fatalf("adding a member twice: %v", err)
	}
	wantErr(t, f.svc.AddMember(t.Context(), f.user(f.alice), ch, uuid.New()), errs.ErrNotFound, "a user that does not exist")
	wantErr(t, f.svc.AddMember(t.Context(), f.agentFor(f.alice), ch, f.carol), errs.ErrForbidden, "an agent manages the channel")
	other := f.seedUser(f.seedWorkspace("other.example"), "Mallory")
	wantErr(t, f.svc.AddMember(t.Context(), f.user(f.alice), ch, other), errs.ErrNotFound, "a user of another workspace")

	members, err := f.svc.ListMembers(t.Context(), f.user(f.bob), ch)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	want := []*chatv1.Member{
		{UserId: f.alice.String(), Name: "Alice", Role: chatv1.MemberRole_MEMBER_ROLE_OWNER},
		{UserId: f.bob.String(), Name: "Bob", Role: chatv1.MemberRole_MEMBER_ROLE_MEMBER},
	}
	if len(members) != len(want) {
		t.Fatalf("members = %v, want %v", members, want)
	}
	for i := range want {
		if !proto.Equal(members[i], want[i]) {
			t.Fatalf("member %d = %v, want %v", i, members[i], want[i])
		}
	}
	if evs := f.outbox(events.TopicMembershipChanged); len(evs) != 1 {
		t.Fatalf("membership.changed rows = %d, want 1 (the second add changed nothing)", len(evs))
	}
}

func TestMemberChannelIDs(t *testing.T) {
	f := newFixture(t)
	a := f.channel("public", f.alice, f.bob)
	b := f.channel("private", f.alice)
	f.channel("public", f.carol)

	got, err := f.svc.MemberChannelIDs(t.Context(), f.ws, f.alice)
	if err != nil {
		t.Fatalf("MemberChannelIDs: %v", err)
	}
	want := []uuid.UUID{a, b}
	slices.SortFunc(want, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) })
	if !slices.Equal(got, want) {
		t.Fatalf("Alice's channels = %v, want %v", got, want)
	}
	if got, _ := f.svc.MemberChannelIDs(t.Context(), f.ws, uuid.New()); len(got) != 0 {
		t.Fatalf("an unknown user's channels = %v, want none", got)
	}
	other := f.seedWorkspace("other.example")
	if got, _ := f.svc.MemberChannelIDs(t.Context(), other, f.alice); len(got) != 0 {
		t.Fatalf("Alice's channels in another workspace = %v, want none", got)
	}
}

// ---- membership cache -------------------------------------------------------

func TestMembership_FillsAndServesFromCache(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice, f.bob)
	m := NewMembership(f.db, f.cache)
	ctx := f.wsCtx()

	if _, hit, _ := f.cache.Members(ctx, ch); hit {
		t.Fatal("members:{channel} is set before any lookup")
	}
	if ok, err := m.IsMember(ctx, ch, f.bob); err != nil || !ok {
		t.Fatalf("IsMember(bob) = %v, %v, want true", ok, err)
	}
	if ids, hit, _ := f.cache.Members(ctx, ch); !hit || len(ids) != 2 {
		t.Fatalf("members cache = %v hit %v, want both members after the first lookup", ids, hit)
	}

	// A row changed behind the service's back is not seen until the cache is invalidated: the answer comes from Valkey.
	f.exec(f.ws, `DELETE FROM chat.channel_members WHERE channel_id = $1 AND user_id = $2`, ch, f.bob)
	if ok, _ := m.IsMember(ctx, ch, f.bob); !ok {
		t.Fatal("IsMember read Postgres although the set was cached")
	}
	if err := f.cache.InvalidateMembers(ctx, ch); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.IsMember(ctx, ch, f.bob); err != nil || ok {
		t.Fatalf("IsMember(bob) after invalidation = %v, %v, want false", ok, err)
	}
	if ok, _ := m.IsMember(ctx, ch, f.carol); ok {
		t.Fatal("Carol is a member")
	}

	// A lookup with no workspace cannot open a scoped transaction.
	_, err := m.IsMember(t.Context(), ch, f.alice)
	wantErr(t, err, errs.ErrInvalid, "lookup without a workspace")
}

func TestMembership_CacheOutageFallsBackToPostgres(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice)
	// A cache whose connection is closed fails every call, like an unreachable Valkey.
	dead, err := cache.OpenValkey(t.Context(), valkeyURL, cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := dead.Close(); err != nil {
		t.Fatal(err)
	}
	m := NewMembership(f.db, dead)
	ctx := f.wsCtx()

	logs := &strings.Builder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if ok, err := m.IsMember(ctx, ch, f.alice); err != nil || !ok {
		t.Fatalf("member with Valkey down: %v, %v, want true from Postgres", ok, err)
	}
	if ok, err := m.IsMember(ctx, ch, f.bob); err != nil || ok {
		t.Fatalf("non-member with Valkey down: %v, %v, want false from Postgres", ok, err)
	}
	if !strings.Contains(logs.String(), "members cache") {
		t.Fatalf("the outage was not reported; log = %q", logs.String())
	}
}

// TestMembers_SlowReaderCannotResurrectRemovedUser pins the cache-aside race: a
// reader that read Postgres before a removal commits and fills the cache after the
// removal invalidated it must not leave the removed user in members:{channel}.
func TestMembers_SlowReaderCannotResurrectRemovedUser(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice, f.bob)
	ctx := f.wsCtx()

	slow := newMembership(f.db, f.cache)
	read := slow.load
	var once sync.Once
	loaded, release := make(chan struct{}), make(chan struct{})
	slow.load = func(ctx context.Context, ws, channel uuid.UUID) ([]uuid.UUID, error) {
		ids, err := read(ctx, ws, channel) // the stale read: Bob is still a member
		once.Do(func() { close(loaded) })
		<-release
		return ids, err
	}
	done := make(chan error, 1)
	go func() {
		_, err := slow.IsMember(ctx, ch, f.bob)
		done <- err
	}()

	<-loaded
	if err := f.svc.RemoveMember(ctx, f.user(f.alice), ch, f.bob); err != nil { // commits, then invalidates
		t.Fatalf("RemoveMember: %v", err)
	}
	close(release) // the slow reader now tries to fill the cache with what it read before
	if err := <-done; err != nil {
		t.Fatalf("slow IsMember: %v", err)
	}

	if ids, hit, err := f.cache.Members(ctx, ch); err != nil || (hit && slices.Contains(ids, f.bob)) {
		t.Fatalf("members cache = %v hit %v err %v: the stale fill resurrected Bob", ids, hit, err)
	}
	if ok, err := NewMembership(f.db, f.cache).IsMember(ctx, ch, f.bob); err != nil || ok {
		t.Fatalf("IsMember(bob) after the race = %v, %v, want false", ok, err)
	}
}

// ---- Connect ----------------------------------------------------------------

func TestConnect_RequiresTheProtocolHeader(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	actor := f.user(f.alice)

	// No option is passed: the handler adds the requirement itself.
	path, h := Handler(f.svc, func(context.Context) authz.Actor { return actor })
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// A browser can send this cross-site without a preflight: POST with a JSON body.
	call := func(version string) (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path+"SendMessage",
			strings.NewReader(fmt.Sprintf(`{"channelId":%q,"clientMsgId":"cx-%s","body":"over Connect"}`, ch, version)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if version != "" {
			req.Header.Set("Connect-Protocol-Version", version)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	status, body := call("")
	if status != http.StatusBadRequest || !strings.Contains(body, "Connect-Protocol-Version") {
		t.Fatalf("POST without the header: %d %s; want 400 naming Connect-Protocol-Version", status, body)
	}
	if rows := f.messageCount(ch); rows != 0 {
		t.Fatalf("the refused call reached the service: %d messages stored", rows)
	}
	status, body = call("1")
	if status != http.StatusOK || !strings.Contains(body, `"over Connect"`) {
		t.Fatalf("POST with the header: %d %s; want 200 and the stored message", status, body)
	}

	// The generated client sends the header itself and reaches every method.
	client := chatv1connect.NewChatServiceClient(srv.Client(), srv.URL)
	sent, err := client.SendMessage(t.Context(), connect.NewRequest(&chatv1.SendMessageRequest{
		ChannelId: ch.String(), ClientMsgId: "via-client", Body: "second",
	}))
	if err != nil || sent.Msg.GetMessage().GetSeq() != 2 || sent.Msg.GetMessage().GetAuthorId() != f.alice.String() {
		t.Fatalf("SendMessage = %v, %v, want seq 2 by Alice", sent, err)
	}
	listed, err := client.ListMessages(t.Context(), connect.NewRequest(&chatv1.ListMessagesRequest{ChannelId: ch.String(), Limit: 10}))
	if err != nil || !slices.Equal(seqs(listed.Msg.GetMessages()), []int64{2, 1}) {
		t.Fatalf("ListMessages = %v, %v, want seqs 2, 1", listed, err)
	}
	synced, err := client.SyncChannels(t.Context(), connect.NewRequest(&chatv1.SyncChannelsRequest{
		Cursors: []*chatv1.Cursor{{ChannelId: ch.String(), LastSeq: 1}},
	}))
	if err != nil || len(synced.Msg.GetDeltas()) != 1 || !slices.Equal(seqs(synced.Msg.GetDeltas()[0].GetMessages()), []int64{2}) {
		t.Fatalf("SyncChannels = %v, %v, want the delta {2}", synced, err)
	}
	chans, err := client.ListChannels(t.Context(), connect.NewRequest(&chatv1.ListChannelsRequest{}))
	if err != nil || len(chans.Msg.GetChannels()) != 1 || chans.Msg.GetChannels()[0].GetLastSeq() != 2 {
		t.Fatalf("ListChannels = %v, %v, want the one channel at last_seq 2", chans, err)
	}
	created, err := client.CreateChannel(t.Context(), connect.NewRequest(&chatv1.CreateChannelRequest{Name: "ops", Kind: chatv1.ChannelKind_CHANNEL_KIND_PUBLIC}))
	if err != nil || created.Msg.GetChannel().GetName() != "ops" {
		t.Fatalf("CreateChannel = %v, %v", created, err)
	}
	if _, err := client.JoinChannel(t.Context(), connect.NewRequest(&chatv1.JoinChannelRequest{ChannelId: created.Msg.GetChannel().GetId()})); err != nil {
		t.Fatalf("JoinChannel: %v", err)
	}
	if _, err := client.AddMember(t.Context(), connect.NewRequest(&chatv1.AddMemberRequest{ChannelId: ch.String(), UserId: f.bob.String()})); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	members, err := client.ListMembers(t.Context(), connect.NewRequest(&chatv1.ListMembersRequest{ChannelId: ch.String()}))
	if err != nil || len(members.Msg.GetMembers()) != 2 {
		t.Fatalf("ListMembers = %v, %v, want Alice and Bob", members, err)
	}
	if _, err := client.RemoveMember(t.Context(), connect.NewRequest(&chatv1.RemoveMemberRequest{ChannelId: ch.String(), UserId: f.bob.String()})); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
}
