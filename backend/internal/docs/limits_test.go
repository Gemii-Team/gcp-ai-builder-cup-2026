package docs

import (
	"testing"

	filesv1 "kiwi/gen/kiwi/files/v1"
)

// PurposeOf is how the composition root tells a recording from an attachment when it
// answers the meetings domain about a file: the same rule CompleteUpload applies.
func TestPurposeOf_NamesTheRecordingTypesAndNothingElse(t *testing.T) {
	for mime := range allowedRecordingMIME {
		if got := PurposeOf(mime); got != filesv1.UploadPurpose_UPLOAD_PURPOSE_RECORDING {
			t.Errorf("PurposeOf(%q) = %v, want RECORDING", mime, got)
		}
	}
	for mime := range allowedAttachmentMIME {
		if got := PurposeOf(mime); got != filesv1.UploadPurpose_UPLOAD_PURPOSE_ATTACHMENT {
			t.Errorf("PurposeOf(%q) = %v, want ATTACHMENT", mime, got)
		}
	}
}
