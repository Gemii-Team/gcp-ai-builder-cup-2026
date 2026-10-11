package frames_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	filesv1 "kiwi/gen/kiwi/files/v1"
	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
	realtimev1 "kiwi/gen/kiwi/realtime/v1"
	tasksv1 "kiwi/gen/kiwi/tasks/v1"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/outbox"
	"kiwi/internal/realtime/frames"
)

// payload is what the outbox stores and the relay hands to the router: protojson
// with proto field names, as every domain writes it.
func payload(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		t.Fatalf("marshal %T: %v", m, err)
	}
	return b
}

// decode parses a fanned-out frame the way a client does.
func decode(t *testing.T, frame []byte) *realtimev1.ServerEvent {
	t.Helper()
	var ev realtimev1.ServerEvent
	if err := proto.Unmarshal(frame, &ev); err != nil {
		t.Fatalf("frame is not a ServerEvent: %v", err)
	}
	return &ev
}

type wantFan struct {
	topic string
	event *realtimev1.ServerEvent
}

func newID() string { return uuid.NewString() }

func TestFrameFor_MapsEveryRealtimeTopic(t *testing.T) {
	ws, ch, user := newID(), newID(), newID()
	msg := &chatv1.Message{Id: newID(), ChannelId: ch, Seq: 7, Body: "hello", AuthorId: user}
	task := &tasksv1.Task{Id: newID(), ChannelId: ch, Title: "Send the deck", Ref: "TSK-1001", Position: "a0"}
	added := &realtimev1.MembershipChanged{ChannelId: ch, UserId: user, Change: chatv1.MembershipChange_MEMBERSHIP_CHANGE_ADDED}
	removed := &realtimev1.MembershipChanged{ChannelId: ch, UserId: user, Change: chatv1.MembershipChange_MEMBERSHIP_CHANGE_REMOVED}

	table := []struct {
		name  string
		topic string
		event proto.Message
		want  []wantFan
	}{
		{
			name:  "chat message goes to the channel",
			topic: events.TopicChatMessageCreated,
			event: &eventsv1.ChatMessageCreated{WorkspaceId: ws, ChannelId: ch, Message: msg},
			want: []wantFan{{"ch:" + ch, &realtimev1.ServerEvent{
				Event: &realtimev1.ServerEvent_MessageCreated{MessageCreated: msg}}}},
		},
		{
			name:  "task change goes to the channel",
			topic: events.TopicTaskChanged,
			event: &eventsv1.TaskChanged{WorkspaceId: ws, ChannelId: ch, Task: task},
			want: []wantFan{{"ch:" + ch, &realtimev1.ServerEvent{
				Event: &realtimev1.ServerEvent_TaskChanged{TaskChanged: task}}}},
		},
		{
			name:  "meeting summary goes to the channel",
			topic: events.TopicMeetingSummaryReady,
			event: &eventsv1.MeetingSummaryReady{WorkspaceId: ws, MeetingId: "m-1", ChannelId: ch,
				Status: meetingsv1.MeetingStatus_MEETING_STATUS_FAILED, FailureReason: "transcript is empty"},
			want: []wantFan{{"ch:" + ch, &realtimev1.ServerEvent{
				Event: &realtimev1.ServerEvent_MeetingSummaryReady{MeetingSummaryReady: &realtimev1.MeetingSummaryReady{
					MeetingId: "m-1", ChannelId: ch,
					Status: meetingsv1.MeetingStatus_MEETING_STATUS_FAILED, FailureReason: "transcript is empty"}}}}},
		},
		{
			name:  "file status goes to the channel",
			topic: events.TopicFileStatusChanged,
			event: &eventsv1.FileStatusChanged{WorkspaceId: ws, FileId: "f-1", ChannelId: ch,
				Status: filesv1.FileStatus_FILE_STATUS_READY},
			want: []wantFan{{"ch:" + ch, &realtimev1.ServerEvent{
				Event: &realtimev1.ServerEvent_FileStatusChanged{FileStatusChanged: &realtimev1.FileStatusChanged{
					FileId: "f-1", ChannelId: ch, Status: filesv1.FileStatus_FILE_STATUS_READY}}}}},
		},
		{
			name:  "member added goes to the user and the channel",
			topic: events.TopicMembershipChanged,
			event: &eventsv1.MembershipChanged{WorkspaceId: ws, ChannelId: ch, UserId: user,
				Change: chatv1.MembershipChange_MEMBERSHIP_CHANGE_ADDED},
			want: []wantFan{
				{"u:" + user, &realtimev1.ServerEvent{Event: &realtimev1.ServerEvent_MembershipChanged{MembershipChanged: added}}},
				{"ch:" + ch, &realtimev1.ServerEvent{Event: &realtimev1.ServerEvent_MembershipChanged{MembershipChanged: added}}},
			},
		},
		{
			name:  "member removed goes to the user only",
			topic: events.TopicMembershipChanged,
			event: &eventsv1.MembershipChanged{WorkspaceId: ws, ChannelId: ch, UserId: user,
				Change: chatv1.MembershipChange_MEMBERSHIP_CHANGE_REMOVED},
			want: []wantFan{
				{"u:" + user, &realtimev1.ServerEvent{Event: &realtimev1.ServerEvent_MembershipChanged{MembershipChanged: removed}}},
			},
		},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			got, err := frames.FrameFor(tc.topic, payload(t, tc.event))
			if err != nil {
				t.Fatalf("FrameFor: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("FrameFor returned %d fanouts, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				if got[i].Topic != w.topic {
					t.Errorf("fanout %d topic = %q, want %q", i, got[i].Topic, w.topic)
				}
				if ev := decode(t, got[i].Frame); !proto.Equal(ev, w.event) {
					t.Errorf("fanout %d frame = %v, want %v", i, ev, w.event)
				}
			}
		})
	}
}

func TestFrameFor_UnknownAndNonRealtimeTopicsProduceNoFanout(t *testing.T) {
	ws, ch := newID(), newID()
	for _, tc := range []struct {
		topic string
		event proto.Message
	}{
		{events.TopicDocUploaded, &eventsv1.DocUploaded{WorkspaceId: ws, ChannelId: ch, FileId: "f"}},
		{events.TopicEmbedIndex, &eventsv1.EmbedIndex{WorkspaceId: ws, ChannelId: ch}},
		{events.TopicAgentInvoke, &eventsv1.AgentInvoke{WorkspaceId: ws, ChannelId: ch}},
		{"no.such.topic", &eventsv1.TaskChanged{WorkspaceId: ws, ChannelId: ch}},
	} {
		got, err := frames.FrameFor(tc.topic, payload(t, tc.event))
		if err != nil || len(got) != 0 {
			t.Errorf("FrameFor(%q) = %v, %v; want no fanout and no error", tc.topic, got, err)
		}
	}
}

// The relay passes the payload back from a jsonb column: the same JSON with its
// keys reordered, not the bytes that were enqueued.
func TestFrameFor_AcceptsPayloadReorderedByJSONB(t *testing.T) {
	ws, ch := newID(), newID()
	msg := &chatv1.Message{Id: newID(), ChannelId: ch, Seq: 9, Body: "reordered"}
	raw := payload(t, &eventsv1.ChatMessageCreated{WorkspaceId: ws, ChannelId: ch, Message: msg})

	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	reordered, err := json.Marshal(generic) // sorted keys, as jsonb stores them
	if err != nil {
		t.Fatal(err)
	}

	got, err := frames.FrameFor(events.TopicChatMessageCreated, reordered)
	if err != nil || len(got) != 1 {
		t.Fatalf("FrameFor = %v, %v; want one fanout", got, err)
	}
	if ev := decode(t, got[0].Frame); !proto.Equal(ev.GetMessageCreated(), msg) {
		t.Errorf("message = %v, want %v", ev.GetMessageCreated(), msg)
	}
}

func TestFrameFor_NormalisesTheChannelIDInTheTopic(t *testing.T) {
	ch := uuid.New()
	upper := []byte(`{"workspace_id":"` + newID() + `","channel_id":"` + strings.ToUpper(ch.String()) + `","message":{"id":"m","body":"x"}}`)
	got, err := frames.FrameFor(events.TopicChatMessageCreated, upper)
	if err != nil || len(got) != 1 {
		t.Fatalf("FrameFor = %v, %v; want one fanout", got, err)
	}
	if want := "ch:" + ch.String(); got[0].Topic != want {
		t.Errorf("topic = %q, want %q (the gateway subscribes to the canonical lower-case form)", got[0].Topic, want)
	}
}

func TestFrameFor_RejectsPayloadsItCannotRoute(t *testing.T) {
	ws, ch := newID(), newID()
	table := []struct {
		name  string
		topic string
		raw   []byte
	}{
		{"not json", events.TopicChatMessageCreated, []byte(`{`)},
		{"message without a channel", events.TopicChatMessageCreated,
			payload(t, &eventsv1.ChatMessageCreated{WorkspaceId: ws, Message: &chatv1.Message{Id: "m"}})},
		{"channel that is not a uuid", events.TopicTaskChanged,
			payload(t, &eventsv1.TaskChanged{WorkspaceId: ws, ChannelId: "general", Task: &tasksv1.Task{Id: "t"}})},
		{"message event without a message", events.TopicChatMessageCreated,
			payload(t, &eventsv1.ChatMessageCreated{WorkspaceId: ws, ChannelId: ch})},
		{"task event without a task", events.TopicTaskChanged,
			payload(t, &eventsv1.TaskChanged{WorkspaceId: ws, ChannelId: ch})},
		{"membership without a user", events.TopicMembershipChanged,
			payload(t, &eventsv1.MembershipChanged{WorkspaceId: ws, ChannelId: ch, Change: chatv1.MembershipChange_MEMBERSHIP_CHANGE_ADDED})},
		{"membership change unspecified", events.TopicMembershipChanged,
			payload(t, &eventsv1.MembershipChanged{WorkspaceId: ws, ChannelId: ch, UserId: newID()})},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			got, err := frames.FrameFor(tc.topic, tc.raw)
			if err == nil {
				t.Fatalf("FrameFor = %v, nil; want an error, a frame on a wrong topic reaches nobody", got)
			}
			if len(got) != 0 {
				t.Errorf("FrameFor returned fanouts %v alongside its error", got)
			}
		})
	}
}

func TestRouter_FansOutThroughFrameForAndAddsNoBusMessages(t *testing.T) {
	var r outbox.Router = frames.Router{}
	ws, ch := newID(), newID()
	task := &tasksv1.Task{Id: newID(), ChannelId: ch, Title: "t"}
	raw := payload(t, &eventsv1.TaskChanged{WorkspaceId: ws, ChannelId: ch, Task: task})

	extra, fan, err := r.Route(outbox.Stored{Event: outbox.Event{Topic: events.TopicTaskChanged, Payload: raw}})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(extra) != 0 {
		t.Errorf("Route returned bus messages %v, want none", extra)
	}
	if len(fan) != 1 || fan[0].Topic != "ch:"+ch || !proto.Equal(decode(t, fan[0].Frame).GetTaskChanged(), task) {
		t.Errorf("Route fan = %+v, want one task_changed frame on ch:%s", fan, ch)
	}

	extra, fan, err = r.Route(outbox.Stored{Event: outbox.Event{Topic: events.TopicDocUploaded, Payload: raw}})
	if err != nil || len(extra) != 0 || len(fan) != 0 {
		t.Errorf("Route(doc.uploaded) = %v, %v, %v; want nothing", extra, fan, err)
	}

	_, _, err = r.Route(outbox.Stored{Event: outbox.Event{Topic: events.TopicTaskChanged, Payload: []byte(`{`)}})
	if !errors.Is(err, errs.ErrInvalid) {
		t.Errorf("Route(bad payload) error = %v, want errs.ErrInvalid so the row is retried and logged", err)
	}
}
