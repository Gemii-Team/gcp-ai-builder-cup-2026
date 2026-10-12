package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"connectrpc.com/connect"

	filesv1 "kiwi/gen/kiwi/files/v1"
	"kiwi/gen/kiwi/files/v1/filesv1connect"
	"kiwi/internal/platform/authz"
)

// Handler serves FileService over Connect and returns the path to mount it at.
// actorFrom reads the actor the API shell's session interceptor put in the request
// context. Domain errors come back unmapped, for the shell to map to Connect codes,
// except an upload refused with a reason, which is answered here (see refuse).
//
// The handler always requires the Connect-Protocol-Version header. With POST-only
// RPCs and the origin check, it is what keeps a cross-site request from reaching a
// cookie-authenticated RPC, and a built http.Handler cannot be given the option
// afterwards, so no caller is left to forget it.
func Handler(svc Service, actorFrom func(context.Context) authz.Actor, opts ...connect.HandlerOption) (string, http.Handler) {
	opts = append([]connect.HandlerOption{connect.WithRequireConnectProtocolHeader()}, opts...)
	return filesv1connect.NewFileServiceHandler(&connectServer{svc: svc, actor: actorFrom}, opts...)
}

type connectServer struct {
	svc   Service
	actor func(context.Context) authz.Actor
}

var _ filesv1connect.FileServiceHandler = (*connectServer)(nil)

// rejectionSentences say, in one fixed sentence per reason, why an upload was refused.
// They are the only words of a refusal besides its reason: the detail of the service's
// error (sizes, what the object store said) is for the log. The web app reads the
// reason, never the sentence, so a sentence can be reworded and a reason cannot.
var rejectionSentences = map[string]string{
	reasonEmpty:            "the file has no content",
	reasonTooLarge:         "the file is over the size limit",
	reasonUnsupportedType:  "files of this type are not accepted",
	reasonUnsupportedAudio: "recordings must be 16-bit PCM WAV files",
	reasonSizeMismatch:     "the file changed while it was checked",
}

// refuse is what the Connect layer answers for an error of the service. An upload
// refused for a reason is InvalidArgument whose text is
//
//	docs: upload rejected (<reason>): <sentence>
//
// which the web client reads the reason from; the API shell's mapper would answer the
// bare sentinel text ("invalid") for it, and every refusal would show the same notice.
// Any other error, and a reason that has no sentence, is the shell's to map.
func refuse(err error) error {
	var rej *uploadRejection
	if !errors.As(err, &rej) {
		return err
	}
	sentence, ok := rejectionSentences[rej.reason]
	if !ok {
		return err
	}
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("docs: upload rejected (%s): %s", rej.reason, sentence))
}

func (s *connectServer) CreateUpload(ctx context.Context, req *connect.Request[filesv1.CreateUploadRequest]) (*connect.Response[filesv1.CreateUploadResponse], error) {
	res, err := s.svc.CreateUpload(ctx, s.actor(ctx), req.Msg)
	if err != nil {
		return nil, refuse(err)
	}
	return connect.NewResponse(res), nil
}

func (s *connectServer) CompleteUpload(ctx context.Context, req *connect.Request[filesv1.CompleteUploadRequest]) (*connect.Response[filesv1.CompleteUploadResponse], error) {
	id, err := parseID(req.Msg.GetFileId(), "file id")
	if err != nil {
		return nil, err
	}
	file, err := s.svc.CompleteUpload(ctx, s.actor(ctx), id)
	if err != nil {
		return nil, refuse(err)
	}
	return connect.NewResponse(&filesv1.CompleteUploadResponse{File: file}), nil
}

func (s *connectServer) GetFile(ctx context.Context, req *connect.Request[filesv1.GetFileRequest]) (*connect.Response[filesv1.GetFileResponse], error) {
	id, err := parseID(req.Msg.GetFileId(), "file id")
	if err != nil {
		return nil, err
	}
	file, err := s.svc.GetFile(ctx, s.actor(ctx), id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&filesv1.GetFileResponse{File: file}), nil
}

func (s *connectServer) GetDownloadUrl(ctx context.Context, req *connect.Request[filesv1.GetDownloadUrlRequest]) (*connect.Response[filesv1.GetDownloadUrlResponse], error) {
	id, err := parseID(req.Msg.GetFileId(), "file id")
	if err != nil {
		return nil, err
	}
	url, err := s.svc.DownloadURL(ctx, s.actor(ctx), id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&filesv1.GetDownloadUrlResponse{Url: url}), nil
}

func (s *connectServer) RetryAnalysis(ctx context.Context, req *connect.Request[filesv1.RetryAnalysisRequest]) (*connect.Response[filesv1.RetryAnalysisResponse], error) {
	id, err := parseID(req.Msg.GetFileId(), "file id")
	if err != nil {
		return nil, err
	}
	if err := s.svc.RetryAnalysis(ctx, s.actor(ctx), id); err != nil {
		return nil, err
	}
	return connect.NewResponse(&filesv1.RetryAnalysisResponse{}), nil
}
