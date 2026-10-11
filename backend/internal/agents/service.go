// Package agents owns the AI agents, their runs and the runtime that executes them.
// Other domains reach it only through Service.
package agents

import (
	"context"

	"github.com/google/uuid"

	agentsv1 "kiwi/gen/kiwi/agents/v1"
	"kiwi/internal/platform/authz"
)

// Service is the agents domain. A run acts with the rights of the user who started
// it, never more.
type Service interface {
	// StartRun creates a run of the named agent for the actor's question and queues
	// it; when the request names a channel it needs authz.ActKnowledgeSearch there.
	StartRun(ctx context.Context, a authz.Actor, req *agentsv1.StartRunRequest) (*agentsv1.Run, error)
	// GetRun returns the stored state of a run to the user who started it; it is not
	// channel-scoped, so it needs no authz action.
	GetRun(ctx context.Context, a authz.Actor, runID uuid.UUID) (*agentsv1.Run, error)
}
