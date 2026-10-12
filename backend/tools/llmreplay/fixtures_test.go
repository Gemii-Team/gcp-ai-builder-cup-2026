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
