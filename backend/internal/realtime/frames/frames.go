// Package frames maps the domain events on the outbox to the realtime frames the
// gateway forwards. It is the only place that knows which event reaches which
// Valkey topic, so the relay (through Router) and the tests agree with it.
package frames

import (
	"fmt"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	realtimev1 "kiwi/gen/kiwi/realtime/v1"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/outbox"
)

// FrameFor returns the frames to publish for the outbox event on topic, whose
// payload is its protojson: one ServerEvent per Valkey topic, encoded as the
// binary protobuf every client receives. A topic without a realtime frame (the
// job topics, or one it does not know) yields no fanout and no error.
//
//	chat.message.created, task.changed,
//	meeting.summary_ready, file.status_changed  ->  ch:{channelId}
//	membership.changed                          ->  u:{userId}, plus ch:{channelId} for ADDED
//
// A payload it cannot route (not decodable, or missing the channel or user a frame
// is addressed to) is errs.ErrInvalid, so the relay retries and logs the row
// instead of dropping it.
func FrameFor(topic string, payload []byte) ([]outbox.Fanout, error) {
	switch topic {
	case events.TopicChatMessageCreated:
		var ev eventsv1.ChatMessageCreated
		if err := decode(topic, payload, &ev); err != nil {
			return nil, err
		}
		if ev.GetMessage() == nil {
			return nil, invalid(topic, "no message")
		}
		return toChannel(topic, ev.GetChannelId(), &realtimev1.ServerEvent{
			Event: &realtimev1.ServerEvent_MessageCreated{MessageCreated: ev.GetMessage()}})

	case events.TopicTaskChanged:
		var ev eventsv1.TaskChanged
		if err := decode(topic, payload, &ev); err != nil {
			return nil, err
		}
		if ev.GetTask() == nil {
			return nil, invalid(topic, "no task")
		}
		return toChannel(topic, ev.GetChannelId(), &realtimev1.ServerEvent{
			Event: &realtimev1.ServerEvent_TaskChanged{TaskChanged: ev.GetTask()}})

	case events.TopicMeetingSummaryReady:
		var ev eventsv1.MeetingSummaryReady
		if err := decode(topic, payload, &ev); err != nil {
			return nil, err
		}
		return toChannel(topic, ev.GetChannelId(), &realtimev1.ServerEvent{
			Event: &realtimev1.ServerEvent_MeetingSummaryReady{MeetingSummaryReady: &realtimev1.MeetingSummaryReady{
				MeetingId: ev.GetMeetingId(), ChannelId: ev.GetChannelId(),
				Status: ev.GetStatus(), FailureReason: ev.GetFailureReason()}}})

	case events.TopicFileStatusChanged:
		var ev eventsv1.FileStatusChanged
		if err := decode(topic, payload, &ev); err != nil {
			return nil, err
		}
		return toChannel(topic, ev.GetChannelId(), &realtimev1.ServerEvent{
			Event: &realtimev1.ServerEvent_FileStatusChanged{FileStatusChanged: &realtimev1.FileStatusChanged{
				FileId: ev.GetFileId(), ChannelId: ev.GetChannelId(),
				Status: ev.GetStatus(), FailureReason: ev.GetFailureReason()}}})

	case events.TopicMembershipChanged:
		return membershipFrames(topic, payload)
	}
	return nil, nil
}

// membershipFrames tells the user whose membership changed (the gateway acts on a
// REMOVED frame on u:{userId}) and, for ADDED, the channel's members. REMOVED has
// no channel frame.
func membershipFrames(topic string, payload []byte) ([]outbox.Fanout, error) {
	var ev eventsv1.MembershipChanged
	if err := decode(topic, payload, &ev); err != nil {
		return nil, err
	}
	userTopic, err := topicOf("u:", ev.GetUserId())
	if err != nil {
		return nil, invalid(topic, "user_id: "+err.Error())
	}
	channelTopic, err := topicOf("ch:", ev.GetChannelId())
	if err != nil {
		return nil, invalid(topic, "channel_id: "+err.Error())
	}
	frame, err := encode(&realtimev1.ServerEvent{
		Event: &realtimev1.ServerEvent_MembershipChanged{MembershipChanged: &realtimev1.MembershipChanged{
			ChannelId: ev.GetChannelId(), UserId: ev.GetUserId(), Change: ev.GetChange()}}})
	if err != nil {
		return nil, err
	}

	switch ev.GetChange() {
	case chatv1.MembershipChange_MEMBERSHIP_CHANGE_ADDED:
		return []outbox.Fanout{{Topic: userTopic, Frame: frame}, {Topic: channelTopic, Frame: frame}}, nil
	case chatv1.MembershipChange_MEMBERSHIP_CHANGE_REMOVED:
		return []outbox.Fanout{{Topic: userTopic, Frame: frame}}, nil
	default:
		return nil, invalid(topic, "change is neither ADDED nor REMOVED")
	}
}

func toChannel(topic, channelID string, ev *realtimev1.ServerEvent) ([]outbox.Fanout, error) {
	to, err := topicOf("ch:", channelID)
	if err != nil {
		return nil, invalid(topic, "channel_id: "+err.Error())
	}
	frame, err := encode(ev)
	if err != nil {
		return nil, err
	}
	return []outbox.Fanout{{Topic: to, Frame: frame}}, nil
}

// topicOf builds prefix + the canonical lower-case form of id, which is how the
// gateway names the topics it subscribes to; any other spelling would reach nobody.
func topicOf(prefix, id string) (string, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return "", err
	}
	return prefix + u.String(), nil
}

// decode reads an event payload. Unknown fields are skipped so an event from a
// newer writer still reaches the clients that understand it.
func decode(topic string, payload []byte, into proto.Message) error {
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(payload, into); err != nil {
		return invalid(topic, err.Error())
	}
	return nil
}

// encode marshals deterministically: a redelivered row is routed again and must
// produce the same frames.
func encode(ev *realtimev1.ServerEvent) ([]byte, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("frames: encode frame: %w", err)
	}
	return b, nil
}

func invalid(topic, why string) error {
	return fmt.Errorf("frames: %s: %s: %w", topic, why, errs.ErrInvalid)
}

// Router is the outbox.Router that fans every realtime event out through FrameFor.
// It adds no bus messages; the app composes it with the agent-mention routing.
type Router struct{}

var _ outbox.Router = Router{}

// Route implements outbox.Router.
func (Router) Route(ev outbox.Stored) ([]bus.Message, []outbox.Fanout, error) {
	fan, err := FrameFor(ev.Topic, ev.Payload)
	return nil, fan, err
}
