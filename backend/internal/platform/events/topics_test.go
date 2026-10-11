package events_test

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	"kiwi/internal/platform/events"
)

// eventMessages returns every top-level message of the kiwi.events.v1 file, failing
// when there are none so the tests below cannot pass vacuously.
func eventMessages(t *testing.T) []protoreflect.MessageDescriptor {
	t.Helper()
	msgs := eventsv1.File_kiwi_events_v1_events_proto.Messages()
	if msgs.Len() == 0 {
		t.Fatal("kiwi.events.v1 declares no messages")
	}
	out := make([]protoreflect.MessageDescriptor, 0, msgs.Len())
	for i := range msgs.Len() {
		out = append(out, msgs.Get(i))
	}
	return out
}

func TestEveryEventMessageHasATopic(t *testing.T) {
	for _, md := range eventMessages(t) {
		topic, ok := events.TopicFor(dynamicpb.NewMessage(md))
		if !ok || topic == "" {
			t.Errorf("%s has no topic: add it to events.TopicFor, an unrouted event would silently never fire", md.FullName())
		}
	}
}

func TestEveryEventStartsWithWorkspaceID(t *testing.T) {
	for _, md := range eventMessages(t) {
		f := md.Fields().ByNumber(1)
		if f == nil || f.Name() != "workspace_id" || f.Kind() != protoreflect.StringKind || f.Cardinality() == protoreflect.Repeated {
			t.Errorf("%s: field 1 must be `string workspace_id`", md.FullName())
		}
	}
}

func TestTopicFor_MapsEachEventToItsFrozenTopic(t *testing.T) {
	table := []struct {
		msg   proto.Message
		topic string
	}{
		{&eventsv1.ChatMessageCreated{}, "chat.message.created"},
		{&eventsv1.AgentInvoke{}, "agent.invoke"},
		{&eventsv1.DocUploaded{}, "doc.uploaded"},
		{&eventsv1.MeetingIngest{}, "meeting.ingest"},
		{&eventsv1.MeetingTranscribe{}, "meeting.transcribe"},
		{&eventsv1.MeetingTranscriptCheck{}, "meeting.transcript_check"},
		{&eventsv1.EmbedIndex{}, "embed.index"},
		{&eventsv1.TaskDraftRequested{}, "task.draft"},
		{&eventsv1.TaskChanged{}, "task.changed"},
		{&eventsv1.FileStatusChanged{}, "file.status_changed"},
		{&eventsv1.MeetingSummaryReady{}, "meeting.summary_ready"},
		{&eventsv1.MembershipChanged{}, "membership.changed"},
	}
	if got, want := len(table), len(eventMessages(t)); got != want {
		t.Fatalf("table covers %d messages, kiwi.events.v1 declares %d", got, want)
	}
	seen := map[string]bool{}
	for _, tc := range table {
		got, ok := events.TopicFor(tc.msg)
		if !ok || got != tc.topic {
			t.Errorf("TopicFor(%T) = %q, %v; want %q, true", tc.msg, got, ok, tc.topic)
		}
		if seen[tc.topic] {
			t.Errorf("topic %q is routed to more than one event", tc.topic)
		}
		seen[tc.topic] = true
	}
}

func TestTopicFor_NonEventMessagesHaveNoTopic(t *testing.T) {
	if topic, ok := events.TopicFor(&chatv1.Message{}); ok {
		t.Errorf("TopicFor(chat.v1.Message) = %q, true; only kiwi.events.v1 messages are routed", topic)
	}
	if topic, ok := events.TopicFor(nil); ok {
		t.Errorf("TopicFor(nil) = %q, true", topic)
	}
}
