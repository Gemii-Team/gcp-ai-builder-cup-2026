package chat

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	"kiwi/internal/chat/internal/store"
	"kiwi/internal/platform/authz"
)

// maxMentions bounds the mentions a request may carry; a message body is at most
// 8000 runes, so more than this is not a conversation.
const maxMentions = 100

// resolveMentions keeps the mentions of a message that point at something real and
// leaves the rest as the plain text they are in the body. A USER mention survives
// when its id is a member of the channel, and carries the name the directory holds
// (a client's own label is never trusted). An AGENT mention survives when its label
// (or, failing that, its id) is the registry name of a known agent, and carries that
// name in label and authz.AgentID(name) in id, which is also the author id of the
// agent's own messages. Duplicates collapse; order is the request's.
func (s *service) resolveMentions(ctx context.Context, q *store.Queries, channelID uuid.UUID, in []*chatv1.Mention) ([]*chatv1.Mention, error) {
	var candidates []string // text, not uuid: see ListMemberNames
	for _, m := range in {
		if m.GetKind() != chatv1.MentionKind_MENTION_KIND_USER {
			continue
		}
		if id, err := uuid.Parse(m.GetId()); err == nil {
			candidates = append(candidates, id.String())
		}
	}
	names := make(map[uuid.UUID]string, len(candidates))
	if len(candidates) > 0 {
		rows, err := q.ListMemberNames(ctx, store.ListMemberNamesParams{ChannelID: channelID, UserIds: candidates})
		if err != nil {
			return nil, fmt.Errorf("chat: look up mentioned members: %w", err)
		}
		for _, r := range rows {
			names[r.UserID] = r.Name
		}
	}

	var out []*chatv1.Mention
	seen := make(map[string]bool, len(in))
	for _, m := range in {
		var kept *chatv1.Mention
		switch m.GetKind() {
		case chatv1.MentionKind_MENTION_KIND_USER:
			if id, err := uuid.Parse(m.GetId()); err == nil {
				if name, ok := names[id]; ok {
					kept = &chatv1.Mention{Kind: m.GetKind(), Id: id.String(), Label: name}
				}
			}
		case chatv1.MentionKind_MENTION_KIND_AGENT:
			if name := s.agentName(m); name != "" {
				kept = &chatv1.Mention{Kind: m.GetKind(), Id: authz.AgentID(name).String(), Label: name}
			}
		}
		if kept == nil {
			continue
		}
		if key := kept.GetKind().String() + kept.GetId(); !seen[key] {
			seen[key] = true
			out = append(out, kept)
		}
	}
	return out, nil
}

// agentName is the registry name an AGENT mention refers to, or "" when no known
// agent has it.
func (s *service) agentName(m *chatv1.Mention) string {
	if s.knownAgent == nil {
		return ""
	}
	for _, name := range []string{m.GetLabel(), m.GetId()} {
		if name != "" && s.knownAgent(name) {
			return name
		}
	}
	return ""
}
