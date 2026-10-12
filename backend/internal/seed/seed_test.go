package seed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	tasksv1 "kiwi/gen/kiwi/tasks/v1"
	"kiwi/internal/chat"
	"kiwi/internal/docs"
	"kiwi/internal/identity"
	"kiwi/internal/knowledge"
	"kiwi/internal/meetings"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/blob"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/llm"
	"kiwi/internal/tasks"
	"kiwi/internal/testkit/containers"
	"kiwi/internal/testkit/dbtest"
	"kiwi/internal/testkit/fakellm"
)

// One plain Valkey serves the package; every test seeds a database of its own.
var (
	valkeyOnce sync.Once
	valkeyURL  string
	valkeyErr  error
	valkeyCtr  testcontainers.Container
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) // Run logs each step it takes
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

// ---- harness ----------------------------------------------------------------

const (
	domain  = "demo.example"
	alice   = "alice@demo.example"
	bob     = "bob@demo.example"
	carol   = "carol@demo.example"
	summary = `{"summary":"A synthetic note on the capacity of the Bang Na warehouse.","key_points":["85% utilisation","Zone A is full on 28 October"]}`
)

var members = []string{alice, bob, carol}

// harness is one migrated database, a Valkey, the local blob store and a scripted
// model: what `kiwi seed` is given in the pod.
type harness struct {
	t     *testing.T
	env   *dbtest.Env
	db    *db.DB
	cache *cache.Cache
	blob  blob.Store
	llm   *fakellm.Client
	deps  Deps
	// generateErr, when set, is what the model answers every Generate with.
	mu          sync.Mutex
	generateErr error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	valkeyOnce.Do(func() { valkeyURL, valkeyErr = bootValkey() })
	if valkeyErr != nil {
		t.Fatalf("valkey: %v", valkeyErr)
	}
	env := dbtest.Start(t)
	d, err := db.Open(t.Context(), config.Config{DatabaseURL: env.AppURL, DatabaseSystemURL: env.SystemURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(d.Close)
	c, err := cache.Open(t.Context(), valkeyURL, cache.Options{})
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	store, err := blob.Open(t.Context(), config.Config{
		BlobURL: "file://" + t.TempDir(), BlobSigningKey: bytes.Repeat([]byte{7}, 32), PublicURL: "http://localhost:8080",
	})
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	if closer, ok := store.(io.Closer); ok {
		t.Cleanup(func() { _ = closer.Close() })
	}

	h := &harness{t: t, env: env, db: d, cache: c, blob: store, llm: fakellm.New()}
	h.llm.HandleGenerate(func(context.Context, llm.Request) (llm.Result, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.generateErr != nil {
			return llm.Result{}, h.generateErr
		}
		return llm.Result{Text: summary}, nil
	})
	h.deps = Deps{DB: d, Cache: c, Blob: store, LLM: h.llm}
	return h
}

func (h *harness) failGenerate(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.generateErr = err
}

func inline(emails ...string) Options {
	return Options{Domain: domain, MemberEmails: emails, IndexInline: true}
}

func (h *harness) run(o Options) error { return Run(h.t.Context(), h.deps, o) }

func (h *harness) mustRun(o Options) {
	h.t.Helper()
	if err := h.run(o); err != nil {
		h.t.Fatalf("Run: %v", err)
	}
}

// workspace returns the workspace of the seeded domain and the user ids of emails.
func (h *harness) workspace(emails ...string) (uuid.UUID, []uuid.UUID) {
	h.t.Helper()
	ws, ids, err := identity.EnsureSeedWorkspace(h.t.Context(), h.db, domain, emails)
	if err != nil {
		h.t.Fatalf("EnsureSeedWorkspace: %v", err)
	}
	return ws, ids
}

func (h *harness) services() (chat.Service, *authz.Authz) {
	az := authz.New(chat.NewMembership(h.db, h.cache))
	return chat.New(chat.Deps{DB: h.db, Authz: az, Cache: h.cache}), az
}

func (h *harness) retriever() knowledge.Retriever {
	svc, az := h.services()
	return knowledge.NewRetriever(knowledge.Deps{DB: h.db, LLM: h.llm, Authz: az, Chat: svc})
}

func user(ws, id uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorUser, WorkspaceID: ws, UserID: id}
}

// channels returns the channels the user sees, by name.
func (h *harness) channels(ws, id uuid.UUID) map[string]*chatv1.Channel {
	h.t.Helper()
	svc, _ := h.services()
	list, err := svc.ListChannels(h.t.Context(), user(ws, id))
	if err != nil {
		h.t.Fatalf("ListChannels: %v", err)
	}
	out := map[string]*chatv1.Channel{}
	for _, c := range list {
		out[c.GetName()] = c
	}
	return out
}

// asOwner runs fn as kiwi_owner in a transaction bound to ws: the tests read tenant
// rows this way, never through kiwi_system.
func (h *harness) asOwner(ws uuid.UUID, fn func(ctx context.Context, tx pgx.Tx)) {
	h.t.Helper()
	ctx := h.t.Context()
	conn, err := pgx.Connect(ctx, h.env.OwnerURL)
	if err != nil {
		h.t.Fatalf("connect as owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('kiwi.workspace_id', $1, true)`, ws.String()); err != nil {
		h.t.Fatal(err)
	}
	fn(ctx, tx)
	if err := tx.Commit(ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) count(ws uuid.UUID, sql string, args ...any) (n int) {
	h.t.Helper()
	h.asOwner(ws, func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			h.t.Fatalf("%s: %v", sql, err)
		}
	})
	return n
}

// tables are everything the seed writes; the platform tables have no workspace_id and
// are counted whole, which is the workspace's own: one workspace is seeded per test.
var tables = []string{
	"identity.workspaces", "identity.users", "chat.channels", "chat.channel_members", "chat.messages",
	"files.files", "meetings.meetings", "meetings.transcript_entries", "meetings.summaries", "tasks.tasks",
	"knowledge.chunks", "platform.outbox", "platform.processed_events", "platform.ref_counters",
}

func (h *harness) counts(ws uuid.UUID) map[string]int {
	h.t.Helper()
	out := make(map[string]int, len(tables))
	for _, tbl := range tables {
		out[tbl] = h.count(ws, `SELECT count(*) FROM `+tbl)
	}
	return out
}

// ---- tests ------------------------------------------------------------------

func TestSeed_Idempotent(t *testing.T) {
	h := newHarness(t)
	h.mustRun(inline(members...))
	ws, _ := h.workspace(members...)
	first := h.counts(ws)
	generated, embedded := len(h.llm.GenerateCalls()), len(h.llm.EmbedCalls())
	t.Logf("rows after a run: %v", first)

	h.mustRun(inline(members...))

	if second := h.counts(ws); !mapsEqual(first, second) {
		t.Errorf("row counts changed on the second run:\n first  %v\n second %v", first, second)
	}
	if n := len(h.llm.GenerateCalls()); n != generated {
		t.Errorf("the second run called the model %d more times, want 0: the document is already analysed", n-generated)
	}
	if n := len(h.llm.EmbedCalls()); n != embedded {
		t.Errorf("the second run embedded %d more batches, want 0: nothing changed", n-embedded)
	}

	// What a run writes: 3 channels, 7 memberships, 14 messages plus the attachment, the
	// document's summary and the meeting's slip, 3 drafts, one meeting of 14 entries.
	want := map[string]int{
		"identity.users": 3, "chat.channels": 3, "chat.channel_members": 7, "chat.messages": 17,
		"files.files": 1, "meetings.meetings": 1, "meetings.transcript_entries": 14, "meetings.summaries": 1, "tasks.tasks": 3,
	}
	for tbl, n := range want {
		if first[tbl] != n {
			t.Errorf("%s has %d rows, want %d", tbl, first[tbl], n)
		}
	}
	if first["knowledge.chunks"] == 0 {
		t.Error("nothing was indexed")
	}

	// Every message went through the chat service, so every message has its event.
	missing := h.count(ws, `
		SELECT count(*) FROM chat.messages m
		WHERE NOT EXISTS (SELECT 1 FROM platform.outbox o
		                  WHERE o.topic = 'chat.message.created' AND o.payload -> 'message' ->> 'id' = m.id::text)`)
	if missing != 0 {
		t.Errorf("%d messages have no chat.message.created row in the outbox", missing)
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestSeed_PrivateChannelHasOnlyFirstMember(t *testing.T) {
	h := newHarness(t)
	h.mustRun(inline(members...))
	ws, ids := h.workspace(members...)
	svc, _ := h.services()

	byName := h.channels(ws, ids[0])
	hr, ok := byName["hr-private"]
	if !ok || hr.GetKind() != chatv1.ChannelKind_CHANNEL_KIND_PRIVATE {
		t.Fatalf("channels = %v, want a private hr-private", byName)
	}
	hrID := uuid.MustParse(hr.GetId())
	got, err := svc.ListMembers(t.Context(), user(ws, ids[0]), hrID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].GetUserId() != ids[0].String() {
		t.Errorf("members of #hr-private = %v, want only %s", got, ids[0])
	}
	for _, other := range ids[1:] {
		if _, seen := h.channels(ws, other)["hr-private"]; seen {
			t.Errorf("user %s sees #hr-private", other)
		}
		if _, err := svc.ListMembers(t.Context(), user(ws, other), hrID); err == nil {
			t.Errorf("user %s may read the members of #hr-private", other)
		}
	}

	for _, name := range []string{"general", "q4-launch"} {
		for i, id := range ids {
			if c, seen := h.channels(ws, id)[name]; !seen || c.GetKind() != chatv1.ChannelKind_CHANNEL_KIND_PUBLIC {
				t.Errorf("member %d does not see the public #%s", i, name)
			}
		}
		ch := uuid.MustParse(byName[name].GetId())
		if n := h.count(ws, `SELECT count(*) FROM chat.channel_members WHERE channel_id = $1`, ch); n != len(ids) {
			t.Errorf("#%s has %d members, want all %d", name, n, len(ids))
		}
	}
}

func TestSeed_ContentIsRetrievableByMembersOnly(t *testing.T) {
	h := newHarness(t)
	h.mustRun(inline(members...))
	ws, ids := h.workspace(members...)
	retr := h.retriever()
	search := func(who uuid.UUID, text string) []knowledge.Hit {
		t.Helper()
		hits, err := retr.Search(t.Context(), user(ws, who), knowledge.Query{Text: text, Lang: "en", Limit: 100})
		if err != nil {
			t.Fatalf("Search(%q): %v", text, err)
		}
		return hits
	}
	contains := func(hits []knowledge.Hit, kind, phrase string) bool {
		return slices.ContainsFunc(hits, func(h knowledge.Hit) bool {
			return (kind == "" || h.SourceKind == kind) && strings.Contains(h.Content, phrase)
		})
	}

	// A member of #q4-launch finds the warehouse document, and the meeting and the chat.
	bobHits := search(ids[1], "Bang Na warehouse capacity")
	if !contains(bobHits, "doc", "9,600 pallet positions") {
		t.Errorf("a member of #q4-launch did not find the warehouse document: %v", bobHits)
	}
	if !contains(bobHits, "meeting", "cross-dock") || !contains(bobHits, "chat", "go-live is 3 November") {
		t.Errorf("a member of #q4-launch did not find the meeting and the channel: %v", bobHits)
	}

	// #hr-private is found by its member and by no one else: the outsider's hits are
	// bounded by the channels they are in, with the vector search handing back everything
	// it can reach.
	const private = "merit increase pool"
	if hits := search(ids[0], private); !contains(hits, "chat", private) {
		t.Fatalf("the member of #hr-private did not find its content: %v", hits)
	}
	hr := uuid.MustParse(h.channels(ws, ids[0])["hr-private"].GetId())
	for _, outsider := range ids[1:] {
		hits := search(outsider, private)
		if contains(hits, "", private) || contains(hits, "", "performance review calibration") {
			t.Errorf("a user outside #hr-private retrieved its content: %v", hits)
		}
		if slices.ContainsFunc(hits, func(h knowledge.Hit) bool { return h.ChannelID != nil && *h.ChannelID == hr }) {
			t.Errorf("a user outside #hr-private got a chunk of that channel: %v", hits)
		}
	}
}

func TestSeed_DocumentGoesThroughUploadAndAnalysis(t *testing.T) {
	h := newHarness(t)
	h.mustRun(inline(members...))
	ws, ids := h.workspace(members...)
	svc, _ := h.services()
	q4 := uuid.MustParse(h.channels(ws, ids[1])["q4-launch"].GetId())

	msgs, err := svc.ListMessages(t.Context(), user(ws, ids[1]), q4, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var attachment, slip *chatv1.Message
	for _, m := range msgs {
		switch {
		case len(m.GetFiles()) > 0:
			attachment = m
		case m.GetArtifact().GetKind() == chatv1.ArtifactKind_ARTIFACT_KIND_DOC_SUMMARY:
			slip = m
		}
	}
	if attachment == nil || slip == nil {
		t.Fatalf("#q4-launch = %v, want the attachment message and the document's summary slip", msgs)
	}
	file := attachment.GetFiles()[0]
	if attachment.GetAuthorId() != ids[0].String() || file.GetName() != "bang-na-warehouse-capacity.txt" || file.GetMime() != "text/plain" {
		t.Errorf("attachment = %v, want the document uploaded by the first member", attachment)
	}
	if got := slip.GetArtifact(); got.GetRef() != "DOC-1001" || !strings.Contains(got.GetBodyMarkdown(), "Bang Na warehouse") || got.GetFileId() != file.GetFileId() {
		t.Errorf("slip = %v, want DOC-1001 with the model's summary of the file", got)
	}

	// The bytes in the store are the corpus's, and the file is ready.
	docsSvc := docs.New(docs.Deps{DB: h.db, Authz: authz.New(chat.NewMembership(h.db, h.cache)), Blob: h.blob, Chat: svc, LLM: h.llm})
	pages, err := docsSvc.ExtractPages(t.Context(), user(ws, ids[1]), uuid.MustParse(file.GetFileId()))
	if err != nil || len(pages) != 1 || !strings.Contains(pages[0].Markdown, "Zone A, ambient fast-moving stock") || !strings.Contains(pages[0].Markdown, "Demo data · ข้อมูลตัวอย่าง") {
		t.Fatalf("ExtractPages = %v, %v, want the corpus document", pages, err)
	}
}

func TestSeed_MeetingHasItsSlipAndThreeDrafts(t *testing.T) {
	h := newHarness(t)
	h.mustRun(inline(members...))
	ws, ids := h.workspace(members...)
	svc, az := h.services()
	q4 := uuid.MustParse(h.channels(ws, ids[0])["q4-launch"].GetId())

	var meetingID string
	h.asOwner(ws, func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT id::text FROM meetings.meetings`).Scan(&meetingID); err != nil {
			t.Fatal(err)
		}
	})
	meetingsSvc := meetings.New(meetings.Deps{DB: h.db, Authz: az, Chat: svc})
	m, err := meetingsSvc.GetMeeting(t.Context(), user(ws, ids[1]), uuid.MustParse(meetingID))
	if err != nil || m.GetStatus().String() != "MEETING_STATUS_READY" || m.GetRef() != "MTG-1001" || m.GetTitle() != "Q4 launch sync" ||
		len(m.GetDecisions()) != 2 || len(m.GetOpenQuestions()) != 1 {
		t.Fatalf("GetMeeting = %v, %v, want the ready MTG-1001 with its summary", m, err)
	}
	entries, err := meetingsSvc.TranscriptEntries(t.Context(), user(ws, ids[1]), uuid.MustParse(meetingID))
	if err != nil || len(entries) != 14 || entries[0].GetSpeaker() != "Alice" || entries[1].GetSpeaker() != "Bob" || entries[11].GetStartMs() != 271000 {
		t.Fatalf("TranscriptEntries = %v, %v, want 14 entries spoken by the first two members, with timing", entries, err)
	}

	tasksSvc := tasks.New(tasks.Deps{DB: h.db, Authz: az, MemberChannels: svc.MemberChannelIDs, ChannelMembers: svc.ListMembers})
	drafts, err := tasksSvc.ListTasks(t.Context(), user(ws, ids[0]), tasks.ListTasksQuery{ChannelID: &q4, States: []tasksv1.TaskState{tasksv1.TaskState_TASK_STATE_DRAFT}})
	if err != nil || len(drafts) != 3 {
		t.Fatalf("drafts = %v, %v, want 3", drafts, err)
	}
	owners := map[string]string{}
	for _, d := range drafts {
		owners[d.GetTitle()] = d.GetAssigneeId()
		if d.GetState() != tasksv1.TaskState_TASK_STATE_DRAFT || d.GetSource().GetKind() != chatv1.SourceKind_SOURCE_KIND_MEETING || d.GetSource().GetId() != meetingID ||
			d.GetSource().GetLabel() != "Q4 launch sync" || d.GetDueAt() == nil {
			t.Errorf("draft = %v, want a draft sourced from the meeting with a due date", d)
		}
	}
	want := map[string]string{
		"Update the launch checklist with the capacity numbers":     ids[1].String(),
		"Confirm the overflow warehouse rental budget with finance": ids[0].String(),
		"Draft the delivery promise for the landing page":           "",
	}
	for title, owner := range want {
		if got, ok := owners[title]; !ok || got != owner {
			t.Errorf("draft %q is owned by %q (listed: %v), want %q", title, got, ok, owner)
		}
	}
	for _, d := range drafts {
		if d.GetTitle() == "Update the launch checklist with the capacity numbers" && (d.GetSource().GetAnchor() != "11" || d.GetSource().GetOffsetMs() != 271000) {
			t.Errorf("draft = %v, want it anchored at transcript entry 11, 271000 ms", d)
		}
	}
}

func TestSeed_FirstRunWithoutInlineIndexLeavesTheWorkToTheWorkers(t *testing.T) {
	h := newHarness(t)
	o := inline(members...)
	o.IndexInline = false
	h.mustRun(o)
	ws, _ := h.workspace(members...)

	if n := h.count(ws, `SELECT count(*) FROM knowledge.chunks`); n != 0 {
		t.Errorf("%d chunks, want none: indexing is left to the embed.index job", n)
	}
	if n := len(h.llm.GenerateCalls()) + len(h.llm.EmbedCalls()); n != 0 {
		t.Errorf("the model was called %d times, want 0", n)
	}
	if st := h.count(ws, `SELECT count(*) FROM files.files WHERE status = 'analyzing'`); st != 1 {
		t.Errorf("%d analysing files, want the document waiting for the doc.analyze job", st)
	}
	for topic, want := range map[string]int{"doc.uploaded": 1, "embed.index": 1} {
		if n := h.count(ws, `SELECT count(*) FROM platform.outbox WHERE topic = $1`, topic); n != want {
			t.Errorf("%d %s rows in the outbox, want %d", n, topic, want)
		}
	}

	// Run inline afterwards: the document is found, analysed and everything indexed.
	h.mustRun(inline(members...))
	if n := h.count(ws, `SELECT count(*) FROM files.files WHERE status = 'ready'`); n != 1 {
		t.Errorf("%d ready files after the inline run, want 1", n)
	}
	if n := h.count(ws, `SELECT count(*) FROM files.files`); n != 1 {
		t.Errorf("%d files, want the one document found again, not uploaded twice", n)
	}
	if n := h.count(ws, `SELECT count(*) FROM knowledge.chunks`); n == 0 {
		t.Error("nothing was indexed by the inline run")
	}
}

func TestSeed_AnalysisThatFailsIsFinishedByTheNextRun(t *testing.T) {
	h := newHarness(t)
	h.failGenerate(errors.New("model down"))

	err := h.run(inline(members...))
	if err == nil || !strings.Contains(err.Error(), "model down") {
		t.Fatalf("Run = %v, want the analysis error", err)
	}
	ws, _ := h.workspace(members...)
	if n := h.count(ws, `SELECT count(*) FROM files.files`); n != 1 {
		t.Fatalf("%d files after a failed run, want the uploaded document", n)
	}

	h.failGenerate(nil)
	h.mustRun(inline(members...))
	if n := h.count(ws, `SELECT count(*) FROM files.files WHERE status = 'ready' AND ref = 'DOC-1001'`); n != 1 {
		t.Errorf("%d ready files after the second run, want the one document, analysed", n)
	}
	if n := h.count(ws, `SELECT count(*) FROM files.files`); n != 1 {
		t.Errorf("%d files, want 1: the second run continues with the first run's upload", n)
	}
	if n := h.count(ws, `SELECT count(*) FROM tasks.tasks`); n != 3 {
		t.Errorf("%d drafts, want 3", n)
	}
}

func TestSeed_OneMemberOwnsEverything(t *testing.T) {
	h := newHarness(t)
	h.mustRun(inline(alice))
	ws, ids := h.workspace(alice)

	if n := h.count(ws, `SELECT count(*) FROM identity.users`); n != 1 {
		t.Errorf("%d users, want 1", n)
	}
	if n := h.count(ws, `SELECT count(*) FROM tasks.tasks WHERE assignee_id = $1`, ids[0]); n != 2 {
		t.Errorf("the one member owns %d drafts, want the 2 that have an owner; the third stays without", n)
	}
	if n := h.count(ws, `SELECT count(*) FROM chat.messages WHERE author_kind = 'user' AND author_id <> $1`, ids[0]); n != 0 {
		t.Errorf("%d messages by another author, want every message by the one member", n)
	}
}

func TestSeed_RefusesWhatItCannotSeed(t *testing.T) {
	h := newHarness(t)
	for name, o := range map[string]Options{
		"no members and no domain": {},
		"no members":               {Domain: domain},
		"a member twice":           {Domain: domain, MemberEmails: []string{alice, "Alice@Demo.example"}},
		"a blank member":           {Domain: domain, MemberEmails: []string{alice, " "}},
		"a malformed email":        {Domain: domain, MemberEmails: []string{"alice"}},
	} {
		if err := h.run(o); err == nil {
			t.Errorf("%s: Run succeeded, want an error", name)
		}
	}
	// No workspace was made either: the system pool sees them all.
	conn, err := pgx.Connect(t.Context(), h.env.SystemURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var n int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM identity.workspaces`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d workspaces after refused runs (%v), want 0", n, err)
	}
}

func TestSeed_RefusesAnAddressThatIsInAnotherWorkspace(t *testing.T) {
	h := newHarness(t)
	h.mustRun(inline(alice))

	err := h.run(Options{MemberEmails: []string{alice}}) // the workspace without a domain
	if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), alice) {
		t.Fatalf("Run = %v, want errs.ErrInvalid naming %s: an address is in one workspace", err, alice)
	}
	if n := h.workspaces(); n != 1 {
		t.Errorf("%d workspaces, want only the first: the refused run made none", n)
	}
}

// workspaces counts every workspace through the system pool.
func (h *harness) workspaces() (n int) {
	h.t.Helper()
	conn, err := pgx.Connect(h.t.Context(), h.env.SystemURL)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = conn.Close(h.t.Context()) }()
	if err := conn.QueryRow(h.t.Context(), `SELECT count(*) FROM identity.workspaces`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// A team whose Google accounts carry no hd is seeded without a domain: the one
// workspace without one, its members the invitations the sign-in rule reads. The seed
// puts its two fixed demo people in front of the members given, so the first of them
// owns the channels and the conversation is not all the real member's.
func TestSeed_WithoutADomain_PutsTwoDemoPeopleBeforeTheMembersGiven(t *testing.T) {
	const first, second = "owner@gmail.example", "Second@Gmail.example"
	h := newHarness(t)

	h.mustRun(Options{MemberEmails: []string{first, second}, IndexInline: true})

	// The demo people are fixed, and on the reserved TLD no Google account can have.
	if want := []string{"nok@kiwi-demo.invalid", "pim@kiwi-demo.invalid"}; !slices.Equal(DemoPeople, want) {
		t.Fatalf("DemoPeople = %v, want %v", DemoPeople, want)
	}
	everyone := slices.Concat(DemoPeople, []string{first, "second@gmail.example"})
	ws, ids, err := identity.EnsureSeedWorkspace(t.Context(), h.db, "", everyone)
	if err != nil {
		t.Fatalf("EnsureSeedWorkspace: %v", err)
	}
	if n := h.workspaces(); n != 1 {
		t.Fatalf("%d workspaces, want the one without a domain", n)
	}
	if n := h.count(ws, `SELECT count(*) FROM identity.workspaces WHERE google_domain IS NULL`); n != 1 {
		t.Errorf("%d workspaces without a domain, want 1", n)
	}
	if n := h.count(ws, `SELECT count(*) FROM identity.users`); n != 4 {
		t.Errorf("%d users, want the 2 demo people and the 2 given", n)
	}
	if n := h.count(ws, `SELECT count(*) FROM identity.users WHERE google_sub = 'seed:' || email
		AND email IN ('nok@kiwi-demo.invalid', 'pim@kiwi-demo.invalid', 'owner@gmail.example', 'second@gmail.example')`); n != 4 {
		t.Errorf("%d placeholder users with the lowercased addresses, want 4", n)
	}
	if hr, ok := h.channels(ws, ids[0])["hr-private"]; !ok || hr.GetKind() != chatv1.ChannelKind_CHANNEL_KIND_PRIVATE {
		t.Errorf("the first demo person sees %v, want the private hr-private", h.channels(ws, ids[0]))
	}
	for i, id := range ids[1:] {
		if _, ok := h.channels(ws, id)["hr-private"]; ok {
			t.Errorf("member %d sees hr-private", i+1)
		}
	}
	first1 := h.counts(ws)

	h.mustRun(Options{MemberEmails: []string{first, second}, IndexInline: true})
	if again := h.counts(ws); !mapsEqual(first1, again) {
		t.Errorf("row counts changed on the second run:\n first  %v\n second %v", first1, again)
	}
}

// The member given is a plain member of the public channels: Nok and Pim write the
// conversation and own what the meeting drafted, and the member cannot read the one
// channel they are not in, which is a real access demonstration.
func TestSeed_WithoutADomain_TheMemberGivenAuthorsAndOwnsNothing(t *testing.T) {
	const given = "Owner@Gmail.example"
	h := newHarness(t)
	h.mustRun(Options{MemberEmails: []string{given}, IndexInline: true})
	ws, ids, err := identity.EnsureSeedWorkspace(t.Context(), h.db, "", slices.Concat(DemoPeople, []string{given}))
	if err != nil {
		t.Fatalf("EnsureSeedWorkspace: %v", err)
	}
	nok, pim, owner := ids[0], ids[1], ids[2]

	for what, query := range map[string]string{
		"messages":            `SELECT count(*) FROM chat.messages WHERE author_kind = 'user' AND author_id = $1`,
		"channels they own":   `SELECT count(*) FROM chat.channel_members WHERE role = 'owner' AND user_id = $1`,
		"drafts they own":     `SELECT count(*) FROM tasks.tasks WHERE assignee_id = $1`,
		"uploaded files":      `SELECT count(*) FROM files.files WHERE uploaded_by = $1`,
		"meetings they own":   `SELECT count(*) FROM meetings.meetings WHERE owner_user_id = $1`,
		"private memberships": `SELECT count(*) FROM chat.channel_members m JOIN chat.channels c ON c.id = m.channel_id WHERE c.kind = 'private' AND m.user_id = $1`,
	} {
		if n := h.count(ws, query, owner); n != 0 {
			t.Errorf("the member given has %d %s, want none: they did nothing in the demo", n, what)
		}
	}
	for _, demo := range []uuid.UUID{nok, pim} {
		if n := h.count(ws, `SELECT count(*) FROM chat.messages WHERE author_kind = 'user' AND author_id = $1`, demo); n == 0 {
			t.Errorf("demo person %s wrote no message", demo)
		}
		if n := h.count(ws, `SELECT count(*) FROM tasks.tasks WHERE assignee_id = $1`, demo); n != 1 {
			t.Errorf("demo person %s owns %d drafts, want 1 of the 2 that have an owner", demo, n)
		}
	}
	if n := h.count(ws, `SELECT count(*) FROM chat.channel_members WHERE role = 'owner' AND user_id = $1`, nok); n != 3 {
		t.Errorf("the first demo person owns %d channels, want all 3", n)
	}
	for _, name := range []string{"general", "q4-launch"} {
		if c, ok := h.channels(ws, owner)[name]; !ok || c.GetKind() != chatv1.ChannelKind_CHANNEL_KIND_PUBLIC {
			t.Errorf("the member given does not see the public #%s", name)
		}
	}

	retr := h.retriever()
	search := func(who uuid.UUID, text string) []knowledge.Hit {
		t.Helper()
		hits, err := retr.Search(t.Context(), user(ws, who), knowledge.Query{Text: text, Lang: "en", Limit: 100})
		if err != nil {
			t.Fatalf("Search(%q): %v", text, err)
		}
		return hits
	}
	contains := func(hits []knowledge.Hit, phrase string) bool {
		return slices.ContainsFunc(hits, func(h knowledge.Hit) bool { return strings.Contains(h.Content, phrase) })
	}
	const private = "merit increase pool"
	if hits := search(nok, private); !contains(hits, private) {
		t.Fatalf("the first demo person did not find #hr-private: %v", hits)
	}
	if hits := search(owner, private); contains(hits, private) || contains(hits, "performance review calibration") {
		t.Errorf("the member given retrieved what is said in #hr-private: %v", hits)
	}
	if hits := search(owner, "Bang Na warehouse capacity"); !contains(hits, "9,600 pallet positions") {
		t.Errorf("the member given did not find the warehouse document in #q4-launch: %v", hits)
	}
}

// A seed with a domain is what it was: the members given, and only them.
func TestSeed_WithADomain_AddsNoDemoPeople(t *testing.T) {
	h := newHarness(t)
	h.mustRun(inline(members...))
	ws, _ := h.workspace(members...)

	if n := h.count(ws, `SELECT count(*) FROM identity.users`); n != len(members) {
		t.Errorf("%d users, want only the %d given", n, len(members))
	}
	if n := h.count(ws, `SELECT count(*) FROM identity.users WHERE email LIKE '%@kiwi-demo.invalid'`); n != 0 {
		t.Errorf("%d demo people in a workspace with a domain, want none", n)
	}
}
