// Package tasks owns the board: confirmed tasks and the AI-proposed drafts awaiting
// a person. Other domains reach it only through Service.
package tasks

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	tasksv1 "kiwi/gen/kiwi/tasks/v1"
	"kiwi/internal/platform/authz"
)

// ListTasksQuery narrows ListTasks. A nil ChannelID means workspace-wide: every task
// in the channels the actor is a member of. Empty States means every state.
type ListTasksQuery struct {
	ChannelID *uuid.UUID
	States    []tasksv1.TaskState
}

// Service is the tasks domain. The comment on each method names the authz.Action it
// checks for the actor on the task's channel.
type Service interface {
	// ListTasks returns the tasks q selects, ordered by channel, status and position.
	// With q.ChannelID set it needs authz.ActTaskRead on that channel; without it the
	// result is bounded to the channels the actor is a member of.
	ListTasks(ctx context.Context, a authz.Actor, q ListTasksQuery) ([]*tasksv1.Task, error)
	// CreateTask creates a confirmed task, or a draft when the request asks for one;
	// it needs authz.ActTaskWrite, and a draft takes the authz.ActTaskWriteDraft path.
	CreateTask(ctx context.Context, a authz.Actor, req *tasksv1.CreateTaskRequest) (*tasksv1.Task, error)
	// CreateDraft creates an AI-proposed draft task for a person to confirm; it needs
	// authz.ActTaskWriteDraft.
	CreateDraft(ctx context.Context, a authz.Actor, req *tasksv1.CreateTaskRequest) (*tasksv1.Task, error)
	// CreateDraftTx is CreateDraft inside the caller's transaction tx, so a pipeline
	// can commit the draft together with its own effects; it needs
	// authz.ActTaskWriteDraft.
	CreateDraftTx(ctx context.Context, tx pgx.Tx, a authz.Actor, req *tasksv1.CreateTaskRequest) (*tasksv1.Task, error)
	// UpdateTask edits a task's fields; it needs authz.ActTaskWrite.
	UpdateTask(ctx context.Context, a authz.Actor, req *tasksv1.UpdateTaskRequest) (*tasksv1.Task, error)
	// MoveTask moves a task to another status or position; it needs
	// authz.ActTaskWrite.
	MoveTask(ctx context.Context, a authz.Actor, req *tasksv1.MoveTaskRequest) (*tasksv1.Task, error)
	// ConfirmDraft turns a draft into a confirmed task; it needs authz.ActTaskConfirm.
	ConfirmDraft(ctx context.Context, a authz.Actor, id uuid.UUID) (*tasksv1.Task, error)
	// DiscardDraft removes a draft; it needs authz.ActTaskWrite.
	DiscardDraft(ctx context.Context, a authz.Actor, id uuid.UUID) error
}
