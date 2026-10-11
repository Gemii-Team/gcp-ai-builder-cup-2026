package docs

import (
	"context"
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
)

// Values of files.status.
const (
	statusUploaded  = "uploaded"
	statusAnalyzing = "analyzing"
	statusReady     = "ready"
	statusFailed    = "failed"
)

// headBytes is how much of a recording checkRecording reads.
const headBytes = 4 << 10

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

// requireActor refuses a principal that has no workspace to read a file in.
func requireActor(a authz.Actor) error {
	switch a.Kind {
	case authz.ActorUser, authz.ActorAgent, authz.ActorSystem:
	default:
		return fmt.Errorf("docs: unknown actor kind %q: %w", a.Kind, errs.ErrForbidden)
	}
	if a.WorkspaceID == uuid.Nil || a.EffectiveUserID() == uuid.Nil {
		return fmt.Errorf("docs: actor has no workspace or user: %w", errs.ErrForbidden)
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
// the type must be the declared one, and a recording must really be a PCM 16-bit WAV.
//
// A violation deletes the object and fails the file with the reason, and returns the
// failed file together with an error that wraps errs.ErrInvalid and names the reason.
// An accepted attachment commits, in one transaction, the status analyzing, its
// message in the channel and the events that start its analysis; an accepted recording
// stays uploaded, for the meetings domain to take over. Completing a file twice is
// harmless: the second call returns the file as it then is.
func (s *service) CompleteUpload(ctx context.Context, a authz.Actor, fileID uuid.UUID) (*filesv1.File, error) {
	row, err := s.authorize(ctx, a, fileID, authz.ActFileUpload)
	if err != nil {
		return nil, err
	}
	if a.EffectiveUserID() != row.UploadedBy {
		return nil, fmt.Errorf("docs: file %s was uploaded by someone else: %w", fileID, errs.ErrForbidden)
	}

	switch {
	case row.Status == statusFailed && row.MessageID == nil:
		// Rejected on an earlier call.
		return fileProto(row), rejection(deref(row.FailureReason), "rejected earlier")
	case row.Status != statusUploaded:
		return fileProto(row), nil
	}

	attrs, err := s.blob.Head(ctx, row.BlobKey)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("docs: nothing has been uploaded for file %s yet: %w", fileID, errs.ErrInvalid)
	}
	if err != nil {
		return nil, fmt.Errorf("docs: head upload: %w", err)
	}
	purpose := purposeOf(row.Mime)
	reason, detail, err := s.validate(ctx, row, purpose, attrs)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return s.reject(ctx, a, row, reason, detail)
	}
	return s.accept(ctx, a, row, purpose, attrs.Size)
}

// validate returns why the stored object may not be accepted, as a failure reason and
// a sentence, or "" when it may. An error is a failure to look, not a verdict.
func (s *service) validate(ctx context.Context, row store.FilesFile, purpose filesv1.UploadPurpose, attrs blob.Attrs) (reason, detail string, err error) {
	limit := min(row.Size, maxBytes(purpose))
	switch {
	case attrs.Size == 0:
		return reasonEmpty, "the file has no content", nil
	case attrs.Size > limit:
		return reasonTooLarge, fmt.Sprintf("the file is %d bytes and the limit is %d", attrs.Size, limit), nil
	}
	if mt, err := mediaType(attrs.ContentType); err != nil || mt != row.Mime {
		return reasonUnsupportedType, fmt.Sprintf("the file was stored as %q, not %q", attrs.ContentType, row.Mime), nil
	}
	if purpose != filesv1.UploadPurpose_UPLOAD_PURPOSE_RECORDING {
		return "", "", nil
	}

	rc, err := s.blob.Read(ctx, row.BlobKey)
	if err != nil {
		return "", "", fmt.Errorf("docs: read recording: %w", err)
	}
	defer func() { _ = rc.Close() }()
	head := make([]byte, headBytes)
	n, err := io.ReadFull(rc, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", "", fmt.Errorf("docs: read recording: %w", err)
	}
	if err := checkRecording(head[:n]); err != nil {
		return reasonUnsupportedAudio, err.Error(), nil
	}
	return "", "", nil
}

// reject deletes an upload that failed validation and fails its file. The object goes
// first: if it cannot be deleted the file stays uploaded and the client may complete
// it again.
func (s *service) reject(ctx context.Context, a authz.Actor, row store.FilesFile, reason, detail string) (*filesv1.File, error) {
	if err := s.blob.Delete(ctx, row.BlobKey); err != nil {
		return nil, fmt.Errorf("docs: delete rejected upload: %w", err)
	}
	var failed store.FilesFile
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		failed, err = failFile(ctx, tx, row.ID, reason)
		return err
	})
	if err != nil {
		return nil, err
	}
	slog.WarnContext(ctx, "docs: upload rejected", "file_id", row.ID, "reason", reason)
	return fileProto(failed), rejection(reason, detail)
}

// rejection is the error CompleteUpload returns for an upload it refused.
func rejection(reason, detail string) error {
	return fmt.Errorf("docs: upload rejected (%s): %s: %w", reason, detail, errs.ErrInvalid)
}

// accept records a validated upload. The size is what was really stored.
func (s *service) accept(ctx context.Context, a authz.Actor, row store.FilesFile, purpose filesv1.UploadPurpose, size int64) (*filesv1.File, error) {
	var accepted store.FilesFile
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		// The lock makes two completions of one file take turns: the second finds
		// it no longer uploaded and returns it as the first left it.
		cur, err := q.LockFile(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("docs: lock file: %w", err)
		}
		if cur.Status != statusUploaded {
			accepted = cur
			return nil
		}

		if purpose == filesv1.UploadPurpose_UPLOAD_PURPOSE_RECORDING {
			accepted, err = q.FinishUpload(ctx, store.FinishUploadParams{ID: row.ID, Size: size, Status: statusUploaded})
			return err
		}

		msg, err := s.chat.PostMessageTx(ctx, tx, systemActor(row), &chatv1.SendMessageRequest{
			ChannelId:   row.ChannelID.String(),
			ClientMsgId: row.ID.String(),
			Body:        row.Name,
			Attachments: []*chatv1.FileRef{{FileId: row.ID.String(), Name: row.Name, Mime: row.Mime, Size: size}},
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
		accepted, err = q.FinishUpload(ctx, store.FinishUploadParams{ID: row.ID, Size: size, Status: statusAnalyzing, MessageID: &messageID})
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

// failFile moves a file to failed with reason and announces it. A file that already
// left uploaded and analyzing is returned as it is, and nothing is announced.
func failFile(ctx context.Context, tx pgx.Tx, id uuid.UUID, reason string) (store.FilesFile, error) {
	q := store.New(tx)
	if _, err := q.LockFile(ctx, id); err != nil {
		return store.FilesFile{}, fmt.Errorf("docs: lock file: %w", err)
	}
	failed, err := q.MarkFileFailed(ctx, store.MarkFileFailedParams{ID: id, Reason: &reason})
	if errors.Is(err, pgx.ErrNoRows) {
		cur, err := q.GetFile(ctx, id)
		if err != nil {
			return store.FilesFile{}, fmt.Errorf("docs: read file: %w", err)
		}
		return cur, nil
	}
	if err != nil {
		return store.FilesFile{}, fmt.Errorf("docs: fail file: %w", err)
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

// DownloadURL implements Service. A file whose object is gone (a rejected upload, or
// one never uploaded) has nothing to download and is errs.ErrNotFound.
func (s *service) DownloadURL(ctx context.Context, a authz.Actor, fileID uuid.UUID) (string, error) {
	row, err := s.authorize(ctx, a, fileID, authz.ActFileRead)
	if err != nil {
		return "", err
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
