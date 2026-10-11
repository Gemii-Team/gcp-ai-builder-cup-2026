package docs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	filesv1 "kiwi/gen/kiwi/files/v1"
	"kiwi/gen/kiwi/files/v1/filesv1connect"
	"kiwi/internal/chat"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/blob"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/testkit/dbtest"
	"kiwi/internal/testkit/fakellm"
)

// ---- fixture ----------------------------------------------------------------

const publicURL = "https://kiwi.test"

const (
	attachment = filesv1.UploadPurpose_UPLOAD_PURPOSE_ATTACHMENT
	recording  = filesv1.UploadPurpose_UPLOAD_PURPOSE_RECORDING
)

// sqlMembership answers channel membership straight from chat.channel_members over
// db.InWorkspace: the docs tests need no Valkey, and the table is the real one behind
// chat's own lookup.
type sqlMembership struct{ db *db.DB }

func (m sqlMembership) IsMember(ctx context.Context, channelID, userID uuid.UUID) (ok bool, err error) {
	err = m.db.InWorkspace(ctx, authz.WorkspaceFrom(ctx), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM chat.channel_members WHERE channel_id = $1 AND user_id = $2)`,
			channelID, userID).Scan(&ok)
	})
	return ok, err
}

// fixture is one migrated database, a local object store on a temp dir, the real chat
// service and a scripted model, with a workspace holding Alice, Bob and Carol.
type fixture struct {
	t     *testing.T
	env   *dbtest.Env
	db    *db.DB
	local *blob.Local
	chat  chat.Service
	llm   *fakellm.Client
	deps  Deps
	svc   Service
	// analyze is the doc.analyze handler.
	analyze bus.Handler
	// logs holds what the docs code logged during the test.
	logs *bytes.Buffer

	ws                uuid.UUID
	alice, bob, carol uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := dbtest.Start(t)
	// The code under test logs the failures it records; keep them out of the test output.
	logs := new(bytes.Buffer)
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	d, err := db.Open(t.Context(), config.Config{DatabaseURL: env.AppURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(d.Close)

	signingKey := make([]byte, 32)
	if _, err := rand.Read(signingKey); err != nil {
		t.Fatal(err)
	}
	st, err := blob.Open(t.Context(), config.Config{
		BlobURL:        (&url.URL{Scheme: "file", Path: t.TempDir()}).String(),
		BlobSigningKey: signingKey,
		PublicURL:      publicURL,
	})
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	local, ok := st.(*blob.Local)
	if !ok {
		t.Fatalf("blob.Open returned %T, want *blob.Local", st)
	}
	t.Cleanup(func() { _ = local.Close() })

	az := authz.New(sqlMembership{db: d})
	f := &fixture{t: t, env: env, db: d, local: local, llm: fakellm.New(), logs: logs}
	f.chat = chat.New(chat.Deps{DB: d, Authz: az})
	f.deps = Deps{DB: d, Authz: az, Blob: local, Chat: f.chat, LLM: f.llm}
	f.svc = New(f.deps)
	f.analyze = Jobs(f.deps)[0].Handler
	f.ws = f.seedWorkspace("acme.example")
	f.alice = f.seedUser("Alice")
	f.bob = f.seedUser("Bob")
	f.carol = f.seedUser("Carol")
	return f
}

// asOwner runs fn as kiwi_owner in a transaction bound to the fixture workspace: the
// tests seed and inspect rows this way and never through kiwi_system.
func (f *fixture) asOwner(fn func(ctx context.Context, tx pgx.Tx)) {
	f.t.Helper()
	f.asOwnerIn(f.ws, fn)
}

func (f *fixture) asOwnerIn(ws uuid.UUID, fn func(ctx context.Context, tx pgx.Tx)) {
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

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	f.asOwner(func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			f.t.Fatalf("%s: %v", sql, err)
		}
	})
}

func (f *fixture) scalar(dst any, sql string, args ...any) {
	f.t.Helper()
	f.asOwner(func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			f.t.Fatalf("%s: %v", sql, err)
		}
	})
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	f.scalar(&n, sql, args...)
	return n
}

func (f *fixture) seedWorkspace(domain string) uuid.UUID {
	f.t.Helper()
	ws := uuid.New()
	f.asOwnerIn(ws, func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, $2, $2)`, ws, domain); err != nil {
			f.t.Fatal(err)
		}
	})
	return ws
}

func (f *fixture) seedUser(name string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO identity.users (id, workspace_id, google_sub, email, name) VALUES ($1, $2, $3, $4, $5)`,
		id, f.ws, "sub-"+id.String(), strings.ToLower(name)+"@example.test", name)
	return id
}

// channel creates a channel of kind in the fixture workspace with the given members.
func (f *fixture) channel(kind string, members ...uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO chat.channels (id, workspace_id, name, kind) VALUES ($1, $2, $3, $4)`,
		id, f.ws, "ch-"+id.String()[:8], kind)
	for _, m := range members {
		f.exec(`INSERT INTO chat.channel_members (channel_id, user_id, workspace_id, role) VALUES ($1, $2, $3, 'member')`,
			id, m, f.ws)
	}
	return id
}

func (f *fixture) user(id uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorUser, WorkspaceID: f.ws, UserID: id}
}

func (f *fixture) system(origin uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorSystem, WorkspaceID: f.ws, UserID: origin}
}

// topics returns the topic of every outbox row, in order.
func (f *fixture) topics() []string {
	f.t.Helper()
	var topics []string
	f.asOwner(func(ctx context.Context, tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT topic FROM platform.outbox ORDER BY id`)
		if err != nil {
			f.t.Fatal(err)
		}
		if topics, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			f.t.Fatal(err)
		}
	})
	return topics
}

func (f *fixture) topicCount(topic string) int {
	return f.count(`SELECT count(*) FROM platform.outbox WHERE topic = $1`, topic)
}

// payloads returns the JSON payload of every outbox row of topic, in order.
func (f *fixture) payloads(topic string) []string {
	f.t.Helper()
	var out []string
	f.asOwner(func(ctx context.Context, tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT payload::text FROM platform.outbox WHERE topic = $1 ORDER BY id`, topic)
		if err != nil {
			f.t.Fatal(err)
		}
		if out, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			f.t.Fatal(err)
		}
	})
	return out
}

// messages returns the messages of a channel, oldest first, as Alice sees them.
func (f *fixture) messages(ch uuid.UUID) []*chatv1.Message {
	f.t.Helper()
	msgs, err := f.chat.ListMessages(f.t.Context(), f.user(f.alice), ch, 0, 100)
	if err != nil {
		f.t.Fatalf("ListMessages: %v", err)
	}
	slices.Reverse(msgs)
	return msgs
}

// put sends body to a signed upload URL as a browser would and returns the status.
func (f *fixture) put(uploadURL string, headers map[string]string, body []byte) int {
	f.t.Helper()
	path, ok := strings.CutPrefix(uploadURL, publicURL)
	if !ok {
		f.t.Fatalf("upload URL %q is not under %s", uploadURL, publicURL)
	}
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.local.ServeHTTP(rec, req)
	return rec.Code
}

// get downloads a signed URL and returns the status, content type and body.
func (f *fixture) get(downloadURL string) (int, string, []byte) {
	f.t.Helper()
	path, ok := strings.CutPrefix(downloadURL, publicURL)
	if !ok {
		f.t.Fatalf("download URL %q is not under %s", downloadURL, publicURL)
	}
	rec := httptest.NewRecorder()
	f.local.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()
}

// putOversize stores size bytes under key directly, past the limit the signed PUT
// enforces, as an upload that lied about its size or type would have.
func (f *fixture) putOversize(key, contentType string, size int) {
	f.t.Helper()
	url, headers, err := f.local.SignedPut(f.t.Context(), key, contentType, int64(size)+1, time.Minute)
	if err != nil {
		f.t.Fatalf("SignedPut: %v", err)
	}
	if code := f.put(url, headers, make([]byte, size)); code != http.StatusOK {
		f.t.Fatalf("direct store of %d bytes: status %d", size, code)
	}
}

// create asks for an upload of body as a and returns the response; declared is the
// size to declare, or len(body) when zero.
func (f *fixture) create(a authz.Actor, ch uuid.UUID, name, mime string, purpose filesv1.UploadPurpose, declared int64, body []byte) (*filesv1.CreateUploadResponse, error) {
	if declared == 0 {
		declared = int64(len(body))
	}
	return f.svc.CreateUpload(f.t.Context(), a, &filesv1.CreateUploadRequest{
		ChannelId: ch.String(), Name: name, Mime: mime, Size: declared, Purpose: purpose,
	})
}

// upload runs CreateUpload and the PUT, and returns the file id; the upload is not
// yet completed.
func (f *fixture) upload(a authz.Actor, ch uuid.UUID, name, mime string, purpose filesv1.UploadPurpose, body []byte) uuid.UUID {
	f.t.Helper()
	resp, err := f.create(a, ch, name, mime, purpose, 0, body)
	if err != nil {
		f.t.Fatalf("CreateUpload(%s): %v", name, err)
	}
	if code := f.put(resp.GetUploadUrl(), resp.GetHeaders(), body); code != http.StatusOK {
		f.t.Fatalf("PUT %s: status %d", name, code)
	}
	return uuid.MustParse(resp.GetFileId())
}

// attach uploads and completes an attachment as Alice.
func (f *fixture) attach(ch uuid.UUID, name, mime string, body []byte) uuid.UUID {
	f.t.Helper()
	id := f.upload(f.user(f.alice), ch, name, mime, attachment, body)
	file, err := f.svc.CompleteUpload(f.t.Context(), f.user(f.alice), id)
	if err != nil {
		f.t.Fatalf("CompleteUpload(%s): %v", name, err)
	}
	if file.GetStatus() != filesv1.FileStatus_FILE_STATUS_ANALYZING {
		f.t.Fatalf("CompleteUpload(%s) status = %v, want ANALYZING", name, file.GetStatus())
	}
	return id
}

func (f *fixture) file(id uuid.UUID) *filesv1.File {
	f.t.Helper()
	file, err := f.svc.GetFile(f.t.Context(), f.user(f.alice), id)
	if err != nil {
		f.t.Fatalf("GetFile: %v", err)
	}
	return file
}

func (f *fixture) objectExists(id uuid.UUID) bool {
	f.t.Helper()
	_, err := f.local.Head(f.t.Context(), blobKey(f.ws, id))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		f.t.Fatalf("Head: %v", err)
	}
	return err == nil
}

var pdfBytes = minimalPDF("A one page document.")

// ---- CreateUpload -----------------------------------------------------------

func TestCreateUpload_BindsKeyMimeAndLimit(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	body := bytes.Repeat([]byte("x"), 1024)

	// A parameter on the declared type is dropped: the PUT must send exactly what
	// the response says.
	resp, err := f.svc.CreateUpload(t.Context(), f.user(f.alice), &filesv1.CreateUploadRequest{
		ChannelId: ch.String(), Name: "report.pdf", Mime: "Application/PDF; charset=binary", Size: 1024, Purpose: attachment,
	})
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	id := uuid.MustParse(resp.GetFileId())
	key := "uploads/" + f.ws.String() + "/" + id.String()
	if got := resp.GetHeaders()["Content-Type"]; got != "application/pdf" {
		t.Fatalf("headers = %v, want Content-Type application/pdf", resp.GetHeaders())
	}
	if !strings.HasPrefix(resp.GetUploadUrl(), publicURL+blob.Prefix) {
		t.Fatalf("upload URL %q is not a same-origin signed URL", resp.GetUploadUrl())
	}
	if _, err := f.local.Head(t.Context(), key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Head before the PUT = %v, want fs.ErrNotExist", err)
	}

	if code := f.put(resp.GetUploadUrl(), map[string]string{"Content-Type": "text/plain"}, body); code != http.StatusUnsupportedMediaType {
		t.Fatalf("PUT with another type: status %d, want 415", code)
	}
	if code := f.put(resp.GetUploadUrl(), resp.GetHeaders(), make([]byte, 1025)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT of one byte over the declared size: status %d, want 413", code)
	}
	if code := f.put(resp.GetUploadUrl(), resp.GetHeaders(), body); code != http.StatusOK {
		t.Fatalf("PUT: status %d, want 200", code)
	}

	// The object is at the documented key, with the signed type.
	attrs, err := f.local.Head(t.Context(), key)
	if err != nil || attrs.Size != 1024 || attrs.ContentType != "application/pdf" {
		t.Fatalf("Head(%s) = %+v, %v, want 1024 bytes of application/pdf", key, attrs, err)
	}
	// The file is recorded as declared, before anyone completes it.
	file := f.file(id)
	if file.GetStatus() != filesv1.FileStatus_FILE_STATUS_UPLOADED || file.GetName() != "report.pdf" ||
		file.GetMime() != "application/pdf" || file.GetSize() != 1024 || file.GetChannelId() != ch.String() ||
		file.GetMessageId() != "" || file.GetRef() != "" {
		t.Fatalf("file after CreateUpload = %v", file)
	}
}

func TestCreateUpload_UnsupportedTypeInvalid(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	private := f.channel("private", f.bob)
	alice := f.user(f.alice)

	cases := []struct {
		name    string
		mime    string
		purpose filesv1.UploadPurpose
		size    int64
	}{
		{"docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", attachment, 100},
		{"html", "text/html", attachment, 100},
		{"svg", "image/svg+xml", attachment, 100},
		{"xml", "application/xml", attachment, 100},
		{"javascript", "text/javascript", attachment, 100},
		{"octet-stream", "application/octet-stream", attachment, 100},
		{"empty type", "", attachment, 100},
		{"not a media type", "pdf", attachment, 100},
		{"wav as an attachment", "audio/wav", attachment, 100},
		{"mp3 recording", "audio/mpeg", recording, 100},
		{"m4a recording", "audio/mp4", recording, 100},
		{"mp4 recording", "video/mp4", recording, 100},
		{"ogg recording", "audio/ogg", recording, 100},
		{"flac recording", "audio/flac", recording, 100},
		{"webm recording", "audio/webm", recording, 100},
		{"pdf recording", "application/pdf", recording, 100},
		{"no purpose", "application/pdf", filesv1.UploadPurpose_UPLOAD_PURPOSE_UNSPECIFIED, 100},
		{"no size", "application/pdf", attachment, 0},
		{"negative size", "application/pdf", attachment, -1},
		{"attachment over 25 MiB", "application/pdf", attachment, MaxAttachmentBytes + 1},
		{"recording over 200 MiB", "audio/wav", recording, MaxRecordingBytes + 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.svc.CreateUpload(t.Context(), alice, &filesv1.CreateUploadRequest{
				ChannelId: ch.String(), Name: "file", Mime: c.mime, Size: c.size, Purpose: c.purpose,
			})
			if !errors.Is(err, errs.ErrInvalid) {
				t.Fatalf("CreateUpload(%q, %v, %d) error = %v, want ErrInvalid", c.mime, c.purpose, c.size, err)
			}
		})
	}
	if n := f.count(`SELECT count(*) FROM files.files`); n != 0 {
		t.Fatalf("%d files recorded for refused uploads, want 0", n)
	}

	// The limits themselves are allowed.
	for _, ok := range []struct {
		mime    string
		purpose filesv1.UploadPurpose
		size    int64
	}{
		{"application/pdf", attachment, MaxAttachmentBytes},
		{"audio/wav", recording, MaxRecordingBytes},
		{"audio/x-wav", recording, 1},
		{"audio/wave", recording, 1},
		{"image/png", attachment, 1},
		{"image/jpeg", attachment, 1},
		{"text/plain", attachment, 1},
		{"text/markdown", attachment, 1},
		{"text/csv", attachment, 1},
	} {
		if _, err := f.svc.CreateUpload(t.Context(), alice, &filesv1.CreateUploadRequest{
			ChannelId: ch.String(), Name: "file", Mime: ok.mime, Size: ok.size, Purpose: ok.purpose,
		}); err != nil {
			t.Errorf("CreateUpload(%q, %d) = %v, want accepted", ok.mime, ok.size, err)
		}
	}

	// Who and where: a channel Alice is not in, and one that is not a UUID.
	if _, err := f.create(alice, private, "a.pdf", "application/pdf", attachment, 10, nil); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("CreateUpload in a private channel of others = %v, want ErrForbidden", err)
	}
	if _, err := f.svc.CreateUpload(t.Context(), alice, &filesv1.CreateUploadRequest{
		ChannelId: "general", Name: "a.pdf", Mime: "application/pdf", Size: 10, Purpose: attachment,
	}); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("CreateUpload with a bad channel id = %v, want ErrInvalid", err)
	}
	if _, err := f.create(f.system(f.alice), ch, "a.pdf", "application/pdf", attachment, 10, nil); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("CreateUpload as a system actor = %v, want ErrForbidden", err)
	}
}

// ---- CompleteUpload ---------------------------------------------------------

func TestCompleteUpload_RevalidatesSizeAndMime(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	alice := f.user(f.alice)

	cases := []struct {
		name        string
		contentType string // what the object is really stored as
		size        int    // what the object really holds
		reason      string
	}{
		{"30 MiB declared as 1 KiB", "application/pdf", 30 << 20, reasonTooLarge},
		{"2 KiB declared as 1 KiB", "application/pdf", 2 << 10, reasonTooLarge},
		{"stored as another type", "text/plain", 1 << 10, reasonUnsupportedType},
		{"stored under a bare token", "pdf", 1 << 10, reasonUnsupportedType},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := f.topicCount(events.TopicDocUploaded)
			resp, err := f.create(alice, ch, "contract.pdf", "application/pdf", attachment, 1<<10, nil)
			if err != nil {
				t.Fatal(err)
			}
			id := uuid.MustParse(resp.GetFileId())
			// Past the PUT limit, as a client that lied would have.
			f.putOversize(blobKey(f.ws, id), c.contentType, c.size)

			file, err := f.svc.CompleteUpload(t.Context(), alice, id)
			if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("CompleteUpload error = %v, want ErrInvalid naming %q", err, c.reason)
			}
			if file.GetStatus() != filesv1.FileStatus_FILE_STATUS_FAILED || file.GetFailureReason() != c.reason {
				t.Fatalf("returned file = %v, want FAILED %s", file, c.reason)
			}
			if got := f.file(id); got.GetStatus() != filesv1.FileStatus_FILE_STATUS_FAILED || got.GetFailureReason() != c.reason {
				t.Fatalf("stored file = %v, want FAILED %s", got, c.reason)
			}
			if f.objectExists(id) {
				t.Fatal("the rejected object is still stored")
			}
			if n := f.topicCount(events.TopicDocUploaded); n != before {
				t.Fatalf("a rejected upload enqueued doc.uploaded (%d rows, was %d)", n, before)
			}
			if len(f.messages(ch)) != 0 {
				t.Fatal("a rejected upload was posted to the channel")
			}

			// The failure is announced with its reason, and repeating the call says the
			// same thing instead of starting over.
			last := f.payloads(events.TopicFileStatusChanged)
			if p := last[len(last)-1]; !strings.Contains(p, `"FILE_STATUS_FAILED"`) || !strings.Contains(p, c.reason) {
				t.Fatalf("last file.status_changed = %s, want FAILED with %s", p, c.reason)
			}
			again, err := f.svc.CompleteUpload(t.Context(), alice, id)
			if !errors.Is(err, errs.ErrInvalid) || again.GetFailureReason() != c.reason {
				t.Fatalf("second CompleteUpload = %v, %v, want the same rejection", again, err)
			}
			if _, err := f.svc.DownloadURL(t.Context(), alice, id); !errors.Is(err, errs.ErrNotFound) {
				t.Fatalf("DownloadURL of a rejected file = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestCompleteUpload_ZeroByteRejected(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	alice := f.user(f.alice)

	resp, err := f.create(alice, ch, "empty.txt", "text/plain", attachment, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(resp.GetFileId())
	if code := f.put(resp.GetUploadUrl(), resp.GetHeaders(), nil); code != http.StatusOK {
		t.Fatalf("PUT of nothing: status %d", code)
	}

	file, err := f.svc.CompleteUpload(t.Context(), alice, id)
	if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), reasonEmpty) {
		t.Fatalf("CompleteUpload error = %v, want ErrInvalid naming %q", err, reasonEmpty)
	}
	if file.GetStatus() != filesv1.FileStatus_FILE_STATUS_FAILED || file.GetFailureReason() != reasonEmpty {
		t.Fatalf("file = %v, want FAILED empty", file)
	}
	if n := f.topicCount(events.TopicDocUploaded); n != 0 {
		t.Fatalf("%d doc.uploaded events, want 0", n)
	}
	if f.objectExists(id) || len(f.messages(ch)) != 0 {
		t.Fatal("the empty upload left an object or a message behind")
	}
}

func TestCompleteUpload_WritesFileMessageAndOutboxInOneTx(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	alice := f.user(f.alice)
	body := pdfBytes

	t.Run("commits the file, the message and the events together", func(t *testing.T) {
		// Declared far larger than the file: the stored size is what is recorded.
		resp, err := f.create(alice, ch, "รายงาน Q4.pdf", "application/pdf", attachment, 4096, nil)
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.MustParse(resp.GetFileId())
		if code := f.put(resp.GetUploadUrl(), resp.GetHeaders(), body); code != http.StatusOK {
			t.Fatalf("PUT: status %d", code)
		}
		file, err := f.svc.CompleteUpload(t.Context(), alice, id)
		if err != nil {
			t.Fatalf("CompleteUpload: %v", err)
		}
		if file.GetStatus() != filesv1.FileStatus_FILE_STATUS_ANALYZING || file.GetSize() != int64(len(body)) || file.GetMessageId() == "" {
			t.Fatalf("file = %v, want ANALYZING, %d bytes and a message", file, len(body))
		}

		msgs := f.messages(ch)
		if len(msgs) != 1 {
			t.Fatalf("%d messages, want the attachment", len(msgs))
		}
		m := msgs[0]
		if m.GetId() != file.GetMessageId() || m.GetAuthorKind() != chatv1.AuthorKind_AUTHOR_KIND_USER || m.GetAuthorId() != f.alice.String() ||
			m.GetClientMsgId() != id.String() || m.GetBody() != "รายงาน Q4.pdf" {
			t.Fatalf("message = %v, want Alice's, client_msg_id %s, body the file name", m, id)
		}
		want := &chatv1.FileRef{FileId: id.String(), Name: "รายงาน Q4.pdf", Mime: "application/pdf", Size: int64(len(body))}
		if len(m.GetFiles()) != 1 || m.GetFiles()[0].String() != want.String() {
			t.Fatalf("message files = %v, want %v", m.GetFiles(), want)
		}

		if got := f.topics(); !slices.Equal(got, []string{events.TopicChatMessageCreated, events.TopicDocUploaded, events.TopicFileStatusChanged}) {
			t.Fatalf("outbox topics = %v", got)
		}
		doc := f.payloads(events.TopicDocUploaded)[0]
		for _, want := range []string{f.ws.String(), ch.String(), id.String(), f.alice.String()} {
			if !strings.Contains(doc, want) {
				t.Fatalf("doc.uploaded = %s, missing %s", doc, want)
			}
		}
		if status := f.payloads(events.TopicFileStatusChanged)[0]; !strings.Contains(status, `"FILE_STATUS_ANALYZING"`) {
			t.Fatalf("file.status_changed = %s, want ANALYZING", status)
		}

		// Completing again is harmless: the same file, no second message or event.
		again, err := f.svc.CompleteUpload(t.Context(), alice, id)
		if err != nil || again.GetMessageId() != file.GetMessageId() || again.GetStatus() != filesv1.FileStatus_FILE_STATUS_ANALYZING {
			t.Fatalf("second CompleteUpload = %v, %v, want the same file", again, err)
		}
		if len(f.messages(ch)) != 1 || f.topicCount(events.TopicDocUploaded) != 1 {
			t.Fatal("the second CompleteUpload posted or enqueued again")
		}
	})

	t.Run("a failure after the message insert undoes the lot", func(t *testing.T) {
		ch := f.channel("public", f.alice)
		before := f.topics()
		failing := New(Deps{DB: f.db, Authz: f.deps.Authz, Blob: f.local, Chat: postThenFail{f.chat}, LLM: f.llm})
		id := f.upload(alice, ch, "notes.txt", "text/plain", attachment, []byte("hello"))

		_, err := failing.CompleteUpload(t.Context(), alice, id)
		if err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("CompleteUpload error = %v, want the injected failure", err)
		}
		if got := f.file(id); got.GetStatus() != filesv1.FileStatus_FILE_STATUS_UPLOADED || got.GetMessageId() != "" {
			t.Fatalf("file after the failure = %v, want still uploaded without a message", got)
		}
		if len(f.messages(ch)) != 0 {
			t.Fatal("the message insert survived the failed transaction")
		}
		if after := f.topics(); !slices.Equal(after, before) {
			t.Fatalf("outbox rows after the failure = %v, was %v", after, before)
		}
		if !f.objectExists(id) {
			t.Fatal("a failure of ours deleted the object")
		}
		// The client can simply complete again.
		if _, err := f.svc.CompleteUpload(t.Context(), alice, id); err != nil {
			t.Fatalf("CompleteUpload after the failure: %v", err)
		}
	})

	t.Run("simultaneous completions take turns", func(t *testing.T) {
		ch := f.channel("public", f.alice)
		id := f.upload(alice, ch, "notes.txt", "text/plain", attachment, []byte("hello"))
		before := f.topicCount(events.TopicDocUploaded)

		var wg sync.WaitGroup
		errsOut := make([]error, 8)
		for i := range errsOut {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errsOut[i] = f.svc.CompleteUpload(t.Context(), alice, id)
			}()
		}
		wg.Wait()
		for _, err := range errsOut {
			if err != nil {
				t.Fatalf("CompleteUpload: %v", err)
			}
		}
		if n := len(f.messages(ch)); n != 1 {
			t.Fatalf("%d messages after 8 completions, want 1", n)
		}
		if n := f.topicCount(events.TopicDocUploaded) - before; n != 1 {
			t.Fatalf("%d doc.uploaded events after 8 completions, want 1", n)
		}
	})
}

// postThenFail inserts the message and then fails, as a later step of the
// transaction would.
type postThenFail struct{ chat.Service }

func (p postThenFail) PostMessageTx(ctx context.Context, tx pgx.Tx, a authz.Actor, req *chatv1.SendMessageRequest) (*chatv1.Message, error) {
	if _, err := p.Service.PostMessageTx(ctx, tx, a, req); err != nil {
		return nil, err
	}
	return nil, errors.New("docs test: injected failure after the message insert")
}

func TestCompleteUpload_PreconditionsAndWho(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice, f.bob)
	alice, bob := f.user(f.alice), f.user(f.bob)

	// Nothing has been PUT yet: refused, and the file stays open for the real upload.
	resp, err := f.create(alice, ch, "a.txt", "text/plain", attachment, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(resp.GetFileId())
	if _, err := f.svc.CompleteUpload(t.Context(), alice, id); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("CompleteUpload before the PUT = %v, want ErrInvalid", err)
	}
	if got := f.file(id); got.GetStatus() != filesv1.FileStatus_FILE_STATUS_UPLOADED {
		t.Fatalf("file after an early CompleteUpload = %v, want still uploaded", got)
	}
	if code := f.put(resp.GetUploadUrl(), resp.GetHeaders(), []byte("hello")); code != http.StatusOK {
		t.Fatal(code)
	}

	// Only the uploader completes it, and only a member at all.
	if _, err := f.svc.CompleteUpload(t.Context(), bob, id); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("CompleteUpload by another member = %v, want ErrForbidden", err)
	}
	if _, err := f.svc.CompleteUpload(t.Context(), f.user(f.carol), id); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("CompleteUpload by a non-member = %v, want ErrForbidden", err)
	}
	if _, err := f.svc.CompleteUpload(t.Context(), alice, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("CompleteUpload of an unknown file = %v, want ErrNotFound", err)
	}
	if _, err := f.svc.CompleteUpload(t.Context(), alice, id); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
}

// ---- recordings -------------------------------------------------------------

func TestCompleteUpload_Recording_WAVPasses(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	alice := f.user(f.alice)

	cases := []struct {
		name string
		mime string
		body []byte
	}{
		{"mono 16 kHz", "audio/wav", wav(1, 1, 16000, 16)},
		{"stereo 44.1 kHz", "audio/wav", wav(1, 2, 44100, 16)},
		{"mono 8 kHz", "audio/wav", wav(1, 1, 8000, 16)},
		{"x-wav", "audio/x-wav", wav(1, 1, 16000, 16)},
		{"wave", "audio/wave", wav(1, 1, 16000, 16)},
		{"fmt after a JUNK chunk", "audio/wav", wav(1, 1, 16000, 16, chunk("JUNK", make([]byte, 28)))},
		{"fmt after an odd-sized chunk", "audio/wav", wav(1, 1, 16000, 16, chunk("LIST", make([]byte, 7)))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := f.upload(alice, ch, "meeting.wav", c.mime, recording, c.body)
			file, err := f.svc.CompleteUpload(t.Context(), alice, id)
			if err != nil {
				t.Fatalf("CompleteUpload: %v", err)
			}
			// A recording is not an attachment: it stays uploaded for the meetings
			// domain, and nothing is posted or queued here.
			if file.GetStatus() != filesv1.FileStatus_FILE_STATUS_UPLOADED || file.GetMessageId() != "" || file.GetSize() != int64(len(c.body)) {
				t.Fatalf("file = %v, want uploaded, no message, %d bytes", file, len(c.body))
			}
			if !f.objectExists(id) {
				t.Fatal("an accepted recording was deleted")
			}
			// Completing it again changes nothing.
			if again, err := f.svc.CompleteUpload(t.Context(), alice, id); err != nil || again.GetStatus() != filesv1.FileStatus_FILE_STATUS_UPLOADED {
				t.Fatalf("second CompleteUpload = %v, %v", again, err)
			}
		})
	}
	if len(f.messages(ch)) != 0 {
		t.Fatal("a recording was posted as a message")
	}
	if got := f.topics(); len(got) != 0 {
		t.Fatalf("recordings enqueued %v, want nothing", got)
	}
}

func TestCompleteUpload_Recording_RejectsWhatIsNotPCM16WAV(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	alice := f.user(f.alice)

	pad := func(head []byte) []byte { return append(head, make([]byte, 4096)...) }
	cases := []struct {
		name string
		body []byte
	}{
		{"mp3 with an ID3 tag", pad([]byte("ID3\x03\x00\x00\x00\x00\x0a\x3f"))},
		{"mp3 frames", pad([]byte{0xFF, 0xFB, 0x90, 0x00})},
		{"mp4", pad([]byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"))},
		{"m4a", pad([]byte("\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00M4A mp42isom"))},
		{"ogg", pad([]byte("OggS\x00\x02\x00\x00"))},
		{"flac", pad([]byte("fLaC\x00\x00\x00\x22"))},
		{"webm", pad([]byte{0x1A, 0x45, 0xDF, 0xA3})},
		{"html", []byte("<html><body>not audio</body></html>")},
		{"float32 wav", wav(3, 1, 16000, 32)},
		{"mu-law wav", wav(7, 1, 8000, 8)},
		{"8-bit pcm wav", wav(1, 1, 8000, 8)},
		{"24-bit pcm wav", wav(1, 1, 48000, 24)},
		{"six channel wav", wav(1, 6, 48000, 16)},
		{"zero sample rate", wav(1, 1, 0, 16)},
		{"riff but not wave", riff("AVI ", chunk("LIST", make([]byte, 100)))},
		{"wave without a fmt chunk", riff("WAVE", chunk("data", make([]byte, 100)))},
		{"fmt chunk cut short", []byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00")},
		{"fmt beyond the first chunks", riff("WAVE", chunk("data", make([]byte, 8000)), chunk("fmt ", fmtBody(1, 1, 16000, 16)))},
		{"chunk size past the end", append([]byte("RIFF\x24\x00\x00\x00WAVE"), []byte("JUNK\xff\xff\xff\xff")...)},
		{"header only", []byte("RIFF")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Labelled as WAV, which is all the browser's word is worth.
			id := f.upload(alice, ch, "meeting.wav", "audio/wav", recording, c.body)
			file, err := f.svc.CompleteUpload(t.Context(), alice, id)
			if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), reasonUnsupportedAudio) {
				t.Fatalf("CompleteUpload error = %v, want ErrInvalid naming %q", err, reasonUnsupportedAudio)
			}
			if file.GetStatus() != filesv1.FileStatus_FILE_STATUS_FAILED || file.GetFailureReason() != reasonUnsupportedAudio {
				t.Fatalf("file = %v, want FAILED %s", file, reasonUnsupportedAudio)
			}
			if f.objectExists(id) {
				t.Fatal("the rejected recording is still stored")
			}
			if _, err := f.svc.DownloadURL(t.Context(), alice, id); !errors.Is(err, errs.ErrNotFound) {
				t.Fatalf("DownloadURL = %v, want ErrNotFound", err)
			}
		})
	}
	// None of it reached the transcription service.
	if calls := f.llm.TranscribeCalls(); len(calls) != 0 {
		t.Fatalf("%d recordings were sent to ASR", len(calls))
	}
	if n := f.topicCount(events.TopicDocUploaded); n != 0 {
		t.Fatalf("%d doc.uploaded events for recordings", n)
	}
}

func TestRecordingUpload_ContentDecides_NotNameOrLabel(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	alice := f.user(f.alice)
	body := wav(1, 1, 16000, 16)

	// A WAV under another extension passes: the name is only shown to people.
	for _, name := range []string{"call.mp3", "call.mp4", "call.m4a", "call", "call.webm"} {
		id := f.upload(alice, ch, name, "audio/wav", recording, body)
		if _, err := f.svc.CompleteUpload(t.Context(), alice, id); err != nil {
			t.Errorf("a WAV named %q: %v, want it accepted", name, err)
		}
	}
	// The same bytes under a type that is not a WAV type never get an upload URL.
	for _, mt := range []string{"audio/mpeg", "audio/mp4", "video/mp4", "audio/ogg", "audio/flac", "audio/webm", "video/webm", "audio/aac", "audio/x-m4a", "video/quicktime"} {
		if _, err := f.create(alice, ch, "call.wav", mt, recording, 0, body); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("CreateUpload of a .wav labelled %q = %v, want ErrInvalid", mt, err)
		}
	}
}

// ---- GetFile and DownloadURL ------------------------------------------------

func TestDownloadURL_RequiresChannelMembership(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice)
	id := f.attach(ch, "notes.txt", "text/plain", []byte("shared notes"))

	if _, err := f.svc.DownloadURL(t.Context(), f.user(f.bob), id); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("DownloadURL by a non-member = %v, want ErrForbidden", err)
	}
	if _, err := f.svc.DownloadURL(t.Context(), f.user(f.alice), uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("DownloadURL of an unknown file = %v, want ErrNotFound", err)
	}
	url, err := f.svc.DownloadURL(t.Context(), f.user(f.alice), id)
	if err != nil {
		t.Fatalf("DownloadURL by a member: %v", err)
	}
	status, contentType, body := f.get(url)
	if status != http.StatusOK || contentType != "text/plain" || string(body) != "shared notes" {
		t.Fatalf("GET %s = %d %q %q, want 200 text/plain with the bytes", url, status, contentType, body)
	}
}

func TestGetFile_RequiresChannelMembership(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice)
	id := f.attach(ch, "notes.txt", "text/plain", []byte("shared notes"))
	f.exec(`UPDATE files.files SET status = 'ready', ref = 'DOC-1001', pages = 3 WHERE id = $1`, id)

	if _, err := f.svc.GetFile(t.Context(), f.user(f.bob), id); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("GetFile by a non-member = %v, want ErrForbidden", err)
	}
	if _, err := f.svc.GetFile(t.Context(), f.user(f.alice), uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("GetFile of an unknown file = %v, want ErrNotFound", err)
	}
	if _, err := f.svc.GetFile(t.Context(), authz.Actor{}, id); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("GetFile without an actor = %v, want ErrForbidden", err)
	}
	got, err := f.svc.GetFile(t.Context(), f.user(f.alice), id)
	if err != nil {
		t.Fatalf("GetFile by a member: %v", err)
	}
	if got.GetStatus() != filesv1.FileStatus_FILE_STATUS_READY || got.GetRef() != "DOC-1001" || got.GetPages() != 3 ||
		got.GetName() != "notes.txt" || got.GetMessageId() == "" {
		t.Fatalf("GetFile = %v, want the ready file with its ref and pages", got)
	}
}

func TestFilenameSanitization_KeepsThaiStripsSeparators(t *testing.T) {
	cases := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"../รายงาน \"Q4\"\\สรุป\n\x00.pdf", `..รายงาน "Q4"สรุป.pdf`},
		{"C:\\Users\\alice\\สัญญา.pdf", "C:Usersaliceสัญญา.pdf"},
		{"it's a \"quoted\" name.txt", "it's a \"quoted\" name.txt"},
		{"tab\there\r\n.md", "tabhere.md"},
		{"  padded  ", "padded"},
		{"invoice\u202Efdp.exe", "invoicefdp.exe"},
		{"bad\xffutf8.txt", "badutf8.txt"},
		{strings.Repeat("ก", 255), strings.Repeat("ก", 255)},
	}
	for _, c := range cases {
		got, err := sanitizeName(c.in)
		if err != nil || got != c.want {
			t.Errorf("sanitizeName(%q) = %q, %v, want %q", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"", "   ", "///", "\\\n\x00", "\u202E", strings.Repeat("ก", 256)} {
		if got, err := sanitizeName(in); !errors.Is(err, errs.ErrInvalid) {
			t.Errorf("sanitizeName(%q) = %q, %v, want ErrInvalid", in, got, err)
		}
	}

	// And end to end: the stored name, the file and the message carry the clean one.
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	resp, err := f.create(f.user(f.alice), ch, "../รายงาน \"Q4\"\\สรุป\n.pdf", "application/pdf", attachment, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.file(uuid.MustParse(resp.GetFileId())).GetName(); got != `..รายงาน "Q4"สรุป.pdf` {
		t.Fatalf("stored name = %q", got)
	}
}

// ---- RetryAnalysis ----------------------------------------------------------

func TestRetryAnalysis_OnlyForFailedFiles(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("private", f.alice)
	alice := f.user(f.alice)
	id := f.attach(ch, "notes.txt", "text/plain", []byte("hello"))
	uploaded := f.topicCount(events.TopicDocUploaded)

	// Analyzing and ready files have nothing to retry.
	if err := f.svc.RetryAnalysis(t.Context(), alice, id); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("RetryAnalysis of an analyzing file = %v, want ErrConflict", err)
	}
	f.exec(`UPDATE files.files SET status = 'ready', ref = 'DOC-1001' WHERE id = $1`, id)
	if err := f.svc.RetryAnalysis(t.Context(), alice, id); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("RetryAnalysis of a ready file = %v, want ErrConflict", err)
	}
	if err := f.svc.RetryAnalysis(t.Context(), alice, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("RetryAnalysis of an unknown file = %v, want ErrNotFound", err)
	}
	if n := f.topicCount(events.TopicDocUploaded); n != uploaded {
		t.Fatalf("a refused retry enqueued doc.uploaded (%d rows, was %d)", n, uploaded)
	}

	// A failed analysis can be retried by a member, and by nobody else.
	f.exec(`UPDATE files.files SET status = 'failed', failure_reason = 'model_unavailable', ref = NULL WHERE id = $1`, id)
	if err := f.svc.RetryAnalysis(t.Context(), f.user(f.bob), id); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("RetryAnalysis by a non-member = %v, want ErrForbidden", err)
	}
	if err := f.svc.RetryAnalysis(t.Context(), alice, id); err != nil {
		t.Fatalf("RetryAnalysis of a failed file: %v", err)
	}
	if got := f.file(id); got.GetStatus() != filesv1.FileStatus_FILE_STATUS_ANALYZING || got.GetFailureReason() != "" {
		t.Fatalf("file after the retry = %v, want ANALYZING without a reason", got)
	}
	if n := f.topicCount(events.TopicDocUploaded); n != uploaded+1 {
		t.Fatalf("%d doc.uploaded events after the retry, want %d", n, uploaded+1)
	}
	if last := f.payloads(events.TopicFileStatusChanged); !strings.Contains(last[len(last)-1], `"FILE_STATUS_ANALYZING"`) {
		t.Fatalf("last file.status_changed = %s, want ANALYZING", last[len(last)-1])
	}
	if err := f.svc.RetryAnalysis(t.Context(), alice, id); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("a second RetryAnalysis = %v, want ErrConflict (it is analyzing again)", err)
	}

	// An upload that was rejected has lost its object: it is uploaded again, not retried.
	zero, err := f.create(alice, ch, "empty.txt", "text/plain", attachment, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	zeroID := uuid.MustParse(zero.GetFileId())
	f.put(zero.GetUploadUrl(), zero.GetHeaders(), nil)
	if _, err := f.svc.CompleteUpload(t.Context(), alice, zeroID); !errors.Is(err, errs.ErrInvalid) {
		t.Fatal(err)
	}
	if err := f.svc.RetryAnalysis(t.Context(), alice, zeroID); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("RetryAnalysis of a rejected upload = %v, want ErrConflict", err)
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
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path+"CreateUpload",
			strings.NewReader(fmt.Sprintf(`{"channelId":%q,"name":"over Connect %s.txt","mime":"text/plain","size":"5","purpose":"UPLOAD_PURPOSE_ATTACHMENT"}`, ch, version)))
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
	if n := f.count(`SELECT count(*) FROM files.files`); n != 0 {
		t.Fatalf("the refused call reached the service: %d files recorded", n)
	}
	status, body = call("1")
	if status != http.StatusOK || !strings.Contains(body, "uploadUrl") {
		t.Fatalf("POST with the header: %d %s; want 200 and an upload URL", status, body)
	}

	// The generated client sends the header itself and reaches every method.
	client := filesv1connect.NewFileServiceClient(srv.Client(), srv.URL)
	created, err := client.CreateUpload(t.Context(), connect.NewRequest(&filesv1.CreateUploadRequest{
		ChannelId: ch.String(), Name: "notes.txt", Mime: "text/plain", Size: 5, Purpose: attachment,
	}))
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if code := f.put(created.Msg.GetUploadUrl(), created.Msg.GetHeaders(), []byte("hello")); code != http.StatusOK {
		t.Fatalf("PUT: status %d", code)
	}
	fileID := created.Msg.GetFileId()
	completed, err := client.CompleteUpload(t.Context(), connect.NewRequest(&filesv1.CompleteUploadRequest{FileId: fileID}))
	if err != nil || completed.Msg.GetFile().GetStatus() != filesv1.FileStatus_FILE_STATUS_ANALYZING {
		t.Fatalf("CompleteUpload = %v, %v, want an analyzing file", completed, err)
	}
	got, err := client.GetFile(t.Context(), connect.NewRequest(&filesv1.GetFileRequest{FileId: fileID}))
	if err != nil || got.Msg.GetFile().GetName() != "notes.txt" {
		t.Fatalf("GetFile = %v, %v", got, err)
	}
	dl, err := client.GetDownloadUrl(t.Context(), connect.NewRequest(&filesv1.GetDownloadUrlRequest{FileId: fileID}))
	if err != nil || !strings.HasPrefix(dl.Msg.GetUrl(), publicURL+blob.Prefix) {
		t.Fatalf("GetDownloadUrl = %v, %v", dl, err)
	}
	f.exec(`UPDATE files.files SET status = 'failed', failure_reason = 'model_unavailable' WHERE id = $1`, uuid.MustParse(fileID))
	if _, err := client.RetryAnalysis(t.Context(), connect.NewRequest(&filesv1.RetryAnalysisRequest{FileId: fileID})); err != nil {
		t.Fatalf("RetryAnalysis: %v", err)
	}
	if _, err := client.GetFile(t.Context(), connect.NewRequest(&filesv1.GetFileRequest{FileId: "not-a-uuid"})); err == nil {
		t.Fatal("GetFile of a malformed id succeeded")
	}
}

// ---- WAV bytes --------------------------------------------------------------

// chunk is a RIFF chunk: its id, its size, its body, padded to an even length.
func chunk(id string, body []byte) []byte {
	b := append([]byte(id), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	b = append(b, body...)
	if len(body)%2 == 1 {
		b = append(b, 0)
	}
	return b
}

// fmtBody is the body of a fmt chunk.
func fmtBody(format, channels uint16, rate uint32, bits uint16) []byte {
	blockAlign := channels * bits / 8
	b := binary.LittleEndian.AppendUint16(nil, format)
	b = binary.LittleEndian.AppendUint16(b, channels)
	b = binary.LittleEndian.AppendUint32(b, rate)
	b = binary.LittleEndian.AppendUint32(b, rate*uint32(blockAlign))
	b = binary.LittleEndian.AppendUint16(b, blockAlign)
	return binary.LittleEndian.AppendUint16(b, bits)
}

// riff wraps chunks in a RIFF container of the given form type.
func riff(form string, chunks ...[]byte) []byte {
	body := []byte(form)
	for _, c := range chunks {
		body = append(body, c...)
	}
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...), body...)
}

// wav is a WAV file of 0.1 s of silence whose fmt chunk says what is given, after any
// chunks that come first.
func wav(format, channels uint16, rate uint32, bits uint16, before ...[]byte) []byte {
	chunks := append(slices.Clone(before), chunk("fmt ", fmtBody(format, channels, rate, bits)), chunk("data", make([]byte, 3200)))
	return riff("WAVE", chunks...)
}
