// Package events is the topic catalog of the domain events defined in
// proto/kiwi/events/v1: it ties each event message to the topic it is published
// on, so the outbox relay, the bus consumers and the realtime frame mapper agree.
package events

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	eventsv1 "kiwi/gen/kiwi/events/v1"
)

// Topic names, one per event message. They are also the consumer names of the
// jobs that subscribe to them (see the delta plan's consumer-to-topic map).
const (
	// TopicChatMessageCreated carries eventsv1.ChatMessageCreated.
	TopicChatMessageCreated = "chat.message.created"
	// TopicAgentInvoke carries eventsv1.AgentInvoke.
	TopicAgentInvoke = "agent.invoke"
	// TopicDocUploaded carries eventsv1.DocUploaded.
	TopicDocUploaded = "doc.uploaded"
	// TopicMeetingIngest carries eventsv1.MeetingIngest.
	TopicMeetingIngest = "meeting.ingest"
	// TopicMeetingTranscribe carries eventsv1.MeetingTranscribe.
	TopicMeetingTranscribe = "meeting.transcribe"
	// TopicMeetingTranscriptCheck carries eventsv1.MeetingTranscriptCheck.
	TopicMeetingTranscriptCheck = "meeting.transcript_check"
	// TopicEmbedIndex carries eventsv1.EmbedIndex.
	TopicEmbedIndex = "embed.index"
	// TopicTaskDraft carries eventsv1.TaskDraftRequested.
	TopicTaskDraft = "task.draft"
	// TopicTaskChanged carries eventsv1.TaskChanged.
	TopicTaskChanged = "task.changed"
	// TopicFileStatusChanged carries eventsv1.FileStatusChanged.
	TopicFileStatusChanged = "file.status_changed"
	// TopicMeetingSummaryReady carries eventsv1.MeetingSummaryReady.
	TopicMeetingSummaryReady = "meeting.summary_ready"
	// TopicMembershipChanged carries eventsv1.MembershipChanged.
	TopicMembershipChanged = "membership.changed"
)

// topicByMessage is keyed by the generated types, so removing an event message
// breaks the build; adding one without a topic fails TestEveryEventMessageHasATopic.
var topicByMessage = func() map[protoreflect.FullName]string {
	routes := []struct {
		msg   proto.Message
		topic string
	}{
		{&eventsv1.ChatMessageCreated{}, TopicChatMessageCreated},
		{&eventsv1.AgentInvoke{}, TopicAgentInvoke},
		{&eventsv1.DocUploaded{}, TopicDocUploaded},
		{&eventsv1.MeetingIngest{}, TopicMeetingIngest},
		{&eventsv1.MeetingTranscribe{}, TopicMeetingTranscribe},
		{&eventsv1.MeetingTranscriptCheck{}, TopicMeetingTranscriptCheck},
		{&eventsv1.EmbedIndex{}, TopicEmbedIndex},
		{&eventsv1.TaskDraftRequested{}, TopicTaskDraft},
		{&eventsv1.TaskChanged{}, TopicTaskChanged},
		{&eventsv1.FileStatusChanged{}, TopicFileStatusChanged},
		{&eventsv1.MeetingSummaryReady{}, TopicMeetingSummaryReady},
		{&eventsv1.MembershipChanged{}, TopicMembershipChanged},
	}
	m := make(map[protoreflect.FullName]string, len(routes))
	for _, r := range routes {
		m[r.msg.ProtoReflect().Descriptor().FullName()] = r.topic
	}
	return m
}()

// TopicFor returns the topic an event message is published on, or false when m
// is not a routed kiwi.events.v1 message.
func TopicFor(m proto.Message) (string, bool) {
	if m == nil {
		return "", false
	}
	topic, ok := topicByMessage[m.ProtoReflect().Descriptor().FullName()]
	return topic, ok
}
