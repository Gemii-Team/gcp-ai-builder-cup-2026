package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// What an agent request is, as far as a fixture can tell: a turn that may call a
// tool (tools declared, no tool message yet), or the turn after a tool ran (a tool
// message is present). Everything else has no stage.
const (
	stageNone       = "none"
	stageAgentTool  = "agent_tool"
	stageAgentFinal = "agent_final"
)

// matchSpec says which requests a chat fixture answers. Every field that is set must
// hold; a fixture with no match answers every request that reaches it.
type matchSpec struct {
	// Schema is the name in response_format.json_schema.name.
	Schema string `json:"schema"`
	// Stage is stageAgentTool or stageAgentFinal.
	Stage string `json:"stage"`
	// Contains lists substrings that must all occur in the text of the request's
	// messages (every role, joined by newlines).
	Contains stringList `json:"contains"`
}

// stringList is a JSON string or a list of strings.
type stringList []string

func (l *stringList) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		*l = nil
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*l = stringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("contains is a string or a list of strings")
	}
	*l = many
	return nil
}

// chatFixture answers the chat completions requests its match fits.
type chatFixture struct {
	name     string // file name, with [n] when the file is a list
	match    matchSpec
	response json.RawMessage // a chat.completion body; decoded afresh for every request
}

// audioFixture is the transcript of one audio file.
type audioFixture struct {
	name string
	text string
}

// fixtureSet is everything a fixture directory holds. It is read-only once loaded.
type fixtureSet struct {
	chat  []chatFixture
	audio map[string]audioFixture // by the lower-case hex SHA-256 of the audio
}

// entry is one fixture as the file writes it.
type entry struct {
	Match       *matchSpec      `json:"match"`
	Response    json.RawMessage `json:"response"`
	AudioSHA256 *string         `json:"audio_sha256"`
	Text        *string         `json:"text"`
}

var (
	// uuidPattern finds ids that differ on every database; a fixture never holds one.
	uuidPattern = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	// uuidAtStart reads the UUID a {{uuid:…}} placeholder points at.
	uuidAtStart = regexp.MustCompile(`^(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	sha256Hex   = regexp.MustCompile(`^(?i)[0-9a-f]{64}$`)
	// placeholder is {{kind:argument}}; the argument runs to the first "}}".
	placeholder = regexp.MustCompile(`(?s)\{\{([a-z_]+):(.*?)\}\}`)
)

// loadFixtures reads every *.json file of dir in name order. A file holds one fixture
// or a list of them; the first fixture that fits a request answers it. Anything it
// cannot serve is an error naming the file (and the position in a list).
func loadFixtures(dir string) (*fixtureSet, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read the fixture directory: %w", err)
	}
	fs := &fixtureSet{audio: map[string]audioFixture{}}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, f.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if err := fs.addFile(path, f.Name(), b); err != nil {
			return nil, err
		}
	}
	return fs, nil
}

// addFile adds the fixtures of one file, named name in logs; its errors name path, and
// the position of the fixture when the file is a list.
func (fs *fixtureSet) addFile(path, name string, b []byte) error {
	switch firstByte(b) {
	case '{':
		if err := fs.add(name, b); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	case '[':
		var list []json.RawMessage
		if err := decodeStrict(b, &list); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for i, raw := range list {
			if err := fs.add(fmt.Sprintf("%s[%d]", name, i), raw); err != nil {
				return fmt.Errorf("%s[%d]: %w", path, i, err)
			}
		}
	default:
		return fmt.Errorf("%s: a fixture file holds one fixture object or a list of them", path)
	}
	return nil
}

func firstByte(b []byte) byte {
	if b = bytes.TrimSpace(b); len(b) > 0 {
		return b[0]
	}
	return 0
}

// decodeStrict decodes exactly one JSON value into v and refuses fields v lacks.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid fixture JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("invalid fixture JSON: unexpected data after the first JSON value")
	}
	return nil
}

// add checks one fixture and keeps it. name is how logs and errors call it.
func (fs *fixtureSet) add(name string, raw json.RawMessage) error {
	if id := uuidPattern.Find(raw); id != nil {
		return fmt.Errorf("the fixture holds the UUID %s: ids differ on every database, so a fixture never contains one (use {{chunk:…}} or {{uuid:…}} to echo the id the request carries)", id)
	}
	var e entry
	if err := decodeStrict(raw, &e); err != nil {
		return err
	}
	hasResponse := len(e.Response) > 0 && !bytes.Equal(bytes.TrimSpace(e.Response), []byte("null"))
	switch {
	case hasResponse && e.AudioSHA256 != nil:
		return errors.New("a fixture is either a chat fixture (response) or a transcription fixture (audio_sha256 and text), not both")
	case hasResponse:
		return fs.addChat(name, e)
	case e.AudioSHA256 != nil:
		return fs.addAudio(name, e)
	}
	return errors.New("a fixture needs a response (a chat completion) or an audio_sha256 and a text")
}

func (fs *fixtureSet) addChat(name string, e entry) error {
	if e.Text != nil {
		return errors.New("text belongs to a transcription fixture (audio_sha256), not to a chat fixture")
	}
	var m matchSpec
	if e.Match != nil {
		m = *e.Match
	}
	if m.Stage != "" && m.Stage != stageAgentTool && m.Stage != stageAgentFinal {
		return fmt.Errorf("match.stage %q is neither %q nor %q", m.Stage, stageAgentTool, stageAgentFinal)
	}
	for _, c := range m.Contains {
		if c == "" {
			return errors.New("match.contains has an empty string, which every request contains")
		}
	}
	if err := checkResponse(e.Response); err != nil {
		return fmt.Errorf("response: %w", err)
	}
	fs.chat = append(fs.chat, chatFixture{name: name, match: m, response: e.Response})
	return nil
}

func (fs *fixtureSet) addAudio(name string, e entry) error {
	if e.Match != nil {
		return errors.New("match belongs to a chat fixture; a transcription fixture is keyed by audio_sha256 alone")
	}
	if !sha256Hex.MatchString(*e.AudioSHA256) {
		return fmt.Errorf("audio_sha256 %q is not 64 hexadecimal digits", *e.AudioSHA256)
	}
	if e.Text == nil {
		return errors.New("a transcription fixture needs a text (empty for a recording with no speech)")
	}
	key := strings.ToLower(*e.AudioSHA256)
	if prev, ok := fs.audio[key]; ok {
		return fmt.Errorf("audio_sha256 %s is already the key of %s", key, prev.name)
	}
	fs.audio[key] = audioFixture{name: name, text: *e.Text}
	return nil
}

// checkResponse refuses a response the server could not turn into a completion: it
// must be one object with one choice, a message, tool calls that name a function, and
// only placeholders it knows.
func checkResponse(raw json.RawMessage) error {
	tree, err := decodeTree(raw)
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	body, ok := tree.(map[string]any)
	if !ok {
		return errors.New("must be a chat.completion object")
	}
	choices, _ := body["choices"].([]any)
	if len(choices) != 1 {
		return fmt.Errorf("choices must hold exactly one choice, it holds %d", len(choices))
	}
	choice, _ := choices[0].(map[string]any)
	msg, ok := choice["message"].(map[string]any)
	if !ok {
		return errors.New("choices[0].message must be an object")
	}
	switch msg["content"].(type) {
	case nil, string, map[string]any, []any:
	default:
		return errors.New("choices[0].message.content must be a string, null, or a JSON object or list (written as the string the wire carries)")
	}
	if calls, present := msg["tool_calls"]; present && calls != nil {
		list, ok := calls.([]any)
		if !ok {
			return errors.New("choices[0].message.tool_calls must be a list")
		}
		for i, c := range list {
			call, _ := c.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			if name, _ := fn["name"].(string); name == "" {
				return fmt.Errorf("choices[0].message.tool_calls[%d].function needs a name", i)
			}
			switch fn["arguments"].(type) {
			case nil, string, map[string]any, []any:
			default:
				return fmt.Errorf("choices[0].message.tool_calls[%d].function.arguments must be a string, or a JSON object or list", i)
			}
		}
	}
	_, err = mapStrings(tree, func(s string) (string, error) {
		for _, m := range placeholder.FindAllStringSubmatch(s, -1) {
			if m[1] != "chunk" && m[1] != "uuid" {
				return "", fmt.Errorf("the placeholder {{%s:…}} is unknown (the kinds are chunk and uuid)", m[1])
			}
			if m[2] == "" {
				return "", fmt.Errorf("the placeholder {{%s:}} names nothing to look for", m[1])
			}
		}
		return s, nil
	})
	return err
}

// decodeTree decodes JSON into maps, lists, strings and json.Number, so that a
// number is written back exactly as the fixture wrote it.
func decodeTree(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// mapStrings returns v with f applied to every string value (not to keys).
func mapStrings(v any, f func(string) (string, error)) (any, error) {
	switch t := v.(type) {
	case string:
		return f(t)
	case map[string]any:
		// In key order, so that the first placeholder to fail is always the same one.
		for _, k := range slices.Sorted(maps.Keys(t)) {
			out, err := mapStrings(t[k], f)
			if err != nil {
				return nil, err
			}
			t[k] = out
		}
	case []any:
		for i, e := range t {
			out, err := mapStrings(e, f)
			if err != nil {
				return nil, err
			}
			t[i] = out
		}
	}
	return v, nil
}
