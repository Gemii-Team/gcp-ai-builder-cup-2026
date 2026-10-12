package app

import (
	"fmt"
	"log/slog"
	"slices"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/outbox"
	"kiwi/internal/realtime/frames"
)

// runNamespace derives the id of the run a message starts (UUIDv5).
var runNamespace = uuid.MustParse("6f3c1a52-8d0b-4e7a-9a4e-0c2f5b7d1e90")

// runID is the id of the run that answers the message triggerMessageID for agent: the
// same message always asks for the same run. The id is agents.runs.id, and the agents
// job writes the row under it, so a row that is routed again (a restart mid-job
// redelivers it) finds the run the first delivery made and does not start a second.
func runID(triggerMessageID, agent string) uuid.UUID {
	return uuid.NewSHA1(runNamespace, []byte(triggerMessageID+"/"+agent))
}

// Router is the outbox.Router of the process. It fans every realtime event out through
// frames.FrameFor and adds the one thing no domain does: a chat.message.created whose
// author mentions an agent becomes an agent.invoke, so this is the single place where a
// mention starts a run.
type Router struct{}

var _ outbox.Router = Router{}

// Route implements outbox.Router. The agent.invoke it adds carries the run id (see
// runID), the agent the mention names, the channel and the message that triggered it,
// and the message's author as the person the run acts for: the runtime fails a run
// whose trigger message was not written by that person. Messages written by an agent
// start nothing, so an agent that quotes @assistant in a reply cannot make two agents
// answer each other for ever. The router is deterministic and adds no random id: the
// relay names each extra "<row id>/<index>".
//
// A chat.message.created it cannot read for mentions is logged and dropped, never
// returned: the relay would retry the row for ever and keep it from its durable
// consumers for the sake of a run the person can ask for again.
func (Router) Route(ev outbox.Stored) ([]bus.Message, []outbox.Fanout, error) {
	_, fan, err := frames.Router{}.Route(ev)
	if err != nil {
		return nil, nil, err
	}
	if ev.Topic != events.TopicChatMessageCreated {
		return nil, fan, nil
	}
	invokes, err := agentInvokes(ev)
	if err != nil {
		slog.Error("app: a chat message cannot start an agent run; no run started, the row still goes to the bus",
			"outbox_id", ev.ID, "error", err)
		return nil, fan, nil
	}
	return invokes, fan, nil
}

// agentInvokes returns one agent.invoke per distinct agent the message mentions.
func agentInvokes(ev outbox.Stored) ([]bus.Message, error) {
	var created eventsv1.ChatMessageCreated
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(ev.Payload, &created); err != nil {
		return nil, fmt.Errorf("decode %s: %w", ev.Topic, err)
	}
	msg := created.GetMessage()
	if msg == nil || msg.GetAuthorKind() != chatv1.AuthorKind_AUTHOR_KIND_USER {
		return nil, nil // nothing to answer, or written by an agent
	}
	var agents []string
	for _, m := range msg.GetMentions() {
		if name := m.GetLabel(); m.GetKind() == chatv1.MentionKind_MENTION_KIND_AGENT && name != "" && !slices.Contains(agents, name) {
			agents = append(agents, name)
		}
	}
	if len(agents) == 0 {
		return nil, nil
	}

	trigger, err := uuid.Parse(msg.GetId())
	if err != nil {
		return nil, fmt.Errorf("the message id %q is not a UUID", msg.GetId())
	}
	author, err := uuid.Parse(msg.GetAuthorId())
	if err != nil {
		return nil, fmt.Errorf("the author id %q is not a UUID", msg.GetAuthorId())
	}
	channel := created.GetChannelId()
	if channel == "" {
		channel = msg.GetChannelId()
	}

	out := make([]bus.Message, 0, len(agents))
	for _, name := range agents {
		id := runID(trigger.String(), name).String()
		payload, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&eventsv1.AgentInvoke{
			WorkspaceId: created.GetWorkspaceId(), RunId: id, Agent: name,
			ChannelId: channel, TriggerMessageId: trigger.String(), OnBehalfOf: author.String(),
		})
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", events.TopicAgentInvoke, err)
		}
		out = append(out, bus.Message{Topic: events.TopicAgentInvoke, Key: id, Data: payload})
	}
	return out, nil
}
