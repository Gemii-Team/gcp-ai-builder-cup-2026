package fakegoogle_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"kiwi/internal/platform/google"
	"kiwi/internal/testkit/fakegoogle"
)

func TestListConferences_QueuedRepliesAreConsumedOncePerPoll(t *testing.T) {
	c := fakegoogle.New()
	user := uuid.New()
	c.QueueListConferences(
		fakegoogle.ConferencesReply{Conferences: []google.Conference{{Name: "conferenceRecords/1", Transcripts: []google.TranscriptRef{{Name: "t/1", State: google.StateStarted}}}}},
		fakegoogle.ConferencesReply{Conferences: []google.Conference{{Name: "conferenceRecords/1", Transcripts: []google.TranscriptRef{{Name: "t/1", State: google.StateFileGenerated}}}}},
	)
	ctx := context.Background()

	for i, want := range []google.TranscriptState{google.StateStarted, google.StateFileGenerated} {
		got, err := c.ListConferences(ctx, user, "spaces/abc")
		if err != nil || got[0].Transcripts[0].State != want {
			t.Fatalf("poll %d = (%+v, %v), want state %s", i+1, got, err, want)
		}
	}
	if _, err := c.ListConferences(ctx, user, "spaces/abc"); !errors.Is(err, fakegoogle.ErrUnscripted) {
		t.Fatalf("third poll error = %v, want ErrUnscripted", err)
	}

	calls := c.Calls()
	if len(calls) != 3 || calls[0] != (fakegoogle.Call{Method: "ListConferences", UserID: user, Name: "spaces/abc"}) {
		t.Fatalf("recorded calls = %+v", calls)
	}
}

func TestHandlersAnswerOnceTheQueueIsEmpty_AndErrorsPassThrough(t *testing.T) {
	c := fakegoogle.New()
	consent := &google.ConsentRequiredError{URL: "https://example.test/consent"}
	c.QueueCreateMeetSpace(fakegoogle.SpaceReply{Err: consent})
	c.HandleCreateMeetSpace(func(_ context.Context, _ uuid.UUID, enable bool) (google.Space, error) {
		if !enable {
			t.Error("enableTranscription not forwarded to the handler")
		}
		return google.Space{Name: "spaces/new", MeetingURI: "https://meet.example/abc", Code: "abc"}, nil
	})
	c.HandleListTranscriptEntries(func(_ context.Context, _ uuid.UUID, name string) ([]google.Entry, error) {
		return []google.Entry{{Idx: 0, Speaker: name, Text: "hi"}}, nil
	})
	ctx := context.Background()
	user := uuid.New()

	_, err := c.CreateMeetSpace(ctx, user, true)
	var ce *google.ConsentRequiredError
	if !errors.As(err, &ce) || ce.URL != consent.URL {
		t.Fatalf("first CreateMeetSpace error = %v, want the queued ConsentRequiredError", err)
	}
	if got, err := c.CreateMeetSpace(ctx, user, true); err != nil || got.Name != "spaces/new" {
		t.Fatalf("second CreateMeetSpace = (%+v, %v), want the handler's space", got, err)
	}
	if got, err := c.ListTranscriptEntries(ctx, user, "t/1"); err != nil || got[0].Speaker != "t/1" {
		t.Fatalf("ListTranscriptEntries = (%+v, %v)", got, err)
	}
}

func TestConcurrentCalls_AreRecordedWithoutRaces(t *testing.T) {
	c := fakegoogle.New()
	c.HandleListConferences(func(context.Context, uuid.UUID, string) ([]google.Conference, error) { return nil, nil })

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, _ = c.ListConferences(context.Background(), uuid.New(), "spaces/abc")
			_ = c.Calls()
		})
	}
	wg.Wait()

	if got := len(c.Calls()); got != 20 {
		t.Fatalf("recorded calls = %d, want 20", got)
	}
}
