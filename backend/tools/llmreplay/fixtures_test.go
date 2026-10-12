package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A chat fixture that is valid, for the files that must fail for one other reason.
const okChat = `{"match": {"schema": "file_summary"}, "response": {"choices": [{"message": {"content": "ok"}}]}}`

func TestLoadFixtures_RefusesWhatItCannotServe(t *testing.T) {
	const sha1 = "1450cd362435a166b2aea3d709dd29278dd7a804c19082eba4b4a2c0379d2fed"
	cases := []struct {
		name  string
		file  string
		other map[string]string // more files in the same directory
		want  string            // in the error, besides the file name
	}{
		{"not JSON", `{"match": `, nil, "JSON"},
		{"not an object or a list", `"fixture"`, nil, "object"},
		{"trailing text", okChat + ` {}`, nil, "JSON"},
		{"unknown field", `{"match": {}, "responce": {}}`, nil, "responce"},
		{"unknown match field", `{"match": {"schemas": "x"}, "response": {"choices": [{"message": {}}]}}`, nil, "schemas"},
		{"neither a response nor an audio hash", `{"match": {"schema": "x"}}`, nil, "response"},
		{"both kinds", `{"response": {"choices": [{"message": {}}]}, "audio_sha256": "` + sha1 + `", "text": "x"}`, nil, "audio_sha256"},
		{"an unknown stage", `{"match": {"stage": "agent_middle"}, "response": {"choices": [{"message": {}}]}}`, nil, "agent_middle"},
		{"an empty contains matches everything silently", `{"match": {"contains": ""}, "response": {"choices": [{"message": {}}]}}`, nil, "contains"},
		{"an empty contains in a list", `{"match": {"contains": ["a", ""]}, "response": {"choices": [{"message": {}}]}}`, nil, "contains"},
		{"a response that is not an object", `{"response": "text"}`, nil, "response"},
		{"no choice", `{"response": {"choices": []}}`, nil, "choices"},
		{"two choices", `{"response": {"choices": [{"message": {}}, {"message": {}}]}}`, nil, "choices"},
		{"a choice with no message", `{"response": {"choices": [{}]}}`, nil, "message"},
		{"a tool call with no function name", `{"response": {"choices": [{"message": {"tool_calls": [{"function": {"arguments": {}}}]}}]}}`, nil, "name"},
		{"a UUID in the response", `{"response": {"choices": [{"message": {"content": "see 0194c1c0-0000-7000-8000-00000000000a"}}]}}`, nil, "UUID"},
		{"a UUID in a match", `{"match": {"contains": "0194C1C0-0000-7000-8000-00000000000A"}, "response": {"choices": [{"message": {}}]}}`, nil, "UUID"},
		{"a placeholder of an unknown kind", `{"response": {"choices": [{"message": {"content": "{{chunks:leave}}"}}]}}`, nil, "chunks"},
		{"a placeholder with nothing to look for", `{"response": {"choices": [{"message": {"content": "{{uuid:}}"}}]}}`, nil, "uuid"},
		{"an audio hash that is not a SHA-256", `{"audio_sha256": "abc123", "text": "x"}`, nil, "audio_sha256"},
		{"an audio fixture with no text", `{"audio_sha256": "` + sha1 + `"}`, nil, "text"},
		{"an audio fixture with a match", `{"audio_sha256": "` + sha1 + `", "text": "x", "match": {}}`, nil, "match"},
		{"the same audio twice", `{"audio_sha256": "` + sha1 + `", "text": "x"}`, map[string]string{"00-first.json": `{"audio_sha256": "` + strings.ToUpper(sha1) + `", "text": "y"}`}, "00-first.json"},
		{"a negative delay", `{"delay_ms": -1, "response": {"choices": [{"message": {"content": "ok"}}]}}`, nil, "delay_ms"},
		{"a delay of over a minute", `{"delay_ms": 60001, "response": {"choices": [{"message": {"content": "ok"}}]}}`, nil, "delay_ms"},
		{"a delay that is not a whole number of milliseconds", `{"delay_ms": 1.5, "response": {"choices": [{"message": {"content": "ok"}}]}}`, nil, "delay_ms"},
		{"a negative delay of a transcription", `{"audio_sha256": "` + sha1 + `", "text": "x", "delay_ms": -5}`, nil, "delay_ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"50-bad.json": tc.file}
			for k, v := range tc.other {
				files[k] = v
			}
			_, err := loadFixtures(writeFixtures(t, files))
			if err == nil {
				t.Fatal("loadFixtures accepted the directory")
			}
			if !strings.Contains(err.Error(), "50-bad.json") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name 50-bad.json and %q", err, tc.want)
			}
		})
	}

	t.Run("an entry of a list is named by its position", func(t *testing.T) {
		_, err := loadFixtures(writeFixtures(t, map[string]string{"50-list.json": "[" + okChat + ", " + okChat + `, {"match": {"stage": "nope"}, "response": {"choices": [{"message": {}}]}}]`}))
		if err == nil || !strings.Contains(err.Error(), "50-list.json[2]") {
			t.Errorf("error = %v, want the third entry named as 50-list.json[2]", err)
		}
	})

	t.Run("a missing directory names itself", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "no-such-dir")
		if _, err := loadFixtures(dir); err == nil || !strings.Contains(err.Error(), "no-such-dir") {
			t.Errorf("error = %v, want the directory named", err)
		}
	})
}

// kbFixture is a kb_answer fixture whose content is content, a JSON object.
func kbFixture(content string) string {
	return `{"match": {"schema": "kb_answer"}, "response": {"choices": [{"message": {"content": ` + content + `}}]}}`
}

// A kb_answer is shown to people as an answer with its sources, and the product
// quietly repairs a bad one (it drops a marker, renumbers the rest, swaps an absent
// quote for the chunk's own words), so a typo in a fixture would change what the
// screen says without any failure. The loader refuses what it can see in the file.
func TestLoadFixtures_RefusesADishonestKBAnswer(t *testing.T) {
	cite := func(chunk, quote string) string {
		return `{"chunk_id": "{{chunk:` + chunk + `}}", "quote": "` + quote + `"}`
	}
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"a marker beyond the citations", `{"answer_markdown": "Fifteen days [1] and five more [2].", "citations": [` + cite("15 days", "15 days") + `]}`, "[2]"},
		{"a marker of zero", `{"answer_markdown": "Fifteen days [0].", "citations": [` + cite("15 days", "15 days") + `]}`, "[0]"},
		{"a marker when there are no citations", `{"answer_markdown": "Fifteen days [1].", "citations": []}`, "[1]"},
		{"a citation nothing marks", `{"answer_markdown": "Fifteen days [1].", "citations": [` + cite("15 days", "15 days") + `, ` + cite("five days", "five days") + `]}`, "citation 2"},
		{"citations and no marker at all", `{"answer_markdown": "Fifteen days.", "citations": [` + cite("15 days", "15 days") + `]}`, "citation 1"},
		{"citations but no answer text", `{"answer_markdown": "", "citations": [` + cite("15 days", "15 days") + `]}`, "answer_markdown"},
		{"an answer that is not an object", `"just text"`, "kb_answer"},
		{"a citation list that is missing", `{"answer_markdown": "Fifteen days [1]."}`, "citations"},
		{"a citation with no quote", `{"answer_markdown": "Fifteen days [1].", "citations": [{"chunk_id": "{{chunk:15 days}}"}]}`, "quote"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFixtures(writeFixtures(t, map[string]string{"50-bad.json": kbFixture(tc.content)}))
			if err == nil {
				t.Fatal("loadFixtures accepted the fixture")
			}
			if !strings.Contains(err.Error(), "50-bad.json") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name 50-bad.json and %q", err, tc.want)
			}
		})
	}

	honest := map[string]string{
		"a not-found answer":              `{"answer_markdown": "", "citations": []}`,
		"markers in any order, one twice": `{"answer_markdown": "Five [2] and fifteen [1], fifteen again [1].", "citations": [` + cite("15 days", "15 days") + `, ` + cite("five days", "five days") + `]}`,
	}
	for name, content := range honest {
		t.Run("accepts "+name, func(t *testing.T) {
			if _, err := loadFixtures(writeFixtures(t, map[string]string{"50-ok.json": kbFixture(content)})); err != nil {
				t.Errorf("loadFixtures refused an honest answer: %v", err)
			}
		})
	}

	t.Run("only a kb_answer is held to it", func(t *testing.T) {
		other := `{"match": {"schema": "file_summary"}, "response": {"choices": [{"message": {"content": {"summary": "See [3]", "key_points": []}}}]}}`
		if _, err := loadFixtures(writeFixtures(t, map[string]string{"50-other.json": other})); err != nil {
			t.Errorf("a file_summary with a bracketed number was refused: %v", err)
		}
	})
}

func TestLoadFixtures_ReadsTheJSONFilesInNameOrder(t *testing.T) {
	// Two catch-alls: the one in the file that sorts first answers.
	dir := writeFixtures(t, map[string]string{
		"20-second.json": `{"response": {"choices": [{"message": {"content": "second"}}]}}`,
		"10-first.json":  `[{"match": {"schema": "other"}, "response": {"choices": [{"message": {"content": "wrong schema"}}]}}, {"response": {"choices": [{"message": {"content": "first"}}]}}]`,
		"README.md":      "not a fixture, and not JSON",
		"notes.json.bak": "not a fixture either",
	})
	if err := os.Mkdir(filepath.Join(dir, "sub.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	srv, _ := startReplay(t, dir)
	got := postJSON(t, srv, chatPath, chatRequest(opts{schema: "anything"}, user("hello")))
	if got.status != 200 || content(t, got.body) != "first" {
		t.Errorf("status %d, content %q, want the first fixture of the file that sorts first", got.status, got.body)
	}
}

func TestLoadFixtures_AnEmptyDirectoryServesEmbeddingsAndRefusesEverythingElse(t *testing.T) {
	srv, _ := startReplay(t, t.TempDir())
	if got := postJSON(t, srv, chatPath, chatRequest(opts{schema: "file_summary"}, user("hello"))); got.status != 599 {
		t.Errorf("chat: status %d, want 599", got.status)
	}
	if got := upload(t, srv, pcm16WAV(8000, 1)); got.status != 599 {
		t.Errorf("transcription: status %d, want 599", got.status)
	}
	if got := postJSON(t, srv, embedPath, map[string]any{"model": "m", "input": []string{"hello"}}); got.status != 200 {
		t.Errorf("embeddings: status %d, want 200", got.status)
	}
}
