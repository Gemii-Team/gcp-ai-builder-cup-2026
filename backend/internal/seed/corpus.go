package seed

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"slices"
)

// The corpus is embedded, so the distroless image carries it and `kiwi seed` reads no
// file. Every text in it is synthetic.
//
//go:embed corpus
var corpusFS embed.FS

const (
	// documentName is the file the warehouse document is uploaded as.
	documentName = "bang-na-warehouse-capacity.txt"
	// documentChannel and meetingChannel are the channels the document and the meeting
	// belong to.
	documentChannel = "q4-launch"
	meetingChannel  = "q4-launch"
	// meetingKey names the meeting in the id derived for it.
	meetingKey = "q4-launch-sync"

	kindPublic  = "public"
	kindPrivate = "private"
)

// corpus is everything a run seeds.
type corpus struct {
	channels []channelSpec
	document []byte
	meeting  meetingSpec
}

// channelSpec is a channel and what is said in it. A message's author is the index of a
// member in the order the members were listed, wrapping round when fewer were listed.
type channelSpec struct {
	Name     string        `json:"name"`
	Kind     string        `json:"kind"`
	Messages []messageSpec `json:"messages"`
}

type messageSpec struct {
	Author int    `json:"author"`
	Body   string `json:"body"`
}

// meetingSpec is one finished meeting: its transcript, and the summary and action items
// a summarizer would have found in it. An entry_refs list names transcript entries by
// their position; an owner is a member index, and an action item without one has no
// owner.
type meetingSpec struct {
	Title         string       `json:"title"`
	Entries       []entrySpec  `json:"entries"`
	Summary       string       `json:"summary"`
	Decisions     []itemSpec   `json:"decisions"`
	OpenQuestions []itemSpec   `json:"open_questions"`
	ActionItems   []actionSpec `json:"action_items"`
}

type entrySpec struct {
	Speaker int    `json:"speaker"`
	StartMs int64  `json:"start_ms"`
	EndMs   int64  `json:"end_ms"`
	Text    string `json:"text"`
}

type itemSpec struct {
	Text      string `json:"text"`
	EntryRefs []int  `json:"entry_refs"`
}

type actionSpec struct {
	Title     string `json:"title"`
	Owner     *int   `json:"owner"`
	Due       string `json:"due"`
	EntryRefs []int  `json:"entry_refs"`
}

// loadCorpus reads and checks the embedded corpus. A corpus that breaks its own rules
// is a defect of the binary, so it is reported rather than seeded half way.
func loadCorpus() (corpus, error) {
	var c corpus
	var channels struct {
		Channels []channelSpec `json:"channels"`
	}
	if err := decode("corpus/channels.json", &channels); err != nil {
		return corpus{}, err
	}
	c.channels = channels.Channels
	if err := decode("corpus/q4-launch-sync.json", &c.meeting); err != nil {
		return corpus{}, err
	}
	doc, err := corpusFS.ReadFile("corpus/bang-na-warehouse-capacity.txt")
	if err != nil {
		return corpus{}, fmt.Errorf("seed: read the document: %w", err)
	}
	c.document = doc
	return c, c.validate()
}

func decode(name string, into any) error {
	raw, err := corpusFS.ReadFile(name)
	if err != nil {
		return fmt.Errorf("seed: read %s: %w", name, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("seed: parse %s: %w", name, err)
	}
	return nil
}

func (c corpus) validate() error {
	names := make([]string, 0, len(c.channels))
	for _, ch := range c.channels {
		if ch.Name == "" || len(ch.Messages) == 0 || slices.Contains(names, ch.Name) {
			return fmt.Errorf("seed: channel %q must be named once and say something", ch.Name)
		}
		names = append(names, ch.Name)
		if ch.Kind != kindPublic && ch.Kind != kindPrivate {
			return fmt.Errorf("seed: channel %q is %q, want %q or %q", ch.Name, ch.Kind, kindPublic, kindPrivate)
		}
		for _, m := range ch.Messages {
			if m.Body == "" || m.Author < 0 || (ch.Kind == kindPrivate && m.Author != 0) {
				return fmt.Errorf("seed: a message of channel %q has no text, or a private channel has a message of someone but its first member", ch.Name)
			}
		}
	}
	for _, name := range []string{documentChannel, meetingChannel} {
		if !slices.Contains(names, name) {
			return fmt.Errorf("seed: the corpus puts content in channel %q, which it does not make", name)
		}
	}
	if len(c.document) == 0 || len(c.meeting.Entries) == 0 || c.meeting.Title == "" || c.meeting.Summary == "" {
		return fmt.Errorf("seed: the document and the meeting must have content")
	}
	refs := func(rs []int) bool {
		return !slices.ContainsFunc(rs, func(r int) bool { return r < 0 || r >= len(c.meeting.Entries) })
	}
	for _, it := range slices.Concat(c.meeting.Decisions, c.meeting.OpenQuestions) {
		if !refs(it.EntryRefs) {
			return fmt.Errorf("seed: %q names a transcript entry the meeting does not have", it.Text)
		}
	}
	for _, ai := range c.meeting.ActionItems {
		if ai.Title == "" || len(ai.EntryRefs) == 0 || !refs(ai.EntryRefs) || (ai.Owner != nil && *ai.Owner < 0) {
			return fmt.Errorf("seed: action item %q is not whole", ai.Title)
		}
	}
	return nil
}
