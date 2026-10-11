package meetings

import (
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/outbox"
)

// Jobs returns the consumers of the meetings domain for the composition root to
// register: meeting.ingest, which reads a Meet transcript until two reads agree and
// then summarizes it, and meeting.transcript_check, which ends a meeting whose
// transcript never came. The poll (PollMeetings) is what produces both events.
func Jobs(d Deps) []bus.Job {
	p := newPipeline(d)
	return []bus.Job{
		{
			Consumer: consumerIngest,
			Topics:   []string{events.TopicMeetingIngest},
			Handler:  outbox.Once(d.DB, consumerIngest, p.prepareIngest),
		},
		{
			Consumer: consumerTranscriptCheck,
			Topics:   []string{events.TopicMeetingTranscriptCheck},
			Handler:  outbox.Once(d.DB, consumerTranscriptCheck, p.prepareCheck),
		},
	}
}
