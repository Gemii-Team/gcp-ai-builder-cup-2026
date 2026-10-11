package knowledge

import (
	"context"
	"errors"

	knowledgev1 "kiwi/gen/kiwi/knowledge/v1"
	"kiwi/internal/platform/authz"
)

// Ask implements Service. Answering from the index is the retrieval task's work and
// this file is replaced by it; until then a Service answers nothing, and says so.
func (s *service) Ask(context.Context, authz.Actor, AskInput) (*knowledgev1.Answer, error) {
	return nil, errors.New("knowledge: Ask is not built yet; this Service indexes and reindexes")
}
