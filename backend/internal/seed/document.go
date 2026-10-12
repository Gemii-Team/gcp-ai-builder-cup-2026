package seed

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"

	eventsv1 "kiwi/gen/kiwi/events/v1"
	filesv1 "kiwi/gen/kiwi/files/v1"
	"kiwi/internal/docs"
	"kiwi/internal/platform/blob"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/events"
)

// ensureDocument returns the file of the warehouse document in channel: the one an
// earlier run uploaded, or a new upload made the way a person makes one (CreateUpload,
// the bytes to the signed URL, CompleteUpload), which posts the attachment message and
// announces the analysis.
func (s *seeder) ensureDocument(ctx context.Context, channel uuid.UUID, text []byte) (uuid.UUID, error) {
	// Newest first, a page of the history: the corpus keeps the channel far below it.
	history, err := s.chat.ListMessages(ctx, s.owner, channel, 0, historyPage)
	if err != nil {
		return uuid.Nil, fmt.Errorf("seed: read #%s: %w", documentChannel, err)
	}
	for _, m := range history {
		for _, f := range m.GetFiles() {
			if f.GetName() == documentName {
				id, err := uuid.Parse(f.GetFileId())
				if err != nil {
					return uuid.Nil, fmt.Errorf("seed: file id %q: %w", f.GetFileId(), err)
				}
				return id, nil
			}
		}
	}

	const mime = "text/plain"
	up, err := s.docs.CreateUpload(ctx, s.owner, &filesv1.CreateUploadRequest{
		ChannelId: channel.String(), Name: documentName, Mime: mime, Size: int64(len(text)), Purpose: filesv1.UploadPurpose_UPLOAD_PURPOSE_ATTACHMENT,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("seed: create the upload: %w", err)
	}
	if err := putSigned(ctx, s.d.Blob, up.GetUploadUrl(), up.GetHeaders(), text); err != nil {
		return uuid.Nil, err
	}
	id, err := uuid.Parse(up.GetFileId())
	if err != nil {
		return uuid.Nil, fmt.Errorf("seed: file id %q: %w", up.GetFileId(), err)
	}
	if _, err := s.docs.CompleteUpload(ctx, s.owner, id); err != nil {
		return uuid.Nil, fmt.Errorf("seed: complete the upload: %w", err)
	}
	return id, nil
}

// putSigned does what the browser does with a signed upload URL, in this process: the
// blob store serves its own URLs (blob.Local is an http.Handler), so the bytes go
// through the same checks of token, type and size without a network and without needing
// the API to be up.
func putSigned(ctx context.Context, store blob.Store, url string, headers map[string]string, data []byte) error {
	h, ok := store.(http.Handler)
	if !ok {
		return errors.New("seed: the blob store does not serve its own signed URLs, so the document cannot be uploaded")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("seed: upload request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := &statusWriter{header: http.Header{}}
	h.ServeHTTP(w, req)
	if w.code != 0 && w.code != http.StatusOK {
		return fmt.Errorf("seed: the blob store refused the upload with status %d", w.code)
	}
	return nil
}

// statusWriter is a ResponseWriter that keeps only the status.
type statusWriter struct {
	header http.Header
	code   int
}

func (w *statusWriter) Header() http.Header         { return w.header }
func (w *statusWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *statusWriter) WriteHeader(code int)        { w.code = code }

// analyze runs the doc.analyze job for the document, the handler the worker runs for the
// doc.uploaded event, so that the file is ready (with its DOC reference and its summary
// slip in the channel) before it is indexed. A file that is ready is left alone; one
// whose analysis failed is retried. The model call is bounded by analyzeTimeout, and its
// error is returned: nothing is written for a failure that the next run cannot repeat.
func (s *seeder) analyze(ctx context.Context, fileID uuid.UUID) error {
	file, err := s.docs.GetFile(ctx, s.owner, fileID)
	if err != nil {
		return fmt.Errorf("seed: read the document: %w", err)
	}
	switch file.GetStatus() {
	case filesv1.FileStatus_FILE_STATUS_READY:
		return nil
	case filesv1.FileStatus_FILE_STATUS_FAILED:
		if err := s.docs.RetryAnalysis(ctx, s.owner, fileID); err != nil {
			return fmt.Errorf("seed: retry the analysis of the document (it failed with %q): %w", file.GetFailureReason(), err)
		}
	case filesv1.FileStatus_FILE_STATUS_ANALYZING:
	default:
		return fmt.Errorf("seed: the document is %s, not waiting for its analysis", file.GetStatus())
	}

	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&eventsv1.DocUploaded{
		WorkspaceId: s.ws.String(), ChannelId: file.GetChannelId(), FileId: fileID.String(), UploadedBy: s.ids[0].String(),
	})
	if err != nil {
		return fmt.Errorf("seed: encode the doc.uploaded event: %w", err)
	}
	// A fresh id each time: the job is exactly-once per message id, and a retry of a
	// failed run must not find its earlier attempt recorded.
	msg := bus.Message{
		ID: uuid.NewString(), Topic: events.TopicDocUploaded, Key: fileID.String(), Data: data,
		Attrs: map[string]string{"workspace_id": s.ws.String()}, Attempt: 1,
	}
	ctx, cancel := context.WithTimeout(ctx, analyzeTimeout)
	defer cancel()
	if err := docs.Jobs(s.docDeps)[0].Handler(ctx, msg); err != nil {
		return fmt.Errorf("seed: analyse the document: %w", err)
	}
	if file, err = s.docs.GetFile(ctx, s.owner, fileID); err != nil {
		return fmt.Errorf("seed: read the document: %w", err)
	}
	if file.GetStatus() != filesv1.FileStatus_FILE_STATUS_READY {
		return fmt.Errorf("seed: the analysis left the document %s (%s)", file.GetStatus(), file.GetFailureReason())
	}
	return nil
}
