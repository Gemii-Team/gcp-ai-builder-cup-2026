// Package docs owns uploaded files: attachments and meeting recordings, their
// analysis and their extracted text. Other domains reach it only through Service.
package docs

import (
	"context"

	"github.com/google/uuid"

	filesv1 "kiwi/gen/kiwi/files/v1"
	"kiwi/internal/platform/authz"
)

// Page is one page of a document as markdown; N counts from 1.
type Page struct {
	N        int
	Markdown string
}

// Service is the docs domain. The comment on each method names the authz.Action it
// checks for the actor on the file's channel.
type Service interface {
	// CreateUpload registers a file and returns the signed URL to upload it to; it
	// needs authz.ActFileUpload.
	CreateUpload(ctx context.Context, a authz.Actor, req *filesv1.CreateUploadRequest) (*filesv1.CreateUploadResponse, error)
	// CompleteUpload validates the uploaded object, records the file and starts its
	// analysis; it needs authz.ActFileUpload.
	CompleteUpload(ctx context.Context, a authz.Actor, fileID uuid.UUID) (*filesv1.File, error)
	// GetFile returns the file with its status and pages; it needs authz.ActFileRead.
	GetFile(ctx context.Context, a authz.Actor, fileID uuid.UUID) (*filesv1.File, error)
	// DownloadURL returns a short-lived signed URL to download the file; it needs
	// authz.ActFileRead.
	DownloadURL(ctx context.Context, a authz.Actor, fileID uuid.UUID) (string, error)
	// RetryAnalysis queues the analysis of a failed file again; it needs
	// authz.ActFileWrite.
	RetryAnalysis(ctx context.Context, a authz.Actor, fileID uuid.UUID) error
	// ExtractPages returns the file's text page by page, for indexing. For a system
	// actor (the indexer) it needs authz.ActKnowledgeIndex; for a user or agent actor
	// it needs the ordinary membership-based authz.ActFileRead on the file's channel.
	ExtractPages(ctx context.Context, a authz.Actor, fileID uuid.UUID) ([]Page, error)
}
