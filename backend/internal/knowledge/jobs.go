package knowledge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "kiwi/gen/kiwi/events/v1"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/outbox"
)

const (
	// consumerIndex is the name of the job; it reads both topics below.
	consumerIndex = "embed.index"
	// chatDebounce is how long a new chat message waits before its window is indexed,
	// so that a burst of messages is embedded once rather than once per message.
	chatDebounce = 30 * time.Second
)

// protoJSON is how events are carried on the outbox: field names as in the .proto,
// which is what outbox.Enqueue reads workspace_id by.
var protoJSON = protojson.MarshalOptions{UseProtoNames: true}

// Jobs returns the consumers of the knowledge domain for the composition root to
// register: embed.index, which indexes the source an embed.index event names, and
// which also turns each chat.message.created into an embed.index event for the
// message's five-minute window, due thirty seconds later. Every message of a window
// makes one, and the runs after the first find nothing changed and embed nothing.
// Jobs panics when a dependency of indexing is missing.
func Jobs(d Deps) []bus.Job {
	s := newService(d)
	if err := s.complete(); err != nil {
		panic(err)
	}
	return []bus.Job{{
		Consumer: consumerIndex,
		Topics:   []string{events.TopicEmbedIndex, events.TopicChatMessageCreated},
		Handler:  outbox.Once(d.DB, consumerIndex, s.prepare),
	}}
}

// prepare is the embed.index handler's work for one message. The indexing itself is
// the slow part and is idempotent, so it runs here, outside the transaction that
// records the message; the only effect that must happen exactly once, the event a
// chat message makes, is the Apply.
func (s *service) prepare(ctx context.Context, m bus.Message) (outbox.Apply, error) {
	switch m.Topic {
	case events.TopicEmbedIndex:
		req, ws, err := decodeIndex(m)
		if err != nil {
			return nil, err
		}
		return nil, s.Index(ctx, authz.Actor{Kind: authz.ActorSystem, WorkspaceID: ws}, req)
	case events.TopicChatMessageCreated:
		return enqueueWindow(m)
	}
	return nil, fmt.Errorf("knowledge: %s: unexpected topic %q: %w", consumerIndex, m.Topic, errs.ErrInvalid)
}

// decodeIndex reads the workspace and the source an embed.index message names.
func decodeIndex(m bus.Message) (IndexRequest, uuid.UUID, error) {
	var ev eventsv1.EmbedIndex
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(m.Data, &ev); err != nil {
		return IndexRequest{}, uuid.Nil, fmt.Errorf("knowledge: decode %s: %v: %w", m.Topic, err, errs.ErrInvalid)
	}
	ws, err := parseID(ev.GetWorkspaceId(), "workspace id")
	if err != nil {
		return IndexRequest{}, uuid.Nil, err
	}
	req := IndexRequest{SourceKind: ev.GetSourceKind()}
	if req.SourceID, err = parseID(ev.GetSourceId(), "source id"); err != nil {
		return IndexRequest{}, uuid.Nil, err
	}
	if ev.GetChannelId() != "" {
		channel, err := parseID(ev.GetChannelId(), "channel id")
		if err != nil {
			return IndexRequest{}, uuid.Nil, err
		}
		req.ChannelID = &channel
	}
	if ev.GetWindowStart() != nil {
		req.WindowStart = new(ev.GetWindowStart().AsTime())
	}
	return req, ws, nil
}

// enqueueWindow turns a chat message into the embed.index event of its window. A message
// that is not indexed itself, one that carries an artifact or no text, makes none.
func enqueueWindow(m bus.Message) (outbox.Apply, error) {
	var ev eventsv1.ChatMessageCreated
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(m.Data, &ev); err != nil {
		return nil, fmt.Errorf("knowledge: decode %s: %v: %w", m.Topic, err, errs.ErrInvalid)
	}
	ws, err := parseID(ev.GetWorkspaceId(), "workspace id")
	if err != nil {
		return nil, err
	}
	channel, err := parseID(ev.GetChannelId(), "channel id")
	if err != nil {
		return nil, err
	}
	msg := ev.GetMessage()
	if msg.GetArtifact() != nil || strings.TrimSpace(msg.GetBody()) == "" || msg.GetCreatedAt() == nil {
		return nil, nil
	}
	w := windowAt(msg.GetCreatedAt().AsTime())
	payload, err := protoJSON.Marshal(&eventsv1.EmbedIndex{
		WorkspaceId: ws.String(), SourceKind: kindChat, SourceId: channel.String(), ChannelId: channel.String(),
		WindowStart: timestamppb.New(w.start),
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: encode %s event: %w", events.TopicEmbedIndex, err)
	}
	return func(ctx context.Context, tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, outbox.Event{
			Topic:     events.TopicEmbedIndex,
			Key:       "chat:" + channel.String() + ":" + w.start.Format(time.RFC3339),
			Payload:   payload,
			NotBefore: time.Now().Add(chatDebounce),
		})
	}, nil
}

func parseID(raw, what string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("knowledge: %s %q is not a UUID: %w", what, raw, errs.ErrInvalid)
	}
	return id, nil
}
