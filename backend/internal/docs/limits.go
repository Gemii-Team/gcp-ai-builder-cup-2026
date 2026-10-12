package docs

import (
	"fmt"
	"mime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	filesv1 "kiwi/gen/kiwi/files/v1"
	"kiwi/internal/platform/errs"
)

// Size limits of an upload (D10). The first is the ceiling for a message attachment,
// the second for a meeting recording; the transcription step chunks a recording, so it
// never has to fit in one model request.
const (
	MaxAttachmentBytes = 25 << 20
	MaxRecordingBytes  = 200 << 20
)

const (
	// uploadTTL is how long the signed PUT of an upload stays valid.
	uploadTTL = 15 * time.Minute
	// downloadTTL is how long a signed download URL stays valid.
	downloadTTL = 5 * time.Minute
	// maxNameRunes is the longest file name an upload may carry.
	maxNameRunes = 255
)

// allowedAttachmentMIME is every type a message attachment may have. The local object
// store serves a download with the type its uploader declared, so nothing that a
// browser runs scripts from (HTML, SVG, XML) may ever be added.
var allowedAttachmentMIME = map[string]bool{
	"application/pdf": true,
	"image/png":       true,
	"image/jpeg":      true,
	"text/plain":      true,
	"text/markdown":   true,
	"text/csv":        true,
}

// allowedRecordingMIME is every type a meeting recording may have: WAV only, because
// the transcription model decodes the content, not the label, and a body it cannot
// decode (an MP3 or MP4 labelled WAV) knocks the transcription service out for every
// user for a minute. The type is therefore only a first filter: wav.Check reads the
// bytes.
var allowedRecordingMIME = map[string]bool{
	"audio/wav":   true,
	"audio/x-wav": true,
	"audio/wave":  true,
}

// Why a file failed: the short codes stored in files.failure_reason, which the web app
// turns into words.
const (
	reasonTooLarge              = "too_large"
	reasonUnsupportedType       = "unsupported_type"
	reasonEmpty                 = "empty"
	reasonUnsupportedAudio      = "unsupported_audio"
	reasonSizeMismatch          = "size_mismatch"
	reasonNoTextLayer           = "no_text_layer"
	reasonUnsupportedByProvider = "unsupported_by_provider"
	reasonUnreadable            = "unreadable"
	reasonModelUnavailable      = "model_unavailable"
)

// PurposeOf says what a stored type is for. The two allowlists share no type, so the
// type CreateUpload accepted tells CompleteUpload the purpose without storing it. The
// composition root uses it to answer the meetings domain, which asks whether a file is
// a recording, from the type of the file docs holds.
func PurposeOf(mediaType string) filesv1.UploadPurpose {
	if allowedRecordingMIME[mediaType] {
		return filesv1.UploadPurpose_UPLOAD_PURPOSE_RECORDING
	}
	return filesv1.UploadPurpose_UPLOAD_PURPOSE_ATTACHMENT
}

// maxBytes is the size limit of purpose.
func maxBytes(purpose filesv1.UploadPurpose) int64 {
	if purpose == filesv1.UploadPurpose_UPLOAD_PURPOSE_RECORDING {
		return MaxRecordingBytes
	}
	return MaxAttachmentBytes
}

// mediaType returns the lower-case type of a Content-Type value, without parameters.
func mediaType(contentType string) (string, error) {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("docs: %q is not a media type: %w", contentType, errs.ErrInvalid)
	}
	return mt, nil
}

// checkDeclared validates what CreateUpload is asked for and returns the media type to
// sign the upload for.
func checkDeclared(purpose filesv1.UploadPurpose, contentType string, size int64) (string, error) {
	var allowed map[string]bool
	switch purpose {
	case filesv1.UploadPurpose_UPLOAD_PURPOSE_ATTACHMENT:
		allowed = allowedAttachmentMIME
	case filesv1.UploadPurpose_UPLOAD_PURPOSE_RECORDING:
		allowed = allowedRecordingMIME
	default:
		return "", fmt.Errorf("docs: an upload needs a purpose: %w", errs.ErrInvalid)
	}
	mt, err := mediaType(contentType)
	if err != nil {
		return "", err
	}
	if !allowed[mt] {
		return "", fmt.Errorf("docs: files of type %q are not accepted here: %w", mt, errs.ErrInvalid)
	}
	if size <= 0 {
		return "", fmt.Errorf("docs: an empty file cannot be uploaded: %w", errs.ErrInvalid)
	}
	if limit := maxBytes(purpose); size > limit {
		return "", fmt.Errorf("docs: a file of %d bytes is over the %d byte limit: %w", size, limit, errs.ErrInvalid)
	}
	return mt, nil
}

// unsafeRune reports whether r is dropped from text that comes from outside, a file
// name or what a model wrote: control characters (the caller decides whether a newline
// or a tab may stay), the line and paragraph separators, and the bidirectional
// overrides and isolates that make text display differently from what it is.
func unsafeRune(r rune) bool {
	switch {
	case unicode.IsControl(r):
		return true
	case r == '\u2028' || r == '\u2029', // line and paragraph separators
		r >= '\u202A' && r <= '\u202E', r >= '\u2066' && r <= '\u2069': // bidirectional overrides
		return true
	}
	return false
}

// cleanText makes text from a model fit to store and show: invalid UTF-8 and NUL, which
// Postgres cannot keep in a jsonb string, are dropped with everything else unsafeRune
// names, except that newlines and tabs stay when keepBreaks is set.
func cleanText(s string, keepBreaks bool) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if keepBreaks && (r == '\n' || r == '\t') {
			return r
		}
		if unsafeRune(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "")))
}

// sanitizeName makes a client-supplied file name safe to store and show: it drops path
// separators, control characters and the bidirectional overrides that make a name
// display differently from what it is, and keeps everything else, Thai and quotes
// included. A name with nothing left or longer than maxNameRunes is errs.ErrInvalid.
func sanitizeName(name string) (string, error) {
	clean := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || unsafeRune(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(name, "")))
	if clean == "" {
		return "", fmt.Errorf("docs: a file needs a name: %w", errs.ErrInvalid)
	}
	if utf8.RuneCountInString(clean) > maxNameRunes {
		return "", fmt.Errorf("docs: a file name is at most %d characters: %w", maxNameRunes, errs.ErrInvalid)
	}
	return clean, nil
}

// headWriter keeps the first max bytes written to it.
type headWriter struct {
	buf []byte
	max int
}

func (h *headWriter) Write(p []byte) (int, error) {
	if room := h.max - len(h.buf); room > 0 {
		h.buf = append(h.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
