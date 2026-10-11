package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	filesv1 "kiwi/gen/kiwi/files/v1"
	"kiwi/internal/chat"
	"kiwi/internal/docs/internal/store"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/blob"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/llm"
	"kiwi/internal/platform/outbox"
	wavcheck "kiwi/internal/platform/wav" // aliased: the package's tests have a wav helper of their own
)

// Values of files.status.
const (
	statusUploaded  = "uploaded"
	statusAnalyzing = "analyzing"
	statusReady     = "ready"
	statusFailed    = "failed"
)

// headBytes is how much of a recording wav.Check reads: enough for the chunks a WAV
// writer puts before its audio.
const headBytes = 64 << 10

// protoJSON is how events are carried on the outbox: field names as in the .proto,
// which is what outbox.Enqueue reads workspace_id by.
var protoJSON = protojson.MarshalOptions{UseProtoNames: true}

// Deps are what New and Jobs build the docs domain from.
type Deps struct {
	DB *db.DB
	// Authz decides every channel-scoped action.
	Authz *authz.Authz
	// Blob keeps the uploaded bytes.
	Blob blob.Store
	// Chat posts the attachment message and the summary slip, in the transaction that
	// records the file.
	Chat chat.Service
	// LLM summarises files and reads images.
	LLM llm.Client
}

type service struct {
	db    *db.DB
	authz *authz.Authz
	blob  blob.Store
	chat  chat.Service
	llm   llm.Client
}

// New returns the docs domain.
func New(d Deps) Service {
	return &service{db: d.DB, authz: d.Authz, blob: d.Blob, chat: d.Chat, llm: d.LLM}
}

var _ Service = (*service)(nil)

// can asks authz whether a may perform act on the channel.
func (s *service) can(ctx context.Context, a authz.Actor, act authz.Action, channelID uuid.UUID) error {
	return s.authz.Can(ctx, a, act, authz.Resource{ChannelID: channelID})
}

// requireActor refuses a principal that has no workspace to read a file in. A system
// actor is a job: the indexer's has no user behind it, because the events it reacts to
// carry none, and authz decides what it may do. A user or an agent needs the person it
// acts for.
func requireActor(a authz.Actor) error {
	switch a.Kind {
	case authz.ActorUser, authz.ActorAgent:
		if a.EffectiveUserID() == uuid.Nil {
			return fmt.Errorf("docs: actor has no user: %w", errs.ErrForbidden)
		}
	case authz.ActorSystem:
	default:
		return fmt.Errorf("docs: unknown actor kind %q: %w", a.Kind, errs.ErrForbidden)
	}
	if a.WorkspaceID == uuid.Nil {
		return fmt.Errorf("docs: actor has no workspace: %w", errs.ErrForbidden)
	}
	return nil
}

func parseID(raw, what string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("docs: %s %q is not a UUID: %w", what, raw, errs.ErrInvalid)
	}
	return id, nil
}

// blobKey is where the bytes of file id of workspace ws live.
func blobKey(ws, id uuid.UUID) string { return "uploads/" + ws.String() + "/" + id.String() }

// digestOf is the hex sha256 of data, as files.sha256 records it.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// load reads a file; a file of another workspace is as good as missing.
func load(ctx context.Context, d *db.DB, ws, id uuid.UUID) (row store.FilesFile, err error) {
	err = d.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) (err error) {
		row, err = store.New(tx).GetFile(ctx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.FilesFile{}, fmt.Errorf("docs: file %s: %w", id, errs.ErrNotFound)
	}
	if err != nil {
		return store.FilesFile{}, fmt.Errorf("docs: read file: %w", err)
	}
	return row, nil
}

// authorize reads file id and checks that a may perform act on its channel, which
// never changes, so the check holds for the work that follows.
func (s *service) authorize(ctx context.Context, a authz.Actor, id uuid.UUID, act authz.Action) (store.FilesFile, error) {
	if err := requireActor(a); err != nil {
		return store.FilesFile{}, err
	}
	row, err := load(ctx, s.db, a.WorkspaceID, id)
	if err != nil {
		return store.FilesFile{}, err
	}
	if err := s.can(ctx, a, act, row.ChannelID); err != nil {
		return store.FilesFile{}, err
	}
	return row, nil
}

// ---- upload -----------------------------------------------------------------

// CreateUpload implements Service. It records the declared file as uploaded, which is
// what lets CompleteUpload, which names only the file, check the object against what
// was asked for, and returns a signed PUT bound to that type and size.
func (s *service) CreateUpload(ctx context.Context, a authz.Actor, req *filesv1.CreateUploadRequest) (*filesv1.CreateUploadResponse, error) {
	channelID, err := parseID(req.GetChannelId(), "channel id")
	if err != nil {
		return nil, err
	}
	if err := s.can(ctx, a, authz.ActFileUpload, channelID); err != nil {
		return nil, err
	}
	name, err := sanitizeName(req.GetName())
	if err != nil {
		return nil, err
	}
	mt, err := checkDeclared(req.GetPurpose(), req.GetMime(), req.GetSize())
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("docs: new file id: %w", err)
	}
	key := blobKey(a.WorkspaceID, id)
	url, headers, err := s.blob.SignedPut(ctx, key, mt, req.GetSize(), uploadTTL)
	if err != nil {
		return nil, fmt.Errorf("docs: sign upload: %w", err)
	}
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.New(tx).InsertFile(ctx, store.InsertFileParams{
			ID: id, WorkspaceID: a.WorkspaceID, ChannelID: channelID, UploadedBy: a.UserID,
			Name: name, BlobKey: key, Mime: mt, Size: req.GetSize(),
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("docs: record upload: %w", err)
	}
	return &filesv1.CreateUploadResponse{FileId: id.String(), UploadUrl: url, Headers: headers}, nil
}

// CompleteUpload implements Service. It checks the stored object against the upload
// that was declared, because the browser, not the server, put the bytes there: the
// size must be above zero and within both the declared size and the purpose's limit,
// the type must be the declared one, and a recording must really be a PCM 16-bit WAV
// with audio in it. The bytes are read once, under that limit, and what is recorded is
// the size and the sha256 of exactly those bytes: the object can be written again until
// its signed URL expires, so every later read of it (the analysis, the indexer) compares
// what it reads with that hash.
//
// A violation fails the file with the reason, deletes the object and returns the failed
// file together with an error that wraps errs.ErrInvalid. The text of that error is a
// contract, which the web client reads the reason from:
//
//	docs: upload rejected (<reason>): <detail>: invalid
//
// where <reason> is one of empty, too_large, unsupported_type, unsupported_audio and
// size_mismatch (the object changed while it was being read), and <detail> is a sentence
// for people. An accepted attachment commits, in one transaction, the status analyzing,
// its message in the channel and the events that start its analysis; an accepted
// recording stays uploaded, for the meetings domain to take over. Completing a file
// twice is harmless: the second call returns the file as it then is, without reading
// the object again (an accepted recording is bound to the hash it was accepted with),
// or the same rejection.
func (s *service) CompleteUpload(ctx context.Context, a authz.Actor, fileID uuid.UUID) (*filesv1.File, error) {
	row, err := s.authorize(ctx, a, fileID, authz.ActFileUpload)
	if err != nil {
		return nil, err
	}
	if a.EffectiveUserID() != row.UploadedBy {
		return nil, fmt.Errorf("docs: file %s was uploaded by someone else: %w", fileID, errs.ErrForbidden)
	}
	if row.Status != statusUploaded || row.Sha256 != "" {
		return s.settled(ctx, row)
	}

	attrs, err := s.blob.Head(ctx, row.BlobKey)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("docs: nothing has been uploaded for file %s yet: %w", fileID, errs.ErrInvalid)
	}
	if err != nil {
		return nil, fmt.Errorf("docs: head upload: %w", err)
	}
	purpose := purposeOf(row.Mime)
	v, err := s.validate(ctx, row, purpose, attrs)
	if err != nil {
		return nil, err
	}
	if v.reason != "" {
		return s.reject(ctx, a, row, v.reason, v.detail)
	}
	return s.accept(ctx, a, row, purpose, v)
}

// settled is what CompleteUpload answers for a file that is no longer waiting to be
// validated: the file as it is, or, for one that was rejected, the same rejection again.
// A rejected upload whose signed URL has not expired can have an object put back since,
// so it is deleted again.
func (s *service) settled(ctx context.Context, row store.FilesFile) (*filesv1.File, error) {
	if row.Status == statusFailed && row.MessageID == nil {
		if err := s.blob.Delete(ctx, row.BlobKey); err != nil {
			slog.ErrorContext(ctx, "docs: delete a rejected upload", "file_id", row.ID, "error", err)
		}
		return fileProto(row), rejection(deref(row.FailureReason), "rejected earlier")
	}
	return fileProto(row), nil
}

// verdict is what validation found out: why the upload may not be accepted, as a failure
// reason and a sentence, or, when it may, the size and the sha256 of its bytes.
type verdict struct {
	reason, detail string
	size           int64
	sha256         string
}

// validate reads the stored object once and returns the verdict on it. An error is a
// failure to look, not a verdict.
func (s *service) validate(ctx context.Context, row store.FilesFile, purpose filesv1.UploadPurpose, attrs blob.Attrs) (verdict, error) {
	limit := min(row.Size, maxBytes(purpose))
	switch {
	case attrs.Size == 0:
		return verdict{reason: reasonEmpty, detail: "the file has no content"}, nil
	case attrs.Size > limit:
		return verdict{reason: reasonTooLarge, detail: fmt.Sprintf("the file is %d bytes and the limit is %d", attrs.Size, limit)}, nil
	}
	if mt, err := mediaType(attrs.ContentType); err != nil || mt != row.Mime {
		return verdict{reason: reasonUnsupportedType, detail: fmt.Sprintf("the file was stored as %q, not %q", attrs.ContentType, row.Mime)}, nil
	}

	rc, err := s.blob.Read(ctx, row.BlobKey)
	if errors.Is(err, fs.ErrNotExist) {
		return verdict{}, fmt.Errorf("docs: nothing has been uploaded for file %s yet: %w", row.ID, errs.ErrInvalid)
	}
	if err != nil {
		return verdict{}, fmt.Errorf("docs: read upload: %w", err)
	}
	defer func() { _ = rc.Close() }()
	hash, head := sha256.New(), &headWriter{max: headBytes}
	// One byte more than it was stored with: an object that grew since the Head is caught.
	n, err := io.Copy(io.MultiWriter(hash, head), io.LimitReader(rc, attrs.Size+1))
	if err != nil {
		return verdict{}, fmt.Errorf("docs: read upload: %w", err)
	}
	if n != attrs.Size {
		return verdict{reason: reasonSizeMismatch, detail: fmt.Sprintf("the file is %d bytes but was stored with %d: it changed while it was checked", n, attrs.Size)}, nil
	}
	if purpose == filesv1.UploadPurpose_UPLOAD_PURPOSE_RECORDING {
		if _, err := wavcheck.Check(head.buf, n); err != nil {
			return verdict{reason: reasonUnsupportedAudio, detail: err.Error()}, nil
		}
	}
	return verdict{size: n, sha256: hex.EncodeToString(hash.Sum(nil))}, nil
}

// reject fails an upload that did not pass validation and deletes its object. The file
// is locked and must still be waiting: if a concurrent completion has settled it in the
// meantime, that one stands and nothing is deleted. The object goes after the commit; if
// it cannot be deleted the file is failed all the same, nothing serves an object of a
// file that was never accepted, and the next completion tries again.
func (s *service) reject(ctx context.Context, a authz.Actor, row store.FilesFile, reason, detail string) (*filesv1.File, error) {
	var cur store.FilesFile
	rejected := false
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		q := store.New(tx)
		if cur, err = q.LockFile(ctx, row.ID); err != nil {
			return fmt.Errorf("docs: lock file: %w", err)
		}
		if cur.Status != statusUploaded || cur.Sha256 != "" {
			return nil
		}
		if cur, err = q.RejectUpload(ctx, store.RejectUploadParams{ID: row.ID, Reason: &reason}); err != nil {
			return fmt.Errorf("docs: reject upload: %w", err)
		}
		rejected = true
		return enqueueStatus(ctx, tx, cur)
	})
	if err != nil {
		return nil, err
	}
	if !rejected {
		return s.settled(ctx, cur)
	}
	if err := s.blob.Delete(ctx, row.BlobKey); err != nil {
		slog.ErrorContext(ctx, "docs: delete a rejected upload", "file_id", row.ID, "error", err)
	}
	slog.WarnContext(ctx, "docs: upload rejected", "file_id", row.ID, "reason", reason)
	return fileProto(cur), rejection(reason, detail)
}

// rejection is the error CompleteUpload returns for an upload it refused; its text is
// the contract documented there.
func rejection(reason, detail string) error {
	return fmt.Errorf("docs: upload rejected (%s): %s: %w", reason, detail, errs.ErrInvalid)
}

// accept records a validated upload: the size and the sha256 of the bytes that were
// validated.
func (s *service) accept(ctx context.Context, a authz.Actor, row store.FilesFile, purpose filesv1.UploadPurpose, v verdict) (*filesv1.File, error) {
	var accepted store.FilesFile
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		// The lock makes two completions of one file take turns: the second finds
		// it no longer uploaded and returns it as the first left it.
		cur, err := q.LockFile(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("docs: lock file: %w", err)
		}
		if cur.Status != statusUploaded || cur.Sha256 != "" {
			accepted = cur
			return nil
		}

		if purpose == filesv1.UploadPurpose_UPLOAD_PURPOSE_RECORDING {
			accepted, err = q.FinishUpload(ctx, store.FinishUploadParams{ID: row.ID, Size: v.size, Sha256: v.sha256, Status: statusUploaded})
			return err
		}

		msg, err := s.chat.PostMessageTx(ctx, tx, systemActor(row), &chatv1.SendMessageRequest{
			ChannelId:   row.ChannelID.String(),
			ClientMsgId: row.ID.String(),
			Body:        row.Name,
			Attachments: []*chatv1.FileRef{{FileId: row.ID.String(), Name: row.Name, Mime: row.Mime, Size: v.size}},
			AuthorKind:  chatv1.AuthorKind_AUTHOR_KIND_USER.Enum(),
			AuthorId:    proto.String(row.UploadedBy.String()),
		})
		if err != nil {
			return fmt.Errorf("docs: post attachment message: %w", err)
		}
		messageID, err := uuid.Parse(msg.GetId())
		if err != nil {
			return fmt.Errorf("docs: attachment message id: %w", err)
		}
		accepted, err = q.FinishUpload(ctx, store.FinishUploadParams{ID: row.ID, Size: v.size, Sha256: v.sha256, Status: statusAnalyzing, MessageID: &messageID})
		if err != nil {
			return fmt.Errorf("docs: record upload: %w", err)
		}
		return enqueueAnalysis(ctx, tx, accepted)
	})
	if err != nil {
		return nil, err
	}
	return fileProto(accepted), nil
}

// systemActor is the job that acts for the user who uploaded the file.
func systemActor(row store.FilesFile) authz.Actor {
	return authz.Actor{Kind: authz.ActorSystem, WorkspaceID: row.WorkspaceID, UserID: row.UploadedBy}
}

// failAnalysis moves a file that is being analysed to failed with reason and announces
// it. A file in any other status, one that is ready, or rejected, or already failed, is
// returned as it is, and nothing is announced.
func failAnalysis(ctx context.Context, tx pgx.Tx, id uuid.UUID, reason string) (store.FilesFile, error) {
	q := store.New(tx)
	if _, err := q.LockFile(ctx, id); err != nil {
		return store.FilesFile{}, fmt.Errorf("docs: lock file: %w", err)
	}
	failed, err := q.FailAnalysis(ctx, store.FailAnalysisParams{ID: id, Reason: &reason})
	if errors.Is(err, pgx.ErrNoRows) {
		cur, err := q.GetFile(ctx, id)
		if err != nil {
			return store.FilesFile{}, fmt.Errorf("docs: read file: %w", err)
		}
		return cur, nil
	}
	if err != nil {
		return store.FilesFile{}, fmt.Errorf("docs: fail analysis: %w", err)
	}
	return failed, enqueueStatus(ctx, tx, failed)
}

// ---- read -------------------------------------------------------------------

// GetFile implements Service.
func (s *service) GetFile(ctx context.Context, a authz.Actor, fileID uuid.UUID) (*filesv1.File, error) {
	row, err := s.authorize(ctx, a, fileID, authz.ActFileRead)
	if err != nil {
		return nil, err
	}
	return fileProto(row), nil
}

// DownloadURL implements Service. It signs a URL only for a file whose upload was
// accepted, which is a file with the sha256 of the bytes that were validated: an upload
// that was rejected (and whose signed PUT, if it has not expired, can put an object
// back) or has not been completed yet holds bytes nobody has looked at, and is
// errs.ErrNotFound. A file whose analysis failed is downloadable: its bytes passed.
func (s *service) DownloadURL(ctx context.Context, a authz.Actor, fileID uuid.UUID) (string, error) {
	row, err := s.authorize(ctx, a, fileID, authz.ActFileRead)
	if err != nil {
		return "", err
	}
	if row.Sha256 == "" {
		return "", fmt.Errorf("docs: file %s was not accepted (%s): %w", fileID, row.Status, errs.ErrNotFound)
	}
	if _, err := s.blob.Head(ctx, row.BlobKey); errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("docs: file %s has no stored content: %w", fileID, errs.ErrNotFound)
	} else if err != nil {
		return "", fmt.Errorf("docs: head file: %w", err)
	}
	url, err := s.blob.SignedGet(ctx, row.BlobKey, downloadTTL)
	if err != nil {
		return "", fmt.Errorf("docs: sign download: %w", err)
	}
	return url, nil
}

// RetryAnalysis implements Service. Only a failed file whose upload was accepted can be
// retried; one that was rejected has lost its object and needs a new upload.
func (s *service) RetryAnalysis(ctx context.Context, a authz.Actor, fileID uuid.UUID) error {
	row, err := s.authorize(ctx, a, fileID, authz.ActFileWrite)
	if err != nil {
		return err
	}
	return s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.LockFile(ctx, row.ID); err != nil {
			return fmt.Errorf("docs: lock file: %w", err)
		}
		restarted, err := q.RestartAnalysis(ctx, row.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("docs: file %s is not a failed analysis: %w", fileID, errs.ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("docs: restart analysis: %w", err)
		}
		return enqueueAnalysis(ctx, tx, restarted)
	})
}

// ---- events and protobuf ----------------------------------------------------

// enqueue writes ev to the outbox in tx, keyed by the file it concerns.
func enqueue(ctx context.Context, tx pgx.Tx, topic string, file store.FilesFile, ev proto.Message) error {
	payload, err := protoJSON.Marshal(ev)
	if err != nil {
		return fmt.Errorf("docs: encode %s event: %w", topic, err)
	}
	if err := outbox.Enqueue(ctx, tx, outbox.Event{Topic: topic, Key: file.ID.String(), Payload: payload}); err != nil {
		return fmt.Errorf("docs: %w", err)
	}
	return nil
}

// enqueueAnalysis announces a file that waits for its summary: the job that makes it
// and the status the channel shows meanwhile.
func enqueueAnalysis(ctx context.Context, tx pgx.Tx, f store.FilesFile) error {
	if err := enqueue(ctx, tx, events.TopicDocUploaded, f, &eventsv1.DocUploaded{
		WorkspaceId: f.WorkspaceID.String(), ChannelId: f.ChannelID.String(),
		FileId: f.ID.String(), UploadedBy: f.UploadedBy.String(),
	}); err != nil {
		return err
	}
	return enqueueStatus(ctx, tx, f)
}

// enqueueStatus announces the status f has just moved to.
func enqueueStatus(ctx context.Context, tx pgx.Tx, f store.FilesFile) error {
	return enqueue(ctx, tx, events.TopicFileStatusChanged, f, &eventsv1.FileStatusChanged{
		WorkspaceId: f.WorkspaceID.String(), FileId: f.ID.String(), ChannelId: f.ChannelID.String(),
		Status: statusEnum(f.Status), FailureReason: deref(f.FailureReason),
	})
}

func statusEnum(status string) filesv1.FileStatus {
	switch status {
	case statusUploaded:
		return filesv1.FileStatus_FILE_STATUS_UPLOADED
	case statusAnalyzing:
		return filesv1.FileStatus_FILE_STATUS_ANALYZING
	case statusReady:
		return filesv1.FileStatus_FILE_STATUS_READY
	case statusFailed:
		return filesv1.FileStatus_FILE_STATUS_FAILED
	}
	return filesv1.FileStatus_FILE_STATUS_UNSPECIFIED
}

func fileProto(r store.FilesFile) *filesv1.File {
	f := &filesv1.File{
		Id: r.ID.String(), ChannelId: r.ChannelID.String(), Name: r.Name, Mime: r.Mime, Size: r.Size,
		Status: statusEnum(r.Status), FailureReason: deref(r.FailureReason), Ref: deref(r.Ref),
	}
	if r.MessageID != nil {
		f.MessageId = r.MessageID.String()
	}
	if r.Pages != nil {
		f.Pages = *r.Pages
	}
	return f
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
