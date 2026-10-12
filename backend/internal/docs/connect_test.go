package docs

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	filesv1 "kiwi/gen/kiwi/files/v1"
	"kiwi/internal/platform/errs"
)

// The reasons an upload is refused with: the codes the web app turns into words.
var rejectionReasons = []string{reasonEmpty, reasonTooLarge, reasonUnsupportedType, reasonUnsupportedAudio, reasonSizeMismatch}

// A refusal of CreateUpload that has a reason of its own carries it, so the web app can
// say it; one that has none (no purpose is a client bug) is a plain invalid.
func TestCheckDeclared_RefusalNamesItsReason(t *testing.T) {
	const docx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	cases := []struct {
		name    string
		purpose filesv1.UploadPurpose
		mime    string
		size    int64
		reason  string // "" for a refusal that names none
	}{
		{"an attachment of a type that is not accepted", attachment, docx, 100, reasonUnsupportedType},
		{"an attachment with no type", attachment, "", 100, reasonUnsupportedType},
		{"an attachment whose type is no media type", attachment, "pdf", 100, reasonUnsupportedType},
		{"a recording that is not WAV", recording, "audio/mpeg", 100, reasonUnsupportedAudio},
		{"a recording of a video", recording, "video/mp4", 100, reasonUnsupportedAudio},
		{"an empty attachment", attachment, "text/plain", 0, reasonEmpty},
		{"a negative size", attachment, "text/plain", -1, reasonEmpty},
		{"an attachment over its limit", attachment, "application/pdf", MaxAttachmentBytes + 1, reasonTooLarge},
		{"a recording over its limit", recording, "audio/wav", MaxRecordingBytes + 1, reasonTooLarge},
		{"no purpose", filesv1.UploadPurpose_UPLOAD_PURPOSE_UNSPECIFIED, "application/pdf", 100, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := checkDeclared(c.purpose, c.mime, c.size)
			if !errors.Is(err, errs.ErrInvalid) {
				t.Fatalf("checkDeclared = %v, want ErrInvalid", err)
			}
			var rej *uploadRejection
			named := errors.As(err, &rej)
			switch {
			case c.reason == "" && named:
				t.Errorf("checkDeclared = %v, want a refusal that names no reason, got %q", err, rej.reason)
			case c.reason != "" && !named:
				t.Errorf("checkDeclared = %v, want a refusal naming %q", err, c.reason)
			case named && rej.reason != c.reason:
				t.Errorf("checkDeclared names %q, want %q", rej.reason, c.reason)
			}
		})
	}
}

// What a client reads of a refusal is made from its reason alone: the detail of the
// service's error, which holds sizes and whatever the object store said, never reaches it.
func TestRefuse_AnUploadRejectionIsInvalidArgumentWithItsReasonAndAFixedSentence(t *testing.T) {
	const detail = "the file is 26214401 bytes at /var/lib/kiwi/blob/uploads/ws/file and postgres://kiwi_app:secret@db:5432/kiwi said no"
	for _, reason := range rejectionReasons {
		t.Run(reason, func(t *testing.T) {
			// Wrapped, as a caller of the service may leave it.
			var coded *connect.Error
			if got := refuse(fmt.Errorf("docs: complete: %w", rejection(reason, detail))); !errors.As(got, &coded) {
				t.Fatalf("refuse = %v (%T), want a *connect.Error", got, got)
			}
			if coded.Code() != connect.CodeInvalidArgument {
				t.Errorf("code = %v, want invalid_argument", coded.Code())
			}
			sentence, ok := strings.CutPrefix(coded.Message(), "docs: upload rejected ("+reason+"): ")
			if !ok || sentence == "" {
				t.Fatalf("message = %q, want \"docs: upload rejected (%s): \" and a sentence", coded.Message(), reason)
			}
			for _, leak := range []string{"26214401", "/var/lib", "postgres", "secret", "bytes"} {
				if strings.Contains(coded.Message(), leak) {
					t.Errorf("message %q carries %q from the detail", coded.Message(), leak)
				}
			}
			var again *connect.Error
			if !errors.As(refuse(rejection(reason, "another detail entirely")), &again) || again.Message() != coded.Message() {
				t.Errorf("the message depends on the detail: %q and %v", coded.Message(), again)
			}
		})
	}
}

// Anything that is not an upload refusal with a reason this layer can say is the shell's
// to map, unchanged.
func TestRefuse_AnythingElseIsLeftToTheShell(t *testing.T) {
	plain := fmt.Errorf("docs: a file needs a name: %w", errs.ErrInvalid)
	// A file that failed for a reason the upload never gives (a stored one) is not described to the client.
	stored := rejection("no_text_layer", "rejected earlier")
	for name, err := range map[string]error{"an error with no reason": plain, "a reason the upload never gives": stored} {
		t.Run(name, func(t *testing.T) {
			got := refuse(err)
			var coded *connect.Error
			if errors.As(got, &coded) {
				t.Errorf("refuse(%v) = %v, want the shell's to map", err, got)
			}
			if !errors.Is(got, errs.ErrInvalid) {
				t.Errorf("refuse(%v) = %v, want it to stay invalid", err, got)
			}
		})
	}
	if got := refuse(nil); got != nil {
		t.Errorf("refuse(nil) = %v", got)
	}
}
