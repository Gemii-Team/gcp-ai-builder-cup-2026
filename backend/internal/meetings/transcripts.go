package meetings

import (
	"sync"

	"github.com/google/uuid"

	meetingsv1 "kiwi/gen/kiwi/meetings/v1"
)

// maxCachedTranscripts is how many transcripts the transcribe job keeps in memory.
const maxCachedTranscripts = 8

// transcriptCache holds the transcripts that the transcription model produced for the
// recording meetings the job is working on, so that a delivery that follows a failed
// one does not ask the model again. A pass of the model takes about a fifth of the
// recording's length and runs on a service every user shares; the summary that follows
// it can fail (the bus delivers the message up to five times) without the transcript
// being wrong.
//
// It is a cache and nothing more: it lives in this process, holds at most max
// transcripts, forgets the one used least recently first, and a miss only costs another
// pass. A transcript is keyed by the meeting and the file it was made from. It is
// dropped when the message that asked for it is acknowledged (see forgetSettled), so a
// Retry of a failed meeting transcribes the recording afresh.
type transcriptCache struct {
	mu    sync.Mutex
	max   int
	items []cachedTranscript // least recently used first
}

type cachedTranscript struct {
	meeting, file uuid.UUID
	entries       []*meetingsv1.Entry
}

func newTranscriptCache(max int) *transcriptCache { return &transcriptCache{max: max} }

// get returns the transcript kept for the meeting and file, and counts it as used.
func (c *transcriptCache) get(meeting, file uuid.UUID) ([]*meetingsv1.Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, it := range c.items {
		if it.meeting == meeting && it.file == file {
			copy(c.items[i:], c.items[i+1:])
			c.items[len(c.items)-1] = it
			return it.entries, true
		}
	}
	return nil, false
}

// put keeps the transcript of the meeting and file, instead of one kept before, and
// forgets the least recently used one when there are more than max.
func (c *transcriptCache) put(meeting, file uuid.UUID, entries []*meetingsv1.Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remove(func(it cachedTranscript) bool { return it.meeting == meeting && it.file == file })
	c.items = append(c.items, cachedTranscript{meeting: meeting, file: file, entries: entries})
	if over := len(c.items) - c.max; over > 0 {
		clear(c.items[:over]) // do not keep what was forgotten reachable
		c.items = c.items[over:]
	}
}

// drop forgets every transcript kept for the meeting.
func (c *transcriptCache) drop(meeting uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remove(func(it cachedTranscript) bool { return it.meeting == meeting })
}

func (c *transcriptCache) remove(match func(cachedTranscript) bool) {
	kept := c.items[:0]
	for _, it := range c.items {
		if !match(it) {
			kept = append(kept, it)
		}
	}
	clear(c.items[len(kept):])
	c.items = kept
}
